package imc

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// Message is a generic IMC message holding fields in a map (the dynamic
// codec, driven by an IMC XML at run time: what log tools use, since every
// log carries its own IMC.xml).
type Message struct {
	Header Header
	Fields map[string]any
}

// Header is the header of an IMC frame.
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

// Protocol is the dynamic codec of one IMC XML specification.
//
// It is strict by default:
//
//   - decoding accepts only the specification's sync number, in either
//     byte order (a byte swapped sync number means a big endian frame,
//     decoded as big endian), and refuses frames whose CRC is wrong or
//     whose length differs from what the header says;
//   - encoding refuses a missing field (CreateMessage fills every field
//     with its zero value, so only fields deleted or never set in a map
//     built by hand are missing), a value of the wrong kind (e.g. a string
//     for a number), an integer that does not fit its field, and frames,
//     texts, rawdata and lists over 65535 bytes or items.
//
// Lenient (for log forensics) keeps the old behaviour: any sync number with
// a 0xFE byte is accepted (decoded as little endian unless it is exactly
// the byte swapped sync number), CRC mismatches are ignored, missing and
// mistyped fields are written as zero and integers are truncated. Sizes
// are checked in both modes.
type Protocol struct {
	Version  string
	SyncWord uint16
	Messages map[uint16]*XMLMessage
	Lookup   map[string]*XMLMessage
	// MD5 of the XML the protocol was parsed from (empty if unknown).
	MD5 string
	// Lenient disables the strict checks (see Protocol).
	Lenient bool
}

func NewProtocol(xmlProto *XMLProtocol) *Protocol {
	p := &Protocol{
		Version:  xmlProto.Version,
		SyncWord: 0xFE55, // Default
		Messages: make(map[uint16]*XMLMessage),
		Lookup:   make(map[string]*XMLMessage),
		MD5:      xmlProto.MD5,
	}

	for _, field := range xmlProto.Header {
		if field.Abbrev == "sync" {
			var val uint64
			if _, err := fmt.Sscanf(field.Value, "0x%x", &val); err == nil {
				p.SyncWord = uint16(val)
			} else {
				fmt.Sscanf(field.Value, "%d", &val)
				p.SyncWord = uint16(val)
			}
			break
		}
	}

	for i := range xmlProto.Messages {
		msg := &xmlProto.Messages[i]
		p.Messages[msg.ID] = msg
		p.Lookup[msg.Abbrev] = msg
	}
	return p
}

// CreateMessage returns a message of the given type with every field set
// to its zero value (0 of the field's Go type, "", empty rawdata, nil for an inline
// message, an empty list).
func (p *Protocol) CreateMessage(abbrev string) (*Message, error) {
	msgDef, ok := p.Lookup[abbrev]
	if !ok {
		return nil, fmt.Errorf("message %s not found in protocol", abbrev)
	}
	m := &Message{
		Header: Header{
			Sync: p.SyncWord,
			MGID: msgDef.ID,
		},
		Fields: make(map[string]any, len(msgDef.Fields)),
	}
	for _, f := range msgDef.Fields {
		m.Fields[f.Abbrev] = zeroValue(f.Type)
	}
	return m, nil
}

func zeroValue(typ string) any {
	switch typ {
	case "int8_t":
		return int8(0)
	case "uint8_t":
		return uint8(0)
	case "int16_t":
		return int16(0)
	case "uint16_t":
		return uint16(0)
	case "int32_t":
		return int32(0)
	case "uint32_t":
		return uint32(0)
	case "int64_t":
		return int64(0)
	case "fp32_t":
		return float32(0)
	case "fp64_t":
		return float64(0)
	case "plaintext":
		return ""
	case "rawdata":
		return []byte{}
	case "message-list":
		return []*Message{}
	}
	return nil
}

func (p *Protocol) GetField(mgid uint16, abbrev string) *XMLField {
	msgDef, ok := p.Messages[mgid]
	if !ok {
		return nil
	}
	for i := range msgDef.Fields {
		if msgDef.Fields[i].Abbrev == abbrev {
			return &msgDef.Fields[i]
		}
	}
	return nil
}

// Serialize writes the message as a little endian frame. It sets the
// header's Sync and Size, and the time stamp to now when it is 0.
func (m *Message) Serialize(p *Protocol) ([]byte, error) {
	if m.Header.Timestamp == 0 {
		m.Header.Timestamp = float64(time.Now().UnixNano()) / 1e9
	}
	return p.Encode(m, false)
}

// Encode writes the message as a frame (big endian if be), without
// touching the time stamp. It sets the header's Sync and Size.
func (p *Protocol) Encode(m *Message, be bool) ([]byte, error) {
	b := make([]byte, HeaderSize, 256)
	b, err := p.appendFields(b, m, be)
	if err != nil {
		return nil, err
	}
	size := len(b) - HeaderSize
	if size > MaxPayloadSize {
		return nil, fmt.Errorf("message %d: payload of %d bytes: %w", m.Header.MGID, size, ErrTooLarge)
	}
	m.Header.Size = uint16(size)
	m.Header.Sync = p.SyncWord
	putHeader(b, &m.Header, be)
	return appendU16(b, CRC16(b), be), nil
}

// SerializePayload writes the fields of the message (little endian).
func (m *Message) SerializePayload(p *Protocol) ([]byte, error) {
	return p.appendFields(nil, m, false)
}

func (p *Protocol) appendFields(b []byte, m *Message, be bool) ([]byte, error) {
	msgDef, ok := p.Messages[m.Header.MGID]
	if !ok {
		return nil, fmt.Errorf("message ID %d not found in protocol", m.Header.MGID)
	}
	for _, field := range msgDef.Fields {
		val, present := m.Fields[field.Abbrev]
		if !present && !p.Lenient {
			return nil, fmt.Errorf("%s.%s: missing field (strict mode)", msgDef.Abbrev, field.Abbrev)
		}
		var err error
		if b, err = p.appendField(b, field.Type, val, be); err != nil {
			return nil, fmt.Errorf("failed to write field %s.%s: %w", msgDef.Abbrev, field.Abbrev, err)
		}
	}
	return b, nil
}

var errKind = errors.New("value of the wrong kind")

func (p *Protocol) intValue(val any, lo, hi int64) (int64, error) {
	if p.Lenient {
		return ToInt64(val), nil
	}
	var v int64
	switch x := val.(type) {
	case int:
		v = int64(x)
	case int8:
		v = int64(x)
	case int16:
		v = int64(x)
	case int32:
		v = int64(x)
	case int64:
		v = x
	case uint:
		if uint64(x) > math.MaxInt64 {
			return 0, fmt.Errorf("%d is out of range", x)
		}
		v = int64(x)
	case uint8:
		v = int64(x)
	case uint16:
		v = int64(x)
	case uint32:
		v = int64(x)
	case uint64:
		if x > math.MaxInt64 {
			return 0, fmt.Errorf("%d is out of range", x)
		}
		v = int64(x)
	case float64:
		if x != math.Trunc(x) || math.IsInf(x, 0) || math.IsNaN(x) || x < -9.3e18 || x > 9.3e18 {
			return 0, fmt.Errorf("%v is not an integer", x)
		}
		v = int64(x)
	case float32:
		f := float64(x)
		if f != math.Trunc(f) || math.IsInf(f, 0) || math.IsNaN(f) {
			return 0, fmt.Errorf("%v is not an integer", x)
		}
		v = int64(f)
	default:
		return 0, fmt.Errorf("%w: %T for an integer", errKind, val)
	}
	if v < lo || v > hi {
		return 0, fmt.Errorf("%d is out of range [%d, %d]", v, lo, hi)
	}
	return v, nil
}

func (p *Protocol) floatValue(val any) (float64, error) {
	if p.Lenient {
		return ToFloat64(val), nil
	}
	switch val.(type) {
	case float64, float32, int, int8, int16, int32, int64, uint8, uint16, uint32:
		return ToFloat64(val), nil
	case uint:
		return float64(val.(uint)), nil
	case uint64:
		return float64(val.(uint64)), nil
	}
	return 0, fmt.Errorf("%w: %T for a number", errKind, val)
}

func (p *Protocol) appendField(b []byte, typ string, val any, be bool) ([]byte, error) {
	switch typ {
	case "int8_t":
		v, err := p.intValue(val, math.MinInt8, math.MaxInt8)
		return append(b, byte(int8(v))), err
	case "uint8_t":
		v, err := p.intValue(val, 0, math.MaxUint8)
		return append(b, uint8(v)), err
	case "int16_t":
		v, err := p.intValue(val, math.MinInt16, math.MaxInt16)
		return appendU16(b, uint16(int16(v)), be), err
	case "uint16_t":
		v, err := p.intValue(val, 0, math.MaxUint16)
		return appendU16(b, uint16(v), be), err
	case "int32_t":
		v, err := p.intValue(val, math.MinInt32, math.MaxInt32)
		return appendU32(b, uint32(int32(v)), be), err
	case "uint32_t":
		v, err := p.intValue(val, 0, math.MaxUint32)
		return appendU32(b, uint32(v), be), err
	case "int64_t":
		v, err := p.intValue(val, math.MinInt64, math.MaxInt64)
		return appendU64(b, uint64(v), be), err
	case "fp32_t":
		if x, ok := val.(float32); ok {
			return appendU32(b, math.Float32bits(x), be), nil
		}
		v, err := p.floatValue(val)
		return appendU32(b, math.Float32bits(float32(v)), be), err
	case "fp64_t":
		v, err := p.floatValue(val)
		return appendU64(b, math.Float64bits(v), be), err
	case "plaintext", "rawdata":
		var d []byte
		switch x := val.(type) {
		case string:
			d = []byte(x)
		case []byte:
			d = x
		case nil:
			if !p.Lenient {
				return b, fmt.Errorf("%w: nil for %s", errKind, typ)
			}
		default:
			if !p.Lenient {
				return b, fmt.Errorf("%w: %T for %s", errKind, val, typ)
			}
		}
		if len(d) > 0xFFFF {
			return b, fmt.Errorf("%d bytes: %w", len(d), ErrTooLarge)
		}
		b = appendU16(b, uint16(len(d)), be)
		return append(b, d...), nil
	case "message":
		msg := ToGenericMessage(val)
		if msg == nil {
			if val != nil && !p.Lenient && !isNilMessage(val) {
				return b, fmt.Errorf("%w: %T for an inline message", errKind, val)
			}
			return appendU16(b, NullID, be), nil
		}
		b = appendU16(b, msg.Header.MGID, be)
		return p.appendFields(b, msg, be)
	case "message-list":
		msgs, ok := toMessageList(val)
		if !ok && !p.Lenient {
			return b, fmt.Errorf("%w: %T for a message list", errKind, val)
		}
		if len(msgs) > 0xFFFF {
			return b, fmt.Errorf("%d messages: %w", len(msgs), ErrTooLarge)
		}
		b = appendU16(b, uint16(len(msgs)), be)
		for _, m := range msgs {
			if m == nil {
				b = appendU16(b, NullID, be)
				continue
			}
			b = appendU16(b, m.Header.MGID, be)
			var err error
			if b, err = p.appendFields(b, m, be); err != nil {
				return b, err
			}
		}
		return b, nil
	default:
		return b, fmt.Errorf("unknown type: %s", typ)
	}
}

func isNilMessage(v any) bool {
	switch x := v.(type) {
	case *Message:
		return x == nil
	case IMessage:
		return x == nil || toGeneric(x) == nil
	}
	return false
}

func toMessageList(v any) ([]*Message, bool) {
	switch x := v.(type) {
	case nil:
		return nil, true
	case []*Message:
		return x, true
	case []IMessage:
		out := make([]*Message, len(x))
		for i, m := range x {
			if m != nil {
				out[i] = m.ToMessage()
			}
		}
		return out, true
	case []any:
		out := make([]*Message, len(x))
		for i, m := range x {
			if m == nil {
				continue
			}
			g := ToGenericMessage(m)
			if g == nil {
				return nil, false
			}
			out[i] = g
		}
		return out, true
	}
	return nil, false
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
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case int64:
		return x
	case uint:
		return int64(x)
	case uint8:
		return int64(x)
	case uint16:
		return int64(x)
	case uint32:
		return int64(x)
	case uint64:
		return int64(x)
	case float32:
		return int64(x)
	case float64:
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
	case int8:
		return float64(x)
	case int16:
		return float64(x)
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	case uint8:
		return float64(x)
	case uint16:
		return float64(x)
	case uint32:
		return float64(x)
	}
	return 0
}

func ToString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	}
	return ""
}

func ToByteSlice(v any) []byte {
	switch x := v.(type) {
	case []byte:
		return x
	case string:
		return []byte(x)
	}
	return nil
}

// ToGenericMessage returns v as a *Message (converting typed messages), or
// nil.
func ToGenericMessage(v any) *Message {
	switch x := v.(type) {
	case nil:
		return nil
	case *Message:
		return x
	case IMessage:
		return toGeneric(x)
	}
	return nil
}

// ToGenericMessageList returns v as a []*Message (converting []IMessage),
// or nil.
func ToGenericMessageList(v any) []*Message {
	out, _ := toMessageList(v)
	return out
}
