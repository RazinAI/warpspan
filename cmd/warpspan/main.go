// Command warpspan is the node agent: it receives kernel records from every
// process running libwarpspan.so on this machine and exports them as OTLP
// profiles (gRPC) to an OpenTelemetry Collector or Pyroscope.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/RazinAI/warpspan/agent/collect"
	"github.com/RazinAI/warpspan/agent/group"
	"github.com/RazinAI/warpspan/agent/otlp"
	"github.com/RazinAI/warpspan/agent/server"
)

const version = "0.1.0"

// headerFlag parses key=value pairs (values percent-decoded, as in
// OTEL_EXPORTER_OTLP_HEADERS).
type headerFlag map[string]string

func (h headerFlag) String() string { return fmt.Sprint(map[string]string(h)) }

func (h headerFlag) Set(v string) error {
	for _, kv := range strings.Split(v, ",") {
		k, val, ok := strings.Cut(kv, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return fmt.Errorf("want key=value, got %q", kv)
		}
		dv, err := url.PathUnescape(strings.TrimSpace(val))
		if err != nil {
			return fmt.Errorf("header %q: %w", k, err)
		}
		h[k] = dv
	}
	return nil
}

func envOr(def string, names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return def
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	headers := headerFlag{}
	if v := envOr("", "OTEL_EXPORTER_OTLP_PROFILES_HEADERS", "OTEL_EXPORTER_OTLP_HEADERS"); v != "" {
		if err := headers.Set(v); err != nil {
			log.Error("invalid OTEL_EXPORTER_OTLP_(PROFILES_)HEADERS", "err", err)
			os.Exit(2)
		}
	}
	socket := flag.String("socket", envOr("/var/run/warpspan/warpspan.sock", "WARPSPAN_SOCKET"),
		"unix socket the capture libraries connect to (env WARPSPAN_SOCKET)")
	endpoint := flag.String("otlp-endpoint", envOr("http://localhost:4317", "OTEL_EXPORTER_OTLP_PROFILES_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT"),
		`OTLP/gRPC endpoint: http://host:port (plaintext), https://host:port or host:port (TLS); "none" disables export`)
	flag.Var(headers, "otlp-header", "gRPC metadata key=value, repeatable (env OTEL_EXPORTER_OTLP_HEADERS)")
	interval := flag.Duration("interval", 10*time.Second, "how often to export profiles")
	nodeName := flag.String("node-name", envOr("", "NODE_NAME"), "added as k8s.node.name to every profile (env NODE_NAME)")
	maxSamples := flag.Int("max-samples", collect.DefaultMaxKeysPerProcess,
		"most distinct samples (kernel x span x NVTX labels) per process per interval; past it, spans and labels are dropped")
	spanProfiles := flag.String("span-profiles", string(group.Grouped),
		"per-span GPU profiles: grouped (about one row per span: group profiles, each span's own total, full detail for the slowest), "+
			"full (every span keeps every kernel; cost grows with traffic), off")
	exemplars := flag.Int("exemplars", group.DefaultOptions().Exemplars,
		"grouped mode: slowest spans per process per interval that keep every kernel")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("warpspan", version)
		return
	}

	var extra []collect.Attr
	if *nodeName != "" {
		extra = append(extra, collect.Attr{Key: "k8s.node.name", Value: *nodeName})
	}
	coll := collect.New(extra)
	coll.SetMaxSamples(*maxSamples)
	mode, err := group.ParseMode(*spanProfiles)
	if err != nil {
		log.Error("invalid --span-profiles", "err", err)
		os.Exit(2)
	}
	gopts := group.DefaultOptions()
	gopts.Mode, gopts.Exemplars = mode, max(*exemplars, 0)
	grouper := group.New(gopts)

	var exporter *otlp.Exporter
	if *endpoint != "none" {
		if exporter, err = otlp.NewExporter(*endpoint, headers); err != nil {
			log.Error("invalid OTLP endpoint", "err", err)
			os.Exit(2)
		}
		defer exporter.Close()
	}

	l, err := server.Listen(*socket)
	if err != nil {
		log.Error("cannot listen", "socket", *socket, "err", err)
		os.Exit(1)
	}
	log.Info("warpspan agent started", "version", version, "socket", *socket, "otlp_endpoint", *endpoint, "interval", *interval,
		"span_profiles", mode, "exemplars", gopts.Exemplars)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &server.Server{Collector: coll, Log: log}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx, l) }()

	opts := otlp.Options{ScopeVersion: version}
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	// Each flush must finish within its budget so a hung Collector neither
	// delays the next interval nor blocks shutdown past the k8s grace period.
	flushWithin := func(budget time.Duration, final bool) {
		fctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		flush(fctx, log, coll, grouper, exporter, opts, final)
	}
	for {
		select {
		case <-ticker.C:
			flushWithin(*interval, false)
		case err := <-serveErr:
			if err != nil {
				log.Error("server failed", "err", err)
			}
			flushWithin(shutdownFlushTimeout, true)
			return
		case <-ctx.Done():
			log.Info("shutting down")
			<-serveErr // wait for connections to drain
			flushWithin(shutdownFlushTimeout, true)
			return
		}
	}
}

const shutdownFlushTimeout = 10 * time.Second

func flush(ctx context.Context, log *slog.Logger, c *collect.Collector, g *group.Grouper, exp *otlp.Exporter,
	opts otlp.Options, final bool) {
	raw := c.Snapshot(time.Now())
	snap, gstats := g.Apply(raw, final)
	if len(raw.Processes) == 0 && len(snap.Processes) == 0 {
		return
	}
	var kernels, linked, unlinkedByCap int64
	var dropped uint64
	for _, p := range raw.Processes {
		for k, st := range p.Samples {
			kernels += st.Count
			if k.Linked() {
				linked += st.Count
			}
		}
		dropped += p.Dropped
		unlinkedByCap += p.Unlinked
	}
	var spanRows int
	for _, p := range snap.Processes {
		spanRows += len(p.SpanSamples)
	}
	log.Info("interval", "processes", len(raw.Processes), "kernels", kernels, "linked_to_spans", linked,
		"dropped", dropped, "unlinked_over_cap", unlinkedByCap, "spans", gstats.Spans, "span_groups", gstats.Groups,
		"exemplars", gstats.Exemplars, "spans_held", gstats.HeldBack, "span_rows", spanRows)
	if exp == nil {
		return
	}
	// At most otlp.MaxSamplesPerProfile samples per request keeps each one
	// well under gRPC's common 4 MiB receive limit.
	for _, req := range otlp.BuildRequests(snap, opts) {
		if err := exp.Export(ctx, req); err != nil {
			pid := ""
			for _, a := range req.ResourceProfiles[0].Resource.Attributes {
				if a.Key == "process.pid" {
					pid = a.Value.GetStringValue()
				}
			}
			log.Error("export failed; this part of the interval is dropped", "pid", pid, "err", err)
		}
	}
}
