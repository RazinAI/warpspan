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
	h := &wire.Hello{Attrs: []wire.KV{
		{Key: "process.pid", Value: "7"},
		{Key: "process.executable.path", Value: "/usr/bin/python3"}, // not exported (too long/high-cardinality for labels)
		{Key: "env.OTEL_RESOURCE_ATTRIBUTES", Value: "k8s.pod.name=p1, deployment.environment=prod,bad"},
	}}
	got := Resource(h, []Attr{{"k8s.node.name", "n1"}})
	want := []Attr{
		{Key: "service.name", Value: "unknown_service"},
		{Key: "deployment.environment", Value: "prod"},
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
		{Name: "k", StartNs: 0, EndNs: 100, TraceID: trace, SpanID: span, Linked: true},
		{Name: "k", StartNs: 0, EndNs: 50, TraceID: trace, SpanID: span, Linked: true},
		{Name: "k", StartNs: 0, EndNs: 10}, // unlinked: separate sample
	}})
	c.Add(2, &wire.Kernels{Records: []wire.Kernel{{Name: "unknown process"}}}) // ignored

	s := c.Snapshot(time.Now())
	if len(s.Processes) != 1 {
		t.Fatalf("want 1 process, got %d", len(s.Processes))
	}
	p := s.Processes[0]
	linked := p.Samples[Key{Name: "k", TraceID: trace, SpanID: span, Linked: true}]
	unlinked := p.Samples[Key{Name: "k"}]
	if linked != (Stat{Count: 2, TotalNs: 150}) || unlinked != (Stat{Count: 1, TotalNs: 10}) {
		t.Fatalf("unexpected samples: %+v", p.Samples)
	}
	if p.Dropped != 3 {
		t.Fatalf("dropped %d, want 3", p.Dropped)
	}
	if k, l, d := s.Totals(); k != 3 || l != 2 || d != 3 {
		t.Fatalf("totals %d %d %d", k, l, d)
	}

	// Next interval: only the new drops count; closed processes disappear after one snapshot.
	c.Add(1, &wire.Kernels{DroppedTotal: 5})
	c.Close(1)
	s = c.Snapshot(time.Now())
	if len(s.Processes) != 1 || s.Processes[0].Dropped != 2 || len(s.Processes[0].Samples) != 0 {
		t.Fatalf("second snapshot: %+v", s.Processes)
	}
	if s = c.Snapshot(time.Now()); len(s.Processes) != 0 {
		t.Fatalf("closed process still reported: %+v", s.Processes)
	}
}
