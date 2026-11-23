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
	"google.golang.org/grpc/metadata"
)

const exportTimeout = 10 * time.Second

// Exporter sends ExportProfilesServiceRequests over gRPC.
type Exporter struct {
	conn    *grpc.ClientConn
	client  collectorpb.ProfilesServiceClient
	headers metadata.MD
}

// NewExporter creates a lazily-connecting exporter (no network I/O here).
// endpoint is http://host:port (plaintext), https://host:port or host:port (TLS).
func NewExporter(endpoint string, headers map[string]string) (*Exporter, error) {
	target, useTLS, err := parseEndpoint(endpoint)
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
	return &Exporter{conn: conn, client: collectorpb.NewProfilesServiceClient(conn), headers: metadata.New(headers)}, nil
}

// Export sends one request. A partial success (some profiles rejected) is
// returned as an error so it shows up in logs.
func (e *Exporter) Export(ctx context.Context, req *collectorpb.ExportProfilesServiceRequest) error {
	ctx, cancel := context.WithTimeout(ctx, exportTimeout)
	defer cancel()
	if len(e.headers) > 0 {
		ctx = metadata.NewOutgoingContext(ctx, e.headers)
	}
	resp, err := e.client.Export(ctx, req)
	if err != nil {
		return fmt.Errorf("otlp: export: %w", err)
	}
	if ps := resp.GetPartialSuccess(); ps.GetRejectedProfiles() > 0 || ps.GetErrorMessage() != "" {
		return fmt.Errorf("otlp: partial success: %d profiles rejected: %s", ps.GetRejectedProfiles(), ps.GetErrorMessage())
	}
	return nil
}

// Close closes the gRPC connection.
func (e *Exporter) Close() error { return e.conn.Close() }

func parseEndpoint(endpoint string) (target string, useTLS bool, err error) {
	target, useTLS = endpoint, true
	if rest, ok := strings.CutPrefix(endpoint, "http://"); ok {
		target, useTLS = rest, false
	} else if rest, ok := strings.CutPrefix(endpoint, "https://"); ok {
		target = rest
	}
	target = strings.TrimSuffix(target, "/")
	if target == "" || strings.Contains(target, "/") {
		return "", false, fmt.Errorf("otlp: endpoint %q must be [http[s]://]host:port (gRPC endpoints have no path)", endpoint)
	}
	return target, useTLS, nil
}
