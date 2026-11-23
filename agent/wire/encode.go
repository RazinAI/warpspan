package wire

// Encoders mirror what libwarpspan.so sends. The agent never sends these; they
// exist for tests and for tools that simulate a capture library.

func header(typ uint16, payloadLen int) []byte {
	b := make([]byte, HeaderSize, HeaderSize+payloadLen)
	le.PutUint32(b[0:], Magic)
	le.PutUint16(b[4:], Version)
	le.PutUint16(b[6:], typ)
	le.PutUint32(b[8:], uint32(payloadLen))
	return b
}

func appendStr16(b []byte, s string) []byte {
	b = le.AppendUint16(b, uint16(len(s)))
	return append(b, s...)
}

// EncodeHello encodes a Hello message.
func EncodeHello(h *Hello) []byte {
	var p []byte
	p = le.AppendUint32(p, h.PID)
	p = le.AppendUint32(p, uint32(len(h.Attrs)))
	for _, kv := range h.Attrs {
		p = appendStr16(p, kv.Key)
		p = appendStr16(p, kv.Value)
	}
	return append(header(TypeHello, len(p)), p...)
}

// EncodeKernels encodes a Kernels message (names are interned per message).
func EncodeKernels(k *Kernels) []byte {
	index := map[string]uint32{}
	var names []string
	for _, r := range k.Records {
		if _, ok := index[r.Name]; !ok {
			index[r.Name] = uint32(len(names))
			names = append(names, r.Name)
		}
	}
	var p []byte
	p = le.AppendUint64(p, k.DroppedTotal)
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
		p = le.AppendUint32(p, r.CorrelationID)
		p = le.AppendUint32(p, index[r.Name])
		p = le.AppendUint32(p, r.Device)
		p = le.AppendUint32(p, r.Stream)
		p = le.AppendUint32(p, r.ThreadID)
		var flags uint32
		if r.Linked {
			flags |= FlagLinked
		}
		p = le.AppendUint32(p, flags)
	}
	return append(header(TypeKernels, len(p)), p...)
}
