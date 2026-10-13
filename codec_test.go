package imc

import (
	"bytes"
	"errors"
	"io"
	"math"
	"math/rand"
	"strings"
	"testing"
)

func sampleState() *EstimatedState {
	return &EstimatedState{
		Header: Header{Timestamp: 1760000000.125, Src: 0x2B01, SrcEnt: 7, Dst: 0xFFFF, DstEnt: 0xFF},
		Lat:    0.7188, Lon: -0.1525, Height: 12.5, X: 1, Y: -2, Z: 3, Phi: 0.1, Theta: 0.2, Psi: 0.3,
		U: 1.5, V: 0.1, W: 0.01, Vx: 1, Vy: 2, Vz: 3, P: 0.01, Q: 0.02, R: 0.03, Depth: 4.25, Alt: 12,
	}
}

func TestTypedRoundTrip(t *testing.T) {
	m := sampleState()
	for _, be := range []bool{false, true} {
		frame, err := AppendFrame(nil, m, be)
		if err != nil {
			t.Fatal(err)
		}
		if len(frame) != HeaderSize+88+FooterSize {
			t.Fatalf("EstimatedState frame of %d bytes", len(frame))
		}
		if be != (frame[0] == 0xFE) {
			t.Fatal("sync byte order")
		}
		back, err := Decode(frame)
		if err != nil {
			t.Fatal(err)
		}
		got := back.(*EstimatedState)
		want := *m
		want.Header.Sync, want.Header.MGID, want.Header.Size = SyncNumber, IDEstimatedState, 88
		if *got != want {
			t.Fatalf("be=%v: %+v != %+v", be, *got, want)
		}
	}
}

func TestEnumsAndBitfields(t *testing.T) {
	s := &EntityState{State: EntityStateState_FAULT, Flags: EntityStateFlags_HUMAN_INTERVENTION, Description: "x"}
	frame, _ := Marshal(s)
	back, _ := Decode(frame)
	es := back.(*EntityState)
	if es.State != EntityStateState_FAULT || es.State.String() != "FAULT" {
		t.Fatalf("enum: %v", es.State)
	}
	if es.Flags.String() != "HUMAN_INTERVENTION" || EntityStateState(9).String() != "EntityStateState(9)" {
		t.Fatalf("bitfield/unknown enum strings: %v %v", es.Flags, EntityStateState(9))
	}
	if (CLoopsMask_DEPTH | CLoopsMask_YAW | 0x100000).String() != "DEPTH|YAW|0x100000" {
		t.Fatalf("bitfield string: %v", CLoopsMask_DEPTH|CLoopsMask_YAW|0x100000)
	}
	if SystemType_UUV.String() != "UUV" || (&Announce{SysType: SystemType_UUV}).SysType != 2 {
		t.Fatal("global enumeration")
	}
	// Unknown enumeration values from the wire are kept, never an error.
	es.State = 200
	frame, _ = Marshal(es)
	back, err := Decode(frame)
	if err != nil || back.(*EntityState).State != 200 {
		t.Fatalf("unknown enum value: %v", err)
	}
}

func TestInlineAndLists(t *testing.T) {
	sp := &SetEntityParameters{Name: "Task", Params: []*EntityParameter{{Name: "A", Value: "1"}, nil, {Name: "B", Value: "x"}}}
	frame, err := Marshal(sp)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Decode(frame)
	if err != nil {
		t.Fatal(err)
	}
	got := back.(*SetEntityParameters)
	if len(got.Params) != 3 || got.Params[1] != nil || got.Params[2].Value != "x" {
		t.Fatalf("typed list: %+v", got.Params)
	}
	// A typed list with another message inside is an error (DUNE would
	// cast blindly).
	bad := &MsgList{Msgs: []IMessage{&Heartbeat{}}}
	payload, _ := bad.AppendPayload(nil, false)
	payload = append([]byte{byte(len("T")), 0, 'T'}, payload...)
	m := &SetEntityParameters{}
	if _, err := m.DecodePayload(payload, false); err == nil || !strings.Contains(err.Error(), "SetEntityParameters.params") {
		t.Fatalf("wrong list element: %v", err)
	}
	// Generic lists and inline messages, including null elements.
	ml := &MsgList{Msgs: []IMessage{&Heartbeat{}, nil, &EntityState{Description: "d"}}}
	frame, _ = Marshal(ml)
	back, _ = Decode(frame)
	if l := back.(*MsgList).Msgs; len(l) != 3 || l[1] != nil || l[2].(*EntityState).Description != "d" {
		t.Fatalf("generic list: %+v", l)
	}
	// A typed nil pointer in an interface field is a null message.
	var nilState *EntityState
	cc := &CacheControl{Message: nilState}
	frame, err = Marshal(cc)
	if err != nil {
		t.Fatal(err)
	}
	back, _ = Decode(frame)
	if back.(*CacheControl).Message != nil {
		t.Fatal("typed nil inline message")
	}
	if cc.ToMessage().Fields["message"] != (*Message)(nil) && cc.ToMessage().Fields["message"] != nil {
		t.Fatal("ToMessage of a typed nil inline message")
	}
}

func TestSizeLimits(t *testing.T) {
	// The largest frame is fine, one more byte is not.
	m := &DevDataBinary{Value: make([]byte, MaxPayloadSize-2)}
	frame, err := Marshal(m)
	if err != nil || len(frame) != MaxFrameSize {
		t.Fatalf("largest frame: %d bytes, %v", len(frame), err)
	}
	if _, err := Decode(frame); err != nil {
		t.Fatal(err)
	}
	m.Value = append(m.Value, 0)
	if _, err := Marshal(m); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized frame: %v", err)
	}
	dst := []byte("prefix")
	out, err := AppendFrame(dst, m, false)
	if err == nil || string(out) != "prefix" {
		t.Fatal("a failed AppendFrame must leave dst unchanged")
	}
	m.Value = make([]byte, 70000)
	if _, err := Marshal(m); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized rawdata: %v", err)
	}
	// Dynamic codec too.
	dm, _ := testProto.CreateMessage("DevDataBinary")
	dm.Fields["value"] = make([]byte, MaxPayloadSize-1)
	if _, err := dm.Serialize(testProto); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("dynamic oversized frame: %v", err)
	}
}

func TestDynamicStrict(t *testing.T) {
	p := testProto
	m, _ := p.CreateMessage("EntityState")
	if _, err := m.Serialize(p); err != nil {
		t.Fatalf("CreateMessage fills every field: %v", err)
	}
	delete(m.Fields, "flags")
	if _, err := m.Serialize(p); err == nil || !strings.Contains(err.Error(), "missing field") {
		t.Fatalf("missing field: %v", err)
	}
	m.Fields["flags"] = 300
	if _, err := m.Serialize(p); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("out of range: %v", err)
	}
	m.Fields["flags"] = "x"
	if _, err := m.Serialize(p); err == nil {
		t.Fatal("string for a number")
	}
	m.Fields["flags"] = 1.0 // integral floats (JSON) are fine
	m.Fields["description"] = []byte("bytes are fine for plaintext")
	frame, err := m.Serialize(p)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Decode(frame)
	if err != nil || back.(*EntityState).Flags != 1 {
		t.Fatalf("decode: %v", err)
	}
	// Lenient: the old behaviour.
	l := *p
	l.Lenient = true
	delete(m.Fields, "state")
	m.Fields["flags"] = 300
	frame, err = m.Serialize(&l)
	if err != nil {
		t.Fatal(err)
	}
	back, _ = Decode(frame)
	if back.(*EntityState).Flags != 44 || back.(*EntityState).State != 0 {
		t.Fatal("lenient truncates and zeroes")
	}
	// Typed messages in dynamic fields.
	sp, _ := p.CreateMessage("SetEntityParameters")
	sp.Fields["params"] = []IMessage{&EntityParameter{Name: "a", Value: "b"}}
	frame, err = sp.Serialize(p)
	if err != nil {
		t.Fatal(err)
	}
	back, _ = Decode(frame)
	if back.(*SetEntityParameters).Params[0].Value != "b" {
		t.Fatal("typed list in a dynamic message")
	}
}

func TestDynamicSyncAndCRC(t *testing.T) {
	frame, _ := Marshal(sampleState())
	bad := append([]byte(nil), frame...)
	bad[0], bad[1] = 0x54, 0xFE // LSTS sync number
	if _, err := testProto.Unmarshal(bad); !errors.Is(err, ErrSync) {
		t.Fatalf("strict accepts 0xFE54: %v", err)
	}
	l := *testProto
	l.Lenient = true
	if _, err := l.Unmarshal(bad); err != nil {
		t.Fatalf("lenient refuses 0xFE54: %v", err)
	}
	// Reading frames from a reader (LSF logs).
	var log bytes.Buffer
	for i := 0; i < 3; i++ {
		log.Write(frame)
	}
	n := 0
	for {
		_, err := testProto.UnmarshalReader(&log)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 3 {
		t.Fatal(n)
	}
	frame[30] ^= 1
	if _, err := testProto.UnmarshalReader(bytes.NewReader(frame)); !errors.Is(err, ErrCRC) {
		t.Fatalf("CRC: %v", err)
	}
}

func TestCRC16(t *testing.T) {
	// CRC-16/ARC check value.
	if c := CRC16([]byte("123456789")); c != 0xBB3D {
		t.Fatalf("CRC16 = %04X", c)
	}
	// Slicing by 8 == bit by bit, for every length and alignment.
	slow := func(data []byte) uint16 {
		var crc uint16
		for _, b := range data {
			crc ^= uint16(b)
			for i := 0; i < 8; i++ {
				if crc&1 != 0 {
					crc = crc>>1 ^ 0xA001
				} else {
					crc >>= 1
				}
			}
		}
		return crc
	}
	rng := rand.New(rand.NewSource(2))
	data := make([]byte, 300)
	rng.Read(data)
	for i := 0; i < 40; i++ {
		for n := 0; n+i <= len(data); n += 7 {
			if CRC16(data[i:i+n]) != slow(data[i:i+n]) {
				t.Fatalf("CRC16 of %d bytes at %d", n, i)
			}
		}
	}
}

// -- FrameReader --------------------------------------------------------------

func frames(t *testing.T, msgs ...IMessage) []byte {
	var b []byte
	for _, m := range msgs {
		var err error
		if b, err = AppendFrame(b, m, false); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

func readAll(t *testing.T, fr *FrameReader) []IMessage {
	var out []IMessage
	for {
		f, err := fr.Next()
		if err == io.EOF || err == ErrNeedMore {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		m, err := f.Decode()
		if err == nil {
			out = append(out, m)
		}
	}
}

func TestFrameReaderResync(t *testing.T) {
	a := frames(t, &DevDataText{Value: "a"})
	b := frames(t, &DevDataText{Value: "b"})
	be, _ := AppendFrame(nil, &DevDataText{Value: "be"}, true)
	badCRC := append([]byte(nil), a...)
	badCRC[len(badCRC)-1] ^= 0xFF
	var stream []byte
	// Garbage with sync bytes (DUNE, like this reader, takes a false sync
	// followed by a big size for the start of a frame and waits for it:
	// the garbage here does not form one).
	stream = append(stream, 0x55, 0x00, 0xFE, 0x00, 0x55, 0x01, 0xFE)
	stream = append(stream, a...)
	stream = append(stream, badCRC...)
	stream = append(stream, bytes.Repeat([]byte{0x55}, 1000)...)
	stream = append(stream, 0)
	stream = append(stream, be...)
	stream = append(stream, bytes.Repeat([]byte{0xFE}, 1000)...)
	stream = append(stream, 0)
	stream = append(stream, b...)
	for _, chunk := range []int{1, 5, 64, 1 << 20} {
		fr := NewFrameReader(&chunkReader{data: stream, n: chunk})
		got := readAll(t, fr)
		var vals []string
		for _, m := range got {
			vals = append(vals, m.(*DevDataText).Value)
		}
		if strings.Join(vals, ",") != "a,be,b" {
			t.Fatalf("chunk %d: %v", chunk, vals)
		}
		if fr.CRCErrors == 0 || fr.SkippedBytes == 0 {
			t.Fatalf("stats: %+v", fr)
		}
	}
}

func TestFrameReaderLargeFrames(t *testing.T) {
	big := &DevDataBinary{Value: make([]byte, MaxPayloadSize-2)}
	for i := range big.Value {
		big.Value[i] = byte(i)
	}
	stream := frames(t, big, &Heartbeat{}, big, big)
	fr := NewFrameReader(&chunkReader{data: stream, n: 1000})
	got := readAll(t, fr)
	if len(got) != 4 || len(got[3].(*DevDataBinary).Value) != MaxPayloadSize-2 {
		t.Fatalf("%d frames", len(got))
	}
	// A header announcing 65535 bytes (bigger than anything DUNE writes)
	// is waited for, then checked.
	h := Header{Sync: SyncNumber, MGID: IDDevDataBinary, Size: 0xFFFF}
	frame := make([]byte, MaxInFrameSize)
	putHeader(frame, &h, false)
	copy(frame[HeaderSize:], []byte{0xFD, 0xFF})
	crc := CRC16(frame[:MaxInFrameSize-2])
	frame[MaxInFrameSize-2], frame[MaxInFrameSize-1] = byte(crc), byte(crc>>8)
	fr = NewFrameReader(&chunkReader{data: frame, n: 4096})
	if f, err := fr.Next(); err != nil || len(f.Raw) != MaxInFrameSize {
		t.Fatalf("65557-byte frame: %v", err)
	}
}

func TestFrameReaderAllocations(t *testing.T) {
	stream := frames(t, sampleState(), &Heartbeat{}, &DevDataText{Value: "x"})
	for i := 0; i < 6; i++ {
		stream = append(stream, stream...)
	}
	r := bytes.NewReader(stream)
	fr := NewFrameReader(r)
	allocs := testing.AllocsPerRun(20, func() {
		r.Reset(stream)
		fr.err = nil
		for {
			if _, err := fr.Next(); err != nil {
				break
			}
		}
	})
	if allocs > 0 {
		t.Fatalf("FrameReader allocates %.1f times per run", allocs)
	}
}

// Random bytes, truncations and bit flips never panic the readers or the
// decoders.
func TestHostileInput(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var valid [][]byte
	for _, m := range []IMessage{sampleState(), &SetEntityParameters{Name: "x", Params: []*EntityParameter{{Name: "a"}}},
		&MsgList{Msgs: []IMessage{&Heartbeat{}, nil, &PlanControl{Arg: &Goto{}}}}, &DevDataText{Value: "abc"}} {
		f, _ := Marshal(m)
		valid = append(valid, f)
	}
	for i := 0; i < 20000; i++ {
		f := append([]byte(nil), valid[rng.Intn(len(valid))]...)
		switch rng.Intn(3) {
		case 0:
			f = f[:rng.Intn(len(f)+1)]
		case 1:
			f[rng.Intn(len(f))] ^= byte(1 << rng.Intn(8))
		case 2:
			f = make([]byte, rng.Intn(200))
			rng.Read(f)
		}
		// Recompute the CRC sometimes so that the decoders see the damage.
		if len(f) >= HeaderSize+2 && rng.Intn(2) == 0 {
			crc := CRC16(f[:len(f)-2])
			f[len(f)-2], f[len(f)-1] = byte(crc), byte(crc>>8)
		}
		Decode(f)
		testProto.Unmarshal(f)
		if len(f) > HeaderSize {
			for _, id := range MessageIDs {
				NewMessage(id).DecodePayload(f[HeaderSize:], rng.Intn(2) == 0)
			}
		}
		fr := NewFrameReader(bytes.NewReader(f))
		readAll(t, fr)
	}
}

func FuzzDecode(f *testing.F) {
	for _, m := range []IMessage{sampleState(), &SetEntityParameters{Name: "x", Params: []*EntityParameter{{Name: "a"}}}} {
		b, _ := Marshal(m)
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if m, err := Decode(data); err == nil {
			if _, err := Marshal(m); err != nil {
				t.Fatalf("decoded message does not encode: %v", err)
			}
		}
		testProto.Unmarshal(data)
		fr := NewFrameReader(bytes.NewReader(data))
		for {
			fm, err := fr.Next()
			if err != nil {
				break
			}
			fm.Decode()
			testProto.DecodeFrame(fm)
		}
	})
}

func TestNaNKeepsBits(t *testing.T) {
	m := &EstimatedState{Lat: math.Float64frombits(0x7FF8000000000123), Depth: float32(math.Inf(-1))}
	frame, _ := Marshal(m)
	back, _ := Decode(frame)
	if math.Float64bits(back.(*EstimatedState).Lat) != 0x7FF8000000000123 {
		t.Fatal("NaN payload changed")
	}
}
