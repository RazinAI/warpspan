package otlp

import (
	"context"
	"encoding/hex"
	"net"
	"strings"
	"testing"
	"time"

	collectorpb "go.opentelemetry.io/proto/otlp/collector/profiles/v1development"
	profilespb "go.opentelemetry.io/proto/otlp/profiles/v1development"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/RazinAI/warpspan/agent/collect"
)

const torchKernel = "_ZN2at6native29vectorized_elementwise_kernelILi4ENS0_15CUDAFunctor_addIfEESt5arrayIPcLm3EEEEviT0_T1_"

func testSnapshot() *collect.Snapshot {
	trace, _ := hex.DecodeString("0af7651916cd43dd8448eb211c80319c")
	span, _ := hex.DecodeString("b7ad6b7169203331")
	var k collect.Key
	k.Name = torchKernel
	copy(k.TraceID[:], trace)
	copy(k.SpanID[:], span)
	start := time.Unix(1700000000, 0)
	return &collect.Snapshot{
		Start: start,
		End:   start.Add(10 * time.Second),
		Processes: []collect.ProcessSnapshot{{
			Process: &collect.Process{PID: 7, Resource: []collect.Attr{{Key: "service.name", Value: "infer"}, {Key: "process.pid", Value: "7"}}},
			Samples: map[collect.Key]collect.Stat{
				k:                     {Count: 3, TotalNs: 3000},
				{Name: "_Z3addiPfS_"}: {Count: 1, TotalNs: 500},
			},
		}},
	}
}

// TestBuildPyroscopeInvariants checks the properties Pyroscope's OTLP converter
// (pkg/ingester/otlp/convert.go, v2.4.x) relies on.
func TestBuildPyroscopeInvariants(t *testing.T) {
	req := Build(testSnapshot(), Options{ScopeVersion: "test"})
	if req == nil {
		t.Fatal("nil request")
	}
	d := req.Dictionary
	str := func(i int32) string {
		if i < 0 || int(i) >= len(d.StringTable) {
			t.Fatalf("string index %d out of range", i)
		}
		return d.StringTable[i]
	}

	if d.StringTable[0] != "" || !proto.Equal(d.LinkTable[0], &profilespb.Link{}) ||
		!proto.Equal(d.StackTable[0], &profilespb.Stack{}) || !proto.Equal(d.MappingTable[0], &profilespb.Mapping{}) {
		t.Fatal("dictionary tables must start with a zero entry")
	}

	if len(req.ResourceProfiles) != 1 {
		t.Fatalf("want 1 resource, got %d", len(req.ResourceProfiles))
	}
	rp := req.ResourceProfiles[0]
	if a := rp.Resource.Attributes[0]; a.Key != "service.name" || a.Value.GetStringValue() != "infer" {
		t.Fatalf("service.name missing: %v", rp.Resource.Attributes)
	}
	profiles := rp.ScopeProfiles[0].Profiles
	if len(profiles) != 2 {
		t.Fatalf("want 2 profiles, got %d", len(profiles))
	}

	wantTypes := []string{"gpu_time:nanoseconds:gpu:nanoseconds", "kernel_launches:count:gpu:nanoseconds"}
	var sawLink bool
	for i, p := range profiles {
		// Pyroscope: profile type = type:unit:periodType:periodUnit, name = periodType (needs Period != 0).
		typ := str(p.SampleType.TypeStrindex) + ":" + str(p.SampleType.UnitStrindex) + ":" +
			str(p.PeriodType.TypeStrindex) + ":" + str(p.PeriodType.UnitStrindex)
		if typ != wantTypes[i] || p.Period == 0 {
			t.Fatalf("profile %d type %q period %d", i, typ, p.Period)
		}
		if len(p.Samples) != 2 {
			t.Fatalf("profile %d: want 2 samples, got %d", i, len(p.Samples))
		}
		for _, s := range p.Samples {
			if len(s.Values) != 1 {
				t.Fatal("each sample needs exactly one value (one sample type per profile)")
			}
			stack := d.StackTable[s.StackIndex]
			if len(stack.LocationIndices) != 2 {
				t.Fatalf("stack %v", stack.LocationIndices)
			}
			var frames []string
			for _, li := range stack.LocationIndices {
				loc := d.LocationTable[li]
				if int(loc.MappingIndex) >= len(d.MappingTable) || len(loc.Lines) != 1 || loc.Lines[0].FunctionIndex <= 0 {
					t.Fatalf("bad location %v", loc)
				}
				frames = append(frames, str(d.FunctionTable[loc.Lines[0].FunctionIndex].NameStrindex))
			}
			if frames[1] != "cuda:0" {
				t.Fatalf("root frame %q", frames[1])
			}
			if s.LinkIndex != 0 {
				sawLink = true
				l := d.LinkTable[s.LinkIndex]
				if hex.EncodeToString(l.TraceId) != "0af7651916cd43dd8448eb211c80319c" || hex.EncodeToString(l.SpanId) != "b7ad6b7169203331" {
					t.Fatalf("bad link %x %x", l.TraceId, l.SpanId)
				}
				if strings.HasPrefix(frames[0], "_Z") || !strings.Contains(frames[0], "vectorized_elementwise_kernel") {
					t.Fatalf("kernel name not demangled: %q", frames[0])
				}
			}
		}
	}
	if !sawLink {
		t.Fatal("linked kernel has no link")
	}
	t.Logf("demangled frame: %s", kernelFrame(torchKernel))
}

func TestBuildEmpty(t *testing.T) {
	if req := Build(&collect.Snapshot{}, Options{}); req != nil {
		t.Fatal("expected nil for empty snapshot")
	}
}

type fakeReceiver struct {
	collectorpb.UnimplementedProfilesServiceServer
	got     chan *collectorpb.ExportProfilesServiceRequest
	headers chan metadata.MD
}

func (f *fakeReceiver) Export(ctx context.Context, req *collectorpb.ExportProfilesServiceRequest) (*collectorpb.ExportProfilesServiceResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	f.headers <- md
	f.got <- req
	return &collectorpb.ExportProfilesServiceResponse{}, nil
}

func TestExportOverGRPC(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	recv := &fakeReceiver{got: make(chan *collectorpb.ExportProfilesServiceRequest, 1), headers: make(chan metadata.MD, 1)}
	collectorpb.RegisterProfilesServiceServer(srv, recv)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	exp, err := NewExporter("http://"+lis.Addr().String(), map[string]string{"x-scope-orgid": "tenant1"})
	if err != nil {
		t.Fatal(err)
	}
	defer exp.Close()

	req := Build(testSnapshot(), Options{})
	if err := exp.Export(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	got := <-recv.got
	if !proto.Equal(got, req) {
		t.Fatal("received request differs from sent request")
	}
	if md := <-recv.headers; len(md.Get("x-scope-orgid")) != 1 || md.Get("x-scope-orgid")[0] != "tenant1" {
		t.Fatalf("header not sent: %v", md)
	}
}

func TestParseEndpoint(t *testing.T) {
	tests := []struct {
		in     string
		target string
		tls    bool
		err    bool
	}{
		{"http://collector:4317", "collector:4317", false, false},
		{"https://otlp.example.com:443/", "otlp.example.com:443", true, false},
		{"collector:4317", "collector:4317", true, false},
		{"http://collector:4318/v1/profiles", "", false, true},
		{"", "", false, true},
	}
	for _, tt := range tests {
		target, useTLS, err := parseEndpoint(tt.in)
		if (err != nil) != tt.err || target != tt.target || useTLS != tt.tls {
			t.Errorf("parseEndpoint(%q) = %q, %v, %v", tt.in, target, useTLS, err)
		}
	}
}
