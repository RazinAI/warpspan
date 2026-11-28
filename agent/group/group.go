// Package group bounds what per-span GPU profiles cost.
//
// The collector keeps one sample per (span, NVTX path, kernel). With spans
// that is about one sample per kernel launch, growing with traffic. Grouper
// turns a snapshot into two views that never count a kernel twice:
//
//   - Samples (exported as gpu_time): the service view. Span links removed.
//     In grouped mode, kernels launched under a span sit under a group frame
//     "[spans 3f2a1c · typical]": spans that ran the same set of (NVTX path,
//     kernel) rows share a group, split by GPU time into typical and slow.
//   - SpanSamples (exported as span_gpu_time): the per-span view. In grouped
//     mode each span gets its own real GPU time under its group frame and its
//     common NVTX prefix, in one row per device; the slowest spans of the
//     interval (exemplars) keep all their rows. In full mode every span keeps
//     all rows.
//
// Kernels finish after the span that launched them, so a span seen near the
// end of an interval may still get kernels: spans whose last kernel ended
// within HoldBackNs of the newest kernel of the process are held to the next
// interval, at most once, so a normal span is grouped whole and one that keeps
// running is exported in parts.
package group

import (
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"

	"github.com/RazinAI/warpspan/agent/collect"
)

// Mode selects what span-linked kernels produce.
type Mode string

const (
	Grouped Mode = "grouped" // group profiles + small per-span profiles + exemplars
	Full    Mode = "full"    // every span keeps all its rows (cost grows with traffic)
	Off     Mode = "off"     // no per-span view; spans only feed the service view
)

// ParseMode parses a --span-profiles value.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case Grouped, Full, Off:
		return m, nil
	}
	return "", fmt.Errorf("span profiles %q: want grouped, full or off", s)
}

// Options configure a Grouper.
type Options struct {
	Mode Mode
	// Exemplars is how many of the slowest spans per process per interval keep
	// all their rows in grouped mode.
	Exemplars int
	// SlowQuantile splits a group: spans with GPU time above this quantile
	// are "slow". Groups with fewer than MinSpansForSlow spans aren't split.
	SlowQuantile    float64
	MinSpansForSlow int
	// HoldBackNs: spans whose last kernel ended this close (GPU clock) to the
	// newest kernel of the process wait for the next interval, once.
	HoldBackNs uint64
	// MinSlowRatio: a slow span must also take at least this times the
	// group's median GPU time, so near-identical spans aren't called slow.
	MinSlowRatio float64
	// MaxGroups per process per interval keep their own group frame; the
	// smaller groups beyond it share "[spans · other shapes]", so code whose
	// spans never repeat (dynamic shapes) can't flood the service view.
	MaxGroups int
	// MaxHeldRows bounds the held rows per process; past it spans aren't held.
	MaxHeldRows int
}

// DefaultOptions are the agent's defaults.
func DefaultOptions() Options {
	return Options{Mode: Grouped, Exemplars: 1, SlowQuantile: 0.9, MinSpansForSlow: 10, HoldBackNs: 2e9,
		MinSlowRatio: 1.2, MaxGroups: 64, MaxHeldRows: 50_000}
}

// Stats describe one Apply, for the agent's log.
type Stats struct {
	Spans, Groups, Exemplars, HeldBack int
}

type spanID struct {
	trace [16]byte
	span  [8]byte
}

// Grouper keeps the spans held back between intervals. Not safe for
// concurrent use; the agent calls it from its flush loop only.
type Grouper struct {
	opts    Options
	pending map[*collect.Process]map[collect.Key]collect.Stat
	// heldOnce holds the spans already held in the previous interval: a span
	// is held at most once, so one that keeps launching kernels (a training
	// loop, a long stream) is exported as it goes, in parts.
	heldOnce map[*collect.Process]map[spanID]bool
}

// New returns a Grouper.
func New(opts Options) *Grouper {
	return &Grouper{opts: opts, pending: map[*collect.Process]map[collect.Key]collect.Stat{},
		heldOnce: map[*collect.Process]map[spanID]bool{}}
}

// Apply turns a collector snapshot into the service and span views. final
// releases every held span (agent shutdown). The input is not modified.
func (g *Grouper) Apply(s *collect.Snapshot, final bool) (*collect.Snapshot, Stats) {
	out := &collect.Snapshot{Start: s.Start, End: s.End}
	var stats Stats
	prev, prevHeld := g.pending, g.heldOnce
	g.pending = map[*collect.Process]map[collect.Key]collect.Stat{}
	g.heldOnce = map[*collect.Process]map[spanID]bool{}
	seen := map[*collect.Process]bool{}
	for _, ps := range s.Processes {
		seen[ps.Process] = true
		samples := ps.Samples
		if held := prev[ps.Process]; len(held) > 0 {
			samples = merged(samples, held)
		}
		out.Processes = append(out.Processes, g.process(ps, samples, prevHeld[ps.Process], final || ps.Closed, &stats))
	}
	// Spans held for processes that sent nothing this interval are complete.
	var idle []*collect.Process
	for p, held := range prev {
		if !seen[p] && len(held) > 0 {
			idle = append(idle, p)
		}
	}
	sort.Slice(idle, func(i, j int) bool { return idle[i].PID < idle[j].PID })
	for _, p := range idle {
		out.Processes = append(out.Processes, g.process(collect.ProcessSnapshot{Process: p}, prev[p], nil, true, &stats))
	}
	return out, stats
}

func merged(a, b map[collect.Key]collect.Stat) map[collect.Key]collect.Stat {
	m := make(map[collect.Key]collect.Stat, len(a)+len(b))
	for k, v := range a {
		m[k] = v
	}
	for k, v := range b {
		m[k] = m[k].Merge(v)
	}
	return m
}

func add(m map[collect.Key]collect.Stat, k collect.Key, st collect.Stat) {
	m[k] = m[k].Merge(st)
}

func unlinked(k collect.Key) collect.Key {
	k.TraceID, k.SpanID = [16]byte{}, [8]byte{}
	return k
}

func withFrame(frame, labels string) string {
	if labels == "" {
		return frame
	}
	return frame + collect.LabelSep + labels
}

func (g *Grouper) process(ps collect.ProcessSnapshot, samples map[collect.Key]collect.Stat, wasHeld map[spanID]bool,
	final bool, stats *Stats) collect.ProcessSnapshot {
	out := ps
	out.Samples = map[collect.Key]collect.Stat{}
	out.SpanSamples = nil

	spans := map[spanID]map[collect.Key]collect.Stat{}
	var newest uint64
	for k, st := range samples {
		newest = max(newest, st.LastEndNs)
		if !k.Linked() {
			add(out.Samples, k, st)
			continue
		}
		id := spanID{k.TraceID, k.SpanID}
		if spans[id] == nil {
			spans[id] = map[collect.Key]collect.Stat{}
		}
		spans[id][k] = st
	}
	if len(spans) == 0 {
		return out
	}

	if g.opts.Mode != Off && !final {
		held := 0
		for id, rows := range spans {
			var last uint64
			for _, st := range rows {
				last = max(last, st.LastEndNs)
			}
			if wasHeld[id] || newest-last >= g.opts.HoldBackNs || held+len(rows) > g.opts.MaxHeldRows {
				continue
			}
			if g.pending[ps.Process] == nil {
				g.pending[ps.Process] = map[collect.Key]collect.Stat{}
				g.heldOnce[ps.Process] = map[spanID]bool{}
			}
			for k, st := range rows {
				g.pending[ps.Process][k] = st
			}
			g.heldOnce[ps.Process][id] = true
			held += len(rows)
			delete(spans, id)
			stats.HeldBack++
		}
	}
	stats.Spans += len(spans)

	switch g.opts.Mode {
	case Off:
		for _, rows := range spans {
			for k, st := range rows {
				add(out.Samples, unlinked(k), st)
			}
		}
	case Full:
		out.SpanSamples = map[collect.Key]collect.Stat{}
		for _, rows := range spans {
			for k, st := range rows {
				add(out.Samples, unlinked(k), st)
				out.SpanSamples[k] = st
			}
		}
	default:
		out.SpanSamples = map[collect.Key]collect.Stat{}
		g.grouped(spans, out.Samples, out.SpanSamples, stats)
	}
	return out
}

type spanInfo struct {
	id    spanID
	rows  map[collect.Key]collect.Stat
	shape uint64
	total int64
	frame string
}

func (g *Grouper) grouped(spans map[spanID]map[collect.Key]collect.Stat, service, perSpan map[collect.Key]collect.Stat, stats *Stats) {
	infos := make([]*spanInfo, 0, len(spans))
	groups := map[uint64][]*spanInfo{}
	for id, rows := range spans {
		si := &spanInfo{id: id, rows: rows, shape: shapeOf(rows)}
		for _, st := range rows {
			si.total += st.TotalNs
		}
		infos = append(infos, si)
		groups[si.shape] = append(groups[si.shape], si)
	}
	stats.Groups += len(groups)

	// The largest MaxGroups groups keep their own frame; smaller ones share one.
	shapes := make([]uint64, 0, len(groups))
	for shape := range groups {
		shapes = append(shapes, shape)
	}
	sort.Slice(shapes, func(i, j int) bool {
		if len(groups[shapes[i]]) != len(groups[shapes[j]]) {
			return len(groups[shapes[i]]) > len(groups[shapes[j]])
		}
		return shapes[i] < shapes[j]
	})
	for rank, shape := range shapes {
		members := groups[shape]
		if rank >= g.opts.MaxGroups {
			for _, m := range members {
				m.frame = "[spans · other shapes]"
			}
			continue
		}
		// Slow: strictly above the SlowQuantile value, so equal times (common:
		// identical batches) all stay typical, and at least MinSlowRatio times
		// the median.
		threshold := int64(math.MaxInt64)
		if len(members) >= g.opts.MinSpansForSlow {
			totals := make([]int64, len(members))
			for i, m := range members {
				totals[i] = m.total
			}
			sort.Slice(totals, func(i, j int) bool { return totals[i] < totals[j] })
			cut := int(math.Ceil(g.opts.SlowQuantile * float64(len(totals))))
			median := totals[len(totals)/2]
			threshold = max(totals[max(cut, 1)-1], int64(math.Ceil(g.opts.MinSlowRatio*float64(median)))-1)
		}
		for _, m := range members {
			class := "typical"
			if m.total > threshold {
				class = "slow"
			}
			m.frame = fmt.Sprintf("[spans %012x · %s]", shape>>16, class)
		}
	}

	// The slowest spans keep all their rows (ties broken by span ID).
	sort.Slice(infos, func(i, j int) bool {
		if infos[i].total != infos[j].total {
			return infos[i].total > infos[j].total
		}
		return string(infos[i].id.span[:]) < string(infos[j].id.span[:])
	})
	for i, si := range infos {
		for k, st := range si.rows {
			sk := unlinked(k)
			sk.Labels = withFrame(si.frame, k.Labels)
			add(service, sk, st)
		}
		if i < g.opts.Exemplars {
			stats.Exemplars++
			for k, st := range si.rows {
				k.Labels = withFrame(si.frame, k.Labels)
				add(perSpan, k, st)
			}
			continue
		}
		// One row per device: the span's real GPU time under its group frame
		// and the NVTX ranges all its kernels share.
		type devRows struct {
			prefix []string
			rows   int
			st     collect.Stat
			first  bool
		}
		byDev := map[uint32]*devRows{}
		for k, st := range si.rows {
			d := byDev[k.Device]
			if d == nil {
				d = &devRows{first: true}
				byDev[k.Device] = d
			}
			labels := k.LabelList()
			if d.first {
				d.prefix, d.first = labels, false
			} else {
				d.prefix = commonPrefix(d.prefix, labels)
			}
			d.rows++
			d.st = d.st.Merge(st)
		}
		for dev, d := range byDev {
			k := collect.Key{
				Name:    fmt.Sprintf("(%d kernel rows; per-kernel detail in gpu_time)", d.rows),
				Device:  dev,
				TraceID: si.id.trace,
				SpanID:  si.id.span,
				Labels:  withFrame(si.frame, strings.Join(d.prefix, collect.LabelSep)),
			}
			add(perSpan, k, d.st)
		}
	}
}

// shapeOf hashes the set of (NVTX path, kernel, device) rows of a span,
// ignoring times: spans with the same shape ran the same kernels in the same
// ranges.
func shapeOf(rows map[collect.Key]collect.Stat) uint64 {
	keys := make([]string, 0, len(rows))
	for k := range rows {
		keys = append(keys, fmt.Sprintf("%s\x01%s\x01%d", k.Labels, k.Name, k.Device))
	}
	sort.Strings(keys)
	h := fnv.New64a()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

func commonPrefix(a, b []string) []string {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return a[:n]
}
