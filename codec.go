package imc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Wire constants (identical to DUNE's IMC::Packet).
const (
	// HeaderSize is the size of the frame header: sync, mgid, size,
	// timestamp, src, src_ent, dst, dst_ent.
	HeaderSize = 20
	// FooterSize is the size of the CRC16 that ends a frame.
	FooterSize = 2
	// MaxFrameSize is the size of the largest frame that can be written
	// (DUNE's Packet::serialize takes a uint16_t).
	MaxFrameSize = 0xFFFF
	// MaxPayloadSize is the largest payload of a frame that can be written.
	MaxPayloadSize = MaxFrameSize - HeaderSize - FooterSize
	// MaxInFrameSize is the size of the largest frame a reader can meet
	// (a header announcing 65535 bytes of payload).
	MaxInFrameSize = HeaderSize + 0xFFFF + FooterSize
	// NullID is the message id of an empty inline message or list element.
	NullID uint16 = 0xFFFF
	// UnknownEntity is entity id 255 (unknown, or every entity as a destination).
	UnknownEntity uint8 = 0xFF
	// Broadcast is system id 0xFFFF (any system).
	Broadcast uint16 = 0xFFFF
)

// Errors of the codecs and of FrameReader.
var (
	ErrSync     = errors.New("imc: bad synchronization number")
	ErrCRC      = errors.New("imc: CRC mismatch")
	ErrShort    = errors.New("imc: buffer too short")
	ErrTooLarge = errors.New("imc: frame too large")
	ErrLength   = errors.New("imc: frame length does not match its header")
	ErrNeedMore = errors.New("imc: incomplete frame, more data needed")
)

// UnknownMessageError is returned when a frame or an inline message has a
// message id the codec does not know.
type UnknownMessageError struct{ MGID uint16 }

func (e *UnknownMessageError) Error() string {
	return fmt.Sprintf("imc: unknown message id %d", e.MGID)
}

// FieldError wraps an error with the field (Message.field) it happened in.
type FieldError struct {
	Field string
	Err   error
}

func (e *FieldError) Error() string { return "imc: " + e.Field + ": " + e.Err.Error() }
func (e *FieldError) Unwrap() error { return e.Err }

func errShort(field string) error { return &FieldError{field, ErrShort} }

func errTooLong(field string, n int) error {
	return &FieldError{field, fmt.Errorf("%d items/bytes is more than 65535: %w", n, ErrTooLarge)}
}

func wrapField(field string, err error) error {
	var fe *FieldError
	if errors.As(err, &fe) {
		return &FieldError{field + "/" + fe.Field, fe.Err}
	}
	return &FieldError{field, err}
}

// IMessage is implemented by every typed (generated) IMC message. The
// methods with a be argument encode or decode big endian when it is true
// (DUNE writes little endian; big endian is what a byte swapped peer sends).
type IMessage interface {
	// GetHeader returns the header of the message (time stamp, source and
	// destination); its Sync, MGID and Size are set when encoding.
	GetHeader() *Header
	// GetMGID returns the message id.
	GetMGID() uint16
	// Abbrev returns the message abbreviation (e.g. "EstimatedState").
	Abbrev() string
	// AppendPayload appends the serialized fields of the message to b.
	AppendPayload(b []byte, be bool) ([]byte, error)
	// DecodePayload decodes the fields from b and returns the number of
	// bytes used; bytes after the fields are ignored, as DUNE does.
	DecodePayload(b []byte, be bool) (int, error)
	// ToMessage converts the message to the dynamic representation.
	ToMessage() *Message
	// FromMessage sets the message from the dynamic representation.
	FromMessage(m *Message) error

	appendInline(b []byte, be bool) ([]byte, error)
}

// ToTyped converts a dynamic message into a typed one.
func (p *Protocol) ToTyped(m *Message) (IMessage, error) {
	t := NewMessage(m.Header.MGID)
	if t == nil {
		return nil, fmt.Errorf("unknown message ID: %d", m.Header.MGID)
	}
	return t, t.FromMessage(m)
}

// -- byte order helpers (inlined by the compiler) -----------------------------

func appendU16(b []byte, v uint16, be bool) []byte {
	if be {
		return binary.BigEndian.AppendUint16(b, v)
	}
	return binary.LittleEndian.AppendUint16(b, v)
}

func appendU32(b []byte, v uint32, be bool) []byte {
	if be {
		return binary.BigEndian.AppendUint32(b, v)
	}
	return binary.LittleEndian.AppendUint32(b, v)
}

func appendU64(b []byte, v uint64, be bool) []byte {
	if be {
		return binary.BigEndian.AppendUint64(b, v)
	}
	return binary.LittleEndian.AppendUint64(b, v)
}

func readU16(b []byte, be bool) uint16 {
	if be {
		return binary.BigEndian.Uint16(b)
	}
	return binary.LittleEndian.Uint16(b)
}

func readU32(b []byte, be bool) uint32 {
	if be {
		return binary.BigEndian.Uint32(b)
	}
	return binary.LittleEndian.Uint32(b)
}

func readU64(b []byte, be bool) uint64 {
	if be {
		return binary.BigEndian.Uint64(b)
	}
	return binary.LittleEndian.Uint64(b)
}

func cloneBytes(b []byte) []byte {
	if len(b) == 0 {
		return []byte{}
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// -- inline messages ------------------------------------------------------------

func appendInline(b []byte, m IMessage, be bool) ([]byte, error) {
	if m == nil {
		return appendU16(b, NullID, be), nil
	}
	return m.appendInline(b, be)
}

func decodeInline(b []byte, be bool) (IMessage, int, error) {
	if len(b) < 2 {
		return nil, 0, ErrShort
	}
	id := readU16(b, be)
	if id == NullID {
		return nil, 2, nil
	}
	m := NewMessage(id)
	if m == nil {
		return nil, 2, &UnknownMessageError{id}
	}
	n, err := m.DecodePayload(b[2:], be)
	return m, 2 + n, err
}

// decodeInlineAs decodes an inline message of a field declared with a
// concrete message type (any other id is an error).
func decodeInlineAs[T any, PT interface {
	*T
	IMessage
}](b []byte, be bool, want uint16) (PT, int, error) {
	if len(b) < 2 {
		return nil, 0, ErrShort
	}
	id := readU16(b, be)
	if id == NullID {
		return nil, 2, nil
	}
	if id != want {
		if NewMessage(id) == nil {
			return nil, 2, &UnknownMessageError{id}
		}
		return nil, 2, fmt.Errorf("message id %d where %d was expected", id, want)
	}
	m := PT(new(T))
	n, err := m.DecodePayload(b[2:], be)
	return m, 2 + n, err
}

func toGeneric[PT interface {
	IMessage
	comparable
}](m PT) *Message {
	var zero PT
	if m == zero {
		return nil
	}
	return m.ToMessage()
}

func fromGeneric(m *Message) (IMessage, error) {
	if m == nil {
		return nil, nil
	}
	t := NewMessage(m.Header.MGID)
	if t == nil {
		return nil, &UnknownMessageError{m.Header.MGID}
	}
	return t, t.FromMessage(m)
}

// -- frames -----------------------------------------------------------------------

var zeroHeader [HeaderSize]byte

// Marshal serializes a typed message into a little endian frame (as DUNE
// writes them). The header's Sync, MGID and Size are set from the message;
// the time stamp, source and destination are taken as they are.
func Marshal(m IMessage) ([]byte, error) {
	return AppendFrame(make([]byte, 0, 256), m, false)
}

// AppendFrame appends the frame of a typed message to dst (big endian when
// be is true, to reproduce byte swapped peers). On error dst is returned
// unchanged. Frames over MaxFrameSize bytes are refused (ErrTooLarge).
func AppendFrame(dst []byte, m IMessage, be bool) ([]byte, error) {
	start := len(dst)
	b := append(dst, zeroHeader[:]...)
	b, err := m.AppendPayload(b, be)
	if err != nil {
		return dst[:start], err
	}
	size := len(b) - start - HeaderSize
	if size > MaxPayloadSize {
		return dst[:start], fmt.Errorf("%s: payload of %d bytes: %w", m.Abbrev(), size, ErrTooLarge)
	}
	h := m.GetHeader()
	h.Sync = SyncNumber
	h.MGID = m.GetMGID()
	h.Size = uint16(size)
	putHeader(b[start:], h, be)
	crc := CRC16(b[start:])
	return appendU16(b, crc, be), nil
}

func putHeader(b []byte, h *Header, be bool) {
	o := binary.ByteOrder(binary.LittleEndian)
	if be {
		o = binary.BigEndian
	}
	o.PutUint16(b[0:], h.Sync)
	o.PutUint16(b[2:], h.MGID)
	o.PutUint16(b[4:], h.Size)
	o.PutUint64(b[6:], math.Float64bits(h.Timestamp))
	o.PutUint16(b[14:], h.Src)
	b[16] = h.SrcEnt
	o.PutUint16(b[17:], h.Dst)
	b[19] = h.DstEnt
}

// syncOrder tells the byte order of a frame from its first two bytes:
// little endian (the sync number's low byte first), big endian (the sync
// number byte swapped), or neither.
func syncOrder(b0, b1 byte, sync uint16) (be bool, ok bool) {
	lo, hi := byte(sync), byte(sync>>8)
	if b0 == lo && b1 == hi {
		return false, true
	}
	if b0 == hi && b1 == lo {
		return true, true
	}
	return false, false
}

// ParseHeader decodes the header at the start of b, in the byte order its
// sync number tells. Only SyncNumber, in either order, is accepted.
func ParseHeader(b []byte) (h Header, be bool, err error) {
	return parseHeader(b, SyncNumber)
}

func parseHeader(b []byte, sync uint16) (h Header, be bool, err error) {
	if len(b) < HeaderSize {
		return h, false, ErrShort
	}
	be, ok := syncOrder(b[0], b[1], sync)
	if !ok {
		return h, false, fmt.Errorf("%w: %02X%02X", ErrSync, b[0], b[1])
	}
	h = decodeHeader(b, be)
	h.Sync = sync
	return h, be, nil
}

func decodeHeader(b []byte, be bool) Header {
	return Header{
		Sync:      readU16(b[0:], be),
		MGID:      readU16(b[2:], be),
		Size:      readU16(b[4:], be),
		Timestamp: math.Float64frombits(readU64(b[6:], be)),
		Src:       readU16(b[14:], be),
		SrcEnt:    b[16],
		Dst:       readU16(b[17:], be),
		DstEnt:    b[19],
	}
}

// Decode decodes exactly one frame into a typed message, strictly: the
// sync number (either byte order; big endian frames are decoded as big
// endian), the length and the CRC must be right, and the id known. Payload
// bytes after the fields are ignored, as DUNE does (a newer peer may have
// appended fields).
func Decode(frame []byte) (IMessage, error) {
	h, be, err := ParseHeader(frame)
	if err != nil {
		return nil, err
	}
	total := HeaderSize + int(h.Size) + FooterSize
	if len(frame) != total {
		return nil, fmt.Errorf("%w: %d bytes, header says %d", ErrLength, len(frame), total)
	}
	n := HeaderSize + int(h.Size)
	if CRC16(frame[:n]) != readU16(frame[n:], be) {
		return nil, ErrCRC
	}
	f := Frame{Header: h, BigEndian: be, Payload: frame[HeaderSize:n], Raw: frame}
	return f.Decode()
}

// Frame is a frame with a valid CRC, as FrameReader returns it. Payload and
// Raw point into the reader's buffer: they are valid until the next call
// to the reader.
type Frame struct {
	Header    Header
	BigEndian bool
	// Payload holds the serialized fields.
	Payload []byte
	// Raw is the whole frame (header, payload and CRC).
	Raw []byte
}

// Decode decodes the frame into a new typed message (its header is the
// frame's). Unknown ids return *UnknownMessageError.
func (f *Frame) Decode() (IMessage, error) {
	m := NewMessage(f.Header.MGID)
	if m == nil {
		return nil, &UnknownMessageError{f.Header.MGID}
	}
	if err := f.DecodeInto(m); err != nil {
		return nil, err
	}
	return m, nil
}

// DecodeInto decodes the frame into m, which must be of the frame's type.
func (f *Frame) DecodeInto(m IMessage) error {
	if m.GetMGID() != f.Header.MGID {
		return fmt.Errorf("imc: frame holds message %d, not %s", f.Header.MGID, m.Abbrev())
	}
	if _, err := m.DecodePayload(f.Payload, f.BigEndian); err != nil {
		return err
	}
	*m.GetHeader() = f.Header
	return nil
}
