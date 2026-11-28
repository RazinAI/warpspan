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

func TestLabels(t *testing.T) {
	kernels := &Kernels{
		PID: 7,
		Records: []Kernel{
			{Name: "sgemm", StartNs: 1, EndNs: 2, Labels: []string{"infer s3 f120", "layer conv1"}},
			{Name: "sgemm", StartNs: 3, EndNs: 4, Labels: []string{"infer s3 f120", "layer conv1"}},
			{Name: "nms", StartNs: 5, EndNs: 6, Labels: []string{"infer s3 f120"}},
			{Name: "copy", StartNs: 7, EndNs: 8},                                      // outside any range
			{Name: "infer s3 f120", StartNs: 9, EndNs: 10, Labels: []string{"sgemm"}}, // strings are shared
		},
	}
	msg := EncodeKernels(kernels)
	got, err := Decode(msg)
	if err != nil {
		t.Fatal(err)
	}
	for i := range kernels.Records {
		kernels.Records[i].LabelKey = strings.Join(kernels.Records[i].Labels, LabelSep)
	}
	if !reflect.DeepEqual(got, kernels) {
		t.Fatalf("got %+v\nwant %+v", got, kernels)
	}
	// 5 strings (sgemm, infer s3 f120, layer conv1, nms, copy) and 3 paths.
	if n := binary.LittleEndian.Uint32(msg[HeaderSize+8:]); n != 5 {
		t.Errorf("string_count %d, want 5", n)
	}
	if n := binary.LittleEndian.Uint32(msg[HeaderSize+12:]); n != 3 {
		t.Errorf("path_count %d, want 3", n)
	}
}

// Libraries from before NVTX labels send version 1; the agent still reads it.
func TestVersion1(t *testing.T) {
	kernels := &Kernels{PID: 3, DroppedTotal: 2, Records: []Kernel{
		{Name: "k", StartNs: 1, EndNs: 2, TraceID: [16]byte{1}, SpanID: [8]byte{2}, Device: 1},
		{Name: "k2", StartNs: 3, EndNs: 4},
	}}
	msg := EncodeKernelsV1(kernels)
	if want := HeaderSize + KernelsFixedSizeV1 + 2*2 + 3 + 2*KernelRecordSizeV1; len(msg) != want {
		t.Fatalf("v1 size %d, want %d", len(msg), want)
	}
	got, err := Decode(msg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, kernels) {
		t.Fatalf("got %+v want %+v", got, kernels)
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
	// One string "k", one path [0], one record using path 0.
	withPath := EncodeKernels(&Kernels{Records: []Kernel{{Name: "k", Labels: []string{"k"}}}})
	pathOff := HeaderSize + KernelsFixedSize + 3 // path entry: u16 depth, u32 index
	recOff := pathOff + 6
	setU32 := func(m []byte, off int, v uint32) []byte {
		m = append([]byte(nil), m...)
		binary.LittleEndian.PutUint32(m[off:], v)
		return m
	}
	setU16 := func(m []byte, off int, v uint16) []byte {
		m = append([]byte(nil), m...)
		binary.LittleEndian.PutUint16(m[off:], v)
		return m
	}

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
		{"path index out of range", setU32(withPath, recOff+48, 1)},
		{"label index out of range", setU32(withPath, pathOff+2, 5)},
		{"path depth zero", setU16(withPath, pathOff, 0)},
		{"path depth too large", setU16(withPath, pathOff, MaxNvtxDepth+1)},
		{"label too long", EncodeKernels(&Kernels{Records: []Kernel{{Name: "k", Labels: []string{strings.Repeat("l", MaxLabelLen+1)}}}})},
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
	f.Add(EncodeKernels(&Kernels{Records: []Kernel{{Name: "k", Labels: []string{"a", "b"}}}}))
	f.Add(EncodeKernelsV1(&Kernels{Records: []Kernel{{Name: "k"}}}))
	f.Fuzz(func(t *testing.T, msg []byte) {
		_, _ = Decode(msg) // must not panic or allocate unboundedly
	})
}
