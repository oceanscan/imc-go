package imc

import (
	"bytes"
	"io"
)

// crcTables are CRC16-IBM (polynomial 0x8005 reflected, i.e. 0xA001)
// tables for slicing by 8: crcTables[k][x] is the CRC of byte x followed by
// k zero bytes.
var crcTables = func() (t [8][256]uint16) {
	for i := range t[0] {
		c := uint16(i)
		for k := 0; k < 8; k++ {
			if c&1 != 0 {
				c = c>>1 ^ 0xA001
			} else {
				c >>= 1
			}
		}
		t[0][i] = c
	}
	for k := 1; k < 8; k++ {
		for i := range t[k] {
			c := t[k-1][i]
			t[k][i] = c>>8 ^ t[0][byte(c)]
		}
	}
	return
}()

// CRC16 computes the CRC16-IBM of data with initial value 0, as DUNE's
// Algorithms::CRC16 (the checksum of IMC frames).
func CRC16(data []byte) uint16 {
	return UpdateCRC16(0, data)
}

// UpdateCRC16 continues a CRC16-IBM computation.
func UpdateCRC16(crc uint16, data []byte) uint16 {
	t := &crcTables
	for len(data) >= 8 {
		c := crc ^ (uint16(data[0]) | uint16(data[1])<<8)
		crc = t[7][byte(c)] ^ t[6][c>>8] ^ t[5][data[2]] ^ t[4][data[3]] ^
			t[3][data[4]] ^ t[2][data[5]] ^ t[1][data[6]] ^ t[0][data[7]]
		data = data[8:]
	}
	for _, b := range data {
		crc = crc>>8 ^ t[0][byte(crc)^b]
	}
	return crc
}

// CalculateCRC16 is CRC16 (kept for compatibility).
func CalculateCRC16(data []byte) uint16 { return CRC16(data) }

// readerBufSize holds two of the largest frames, so that an incomplete
// frame can always be completed after compacting the buffer.
const readerBufSize = 2 * MaxInFrameSize

// FrameReader splits a byte stream (a TCP connection, a Unix socket, an LSF
// log) into frames with the same rules as DUNE (Packet's stream
// deserialization and the DistributedBus deframer):
//
//   - bytes that do not start a sync number (in either byte order) are
//     skipped;
//   - an incomplete frame waits for more bytes (partial reads are fine);
//   - a frame whose CRC is wrong is skipped one byte at a time (its size
//     cannot be trusted), so the reader resynchronizes at the next frame;
//   - frames whose sync number is byte swapped are big endian.
//
// Next returns frames whose CRC is valid; decoding them (Frame.Decode, or
// Protocol.DecodeFrame for the dynamic codec) is separate, so a caller can
// look at Header.MGID and skip frames it does not want without decoding
// them. A frame with an unknown id or a payload shorter than its fields is
// a valid frame: skip it whole.
//
// The reader uses one fixed buffer (2 x 65557 bytes) and never allocates
// per frame or per byte. It is not safe for concurrent use.
type FrameReader struct {
	r          io.Reader
	buf        []byte
	start, end int
	err        error
	sync       uint16

	// Frames counts the frames returned.
	Frames uint64
	// SkippedBytes counts bytes skipped while looking for a frame (garbage
	// and the first byte of frames with a bad CRC).
	SkippedBytes uint64
	// CRCErrors counts frames with a bad CRC.
	CRCErrors uint64
	// ReadBytes counts the bytes read or fed.
	ReadBytes uint64
}

// NewFrameReader returns a reader of the frames of r. With a nil r, data is
// given with Feed and Next returns ErrNeedMore when it needs more.
func NewFrameReader(r io.Reader) *FrameReader {
	return &FrameReader{r: r, buf: make([]byte, readerBufSize), sync: SyncNumber}
}

// SetSync changes the expected synchronization number (default SyncNumber),
// e.g. to the one of the XML of a log.
func (fr *FrameReader) SetSync(sync uint16) { fr.sync = sync }

// Buffered returns the number of bytes received but not consumed yet (an
// incomplete frame).
func (fr *FrameReader) Buffered() int { return fr.end - fr.start }

// Feed adds received bytes (for readers created with a nil io.Reader). It
// copies p; frames returned by Next before are no longer valid.
func (fr *FrameReader) Feed(p []byte) {
	fr.ReadBytes += uint64(len(p))
	for len(p) > 0 {
		fr.compact()
		if fr.end == len(fr.buf) {
			// Only possible when the caller feeds more than a buffer
			// without calling Next: grow.
			nb := make([]byte, 2*len(fr.buf))
			copy(nb, fr.buf[:fr.end])
			fr.buf = nb
		}
		n := copy(fr.buf[fr.end:], p)
		fr.end += n
		p = p[n:]
	}
}

// Write is Feed, so a FrameReader can be the destination of io.Copy.
func (fr *FrameReader) Write(p []byte) (int, error) {
	fr.Feed(p)
	return len(p), nil
}

func (fr *FrameReader) compact() {
	if fr.start == 0 {
		return
	}
	if fr.start == fr.end {
		fr.start, fr.end = 0, 0
		return
	}
	// Move the unconsumed bytes to the front only when needed: when there
	// is less room after them than a whole frame.
	if len(fr.buf)-fr.end < MaxInFrameSize {
		n := copy(fr.buf, fr.buf[fr.start:fr.end])
		fr.start, fr.end = 0, n
	}
}

// Next returns the next frame with a valid CRC. Its slices point into the
// reader's buffer and are valid until the next call. At the end of the
// stream it returns the reader's error (io.EOF), after the last complete
// frame; an incomplete frame left at the end stays in Buffered(). With a
// nil io.Reader it returns ErrNeedMore when the data fed so far holds no
// complete frame.
func (fr *FrameReader) Next() (Frame, error) {
	for {
		if f, ok := fr.scan(); ok {
			fr.Frames++
			return f, nil
		}
		if fr.err != nil {
			return Frame{}, fr.err
		}
		if fr.r == nil {
			return Frame{}, ErrNeedMore
		}
		fr.compact()
		n, err := fr.r.Read(fr.buf[fr.end:])
		if n > 0 {
			fr.end += n
			fr.ReadBytes += uint64(n)
		}
		if err != nil {
			fr.err = err
		} else if n == 0 {
			// A reader that returns 0, nil: try again (io.Reader allows it).
			continue
		}
	}
}

// scan looks for a complete valid frame in buf[start:end].
func (fr *FrameReader) scan() (Frame, bool) {
	lo, hi := byte(fr.sync), byte(fr.sync>>8)
	for {
		b := fr.buf[fr.start:fr.end]
		if len(b) < 2 {
			if len(b) == 1 && b[0] != lo && b[0] != hi {
				fr.SkippedBytes++
				fr.start++
			}
			return Frame{}, false
		}
		be, ok := syncOrder(b[0], b[1], fr.sync)
		if !ok {
			// Skip to the next byte that may start a sync number.
			skip := nextSync(b[1:], lo, hi) + 1
			fr.SkippedBytes += uint64(skip)
			fr.start += skip
			continue
		}
		if len(b) < HeaderSize {
			return Frame{}, false
		}
		size := int(readU16(b[4:], be))
		n := HeaderSize + size
		if len(b) < n+FooterSize {
			return Frame{}, false
		}
		if CRC16(b[:n]) != readU16(b[n:], be) {
			fr.CRCErrors++
			fr.SkippedBytes++
			fr.start++
			continue
		}
		h := decodeHeader(b, be)
		h.Sync = fr.sync
		fr.start += n + FooterSize
		return Frame{Header: h, BigEndian: be, Payload: b[HeaderSize:n:n], Raw: b[: n+FooterSize : n+FooterSize]}, true
	}
}

// nextSync returns the index of the first byte of b that is lo or hi (or
// len(b)).
func nextSync(b []byte, lo, hi byte) int {
	// Search hi only before the first lo: linear even in long runs of one
	// of the two bytes.
	i := bytes.IndexByte(b, lo)
	limit := b
	if i >= 0 {
		limit = b[:i]
	}
	if j := bytes.IndexByte(limit, hi); j >= 0 {
		return j
	}
	if i >= 0 {
		return i
	}
	return len(b)
}
