// Package collect aggregates kernel records from all connected processes
// between exports.
package collect

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/RazinAI/warpspan/agent/wire"
)

// MaxKeysPerProcess caps distinct (kernel, device, span, NVTX labels) samples
// per process per interval. Beyond it, new samples are counted without their
// span and NVTX labels, under the bare kernel name, which may use
// maxKeysHard - MaxKeysPerProcess more keys; after that, under OtherKernels.
const (
	MaxKeysPerProcess = 20_000
	maxKeysHard       = MaxKeysPerProcess + 2_000
	OtherKernels      = "[other kernels]"
)

// Attr is one resource attribute.
type Attr struct {
	Key, Value string
}

// Process is one injected process (one connection from libwarpspan.so).
type Process struct {
	PID      uint32
	Resource []Attr // OTel resource attributes, service.name first
}

// Key groups kernel executions into one profile sample. TraceID/SpanID are
// zero for kernels not launched inside a span.
type Key struct {
	Name    string
	Device  uint32
	TraceID [16]byte
	SpanID  [8]byte
	// Labels holds the NVTX ranges open at launch, outermost first, joined
	// with LabelSep ("" when there were none). A string keeps Key comparable.
	Labels string
}

// LabelSep separates NVTX labels in Key.Labels. NVTX messages are C strings,
// so they can't contain it.
const LabelSep = wire.LabelSep

// LabelList returns the NVTX labels of the key, outermost first.
func (k Key) LabelList() []string {
	if k.Labels == "" {
		return nil
	}
	return strings.Split(k.Labels, LabelSep)
}

// Linked reports whether the sample belongs to a span.
func (k Key) Linked() bool { return k.SpanID != [8]byte{} }

// Stat is the aggregate for one Key.
type Stat struct {
	Count   int64
	TotalNs int64
}

// ProcessSnapshot is what one process contributed during one interval.
type ProcessSnapshot struct {
	Process  *Process
	Samples  map[Key]Stat
	Dropped  uint64 // records the capture library lost during the interval
	Unlinked int64  // kernels whose span was discarded because of MaxKeysPerProcess
}

// Snapshot is everything collected during one interval.
type Snapshot struct {
	Start, End time.Time
	Processes  []ProcessSnapshot
}

type procState struct {
	snap         ProcessSnapshot
	droppedTotal uint64 // last DroppedTotal reported
	droppedBase  uint64 // DroppedTotal at the start of the interval
	seenDropped  bool
	closed       bool
}

// Collector is safe for concurrent use.
type Collector struct {
	mu    sync.Mutex
	procs map[uint64]*procState
	start time.Time
	extra []Attr // added to every process resource (e.g. k8s.node.name)
}

// New returns a Collector. extra attributes are added to every resource.
func New(extra []Attr) *Collector {
	return &Collector{procs: map[uint64]*procState{}, start: time.Now(), extra: extra}
}

// Hello registers the process behind connection id.
func (c *Collector) Hello(id uint64, h *wire.Hello) *Process {
	p := &Process{PID: h.PID, Resource: Resource(h, c.extra)}
	c.mu.Lock()
	c.procs[id] = &procState{snap: ProcessSnapshot{Process: p, Samples: map[Key]Stat{}}}
	c.mu.Unlock()
	return p
}

// Add records a batch of kernels for a connection registered with Hello.
func (c *Collector) Add(id uint64, k *wire.Kernels) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ps := c.procs[id]
	if ps == nil {
		return
	}
	for i := range k.Records {
		r := &k.Records[i]
		key := Key{Name: r.Name, Device: r.Device, TraceID: r.TraceID, SpanID: r.SpanID, Labels: r.LabelKey}
		st, ok := ps.snap.Samples[key]
		if !ok && len(ps.snap.Samples) >= MaxKeysPerProcess && (key.Linked() || key.Labels != "") {
			// Past the soft cap, new samples lose their span and NVTX labels;
			// the reserve up to maxKeysHard keeps bare kernel names.
			if key.Linked() {
				ps.snap.Unlinked++
			}
			key = Key{Name: r.Name, Device: r.Device}
			st, ok = ps.snap.Samples[key]
		}
		if !ok && len(ps.snap.Samples) >= maxKeysHard {
			key = Key{Name: OtherKernels, Device: r.Device}
			st = ps.snap.Samples[key]
		}
		st.Count++
		if r.EndNs > r.StartNs {
			st.TotalNs += int64(r.EndNs - r.StartNs)
		}
		ps.snap.Samples[key] = st
	}
	// DroppedTotal is a lifetime counter; the first value seen (e.g. after an
	// agent restart) is the baseline, not drops of this interval.
	if !ps.seenDropped {
		ps.seenDropped = true
		ps.droppedBase = k.DroppedTotal
	}
	if k.DroppedTotal > ps.droppedTotal {
		ps.droppedTotal = k.DroppedTotal
	}
}

// Close marks a connection as gone; it is removed after the next Snapshot.
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
		if ps.droppedTotal > ps.droppedBase {
			ps.snap.Dropped = ps.droppedTotal - ps.droppedBase
		}
		if len(ps.snap.Samples) > 0 || ps.snap.Dropped > 0 {
			s.Processes = append(s.Processes, ps.snap)
		}
		ps.snap = ProcessSnapshot{Process: ps.snap.Process, Samples: map[Key]Stat{}}
		ps.droppedBase = max(ps.droppedBase, ps.droppedTotal)
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
	attrs["process.pid"] = strconv.FormatUint(uint64(h.PID), 10)
	for _, k := range []string{"process.executable.name", "host.name"} {
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

	out := []Attr{{Key: "service.name", Value: attrs["service.name"]}}
	delete(attrs, "service.name")
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, Attr{Key: k, Value: attrs[k]})
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
