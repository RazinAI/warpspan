package group

import (
	"strings"
	"testing"
	"time"

	"github.com/RazinAI/warpspan/agent/collect"
)

var proc = &collect.Process{PID: 7}

func lbl(l ...string) string { return strings.Join(l, collect.LabelSep) }

// batch returns the rows of one span: the same 3 layers every time (one
// shape), with total GPU time 3*ns, ending at end.
func batch(span byte, ns int64, end uint64) map[collect.Key]collect.Stat {
	rows := map[collect.Key]collect.Stat{}
	for i, layer := range []string{"conv1", "conv2", "nms"} {
		k := collect.Key{Name: "kern" + string(rune('a'+i)), SpanID: [8]byte{span, 1}, TraceID: [16]byte{span, 1},
			Labels: lbl("infer", "enqueueV3", layer)}
		rows[k] = collect.Stat{Count: 1, TotalNs: ns, LastEndNs: end}
	}
	return rows
}

func snapshot(parts ...map[collect.Key]collect.Stat) *collect.Snapshot {
	all := map[collect.Key]collect.Stat{}
	for _, p := range parts {
		for k, v := range p {
			all[k] = v
		}
	}
	return &collect.Snapshot{Start: time.Unix(0, 0), End: time.Unix(10, 0),
		Processes: []collect.ProcessSnapshot{{Process: proc, Samples: all}}}
}

func sum(m map[collect.Key]collect.Stat) (ns int64) {
	for _, st := range m {
		ns += st.TotalNs
	}
	return ns
}

func opts(mode Mode) Options {
	o := DefaultOptions()
	o.Mode = mode
	o.HoldBackNs = 0
	return o
}

func TestGroupedViews(t *testing.T) {
	var parts []map[collect.Key]collect.Stat
	for i := 0; i < 19; i++ {
		parts = append(parts, batch(byte(i+1), 100, 1))
	}
	parts = append(parts, batch(50, 900, 1)) // one slow batch
	unl := map[collect.Key]collect.Stat{{Name: "decode"}: {Count: 4, TotalNs: 40, LastEndNs: 1}}
	parts = append(parts, unl)
	in := snapshot(parts...)

	out, stats := New(opts(Grouped)).Apply(in, false)
	p := out.Processes[0]

	if stats.Spans != 20 || stats.Groups != 1 || stats.Exemplars != 1 {
		t.Fatalf("stats %+v", stats)
	}
	// Service view: every kernel once, no span links.
	if sum(p.Samples) != sum(in.Processes[0].Samples) {
		t.Fatalf("service view holds %d ns, input %d", sum(p.Samples), sum(in.Processes[0].Samples))
	}
	frames := map[string]int64{}
	for k, st := range p.Samples {
		if k.Linked() {
			t.Fatalf("service view has a span link: %+v", k)
		}
		if l := k.LabelList(); len(l) > 0 && strings.HasPrefix(l[0], "[spans ") {
			frames[l[0]] += st.TotalNs
		}
	}
	if len(frames) != 2 { // typical and slow
		t.Fatalf("group frames %v", frames)
	}
	for f, ns := range frames {
		if strings.HasSuffix(f, "slow]") && ns != 2700 || strings.HasSuffix(f, "typical]") && ns != 19*300 {
			t.Fatalf("frame %s holds %d ns", f, ns)
		}
	}
	// Span view: every linked kernel once; the exemplar keeps its 3 rows, the
	// others get one row with their real total.
	if sum(p.SpanSamples) != 19*300+2700 {
		t.Fatalf("span view holds %d ns", sum(p.SpanSamples))
	}
	perSpan := map[[8]byte][]collect.Key{}
	for k := range p.SpanSamples {
		if !k.Linked() {
			t.Fatalf("span view row without a span: %+v", k)
		}
		perSpan[k.SpanID] = append(perSpan[k.SpanID], k)
	}
	if n := len(perSpan[[8]byte{50, 1}]); n != 3 {
		t.Fatalf("exemplar has %d rows, want 3", n)
	}
	small := perSpan[[8]byte{1, 1}]
	if len(small) != 1 {
		t.Fatalf("typical span has %d rows, want 1", len(small))
	}
	k := small[0]
	if got := k.LabelList(); len(got) != 3 || !strings.HasSuffix(got[0], "typical]") || got[1] != "infer" || got[2] != "enqueueV3" {
		t.Fatalf("typical span row labels %q", got)
	}
	if p.SpanSamples[k].TotalNs != 300 || !strings.Contains(k.Name, "3 kernel rows") {
		t.Fatalf("typical span row %+v %+v", k, p.SpanSamples[k])
	}
}

func TestDifferentShapesAreDifferentGroups(t *testing.T) {
	a := batch(1, 100, 1)
	b := batch(2, 100, 1)
	for k, v := range b {
		if strings.HasSuffix(k.Labels, "nms") {
			delete(b, k)
			k.Name = "other"
			b[k] = v
		}
	}
	_, stats := New(opts(Grouped)).Apply(snapshot(a, b), false)
	if stats.Groups != 2 {
		t.Fatalf("groups %d, want 2", stats.Groups)
	}
}

func TestHoldBack(t *testing.T) {
	g := New(DefaultOptions()) // 2 s hold-back
	done := batch(1, 100, 1e9)
	recent := batch(2, 100, 5e9) // ends with the newest kernel: may still grow
	out, stats := g.Apply(snapshot(done, recent), false)
	if stats.Spans != 1 || stats.HeldBack != 1 {
		t.Fatalf("stats %+v", stats)
	}
	if sum(out.Processes[0].Samples) != 300 {
		t.Fatalf("service view %d ns, want only the finished span", sum(out.Processes[0].Samples))
	}
	// Late kernels of span 2 arrive; next interval has both parts.
	late := map[collect.Key]collect.Stat{}
	for k, v := range batch(2, 100, 5e9) {
		v.LastEndNs = 6e9
		late[k] = v
	}
	out, stats = g.Apply(snapshot(late, batch(3, 100, 9e9)), false)
	if stats.Spans != 1 || stats.HeldBack != 1 {
		t.Fatalf("second stats %+v", stats)
	}
	if sum(out.Processes[0].Samples) != 600 {
		t.Fatalf("span 2 should be whole: %d ns", sum(out.Processes[0].Samples))
	}
	// A process that sends nothing more gets its held spans released.
	out, stats = g.Apply(&collect.Snapshot{}, false)
	if len(out.Processes) != 1 || stats.Spans != 1 || sum(out.Processes[0].Samples) != 300 {
		t.Fatalf("release: %+v %+v", stats, out.Processes)
	}
}

// A span that keeps launching kernels is held once, then exported as it goes.
func TestLongSpanIsHeldOnlyOnce(t *testing.T) {
	g := New(DefaultOptions())
	long := func(end uint64) map[collect.Key]collect.Stat { return batch(1, 100, end) }
	_, st := g.Apply(snapshot(long(5e9)), false)
	if st.HeldBack != 1 || st.Spans != 0 {
		t.Fatalf("first interval: %+v", st)
	}
	out, st := g.Apply(snapshot(long(9e9)), false) // still running, still newest
	if st.HeldBack != 0 || st.Spans != 1 || sum(out.Processes[0].Samples) != 600 {
		t.Fatalf("second interval must emit both parts: %+v, %d ns", st, sum(out.Processes[0].Samples))
	}
	_, st = g.Apply(snapshot(long(13e9)), false) // can be held again after being emitted
	if st.HeldBack != 1 {
		t.Fatalf("third interval: %+v", st)
	}
}

// Spans that never repeat their shape can't flood the service view.
func TestManyShapesShareAFrame(t *testing.T) {
	o := opts(Grouped)
	o.MaxGroups = 2
	var parts []map[collect.Key]collect.Stat
	for i := 0; i < 5; i++ {
		b := batch(byte(i+1), 100, 1)
		for k, v := range b {
			delete(b, k)
			k.Name += string(rune('0' + i)) // a different kernel per span: 5 shapes
			b[k] = v
		}
		parts = append(parts, b)
	}
	out, st := New(o).Apply(snapshot(parts...), false)
	frames := map[string]bool{}
	for k := range out.Processes[0].Samples {
		frames[k.LabelList()[0]] = true
	}
	if st.Groups != 5 || len(frames) != 3 || !frames["[spans · other shapes]"] {
		t.Fatalf("groups %d, frames %v", st.Groups, frames)
	}
}

// Spans within MinSlowRatio of the median stay typical even above p90.
func TestNearIdenticalSpansAreNotSlow(t *testing.T) {
	var parts []map[collect.Key]collect.Stat
	for i := 0; i < 20; i++ {
		parts = append(parts, batch(byte(i+1), 100+int64(i), 1)) // within 20%
	}
	out, _ := New(opts(Grouped)).Apply(snapshot(parts...), false)
	for k := range out.Processes[0].Samples {
		if strings.HasSuffix(k.LabelList()[0], "slow]") {
			t.Fatalf("near-identical spans marked slow: %q", k.LabelList()[0])
		}
	}
}

func TestFinalReleasesEverything(t *testing.T) {
	out, stats := New(DefaultOptions()).Apply(snapshot(batch(1, 100, 5e9)), true)
	if stats.HeldBack != 0 || sum(out.Processes[0].Samples) != 300 {
		t.Fatalf("final: %+v", stats)
	}
}

func TestFullAndOff(t *testing.T) {
	in := snapshot(batch(1, 100, 1), batch(2, 100, 1))
	full, _ := New(opts(Full)).Apply(in, false)
	if p := full.Processes[0]; len(p.SpanSamples) != 6 || sum(p.SpanSamples) != 600 || sum(p.Samples) != 600 {
		t.Fatalf("full: %d span rows", len(p.SpanSamples))
	}
	for k := range full.Processes[0].Samples {
		if k.Linked() {
			t.Fatal("full: service view must not carry span links")
		}
	}
	off, _ := New(opts(Off)).Apply(in, false)
	if p := off.Processes[0]; p.SpanSamples != nil || sum(p.Samples) != 600 || len(p.Samples) != 3 {
		t.Fatalf("off: %+v", p)
	}
}

func TestParseMode(t *testing.T) {
	for _, s := range []string{"grouped", "full", "off"} {
		if _, err := ParseMode(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ParseMode("all"); err == nil {
		t.Fatal("want error")
	}
}
