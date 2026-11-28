package wire

import "strings"

// Encoders mirror what libwarpspan.so sends. They are used by tests only.

func header(typ uint16, pid uint32, payloadLen int) []byte {
	return headerV(Version, typ, pid, payloadLen)
}

func headerV(version, typ uint16, pid uint32, payloadLen int) []byte {
	b := make([]byte, HeaderSize, HeaderSize+payloadLen)
	le.PutUint32(b[0:], Magic)
	le.PutUint16(b[4:], version)
	le.PutUint16(b[6:], typ)
	le.PutUint32(b[8:], uint32(payloadLen))
	le.PutUint32(b[12:], pid)
	return b
}

func appendStr16(b []byte, s string) []byte {
	b = le.AppendUint16(b, uint16(len(s)))
	return append(b, s...)
}

// EncodeHello encodes a Hello message.
func EncodeHello(h *Hello) []byte {
	p := le.AppendUint32(nil, uint32(len(h.Attrs)))
	for _, kv := range h.Attrs {
		p = appendStr16(p, kv.Key)
		p = appendStr16(p, kv.Value)
	}
	return append(header(TypeHello, h.PID, len(p)), p...)
}

// EncodeKernels encodes a version 2 Kernels message (strings and label paths
// are interned per message).
func EncodeKernels(k *Kernels) []byte {
	strIndex := map[string]uint32{}
	var strs []string
	intern := func(s string) uint32 {
		if i, ok := strIndex[s]; ok {
			return i
		}
		strIndex[s] = uint32(len(strs))
		strs = append(strs, s)
		return strIndex[s]
	}
	pathIndex := map[string]uint32{}
	var paths [][]uint32
	recPath := make([]uint32, len(k.Records))
	for i, r := range k.Records {
		intern(r.Name)
		recPath[i] = NoPath
		if len(r.Labels) == 0 {
			continue
		}
		key := strings.Join(r.Labels, LabelSep)
		idx, ok := pathIndex[key]
		if !ok {
			p := make([]uint32, len(r.Labels))
			for j, l := range r.Labels {
				p[j] = intern(l)
			}
			idx = uint32(len(paths))
			paths = append(paths, p)
			pathIndex[key] = idx
		}
		recPath[i] = idx
	}
	p := le.AppendUint64(nil, k.DroppedTotal)
	p = le.AppendUint32(p, uint32(len(strs)))
	p = le.AppendUint32(p, uint32(len(paths)))
	p = le.AppendUint32(p, uint32(len(k.Records)))
	for _, s := range strs {
		p = appendStr16(p, s)
	}
	for _, path := range paths {
		p = le.AppendUint16(p, uint16(len(path)))
		for _, i := range path {
			p = le.AppendUint32(p, i)
		}
	}
	for i, r := range k.Records {
		p = le.AppendUint64(p, r.StartNs)
		p = le.AppendUint64(p, r.EndNs)
		p = append(p, r.TraceID[:]...)
		p = append(p, r.SpanID[:]...)
		p = le.AppendUint32(p, strIndex[r.Name])
		p = le.AppendUint32(p, r.Device)
		p = le.AppendUint32(p, recPath[i])
	}
	return append(header(TypeKernels, k.PID, len(p)), p...)
}

// EncodeKernelsV1 encodes a version 1 Kernels message, as libraries from
// before NVTX labels send it. Labels are not encoded.
func EncodeKernelsV1(k *Kernels) []byte {
	index := map[string]uint32{}
	var names []string
	for _, r := range k.Records {
		if _, ok := index[r.Name]; !ok {
			index[r.Name] = uint32(len(names))
			names = append(names, r.Name)
		}
	}
	p := le.AppendUint64(nil, k.DroppedTotal)
	p = le.AppendUint32(p, uint32(len(names)))
	p = le.AppendUint32(p, uint32(len(k.Records)))
	for _, n := range names {
		p = appendStr16(p, n)
	}
	for _, r := range k.Records {
		p = le.AppendUint64(p, r.StartNs)
		p = le.AppendUint64(p, r.EndNs)
		p = append(p, r.TraceID[:]...)
		p = append(p, r.SpanID[:]...)
		p = le.AppendUint32(p, index[r.Name])
		p = le.AppendUint32(p, r.Device)
	}
	return append(headerV(1, TypeKernels, k.PID, len(p)), p...)
}
