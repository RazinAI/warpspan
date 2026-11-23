// Command profiles-dump is a tiny OTLP/gRPC profiles receiver that prints what
// it gets. Use it to check the agent's output without a Collector or Pyroscope:
//
//	go run ./tools/profiles-dump -listen 127.0.0.1:4317
//	./warpspan --otlp-endpoint 127.0.0.1:4317 ...
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"strings"

	collectorpb "go.opentelemetry.io/proto/otlp/collector/profiles/v1development"
	"google.golang.org/grpc"
	_ "google.golang.org/grpc/encoding/gzip"
)

type receiver struct {
	collectorpb.UnimplementedProfilesServiceServer
}

func (receiver) Export(_ context.Context, req *collectorpb.ExportProfilesServiceRequest) (*collectorpb.ExportProfilesServiceResponse, error) {
	d := req.GetDictionary()
	str := func(i int32) string {
		if i >= 0 && int(i) < len(d.GetStringTable()) {
			return d.GetStringTable()[i]
		}
		return fmt.Sprintf("<bad string %d>", i)
	}
	for _, rp := range req.GetResourceProfiles() {
		var attrs []string
		for _, a := range rp.GetResource().GetAttributes() {
			attrs = append(attrs, a.GetKey()+"="+a.GetValue().GetStringValue())
		}
		fmt.Printf("resource %s\n", strings.Join(attrs, " "))
		for _, sp := range rp.GetScopeProfiles() {
			for _, p := range sp.GetProfiles() {
				st, pt := p.GetSampleType(), p.GetPeriodType()
				fmt.Printf("  profile %s:%s:%s:%s  samples=%d\n", str(st.GetTypeStrindex()), str(st.GetUnitStrindex()),
					str(pt.GetTypeStrindex()), str(pt.GetUnitStrindex()), len(p.GetSamples()))
				for _, s := range p.GetSamples() {
					var frames []string
					if s.GetStackIndex() < int32(len(d.GetStackTable())) {
						for _, li := range d.GetStackTable()[s.GetStackIndex()].GetLocationIndices() {
							for _, line := range d.GetLocationTable()[li].GetLines() {
								frames = append(frames, str(d.GetFunctionTable()[line.GetFunctionIndex()].GetNameStrindex()))
							}
						}
					}
					link := "-"
					if li := s.GetLinkIndex(); li > 0 && int(li) < len(d.GetLinkTable()) {
						l := d.GetLinkTable()[li]
						link = "trace=" + hex.EncodeToString(l.GetTraceId()) + " span=" + hex.EncodeToString(l.GetSpanId())
					}
					// frames are leaf first; print root first like a flame graph
					for i, j := 0, len(frames)-1; i < j; i, j = i+1, j-1 {
						frames[i], frames[j] = frames[j], frames[i]
					}
					fmt.Printf("    %v  %s  [%s]\n", s.GetValues(), strings.Join(frames, " > "), link)
				}
			}
		}
	}
	return &collectorpb.ExportProfilesServiceResponse{}, nil
}

func main() {
	listen := flag.String("listen", "127.0.0.1:4317", "address to listen on")
	flag.Parse()
	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	srv := grpc.NewServer()
	collectorpb.RegisterProfilesServiceServer(srv, receiver{})
	log.Printf("listening for OTLP profiles on %s", *listen)
	log.Fatal(srv.Serve(lis))
}
