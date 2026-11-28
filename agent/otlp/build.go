// Package otlp turns collected kernel data into OTLP profiles and exports them.
//
// Profiles proto: go.opentelemetry.io/proto/otlp/profiles/v1development
// (alpha; wire-identical between v0.3.0 used by Pyroscope v2.4.x and v0.4.1 used
// by Collector v0.165.x). Expect breaking changes on upgrades.
//
// Shape of what we send, per process (one ResourceProfiles each):
//
//	resource: service.name, process.pid, host.name, k8s.*, ... (become Pyroscope labels)
//	profile "gpu_time"        sample type gpu_time/nanoseconds, period type gpu/nanoseconds
//	profile "kernel_launches" sample type kernel_launches/count, same period type
//	                          (both from ProcessSnapshot.Samples: the service view)
//	profile "span_gpu_time"   sample type span_gpu_time/nanoseconds, from SpanSamples:
//	                          the per-span view agent/group builds, with sample links
//	sample stack (leaf first): [kernel name, NVTX ranges innermost first..., "cuda:<device>"]
//	sample link:  trace_id + span_id of the span that launched the kernel
//
// Pyroscope names the profile after the period type ("gpu") and turns each
// sample link into span_id / trace_id labels, which is what Grafana's
// traces-to-profiles uses.
package otlp

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"sort"

	collectorpb "go.opentelemetry.io/proto/otlp/collector/profiles/v1development"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"

	"github.com/RazinAI/warpspan/agent/collect"
)

// Profile and sample type names. Changing them changes the Pyroscope profile type IDs:
//
//	gpu:gpu_time:nanoseconds:gpu:nanoseconds
//	gpu:kernel_launches:count:gpu:nanoseconds
const (
	PeriodType      = "gpu"
	TimeSampleType  = "gpu_time"
	CountSampleType = "kernel_launches"
	// SpanTimeSampleType is the per-span view (agent/group): what Grafana's
	// "Profiles for this span" should query. Profile type ID
	// gpu:span_gpu_time:nanoseconds:gpu:nanoseconds.
	SpanTimeSampleType = "span_gpu_time"
)

// MaxSamplesPerProfile bounds the samples in one profile message. Pyroscope's
// default max_profile_stacktrace_samples is 16000 and it drops the whole
// profile above it; spans x NVTX labels x kernels can exceed that in one
// interval.
const MaxSamplesPerProfile = 15000

// BuildRequests splits a snapshot into export requests of at most
// MaxSamplesPerProfile samples each (one or more per process), so no request
// comes near gRPC's common 4 MiB message limit however high the collector's
// sample cap is. A span's samples can land in different requests; Pyroscope
// merges profiles of the same series and time at query time.
func BuildRequests(s *collect.Snapshot, opts Options) []*collectorpb.ExportProfilesServiceRequest {
	var reqs []*collectorpb.ExportProfilesServiceRequest
	for _, ps := range s.Processes {
		for _, view := range []struct {
			samples map[collect.Key]collect.Stat
			span    bool
		}{{ps.Samples, false}, {ps.SpanSamples, true}} {
			keys := make([]collect.Key, 0, len(view.samples))
			for k := range view.samples {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool { return keyLess(keys[i], keys[j]) })
			for lo := 0; lo < len(keys); lo += MaxSamplesPerProfile {
				hi := min(lo+MaxSamplesPerProfile, len(keys))
				chunk := collect.ProcessSnapshot{Process: ps.Process}
				part := make(map[collect.Key]collect.Stat, hi-lo)
				for _, k := range keys[lo:hi] {
					part[k] = view.samples[k]
				}
				if view.span {
					chunk.SpanSamples = part
				} else {
					chunk.Samples = part
				}
				one := &collect.Snapshot{Start: s.Start, End: s.End, Processes: []collect.ProcessSnapshot{chunk}}
				if req := Build(one, opts); req != nil {
					reqs = append(reqs, req)
				}
			}
		}
	}
	return reqs
}

// Options control how profiles are built.
type Options struct {
	ScopeVersion string
}

// Build converts a snapshot into one export request. It returns nil when there
// is nothing to export.
func Build(s *collect.Snapshot, opts Options) *collectorpb.ExportProfilesServiceRequest {
	d := newDict()
	req := &collectorpb.ExportProfilesServiceRequest{Dictionary: d.d}
	start := uint64(s.Start.UnixNano())
	duration := uint64(s.End.Sub(s.Start).Nanoseconds())
	frames := map[string]string{} // kernel name -> frame; many samples share a kernel

	for _, ps := range s.Processes {
		profiles := d.profiles(ps.Samples, frames, []valueType{
			{TimeSampleType, "nanoseconds", func(st collect.Stat) int64 { return st.TotalNs }},
			{CountSampleType, "count", func(st collect.Stat) int64 { return st.Count }},
		}, start, duration)
		profiles = append(profiles, d.profiles(ps.SpanSamples, frames, []valueType{
			{SpanTimeSampleType, "nanoseconds", func(st collect.Stat) int64 { return st.TotalNs }},
		}, start, duration)...)
		if len(profiles) == 0 {
			continue
		}

		res := &resourcepb.Resource{}
		for _, a := range ps.Process.Resource {
			res.Attributes = append(res.Attributes, &commonpb.KeyValue{
				Key:   a.Key,
				Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: a.Value}},
			})
		}
		req.ResourceProfiles = append(req.ResourceProfiles, &profilespb.ResourceProfiles{
			Resource: res,
			ScopeProfiles: []*profilespb.ScopeProfiles{{
				Scope:    &commonpb.InstrumentationScope{Name: "warpspan", Version: opts.ScopeVersion},
				Profiles: profiles,
			}},
		})
	}
	if len(req.ResourceProfiles) == 0 {
		return nil
	}
	return req
}

type valueType struct {
	sampleType, unit string
	value            func(collect.Stat) int64
}

// profiles builds one profile per value type from samples, split so none has
// more than MaxSamplesPerProfile samples: Pyroscope rejects a whole profile
// above max_profile_stacktrace_samples (16000 by default). Chunks follow key
// order, so one span can span two profiles; that relies on Pyroscope merging
// profiles of the same series and time.
func (d *dict) profiles(samples map[collect.Key]collect.Stat, frames map[string]string, types []valueType,
	start, duration uint64) []*profilespb.Profile {
	if len(samples) == 0 {
		return nil
	}
	keys := make([]collect.Key, 0, len(samples))
	for k := range samples {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keyLess(keys[i], keys[j]) })

	var out, cur []*profilespb.Profile
	for i, k := range keys {
		if i%MaxSamplesPerProfile == 0 {
			cur = make([]*profilespb.Profile, len(types))
			for t, vt := range types {
				cur[t] = d.newProfile(vt.sampleType, vt.unit, start, duration)
			}
			out = append(out, cur...)
		}
		st := samples[k]
		frame, ok := frames[k.Name]
		if !ok {
			frame = kernelFrame(k.Name)
			frames[k.Name] = frame
		}
		labels := k.LabelList()
		stackFrames := make([]string, 0, len(labels)+2)
		stackFrames = append(stackFrames, frame)
		for j := len(labels) - 1; j >= 0; j-- {
			stackFrames = append(stackFrames, labels[j])
		}
		stackFrames = append(stackFrames, fmt.Sprintf("cuda:%d", k.Device))
		stack := d.stack(stackFrames)
		var link int32
		if k.Linked() {
			link = d.link(k.TraceID, k.SpanID)
		}
		for t, vt := range types {
			cur[t].Samples = append(cur[t].Samples,
				&profilespb.Sample{StackIndex: stack, Values: []int64{vt.value(st)}, LinkIndex: link})
		}
	}
	return out
}

func keyLess(a, b collect.Key) bool {
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	if a.Device != b.Device {
		return a.Device < b.Device
	}
	if a.TraceID != b.TraceID {
		return string(a.TraceID[:]) < string(b.TraceID[:])
	}
	if a.SpanID != b.SpanID {
		return string(a.SpanID[:]) < string(b.SpanID[:])
	}
	return a.Labels < b.Labels
}

// dict builds the request-wide ProfilesDictionary. Index 0 of every table is
// the zero value, as the OTLP profiles spec requires.
type dict struct {
	d         *profilespb.ProfilesDictionary
	strings   map[string]int32
	locations map[string]int32 // frame name -> location index
	stacks    map[string]int32 // location indices, 4 bytes each -> stack index
	links     map[[24]byte]int32
	mapping   int32
}

func newDict() *dict {
	d := &dict{
		d: &profilespb.ProfilesDictionary{
			StringTable:    []string{""},
			MappingTable:   []*profilespb.Mapping{{}},
			LocationTable:  []*profilespb.Location{{}},
			FunctionTable:  []*profilespb.Function{{}},
			LinkTable:      []*profilespb.Link{{}},
			AttributeTable: []*profilespb.KeyValueAndUnit{{}},
			StackTable:     []*profilespb.Stack{{}},
		},
		strings:   map[string]int32{"": 0},
		locations: map[string]int32{},
		stacks:    map[string]int32{},
		links:     map[[24]byte]int32{},
	}
	d.mapping = int32(len(d.d.MappingTable))
	d.d.MappingTable = append(d.d.MappingTable, &profilespb.Mapping{FilenameStrindex: d.str("cuda")})
	return d
}

func (d *dict) str(s string) int32 {
	if i, ok := d.strings[s]; ok {
		return i
	}
	i := int32(len(d.d.StringTable))
	d.d.StringTable = append(d.d.StringTable, s)
	d.strings[s] = i
	return i
}

func (d *dict) location(frame string) int32 {
	if i, ok := d.locations[frame]; ok {
		return i
	}
	fn := int32(len(d.d.FunctionTable))
	name := d.str(frame)
	d.d.FunctionTable = append(d.d.FunctionTable, &profilespb.Function{NameStrindex: name, SystemNameStrindex: name})
	i := int32(len(d.d.LocationTable))
	d.d.LocationTable = append(d.d.LocationTable, &profilespb.Location{
		MappingIndex: d.mapping,
		Lines:        []*profilespb.Line{{FunctionIndex: fn}},
	})
	d.locations[frame] = i
	return i
}

// stack returns the stack for frames, leaf first.
func (d *dict) stack(frames []string) int32 {
	locs := make([]int32, len(frames))
	key := make([]byte, 0, 4*len(frames))
	for i, f := range frames {
		locs[i] = d.location(f)
		key = binary.LittleEndian.AppendUint32(key, uint32(locs[i]))
	}
	if i, ok := d.stacks[string(key)]; ok {
		return i
	}
	i := int32(len(d.d.StackTable))
	d.d.StackTable = append(d.d.StackTable, &profilespb.Stack{LocationIndices: locs})
	d.stacks[string(key)] = i
	return i
}

func (d *dict) link(trace [16]byte, span [8]byte) int32 {
	var key [24]byte
	copy(key[:16], trace[:])
	copy(key[16:], span[:])
	if i, ok := d.links[key]; ok {
		return i
	}
	i := int32(len(d.d.LinkTable))
	d.d.LinkTable = append(d.d.LinkTable, &profilespb.Link{TraceId: trace[:], SpanId: span[:]})
	d.links[key] = i
	return i
}

func (d *dict) newProfile(sampleType, unit string, start, duration uint64) *profilespb.Profile {
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return &profilespb.Profile{
		SampleType:   &profilespb.ValueType{TypeStrindex: d.str(sampleType), UnitStrindex: d.str(unit)},
		PeriodType:   &profilespb.ValueType{TypeStrindex: d.str(PeriodType), UnitStrindex: d.str("nanoseconds")},
		Period:       1,
		TimeUnixNano: start,
		DurationNano: duration,
		ProfileId:    id,
	}
}
