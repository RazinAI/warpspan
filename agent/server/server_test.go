package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/RazinAI/warpspan/agent/collect"
	"github.com/RazinAI/warpspan/agent/wire"
)

func TestServerReceivesKernels(t *testing.T) {
	// Unix socket paths are limited to 108 bytes; use a short relative path.
	t.Chdir(t.TempDir())
	l, err := Listen("w.sock")
	if err != nil {
		t.Fatal(err)
	}
	c := collect.New(nil)
	s := &Server{Collector: c, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l) }()

	conn, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: "w.sock", Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	send := func(b []byte) {
		if _, err := conn.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	send(wire.EncodeHello(&wire.Hello{PID: 5, Attrs: []wire.KV{{Key: "env.OTEL_SERVICE_NAME", Value: "svc"}}}))
	send(wire.EncodeKernels(&wire.Kernels{Records: []wire.Kernel{{Name: "k", StartNs: 1, EndNs: 11}}}))
	conn.Close()

	// Wait for the disconnect to be processed, then snapshot.
	deadline := time.Now().Add(2 * time.Second)
	for {
		snap := c.Snapshot(time.Now())
		if len(snap.Processes) == 1 {
			p := snap.Processes[0]
			if p.Process.Resource[0].Value != "svc" || p.Samples[collect.Key{Name: "k"}] != (collect.Stat{Count: 1, TotalNs: 10}) {
				t.Fatalf("unexpected snapshot: %+v", p)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no data received")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestListenRefusesRegularFile(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := writeFile("not-a-socket"); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen("not-a-socket"); err == nil {
		t.Fatal("expected error for a regular file")
	}
}

func writeFile(name string) error {
	return os.WriteFile(name, []byte("x"), 0o644)
}

func TestServerClosesOnSecondHello(t *testing.T) {
	t.Chdir(t.TempDir())
	l, err := Listen("w.sock")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Collector: collect.New(nil), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Serve(ctx, l)

	conn, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: "w.sock", Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	hello := wire.EncodeHello(&wire.Hello{PID: 5})
	for range 2 {
		if _, err := conn.Write(hello); err != nil {
			t.Fatal(err)
		}
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("expected the server to close the connection, got %v", err)
	}
}
