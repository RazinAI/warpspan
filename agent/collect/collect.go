// Package collect aggregates kernel records from all connected processes
// between exports.
package collect

import (
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zraisan/warpspan/agent/wire"
)

// Attr is one resource attribute. Order is kept stable for deterministic output.
type Attr struct {
	Key, Value string
}

// Process is one injected process (one connection from libwarpspan.so).
type Process struct {
	ID       uint64 // agent-local connection id
	PID      uint32
	Resource []Attr // OTel resource attributes (service.name first)
}

// Key groups kernel executions into one profile sample.
type Key struct {
	Name    string
	Device  uint32
	TraceID [16]byte
	SpanID  [8]byte
	Linked  bool
}

// Stat is the aggregate for one Key.
type Stat struct {
	Count   int64
	TotalNs int64
}

// ProcessSnapshot is what one process contributed during one interval.
type ProcessSnapshot struct {
	Process *Process
	Samples map[Key]Stat
	Dropped uint64 // records the capture library dropped during the interval
}

// Snapshot is everything collected during one interval.
type Snapshot struct {
	Start, End time.Time
	Processes  []ProcessSnapshot
}

// Totals summarises a snapshot (for logging).
func (s *Snapshot) Totals() (kernels, linked, dropped int64) {
	for _, p := range s.Processes {
		for k, st := range p.Samples {
			kernels += st.Count
			if k.Linked {
				linked += st.Count
			}
		}
		dropped += int64(p.Dropped)
	}
	return
}

type procState struct {
	proc         *Process
	samples      map[Key]Stat
	droppedTotal uint64 // last DroppedTotal reported
	droppedBase  uint64 // DroppedTotal at the start of the interval
	closed       bool
}

// Collector is safe for concurrent use.
type Collector struct {
	mu        sync.Mutex
	procs     map[uint64]*procState
	start     time.Time
	extraAttr []Attr // added to every process resource (e.g. k8s.node.name)
}

// New returns a Collector. extra attributes are added to every resource.
func New(extra []Attr) *Collector {
	return &Collector{procs: map[uint64]*procState{}, start: time.Now(), extraAttr: extra}
}

// Hello registers a process.
func (c *Collector) Hello(id uint64, h *wire.Hello) *Process {
	p := &Process{ID: id, PID: h.PID, Resource: Resource(h, c.extraAttr)}
	c.mu.Lock()
	c.procs[id] = &procState{proc: p, samples: map[Key]Stat{}}
	c.mu.Unlock()
	return p
}

// Add records a batch of kernels for a process registered with Hello.
func (c *Collector) Add(id uint64, k *wire.Kernels) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ps := c.procs[id]
	if ps == nil {
		return
	}
	for _, r := range k.Records {
		key := Key{Name: r.Name, Device: r.Device, Linked: r.Linked}
		if r.Linked {
			key.TraceID, key.SpanID = r.TraceID, r.SpanID
		}
		st := ps.samples[key]
		st.Count++
		if r.EndNs > r.StartNs {
			st.TotalNs += int64(r.EndNs - r.StartNs)
		}
		ps.samples[key] = st
	}
	if k.DroppedTotal > ps.droppedTotal {
		ps.droppedTotal = k.DroppedTotal
	}
}

// Close marks a process as gone; it is removed after the next Snapshot.
func (c *Collector) Close(id uint64) {
	c.mu.Lock()
	if ps := c.procs[id]; ps != nil {
		ps.closed = true
	}
	c.mu.Unlock()
}

// Snapshot returns the data collected since the previous call and resets it.
func (c *Collector) Snapshot(now time.Time) *Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := &Snapshot{Start: c.start, End: now}
	ids := make([]uint64, 0, len(c.procs))
	for id := range c.procs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		ps := c.procs[id]
		dropped := ps.droppedTotal - ps.droppedBase
		if len(ps.samples) > 0 || dropped > 0 {
			s.Processes = append(s.Processes, ProcessSnapshot{Process: ps.proc, Samples: ps.samples, Dropped: dropped})
		}
		ps.samples = map[Key]Stat{}
		ps.droppedBase = ps.droppedTotal
		if ps.closed {
			delete(c.procs, id)
		}
	}
	c.start = now
	return s
}

// Resource builds OTel resource attributes for a process from its Hello.
//
// service.name follows the OTel SDK rules, so profiles land under the same
// service as the app's traces: OTEL_SERVICE_NAME, then service.name in
// OTEL_RESOURCE_ATTRIBUTES, then "unknown_service:<executable>".
func Resource(h *wire.Hello, extra []Attr) []Attr {
	hello := map[string]string{}
	for _, kv := range h.Attrs {
		hello[kv.Key] = kv.Value
	}

	attrs := map[string]string{}
	for _, a := range extra {
		attrs[a.Key] = a.Value
	}
	for _, k := range []string{"process.pid", "process.executable.name", "host.name", "container.id"} {
		if v := hello[k]; v != "" {
			attrs[k] = v
		}
	}
	for k, v := range ParseResourceAttributes(hello["env.OTEL_RESOURCE_ATTRIBUTES"]) {
		attrs[k] = v
	}
	if v := hello["env.OTEL_SERVICE_NAME"]; v != "" {
		attrs["service.name"] = v
	}
	if attrs["service.name"] == "" {
		attrs["service.name"] = "unknown_service"
		if exe := hello["process.executable.name"]; exe != "" {
			attrs["service.name"] += ":" + exe
		}
	}

	out := []Attr{{"service.name", attrs["service.name"]}}
	delete(attrs, "service.name")
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, Attr{k, attrs[k]})
	}
	return out
}

// ParseResourceAttributes parses OTEL_RESOURCE_ATTRIBUTES ("k1=v1,k2=v2",
// values percent-encoded). Malformed entries are skipped.
func ParseResourceAttributes(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(part, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			continue
		}
		if dv, err := url.PathUnescape(strings.TrimSpace(v)); err == nil {
			out[k] = dv
		}
	}
	return out
}
