package imc

// Benchmarks of the dynamic (XML driven) and the typed (generated) codecs
// on EstimatedState, the most frequent message on a vehicle's bus:
//
//	go test -run XXX -bench . -benchmem
//
// The Dynamic benchmarks use only the API of the old codec (CreateMessage,
// Serialize, Unmarshal), so they also run against older versions.

import (
	"bytes"
	"testing"
)

func benchDynamicState(b *testing.B) (*Protocol, *Message) {
	x, err := ParseXML("IMC.xml")
	if err != nil {
		b.Fatal(err)
	}
	p := NewProtocol(x)
	m, _ := p.CreateMessage("EstimatedState")
	for _, f := range []string{"lat", "lon", "depth"} {
		m.Fields[f] = 0.5
	}
	for _, f := range []string{"x", "y", "z", "phi", "theta", "psi", "u", "v", "w", "vx", "vy", "vz", "p", "q", "r", "alt", "height"} {
		m.Fields[f] = float32(1.25)
	}
	m.Fields["lat"], m.Fields["lon"] = 0.7188, -0.1525
	m.Fields["depth"] = float32(4.25)
	m.Header.Timestamp = 1760000000.125
	return p, m
}

func BenchmarkDynamicEncode(b *testing.B) {
	p, m := benchDynamicState(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := m.Serialize(p); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDynamicDecode(b *testing.B) {
	p, m := benchDynamicState(b)
	frame, err := m.Serialize(p)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := p.Unmarshal(frame); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTypedEncode(b *testing.B) {
	m := sampleState()
	buf := make([]byte, 0, 256)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var err error
		if buf, err = AppendFrame(buf[:0], m, false); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTypedMarshal(b *testing.B) {
	m := sampleState()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Marshal(m); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTypedDecode(b *testing.B) {
	frame, _ := Marshal(sampleState())
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Decode(frame); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTypedDecodeInto(b *testing.B) {
	frame, _ := Marshal(sampleState())
	var m EstimatedState
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		f := Frame{BigEndian: false, Payload: frame[HeaderSize : len(frame)-2]}
		f.Header.MGID = IDEstimatedState
		if err := f.DecodeInto(&m); err != nil {
			b.Fatal(err)
		}
	}
}

// A stream of EstimatedState frames through FrameReader (CRC checked) and
// the typed decoder: what a task does with what it receives.
func BenchmarkStreamTyped(b *testing.B) {
	frame, _ := Marshal(sampleState())
	stream := bytes.Repeat(frame, 1000)
	r := bytes.NewReader(stream)
	fr := NewFrameReader(r)
	b.SetBytes(int64(len(frame)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		f, err := fr.Next()
		if err != nil {
			r.Reset(stream)
			fr.err = nil
			f, _ = fr.Next()
		}
		if _, err := f.Decode(); err != nil {
			b.Fatal(err)
		}
	}
}

// The same, skipping the frames after the CRC check (a message no handler
// wants).
func BenchmarkStreamSkip(b *testing.B) {
	frame, _ := Marshal(sampleState())
	stream := bytes.Repeat(frame, 1000)
	r := bytes.NewReader(stream)
	fr := NewFrameReader(r)
	b.SetBytes(int64(len(frame)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := fr.Next(); err != nil {
			r.Reset(stream)
			fr.err = nil
		}
	}
}

func BenchmarkCRC16(b *testing.B) {
	data := make([]byte, 1024)
	b.SetBytes(1024)
	var sum uint16
	for i := 0; i < b.N; i++ {
		sum += CRC16(data)
		data[0] = byte(sum)
	}
}
