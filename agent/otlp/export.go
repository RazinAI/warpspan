package otlp

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"time"

	collectorpb "go.opentelemetry.io/proto/otlp/collector/profiles/v1development"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	_ "google.golang.org/grpc/encoding/gzip" // registers the gzip compressor
	"google.golang.org/grpc/metadata"
)

// ExporterConfig configures the OTLP/gRPC profiles exporter.
type ExporterConfig struct {
	// Endpoint is host:port, or http://host:port (plaintext) / https://host:port (TLS).
	Endpoint string
	// Insecure forces plaintext for a bare host:port endpoint.
	Insecure bool
	Headers  map[string]string
	Gzip     bool
	Timeout  time.Duration
}

// Exporter sends ExportProfilesServiceRequests over gRPC.
type Exporter struct {
	conn    *grpc.ClientConn
	client  collectorpb.ProfilesServiceClient
	headers metadata.MD
	timeout time.Duration
	callOpt []grpc.CallOption
}

// NewExporter creates a lazily-connecting exporter (no network I/O here).
func NewExporter(cfg ExporterConfig) (*Exporter, error) {
	target, useTLS, err := parseEndpoint(cfg.Endpoint, cfg.Insecure)
	if err != nil {
		return nil, err
	}
	creds := insecure.NewCredentials()
	if useTLS {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("otlp: create client for %s: %w", target, err)
	}
	e := &Exporter{
		conn:    conn,
		client:  collectorpb.NewProfilesServiceClient(conn),
		headers: metadata.New(cfg.Headers),
		timeout: cfg.Timeout,
	}
	if e.timeout <= 0 {
		e.timeout = 10 * time.Second
	}
	if cfg.Gzip {
		e.callOpt = append(e.callOpt, grpc.UseCompressor("gzip"))
	}
	return e, nil
}

// Export sends one request. A partial success (some profiles rejected) is
// reported as an error so it shows up in logs.
func (e *Exporter) Export(ctx context.Context, req *collectorpb.ExportProfilesServiceRequest) error {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	if len(e.headers) > 0 {
		ctx = metadata.NewOutgoingContext(ctx, e.headers)
	}
	resp, err := e.client.Export(ctx, req, e.callOpt...)
	if err != nil {
		return fmt.Errorf("otlp: export: %w", err)
	}
	if ps := resp.GetPartialSuccess(); ps != nil && (ps.GetRejectedProfiles() > 0 || ps.GetErrorMessage() != "") {
		return fmt.Errorf("otlp: partial success: %d profiles rejected: %s", ps.GetRejectedProfiles(), ps.GetErrorMessage())
	}
	return nil
}

// Close closes the gRPC connection.
func (e *Exporter) Close() error { return e.conn.Close() }

func parseEndpoint(endpoint string, forceInsecure bool) (target string, useTLS bool, err error) {
	switch {
	case strings.HasPrefix(endpoint, "http://"):
		target = strings.TrimPrefix(endpoint, "http://")
	case strings.HasPrefix(endpoint, "https://"):
		target, useTLS = strings.TrimPrefix(endpoint, "https://"), true
	default:
		target, useTLS = endpoint, !forceInsecure
	}
	target = strings.TrimSuffix(target, "/")
	if target == "" || strings.Contains(target, "/") {
		return "", false, fmt.Errorf("otlp: endpoint %q must be host:port (gRPC endpoints have no path)", endpoint)
	}
	return target, useTLS, nil
}
