package collect

import (
	"reflect"
	"testing"
	"time"

	"github.com/zraisan/warpspan/agent/wire"
)

func TestResourceServiceName(t *testing.T) {
	tests := []struct {
		name  string
		attrs []wire.KV
		want  string
	}{
		{"OTEL_SERVICE_NAME wins", []wire.KV{
			{Key: "env.OTEL_SERVICE_NAME", Value: "api"},
			{Key: "env.OTEL_RESOURCE_ATTRIBUTES", Value: "service.name=other"},
		}, "api"},
		{"from resource attributes", []wire.KV{{Key: "env.OTEL_RESOURCE_ATTRIBUTES", Value: "service.name=infer%20svc"}}, "infer svc"},
		{"fallback to executable", []wire.KV{{Key: "process.executable.name", Value: "python3"}}, "unknown_service:python3"},
		{"bare fallback", nil, "unknown_service"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Resource(&wire.Hello{Attrs: tt.attrs}, nil)
			if res[0].Key != "service.name" || res[0].Value != tt.want {
				t.Fatalf("got %v, want service.name=%q first", res, tt.want)
			}
		})
	}
}

func TestResourceMerge(t *testing.T) {
	h := &wire.Hello{PID: 7, Attrs: []wire.KV{
		{Key: "host.name", Value: "gpu-1"},
		{Key: "env.OTEL_RESOURCE_ATTRIBUTES", Value: "k8s.pod.name=p1, deployment.environment=prod,bad"},
	}}
	got := Resource(h, []Attr{{Key: "k8s.node.name", Value: "n1"}})
	want := []Attr{
		{Key: "service.name", Value: "unknown_service"},
		{Key: "deployment.environment", Value: "prod"},
		{Key: "host.name", Value: "gpu-1"},
		{Key: "k8s.node.name", Value: "n1"},
		{Key: "k8s.pod.name", Value: "p1"},
		{Key: "process.pid", Value: "7"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestCollectorAggregatesAndResets(t *testing.T) {
	c := New(nil)
	c.Hello(1, &wire.Hello{PID: 10})
	span := [8]byte{9}
	trace := [16]byte{8}
	c.Add(1, &wire.Kernels{DroppedTotal: 3, Records: []wire.Kernel{
		{Name: "k", StartNs: 0, EndNs: 100, TraceID: trace, SpanID: span},
		{Name: "k", StartNs: 0, EndNs: 50, TraceID: trace, SpanID: span},
		{Name: "k", StartNs: 0, EndNs: 10}, // unlinked: separate sample
	}})
	c.Add(1, &wire.Kernels{DroppedTotal: 5})
	c.Add(2, &wire.Kernels{Records: []wire.Kernel{{Name: "unknown connection"}}}) // ignored

	s := c.Snapshot(time.Now())
	if len(s.Processes) != 1 {
		t.Fatalf("want 1 process, got %d", len(s.Processes))
	}
	p := s.Processes[0]
	if got := p.Samples[Key{Name: "k", TraceID: trace, SpanID: span}]; got != (Stat{Count: 2, TotalNs: 150}) {
		t.Fatalf("linked sample %+v", got)
	}
	if got := p.Samples[Key{Name: "k"}]; got != (Stat{Count: 1, TotalNs: 10}) {
		t.Fatalf("unlinked sample %+v", got)
	}
	// The first DroppedTotal (3) is a baseline from before we connected.
	if p.Dropped != 2 {
		t.Fatalf("dropped %d, want 2", p.Dropped)
	}

	// Closed processes are reported once more, then removed.
	c.Add(1, &wire.Kernels{DroppedTotal: 6})
	c.Close(1)
	s = c.Snapshot(time.Now())
	if len(s.Processes) != 1 || s.Processes[0].Dropped != 1 || len(s.Processes[0].Samples) != 0 {
		t.Fatalf("second snapshot: %+v", s.Processes)
	}
	if s = c.Snapshot(time.Now()); len(s.Processes) != 0 {
		t.Fatalf("closed process still reported: %+v", s.Processes)
	}
}

func TestCollectorCapsKeys(t *testing.T) {
	c := New(nil)
	c.Hello(1, &wire.Hello{PID: 1})
	recs := make([]wire.Kernel, MaxKeysPerProcess+10)
	for i := range recs {
		recs[i] = wire.Kernel{Name: "k", EndNs: 1, SpanID: [8]byte{byte(i), byte(i >> 8), byte(i >> 16), 1}}
	}
	c.Add(1, &wire.Kernels{Records: recs})
	p := c.Snapshot(time.Now()).Processes[0]
	if len(p.Samples) != MaxKeysPerProcess+1 || p.Unlinked != 10 || p.Samples[Key{Name: "k"}].Count != 10 {
		t.Fatalf("samples=%d unlinked=%d", len(p.Samples), p.Unlinked)
	}
}
