package imc_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	imc "github.com/oceanscan/imc-go"
)

// Typed messages: structs with enumeration types, encoded and decoded
// directly (no reflection).
func ExampleMarshal() {
	m := &imc.EntityState{State: imc.EntityStateState_FAULT, Description: "too deep"}
	m.Header.Timestamp = 1760000000
	m.Header.Src, m.Header.SrcEnt, m.Header.Dst, m.Header.DstEnt = 0x2b01, 7, imc.Broadcast, imc.UnknownEntity
	frame, _ := imc.Marshal(m)
	fmt.Printf("% X ...\n", frame[:6])

	back, _ := imc.Decode(frame)
	es := back.(*imc.EntityState)
	fmt.Println(es.Abbrev(), es.State, es.Description, es.Header.Src)
	// Output:
	// 55 FE 01 00 0C 00 ...
	// EntityState FAULT too deep 11009
}

// A stream reader with DUNE's resynchronization: garbage and broken frames
// are skipped, big endian frames decoded.
func ExampleFrameReader() {
	var stream []byte
	stream = append(stream, "garbage"...)
	stream, _ = imc.AppendFrame(stream, &imc.DevDataText{Value: "one"}, false)
	stream, _ = imc.AppendFrame(stream, &imc.DevDataText{Value: "two"}, true) // big endian
	stream, _ = imc.AppendFrame(stream, &imc.Heartbeat{}, false)

	fr := imc.NewFrameReader(bytes.NewReader(stream))
	for {
		f, err := fr.Next()
		if err == io.EOF {
			break
		}
		if f.Header.MGID != imc.IDDevDataText {
			continue // skipped without decoding
		}
		m, err := f.Decode()
		var unknown *imc.UnknownMessageError
		if errors.As(err, &unknown) {
			continue
		}
		fmt.Println(m.(*imc.DevDataText).Value, f.BigEndian)
	}
	fmt.Println(fr.SkippedBytes, "bytes skipped")
	// Output:
	// one false
	// two true
	// 7 bytes skipped
}

// The dynamic codec: fields in a map, driven by an IMC XML at run time (log
// tools use the XML stored in each log).
func ExampleProtocol() {
	x, _ := imc.ParseXML("IMC.xml")
	p := imc.NewProtocol(x)
	m, _ := p.CreateMessage("DevDataText") // every field set to its zero value
	m.Fields["value"] = "hello"
	frame, _ := m.Serialize(p)

	back, _ := p.Unmarshal(frame)
	fmt.Println(back.Fields["value"], p.MD5 == imc.IMCMD5)

	m.Fields["value"] = 42 // strict: a number for a text
	_, err := m.Serialize(p)
	fmt.Println(err != nil)
	// Output:
	// hello true
	// true
}

// Enumerations and bitfields are types with constants and String methods.
func ExampleCLoopsMask() {
	loops := imc.CLoopsMask_DEPTH | imc.CLoopsMask_YAW
	fmt.Println(loops, uint32(loops), imc.SystemType_UUV)
	// Output: DEPTH|YAW 72 UUV
}
