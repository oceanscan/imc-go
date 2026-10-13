package imc

import (
	"fmt"
	"io"
	"math"
)

// Unmarshal decodes exactly one frame (e.g. a UDP datagram). In strict mode
// (the default) the sync number, the frame length and the CRC are checked
// (see Protocol); big endian frames are decoded as big endian.
func (p *Protocol) Unmarshal(data []byte) (*Message, error) {
	h, be, err := p.parseHeader(data)
	if err != nil {
		return nil, err
	}
	n := HeaderSize + int(h.Size)
	if len(data) < n+FooterSize || (!p.Lenient && len(data) != n+FooterSize) {
		return nil, fmt.Errorf("%w: %d bytes, header says %d", ErrLength, len(data), n+FooterSize)
	}
	if !p.Lenient && CRC16(data[:n]) != readU16(data[n:], be) {
		return nil, ErrCRC
	}
	return p.decodeFrame(h, be, data[HeaderSize:n])
}

// DecodeFrame decodes a frame returned by a FrameReader.
func (p *Protocol) DecodeFrame(f Frame) (*Message, error) {
	return p.decodeFrame(f.Header, f.BigEndian, f.Payload)
}

func (p *Protocol) decodeFrame(h Header, be bool, payload []byte) (*Message, error) {
	m, _, err := p.decodeFields(payload, h.MGID, be)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal fields: %w", err)
	}
	m.Header = h
	return m, nil
}

// UnmarshalReader reads one frame from r (header, payload and CRC) and
// decodes it, with the checks of Unmarshal. It does not resynchronize after
// garbage: use a FrameReader for streams that may be corrupted.
func (p *Protocol) UnmarshalReader(r io.Reader) (*Message, error) {
	var hb [HeaderSize]byte
	if _, err := io.ReadFull(r, hb[:]); err != nil {
		return nil, err
	}
	h, be, err := p.parseHeader(hb[:])
	if err != nil {
		return nil, err
	}
	rest := make([]byte, int(h.Size)+FooterSize)
	if _, err := io.ReadFull(r, rest); err != nil {
		return nil, fmt.Errorf("failed to read payload: %w", err)
	}
	payload := rest[:h.Size]
	if !p.Lenient {
		crc := UpdateCRC16(CRC16(hb[:]), payload)
		if crc != readU16(rest[h.Size:], be) {
			return nil, ErrCRC
		}
	}
	return p.decodeFrame(h, be, payload)
}

// parseHeader checks the sync number (see Protocol) and decodes a header.
func (p *Protocol) parseHeader(b []byte) (Header, bool, error) {
	if len(b) < HeaderSize {
		return Header{}, false, ErrShort
	}
	be, ok := syncOrder(b[0], b[1], p.SyncWord)
	if !ok {
		if !p.Lenient || (b[0] != 0xFE && b[1] != 0xFE) {
			return Header{}, false, fmt.Errorf("%w: 0x%02X%02X (expected 0x%04X)", ErrSync, b[1], b[0], p.SyncWord)
		}
		// Lenient: another IMC sync number (e.g. 0xFE54): little endian.
		h := decodeHeader(b, false)
		return h, false, nil
	}
	h := decodeHeader(b, be)
	h.Sync = p.SyncWord
	return h, be, nil
}

// UnmarshalHeader reads and decodes a header from r.
func (p *Protocol) UnmarshalHeader(r io.Reader) (*Header, error) {
	var hb [HeaderSize]byte
	if _, err := io.ReadFull(r, hb[:]); err != nil {
		return nil, err
	}
	h, _, err := p.parseHeader(hb[:])
	if err != nil {
		return nil, err
	}
	return &h, nil
}

// UnmarshalFields decodes the (little endian) fields of a message of the
// given id from the rest of r.
func (p *Protocol) UnmarshalFields(r io.Reader, mgid uint16) (*Message, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	m, _, err := p.decodeFields(data, mgid, false)
	return m, err
}

// decodeFields decodes the fields of message mgid from b. @return the
// message and the number of bytes used.
func (p *Protocol) decodeFields(b []byte, mgid uint16, be bool) (*Message, int, error) {
	msgDef, ok := p.Messages[mgid]
	if !ok {
		return nil, 0, &UnknownMessageError{mgid}
	}
	m := &Message{
		Header: Header{MGID: mgid},
		Fields: make(map[string]any, len(msgDef.Fields)),
	}
	off := 0
	for _, field := range msgDef.Fields {
		val, n, err := p.readField(b[off:], field.Type, be)
		if err != nil {
			return nil, off, fmt.Errorf("failed to read field %s.%s: %w", msgDef.Abbrev, field.Abbrev, err)
		}
		off += n
		m.Fields[field.Abbrev] = val
	}
	return m, off, nil
}

var primSize = map[string]int{
	"int8_t": 1, "uint8_t": 1, "int16_t": 2, "uint16_t": 2, "int32_t": 4, "uint32_t": 4,
	"int64_t": 8, "fp32_t": 4, "fp64_t": 8,
}

func (p *Protocol) readField(b []byte, typ string, be bool) (any, int, error) {
	if n, ok := primSize[typ]; ok && len(b) < n {
		return nil, 0, ErrShort
	}
	switch typ {
	case "int8_t":
		return int8(b[0]), 1, nil
	case "uint8_t":
		return b[0], 1, nil
	case "int16_t":
		return int16(readU16(b, be)), 2, nil
	case "uint16_t":
		return readU16(b, be), 2, nil
	case "int32_t":
		return int32(readU32(b, be)), 4, nil
	case "uint32_t":
		return readU32(b, be), 4, nil
	case "int64_t":
		return int64(readU64(b, be)), 8, nil
	case "fp32_t":
		return math.Float32frombits(readU32(b, be)), 4, nil
	case "fp64_t":
		return math.Float64frombits(readU64(b, be)), 8, nil
	case "plaintext", "rawdata":
		if len(b) < 2 {
			return nil, 0, ErrShort
		}
		n := int(readU16(b, be))
		if len(b)-2 < n {
			return nil, 2, ErrShort
		}
		if typ == "plaintext" {
			return string(b[2 : 2+n]), 2 + n, nil
		}
		return cloneBytes(b[2 : 2+n]), 2 + n, nil
	case "message":
		if len(b) < 2 {
			return nil, 0, ErrShort
		}
		id := readU16(b, be)
		if id == NullID {
			return nil, 2, nil
		}
		m, n, err := p.decodeFields(b[2:], id, be)
		return m, 2 + n, err
	case "message-list":
		if len(b) < 2 {
			return nil, 0, ErrShort
		}
		count := int(readU16(b, be))
		off := 2
		res := make([]*Message, 0, min(count, len(b)/2))
		for i := 0; i < count; i++ {
			if len(b)-off < 2 {
				return nil, off, ErrShort
			}
			id := readU16(b[off:], be)
			off += 2
			if id == NullID {
				res = append(res, nil)
				continue
			}
			m, n, err := p.decodeFields(b[off:], id, be)
			off += n
			if err != nil {
				return nil, off, err
			}
			res = append(res, m)
		}
		return res, off, nil
	default:
		return nil, 0, fmt.Errorf("unknown type: %s", typ)
	}
}
