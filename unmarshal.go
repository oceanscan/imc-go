package imc

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

func (p *Protocol) Unmarshal(data []byte) (*Message, error) {
	buf := bytes.NewReader(data)
	return p.UnmarshalReader(buf)
}

func (p *Protocol) UnmarshalReader(r io.Reader) (*Message, error) {
	h, err := p.UnmarshalHeader(r)
	if err != nil {
		return nil, err
	}

	m, err := p.UnmarshalFields(r, h.MGID)
	if err != nil {
		return nil, err
	}
	m.Header = *h

	// Read CRC (optional validation)
	var crc uint16
	if err := binary.Read(r, binary.LittleEndian, &crc); err != nil {
		// Footer might be missing in some streams
		return m, nil
	}

	return m, nil
}

func (p *Protocol) UnmarshalHeader(r io.Reader) (*Header, error) {
	var h Header
	if err := binary.Read(r, binary.LittleEndian, &h.Sync); err != nil {
		return nil, err
	}
	if h.Sync != 0xFE55 && h.Sync != 0x55FE {
		return nil, fmt.Errorf("invalid sync number: 0x%04X", h.Sync)
	}

	if err := binary.Read(r, binary.LittleEndian, &h.MGID); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &h.Size); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &h.Timestamp); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &h.Src); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &h.SrcEnt); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &h.Dst); err != nil {
		return nil, err
	}
	if err := binary.Read(r, binary.LittleEndian, &h.DstEnt); err != nil {
		return nil, err
	}
	return &h, nil
}

// UnmarshalFields reads the fields of a message given its MGID.
// This is used for nested messages which don't have the full header.
func (p *Protocol) UnmarshalFields(r io.Reader, mgid uint16) (*Message, error) {
	msgDef, ok := p.Messages[mgid]
	if !ok {
		return nil, fmt.Errorf("unknown message ID: %d", mgid)
	}

	m := &Message{
		Header: Header{MGID: mgid},
		Fields: make(map[string]any),
	}

	for _, field := range msgDef.Fields {
		val, err := p.readField(r, field.Type)
		if err != nil {
			return nil, fmt.Errorf("failed to read field %s: %w", field.Abbrev, err)
		}
		m.Fields[field.Abbrev] = val
	}

	return m, nil
}

func (p *Protocol) readField(r io.Reader, typ string) (any, error) {
	switch typ {
	case "int8_t":
		var v int8
		err := binary.Read(r, binary.LittleEndian, &v)
		return v, err
	case "uint8_t":
		var v uint8
		err := binary.Read(r, binary.LittleEndian, &v)
		return v, err
	case "int16_t":
		var v int16
		err := binary.Read(r, binary.LittleEndian, &v)
		return v, err
	case "uint16_t":
		var v uint16
		err := binary.Read(r, binary.LittleEndian, &v)
		return v, err
	case "int32_t":
		var v int32
		err := binary.Read(r, binary.LittleEndian, &v)
		return v, err
	case "uint32_t":
		var v uint32
		err := binary.Read(r, binary.LittleEndian, &v)
		return v, err
	case "int64_t":
		var v int64
		err := binary.Read(r, binary.LittleEndian, &v)
		return v, err
	case "fp32_t":
		var v float32
		err := binary.Read(r, binary.LittleEndian, &v)
		return v, err
	case "fp64_t":
		var v float64
		err := binary.Read(r, binary.LittleEndian, &v)
		return v, err
	case "plaintext":
		var size uint16
		if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
			return nil, err
		}
		if size == 0 {
			return "", nil
		}
		b := make([]byte, size)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		return string(b), nil
	case "rawdata":
		var size uint16
		if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
			return nil, err
		}
		if size == 0 {
			return []byte{}, nil
		}
		b := make([]byte, size)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		return b, nil
	case "message":
		var mgid uint16
		if err := binary.Read(r, binary.LittleEndian, &mgid); err != nil {
			return nil, err
		}
		if mgid == 0xFFFF {
			return nil, nil
		}
		return p.UnmarshalFields(r, mgid)
	case "message-list":
		var count uint16
		if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
			return nil, err
		}
		res := make([]*Message, count)
		for i := uint16(0); i < count; i++ {
			var mgid uint16
			if err := binary.Read(r, binary.LittleEndian, &mgid); err != nil {
				return nil, err
			}
			if mgid == 0xFFFF {
				res[i] = nil
				continue
			}
			m, err := p.UnmarshalFields(r, mgid)
			if err != nil {
				return nil, err
			}
			res[i] = m
		}
		return res, nil
	default:
		return nil, fmt.Errorf("unknown type: %s", typ)
	}
}
