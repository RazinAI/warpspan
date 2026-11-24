package wire

import (
	"encoding/binary"
	"reflect"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	hello := &Hello{PID: 42, Attrs: []KV{{Key: "host.name", Value: "gpu-1"}, {Key: "env.OTEL_SERVICE_NAME", Value: "svc"}}}
	got, err := Decode(EncodeHello(hello))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, hello) {
		t.Fatalf("hello: got %+v want %+v", got, hello)
	}

	kernels := &Kernels{
		PID:          42,
		DroppedTotal: 7,
		Records: []Kernel{
			{Name: "_Z3addiPfS_", StartNs: 100, EndNs: 250, TraceID: [16]byte{1}, SpanID: [8]byte{2}, Device: 1},
			{Name: "other", StartNs: 300, EndNs: 400},
			{Name: "_Z3addiPfS_", StartNs: 500, EndNs: 600},
		},
	}
	got, err = Decode(EncodeKernels(kernels))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, kernels) {
		t.Fatalf("kernels: got %+v want %+v", got, kernels)
	}
	if !kernels.Records[0].Linked() || kernels.Records[1].Linked() {
		t.Fatal("Linked() wrong")
	}
}

// The record size must match capture/protocol.h.
func TestRecordSize(t *testing.T) {
	one := EncodeKernels(&Kernels{Records: []Kernel{{Name: ""}}})
	if want := HeaderSize + KernelsFixedSize + 2 + KernelRecordSize; len(one) != want {
		t.Fatalf("encoded size %d, want %d", len(one), want)
	}
}

func TestDecodeErrors(t *testing.T) {
	valid := EncodeKernels(&Kernels{Records: []Kernel{{Name: "k"}}})
	corrupt := func(off int, b byte) []byte {
		m := append([]byte(nil), valid...)
		m[off] = b
		return m
	}
	hugeCount := append([]byte(nil), valid...)
	binary.LittleEndian.PutUint32(hugeCount[HeaderSize+8:], 0xFFFFFFFF) // string_count
	trailing := append(append([]byte(nil), valid...), 0)
	binary.LittleEndian.PutUint32(trailing[8:], uint32(len(trailing)-HeaderSize)) // payload_len
	longName := EncodeKernels(&Kernels{Records: []Kernel{{Name: strings.Repeat("k", MaxNameLen+1)}}})

	tests := []struct {
		name string
		msg  []byte
	}{
		{"empty", nil},
		{"short header", valid[:10]},
		{"bad magic", corrupt(0, 0)},
		{"bad version", corrupt(4, 99)},
		{"truncated payload", valid[:len(valid)-1]},
		{"name index out of range", corrupt(HeaderSize+KernelsFixedSize+3+40, 9)},
		{"huge string count", hugeCount},
		{"too large", make([]byte, MaxMessage+1)},
		{"trailing bytes", trailing},
		{"kernel name too long", longName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Decode(tt.msg); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func FuzzDecode(f *testing.F) {
	f.Add(EncodeHello(&Hello{PID: 1, Attrs: []KV{{Key: "a", Value: "b"}}}))
	f.Add(EncodeKernels(&Kernels{Records: []Kernel{{Name: "k", SpanID: [8]byte{1}}}}))
	f.Fuzz(func(t *testing.T, msg []byte) {
		_, _ = Decode(msg) // must not panic or allocate unboundedly
	})
}
