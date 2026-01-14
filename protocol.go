package imc

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"
)

// Message is a generic IMC message holding fields in a map.
type Message struct {
	Header Header
	Fields map[string]any
}

type Header struct {
	Sync      uint16
	MGID      uint16
	Size      uint16
	Timestamp float64
	Src       uint16
	SrcEnt    uint8
	Dst       uint16
	DstEnt    uint8
}

type Protocol struct {
	Version  string
	Messages map[uint16]*XMLMessage
	Lookup   map[string]*XMLMessage
}

func NewProtocol(xmlProto *XMLProtocol) *Protocol {
	p := &Protocol{
		Version:  xmlProto.Version,
		Messages: make(map[uint16]*XMLMessage),
		Lookup:   make(map[string]*XMLMessage),
	}
	for i := range xmlProto.Messages {
		msg := &xmlProto.Messages[i]
		p.Messages[msg.ID] = msg
		p.Lookup[msg.Abbrev] = msg
	}
	return p
}

func (p *Protocol) CreateMessage(abbrev string) (*Message, error) {
	msgDef, ok := p.Lookup[abbrev]
	if !ok {
		return nil, fmt.Errorf("message %s not found in protocol", abbrev)
	}

	return &Message{
		Header: Header{
			Sync: 0xFE55,
			MGID: msgDef.ID,
		},
		Fields: make(map[string]any),
	}, nil
}

// Serialization and Deserialization implementation will go here.
// I'll need a way to map IMC types (uint8_t, fp64_t, plaintext, etc.) to binary.

func (m *Message) Serialize(p *Protocol) ([]byte, error) {
	payload, err := m.SerializePayload(p)
	if err != nil {
		return nil, err
	}

	buf := new(bytes.Buffer)
	m.Header.Size = uint16(len(payload))
	if m.Header.Timestamp == 0 {
		m.Header.Timestamp = float64(time.Now().UnixNano()) / 1e9
	}

	// Write Header
	binary.Write(buf, binary.LittleEndian, m.Header.Sync)
	binary.Write(buf, binary.LittleEndian, m.Header.MGID)
	binary.Write(buf, binary.LittleEndian, m.Header.Size)
	binary.Write(buf, binary.LittleEndian, m.Header.Timestamp)
	binary.Write(buf, binary.LittleEndian, m.Header.Src)
	binary.Write(buf, binary.LittleEndian, m.Header.SrcEnt)
	binary.Write(buf, binary.LittleEndian, m.Header.Dst)
	binary.Write(buf, binary.LittleEndian, m.Header.DstEnt)

	// Write Payload
	buf.Write(payload)

	// Write Footer (CRC)
	crc := CalculateCRC16(buf.Bytes())
	binary.Write(buf, binary.LittleEndian, crc)

	return buf.Bytes(), nil
}

func (m *Message) SerializePayload(p *Protocol) ([]byte, error) {
	msgDef, ok := p.Messages[m.Header.MGID]
	if !ok {
		return nil, fmt.Errorf("message ID %d not found in protocol", m.Header.MGID)
	}

	buf := new(bytes.Buffer)
	for _, field := range msgDef.Fields {
		val := m.Fields[field.Abbrev]
		if err := p.writeField(buf, field.Type, val); err != nil {
			return nil, fmt.Errorf("failed to write field %s: %w", field.Abbrev, err)
		}
	}
	return buf.Bytes(), nil
}

func (p *Protocol) writeField(buf *bytes.Buffer, typ string, val any) error {
	switch typ {
	case "int8_t":
		return binary.Write(buf, binary.LittleEndian, ToInt8(val))
	case "uint8_t":
		return binary.Write(buf, binary.LittleEndian, ToUint8(val))
	case "int16_t":
		return binary.Write(buf, binary.LittleEndian, ToInt16(val))
	case "uint16_t":
		return binary.Write(buf, binary.LittleEndian, ToUint16(val))
	case "int32_t":
		return binary.Write(buf, binary.LittleEndian, ToInt32(val))
	case "uint32_t":
		return binary.Write(buf, binary.LittleEndian, ToUint32(val))
	case "int64_t":
		return binary.Write(buf, binary.LittleEndian, ToInt64(val))
	case "fp32_t":
		return binary.Write(buf, binary.LittleEndian, ToFloat32(val))
	case "fp64_t":
		return binary.Write(buf, binary.LittleEndian, ToFloat64(val))
	case "plaintext":
		s := ToString(val)
		binary.Write(buf, binary.LittleEndian, uint16(len(s)))
		buf.WriteString(s)
		return nil
	case "rawdata":
		d := ToByteSlice(val)
		binary.Write(buf, binary.LittleEndian, uint16(len(d)))
		buf.Write(d)
		return nil
	case "message":
		msg := ToGenericMessage(val)
		if msg == nil {
			return binary.Write(buf, binary.LittleEndian, uint16(0xFFFF))
		}
		if err := binary.Write(buf, binary.LittleEndian, msg.Header.MGID); err != nil {
			return err
		}
		data, err := msg.SerializePayload(p)
		if err != nil {
			return err
		}
		_, err = buf.Write(data)
		return err
	case "message-list":
		msgs := ToGenericMessageList(val)
		if err := binary.Write(buf, binary.LittleEndian, uint16(len(msgs))); err != nil {
			return err
		}
		for _, m := range msgs {
			if m == nil {
				binary.Write(buf, binary.LittleEndian, uint16(0xFFFF))
				continue
			}
			binary.Write(buf, binary.LittleEndian, m.Header.MGID)
			data, err := m.SerializePayload(p)
			if err != nil {
				return err
			}
			buf.Write(data)
		}
		return nil
	default:
		return fmt.Errorf("unknown type: %s", typ)
	}
}

// Helpers to handle various input types for values
func ToUint8(v any) uint8 {
	if x, ok := v.(uint8); ok {
		return x
	}
	return uint8(ToInt64(v))
}

func ToInt8(v any) int8 {
	if x, ok := v.(int8); ok {
		return x
	}
	return int8(ToInt64(v))
}

func ToUint16(v any) uint16 {
	if x, ok := v.(uint16); ok {
		return x
	}
	return uint16(ToInt64(v))
}

func ToInt16(v any) int16 {
	if x, ok := v.(int16); ok {
		return x
	}
	return int16(ToInt64(v))
}

func ToUint32(v any) uint32 {
	if x, ok := v.(uint32); ok {
		return x
	}
	return uint32(ToInt64(v))
}

func ToInt32(v any) int32 {
	if x, ok := v.(int32); ok {
		return x
	}
	return int32(ToInt64(v))
}

func ToInt64(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	case float64:
		return int64(x)
	case uint16:
		return int64(x)
	case uint8:
		return int64(x)
	case uint32:
		return int64(x)
	case int32:
		return int64(x)
	}
	return 0
}

func ToFloat32(v any) float32 {
	if x, ok := v.(float32); ok {
		return x
	}
	return float32(ToFloat64(v))
}

func ToFloat64(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	}
	return 0
}

func ToString(v any) string {
	if x, ok := v.(string); ok {
		return x
	}
	return ""
}

func ToByteSlice(v any) []byte {
	if x, ok := v.([]byte); ok {
		return x
	}
	return nil
}

func ToGenericMessage(v any) *Message {
	if v == nil {
		return nil
	}
	if m, ok := v.(*Message); ok {
		return m
	}
	if im, ok := v.(IMessage); ok {
		return im.ToMessage()
	}
	return nil
}

func ToGenericMessageList(v any) []*Message {
	if v == nil {
		return nil
	}
	if ms, ok := v.([]*Message); ok {
		return ms
	}
	// Use reflection or type assertion if we know the types.
	// Since we use slice of pointer to structs, we can't easily assert to []IMessage.
	// But the generator can be made to emit []*Message for simplicity.
	return nil
}

func CalculateCRC16(data []byte) uint16 {
	var crc uint16 = 0xFFFF
	// Simplistic CRC16 calculation - needs to be the specific one used by IMC (usually CCITT or IBM)
	// IMC typically uses CRC-16-IBM (poly 0x8005)
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if (crc & 0x0001) != 0 {
				crc = (crc >> 1) ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}
