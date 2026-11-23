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
//	sample stack (leaf first): [kernel name, "cuda:<device>"]
//	sample link:  trace_id + span_id of the span that launched the kernel
//
// Pyroscope names the profile after the period type ("gpu") and turns each
// sample link into span_id / trace_id labels, which is what Grafana's
// traces-to-profiles uses.
package otlp

import (
	"crypto/rand"
	"fmt"
	"sort"

	"github.com/ianlancetaylor/demangle"
	collectorpb "go.opentelemetry.io/proto/otlp/collector/profiles/v1development"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"

	"github.com/zraisan/warpspan/agent/collect"
)

// Profile and sample type names. Changing them changes the Pyroscope profile type IDs:
//
//	gpu:gpu_time:nanoseconds:gpu:nanoseconds
//	gpu:kernel_launches:count:gpu:nanoseconds
const (
	PeriodType      = "gpu"
	TimeSampleType  = "gpu_time"
	CountSampleType = "kernel_launches"
)

// Options control how profiles are built.
type Options struct {
	ScopeVersion string
}

// kernelFrame demangles a C++ kernel name, dropping parameters to keep frames
// readable. Non-C++ names are returned unchanged.
func kernelFrame(name string) string { return demangle.Filter(name, demangle.NoParams) }

// Build converts a snapshot into one export request. It returns nil when there
// is nothing to export.
func Build(s *collect.Snapshot, opts Options) *collectorpb.ExportProfilesServiceRequest {
	d := newDict()
	req := &collectorpb.ExportProfilesServiceRequest{Dictionary: d.d}
	start := uint64(s.Start.UnixNano())
	duration := uint64(s.End.Sub(s.Start).Nanoseconds())

	for _, ps := range s.Processes {
		if len(ps.Samples) == 0 {
			continue
		}
		keys := make([]collect.Key, 0, len(ps.Samples))
		for k := range ps.Samples {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return keyLess(keys[i], keys[j]) })

		timeProfile := d.newProfile(TimeSampleType, "nanoseconds", start, duration)
		countProfile := d.newProfile(CountSampleType, "count", start, duration)
		for _, k := range keys {
			st := ps.Samples[k]
			stack := d.stack(kernelFrame(k.Name), fmt.Sprintf("cuda:%d", k.Device))
			var link int32
			if k.Linked() {
				link = d.link(k.TraceID, k.SpanID)
			}
			timeProfile.Samples = append(timeProfile.Samples,
				&profilespb.Sample{StackIndex: stack, Values: []int64{st.TotalNs}, LinkIndex: link})
			countProfile.Samples = append(countProfile.Samples,
				&profilespb.Sample{StackIndex: stack, Values: []int64{st.Count}, LinkIndex: link})
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
				Profiles: []*profilespb.Profile{timeProfile, countProfile},
			}},
		})
	}
	if len(req.ResourceProfiles) == 0 {
		return nil
	}
	return req
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
	return string(a.SpanID[:]) < string(b.SpanID[:])
}

// dict builds the request-wide ProfilesDictionary. Index 0 of every table is
// the zero value, as the OTLP profiles spec requires.
type dict struct {
	d         *profilespb.ProfilesDictionary
	strings   map[string]int32
	locations map[string]int32 // frame name -> location index
	stacks    map[[2]int32]int32
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
		stacks:    map[[2]int32]int32{},
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

// stack returns a stack with leaf first: [leaf, root].
func (d *dict) stack(leaf, root string) int32 {
	key := [2]int32{d.location(leaf), d.location(root)}
	if i, ok := d.stacks[key]; ok {
		return i
	}
	i := int32(len(d.d.StackTable))
	d.d.StackTable = append(d.d.StackTable, &profilespb.Stack{LocationIndices: key[:]})
	d.stacks[key] = i
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
