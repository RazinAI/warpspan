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
	KernelRecordSize        = 48
	MaxMessage              = 60 * 1024
	MaxNameLen              = 2048 // the capture library truncates kernel names to this
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
	Name    string
	StartNs uint64
	EndNs   uint64
	TraceID [16]byte
	SpanID  [8]byte // all zero: not linked to a span
	Device  uint32
}

// Linked reports whether the kernel was launched inside a span.
func (k *Kernel) Linked() bool { return k.SpanID != [8]byte{} }

// Kernels is a batch of kernel executions.
type Kernels struct {
	PID uint32
	// DroppedTotal is the process's running total of records lost to CUPTI
	// buffer overflow or to the agent being busy.
	DroppedTotal uint64
	Records      []Kernel
}

var le = binary.LittleEndian

// ErrShort is returned when a message ends before its declared contents.
var ErrShort = errors.New("wire: message too short")

// Decode parses one message. It returns *Hello or *Kernels.
func Decode(msg []byte) (any, error) {
	if len(msg) > MaxMessage {
		return nil, fmt.Errorf("wire: message of %d bytes exceeds %d", len(msg), MaxMessage)
	}
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
	pid := le.Uint32(msg[12:])
	payload := msg[HeaderSize:]
	if len(payload) != n {
		return nil, fmt.Errorf("wire: payload length %d, header says %d", len(payload), n)
	}
	r := reader{b: payload}
	var m any
	var err error
	switch typ {
	case TypeHello:
		m, err = decodeHello(&r, pid)
	case TypeKernels:
		m, err = decodeKernels(&r, pid)
	default:
		return nil, fmt.Errorf("wire: unknown message type %d", typ)
	}
	if err == nil && len(r.b) != 0 {
		return nil, fmt.Errorf("wire: %d trailing bytes", len(r.b))
	}
	return m, err
}

func decodeHello(r *reader, pid uint32) (*Hello, error) {
	h := &Hello{PID: pid}
	count := r.u32()
	for i := uint32(0); i < count && r.err == nil; i++ {
		k, v := r.str16(), r.str16()
		if r.err == nil {
			h.Attrs = append(h.Attrs, KV{Key: k, Value: v})
		}
	}
	return h, r.err
}

func decodeKernels(r *reader, pid uint32) (*Kernels, error) {
	k := &Kernels{PID: pid, DroppedTotal: r.u64()}
	nstrings, nrecords := int(r.u32()), int(r.u32())
	if r.err != nil {
		return nil, r.err
	}
	// Counts come from the peer: bound them by the bytes actually present
	// before allocating (each string takes >= 2 bytes, each record 48).
	if nstrings*2+nrecords*KernelRecordSize > len(r.b) {
		return nil, ErrShort
	}
	names := make([]string, 0, nstrings)
	for i := 0; i < nstrings && r.err == nil; i++ {
		name := r.str16()
		if len(name) > MaxNameLen {
			return nil, fmt.Errorf("wire: kernel name of %d bytes exceeds %d", len(name), MaxNameLen)
		}
		names = append(names, name)
	}
	k.Records = make([]Kernel, 0, nrecords)
	for i := 0; i < nrecords && r.err == nil; i++ {
		var rec Kernel
		rec.StartNs = r.u64()
		rec.EndNs = r.u64()
		r.bytes(rec.TraceID[:])
		r.bytes(rec.SpanID[:])
		nameIdx := int(r.u32())
		rec.Device = r.u32()
		if r.err != nil {
			break
		}
		if nameIdx >= len(names) {
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
