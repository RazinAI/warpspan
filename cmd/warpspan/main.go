// Command warpspan is the node agent: it receives kernel records from every
// process running libwarpspan.so on this machine and exports them as OTLP
// profiles (gRPC) to an OpenTelemetry Collector or Pyroscope.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/zraisan/warpspan/agent/collect"
	"github.com/zraisan/warpspan/agent/otlp"
	"github.com/zraisan/warpspan/agent/server"
)

const version = "0.1.0"

type headerFlag map[string]string

func (h headerFlag) String() string { return fmt.Sprint(map[string]string(h)) }
func (h headerFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || strings.TrimSpace(k) == "" {
		return fmt.Errorf("want key=value, got %q", v)
	}
	h[strings.TrimSpace(k)] = strings.TrimSpace(val)
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
	headers := headerFlag{}
	// OTEL_EXPORTER_OTLP_HEADERS uses the same k=v,k=v format.
	for _, kv := range strings.Split(envOr("", "OTEL_EXPORTER_OTLP_PROFILES_HEADERS", "OTEL_EXPORTER_OTLP_HEADERS"), ",") {
		if kv != "" {
			_ = headers.Set(kv)
		}
	}

	socket := flag.String("socket", envOr("/var/run/warpspan/warpspan.sock", "WARPSPAN_SOCKET"),
		"unix socket the capture libraries connect to (env WARPSPAN_SOCKET)")
	endpoint := flag.String("otlp-endpoint", envOr("localhost:4317", "OTEL_EXPORTER_OTLP_PROFILES_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT"),
		`OTLP/gRPC endpoint: host:port, http://host:port or https://host:port; "none" disables export`)
	insecure := flag.Bool("otlp-insecure", true, "use plaintext for a bare host:port endpoint")
	gzip := flag.Bool("otlp-gzip", false, "gzip-compress export requests")
	flag.Var(headers, "otlp-header", "extra gRPC metadata key=value (repeatable; env OTEL_EXPORTER_OTLP_HEADERS)")
	interval := flag.Duration("interval", 10*time.Second, "how often to export profiles")
	top := flag.Int("print-top", 5, "log the N kernels with the most GPU time each interval (0 = off)")
	nodeName := flag.String("node-name", envOr("", "NODE_NAME"), "added as k8s.node.name to every profile (env NODE_NAME)")
	noDemangle := flag.Bool("no-demangle", false, "keep mangled C++ kernel names")
	debug := flag.Bool("debug", false, "debug logging")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("warpspan", version)
		return
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	var extra []collect.Attr
	if *nodeName != "" {
		extra = append(extra, collect.Attr{Key: "k8s.node.name", Value: *nodeName})
	}
	coll := collect.New(extra)

	var exporter *otlp.Exporter
	if *endpoint != "none" && *endpoint != "" {
		var err error
		exporter, err = otlp.NewExporter(otlp.ExporterConfig{Endpoint: *endpoint, Insecure: *insecure, Headers: headers, Gzip: *gzip})
		if err != nil {
			log.Error("invalid OTLP config", "err", err)
			os.Exit(2)
		}
		defer exporter.Close()
	}

	l, err := server.Listen(*socket)
	if err != nil {
		log.Error("cannot listen", "socket", *socket, "err", err)
		os.Exit(1)
	}
	defer os.Remove(*socket)
	log.Info("warpspan agent started", "version", version, "socket", *socket, "otlp_endpoint", *endpoint, "interval", *interval)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &server.Server{Collector: coll, Log: log}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx, l) }()

	opts := otlp.Options{ScopeVersion: version, Demangle: !*noDemangle}
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			flush(log, coll, exporter, opts, *top)
		case err := <-serveErr:
			if err != nil {
				log.Error("server failed", "err", err)
			}
			flush(log, coll, exporter, opts, *top)
			return
		case <-ctx.Done():
			log.Info("shutting down")
			<-serveErr // wait for connections to drain
			flush(log, coll, exporter, opts, *top)
			return
		}
	}
}

func flush(log *slog.Logger, c *collect.Collector, exp *otlp.Exporter, opts otlp.Options, top int) {
	snap := c.Snapshot(time.Now())
	kernels, linked, dropped := snap.Totals()
	if kernels == 0 && dropped == 0 {
		log.Debug("nothing to export")
		return
	}
	log.Info("interval", "processes", len(snap.Processes), "kernels", kernels, "linked_to_spans", linked, "dropped", dropped)
	if top > 0 {
		logTop(log, snap, top, !opts.Demangle)
	}
	if exp == nil {
		return
	}
	req := otlp.Build(snap, opts)
	if req == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := exp.Export(ctx, req); err != nil {
		log.Error("export failed (data for this interval is dropped)", "err", err)
		return
	}
	log.Debug("exported", "resources", len(req.ResourceProfiles))
}

func logTop(log *slog.Logger, snap *collect.Snapshot, n int, mangled bool) {
	type row struct {
		service, name string
		count, ns     int64
	}
	byName := map[[2]string]*row{}
	for _, p := range snap.Processes {
		svc := p.Process.Resource[0].Value
		for k, st := range p.Samples {
			key := [2]string{svc, k.Name}
			r := byName[key]
			if r == nil {
				r = &row{service: svc, name: k.Name}
				byName[key] = r
			}
			r.count += st.Count
			r.ns += st.TotalNs
		}
	}
	rows := make([]*row, 0, len(byName))
	for _, r := range byName {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ns > rows[j].ns })
	for i, r := range rows {
		if i == n {
			break
		}
		name := r.name
		if !mangled {
			name = otlp.KernelName(name)
		}
		if len(name) > 120 {
			name = name[:117] + "..."
		}
		log.Info("top kernel", "service", r.service, "kernel", name, "launches", r.count, "gpu_time", time.Duration(r.ns))
	}
}
