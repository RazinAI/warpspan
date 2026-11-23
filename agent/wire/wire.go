// Package wire decodes the messages libwarpspan.so sends to the agent.
//
// The format is defined in capture/protocol.h and documented in docs/protocol.md.
// Every message is one SOCK_SEQPACKET datagram, little-endian.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	Magic            uint32 = 0x4E505357 // "WSPN"
	Version          uint16 = 1
	HeaderSize              = 16
	KernelsFixedSize        = 16
	KernelRecordSize        = 64
	MaxMessage              = 60 * 1024
	FlagLinked       uint32 = 1 << 0
)

// Message types.
const (
	TypeHello   uint16 = 1
	TypeKernels uint16 = 2
)

// KV is one process metadata entry from a Hello message.
type KV struct {
	Key, Value string
}

// Hello is sent once per connection, before any kernels.
type Hello struct {
	PID   uint32
	Attrs []KV
}

// Kernel is one kernel execution, joined with the span that launched it.
type Kernel struct {
	Name          string
	StartNs       uint64
	EndNs         uint64
	TraceID       [16]byte
	SpanID        [8]byte
	CorrelationID uint32
	Device        uint32
	Stream        uint32
	ThreadID      uint32
	Linked        bool // TraceID/SpanID are valid
}

// Kernels is a batch of kernel executions.
type Kernels struct {
	// DroppedTotal is the running total of records the capture library could not
	// deliver (CUPTI buffer overflow or agent busy/unreachable).
	DroppedTotal uint64
	Records      []Kernel
}

var le = binary.LittleEndian

// ErrShort is returned when a message ends before its declared contents.
var ErrShort = errors.New("wire: message too short")

// Decode parses one message. It returns *Hello or *Kernels.
func Decode(msg []byte) (any, error) {
	if len(msg) < HeaderSize {
		return nil, ErrShort
	}
	if m := le.Uint32(msg[0:]); m != Magic {
		return nil, fmt.Errorf("wire: bad magic %#x", m)
	}
	if v := le.Uint16(msg[4:]); v != Version {
		return nil, fmt.Errorf("wire: unsupported version %d", v)
	}
	typ := le.Uint16(msg[6:])
	n := int(le.Uint32(msg[8:]))
	payload := msg[HeaderSize:]
	if len(payload) != n {
		return nil, fmt.Errorf("wire: payload length %d, header says %d", len(payload), n)
	}
	r := reader{b: payload}
	switch typ {
	case TypeHello:
		return decodeHello(&r)
	case TypeKernels:
		return decodeKernels(&r)
	default:
		return nil, fmt.Errorf("wire: unknown message type %d", typ)
	}
}

func decodeHello(r *reader) (*Hello, error) {
	h := &Hello{PID: r.u32()}
	count := r.u32()
	if r.err != nil {
		return nil, r.err
	}
	for i := uint32(0); i < count && r.err == nil; i++ {
		k, v := r.str16(), r.str16()
		h.Attrs = append(h.Attrs, KV{Key: k, Value: v})
	}
	return h, r.err
}

func decodeKernels(r *reader) (*Kernels, error) {
	k := &Kernels{DroppedTotal: r.u64()}
	nstrings, nrecords := r.u32(), r.u32()
	if r.err != nil {
		return nil, r.err
	}
	if int(nrecords)*KernelRecordSize > len(r.b) {
		return nil, ErrShort
	}
	names := make([]string, 0, nstrings)
	for i := uint32(0); i < nstrings && r.err == nil; i++ {
		names = append(names, r.str16())
	}
	k.Records = make([]Kernel, 0, nrecords)
	for i := uint32(0); i < nrecords && r.err == nil; i++ {
		var rec Kernel
		rec.StartNs = r.u64()
		rec.EndNs = r.u64()
		r.bytes(rec.TraceID[:])
		r.bytes(rec.SpanID[:])
		rec.CorrelationID = r.u32()
		nameIdx := r.u32()
		rec.Device = r.u32()
		rec.Stream = r.u32()
		rec.ThreadID = r.u32()
		rec.Linked = r.u32()&FlagLinked != 0
		if r.err != nil {
			break
		}
		if int(nameIdx) >= len(names) {
			return nil, fmt.Errorf("wire: name index %d out of range (%d names)", nameIdx, len(names))
		}
		rec.Name = names[nameIdx]
		k.Records = append(k.Records, rec)
	}
	return k, r.err
}

type reader struct {
	b   []byte
	err error
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if len(r.b) < n {
		r.err = ErrShort
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *reader) u16() uint16 {
	if b := r.take(2); b != nil {
		return le.Uint16(b)
	}
	return 0
}

func (r *reader) u32() uint32 {
	if b := r.take(4); b != nil {
		return le.Uint32(b)
	}
	return 0
}

func (r *reader) u64() uint64 {
	if b := r.take(8); b != nil {
		return le.Uint64(b)
	}
	return 0
}

func (r *reader) bytes(dst []byte) {
	if b := r.take(len(dst)); b != nil {
		copy(dst, b)
	}
}

func (r *reader) str16() string {
	n := int(r.u16())
	if b := r.take(n); b != nil {
		return string(b)
	}
	return ""
}
