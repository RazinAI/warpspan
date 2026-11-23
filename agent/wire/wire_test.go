package wire

import (
	"errors"
	"reflect"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	hello := &Hello{PID: 42, Attrs: []KV{{"process.pid", "42"}, {"env.OTEL_SERVICE_NAME", "svc"}}}
	got, err := Decode(EncodeHello(hello))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, hello) {
		t.Fatalf("hello: got %+v want %+v", got, hello)
	}

	kernels := &Kernels{
		DroppedTotal: 7,
		Records: []Kernel{
			{Name: "_Z3addiPfS_", StartNs: 100, EndNs: 250, TraceID: [16]byte{1}, SpanID: [8]byte{2},
				CorrelationID: 5, Device: 1, Stream: 7, ThreadID: 99, Linked: true},
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
}

func TestRecordSize(t *testing.T) {
	one := EncodeKernels(&Kernels{Records: []Kernel{{Name: ""}}})
	// header + fixed + one empty string (2 bytes) + one record
	if want := HeaderSize + KernelsFixedSize + 2 + KernelRecordSize; len(one) != want {
		t.Fatalf("encoded size %d, want %d (record layout drifted from capture/protocol.h?)", len(one), want)
	}
}

func TestDecodeErrors(t *testing.T) {
	valid := EncodeKernels(&Kernels{Records: []Kernel{{Name: "k"}}})

	badMagic := append([]byte(nil), valid...)
	badMagic[0] ^= 0xFF

	badVersion := append([]byte(nil), valid...)
	badVersion[4] = 99

	badNameIndex := append([]byte(nil), valid...)
	// name_index lives 4 bytes after correlation_id: header+fixed+string(2+1)+40
	off := HeaderSize + KernelsFixedSize + 3 + 44
	badNameIndex[off] = 9

	tests := []struct {
		name string
		msg  []byte
	}{
		{"empty", nil},
		{"short header", valid[:10]},
		{"bad magic", badMagic},
		{"bad version", badVersion},
		{"truncated payload", valid[:len(valid)-1]},
		{"name index out of range", badNameIndex},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Decode(tt.msg); err == nil {
				t.Fatal("expected error")
			}
		})
	}

	if _, err := Decode(valid[:HeaderSize-1]); !errors.Is(err, ErrShort) {
		t.Fatalf("want ErrShort, got %v", err)
	}
}
