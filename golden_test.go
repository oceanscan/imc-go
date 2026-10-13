package imc

// The codecs against DUNE's golden frames (tests/golden of the DUNE tree,
// written by DUNE's own serializer): every frame, little and big endian,
// decodes to the manifest's values and re-encodes byte for byte, with the
// typed and with the dynamic codec; the invalid frames and the stream are
// read as DUNE reads them.
//
// The corpus is read from $DUNE_GOLDEN, or ../dune-ml3-go/tests/golden or
// ../dune/tests/golden (a DUNE checkout next to this one); the tests are
// skipped without it.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type goldenFrame struct {
	File   string         `json:"file"`
	Hex    string         `json:"hex"`
	Length int            `json:"length"`
	Header map[string]any `json:"header"`
	CRC    string         `json:"crc16"`
}

type goldenCase struct {
	Abbrev  string            `json:"abbrev"`
	ID      uint16            `json:"id"`
	Covers  string            `json:"covers"`
	Fields  map[string]any    `json:"fields"`
	Special map[string]string `json:"special"`
	LE      goldenFrame       `json:"le"`
	BE      goldenFrame       `json:"be"`
}

type goldenManifest struct {
	Cases      []goldenCase `json:"cases"`
	IMCMD5     string       `json:"imc_md5"`
	IMCVersion string       `json:"imc_version"`
	Sync       string       `json:"sync"`
	Invalid    []struct {
		File string `json:"file"`
	} `json:"invalid"`
	Stream struct {
		File     string `json:"file"`
		Length   int    `json:"length"`
		Consumed int    `json:"consumed"`
		Messages []struct {
			Abbrev string `json:"abbrev"`
			Length int    `json:"length"`
			Offset int    `json:"offset"`
		} `json:"messages"`
	} `json:"stream"`
}

func goldenDir(t testing.TB) string {
	dirs := []string{filepath.Join("..", "dune-ml3-go", "tests", "golden"), filepath.Join("..", "dune", "tests", "golden")}
	if d := os.Getenv("DUNE_GOLDEN"); d != "" {
		dirs = []string{d}
	}
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err == nil {
			return dir
		}
	}
	t.Skipf("no golden corpus in %v (set DUNE_GOLDEN)", dirs)
	return ""
}

func loadGolden(t testing.TB) (string, *goldenManifest) {
	dir := goldenDir(t)
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m goldenManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return dir, &m
}

func readGolden(t testing.TB, dir, file string) []byte {
	data, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

var testProto = func() *Protocol {
	x, err := ParseXML("IMC.xml")
	if err != nil {
		panic(err)
	}
	return NewProtocol(x)
}()

var specials = map[string]float64{"NaN": math.NaN(), "Infinity": math.Inf(1), "-Infinity": math.Inf(-1), "-0": math.Copysign(0, -1)}

// checkFields compares a dynamic message (decoded, or converted from a
// typed one) with DUNE's JSON form of it.
func checkFields(t *testing.T, m *Message, want map[string]any, special map[string]string, path string) {
	t.Helper()
	def := testProto.Messages[m.Header.MGID]
	if def == nil {
		t.Fatalf("%s: unknown message %d", path, m.Header.MGID)
	}
	if len(want) != len(def.Fields) {
		t.Errorf("%s: %d fields, manifest has %d", path, len(def.Fields), len(want))
	}
	for _, f := range def.Fields {
		where := path + f.Abbrev
		w, ok := want[f.Abbrev]
		if !ok {
			t.Errorf("%s: not in the manifest", where)
			continue
		}
		got := m.Fields[f.Abbrev]
		switch f.Type {
		case "fp32_t", "fp64_t":
			var wf float64
			if s, ok := special[where]; ok {
				wf = specials[s]
			} else {
				wf = w.(float64)
			}
			var gf float64
			var same bool
			if f.Type == "fp32_t" {
				g, ok := got.(float32)
				if !ok {
					t.Errorf("%s: %T, not float32", where, got)
					continue
				}
				gf = float64(g)
				same = math.Float32bits(g) == math.Float32bits(float32(wf)) || (math.IsNaN(wf) && g != g)
			} else {
				g, ok := got.(float64)
				if !ok {
					t.Errorf("%s: %T, not float64", where, got)
					continue
				}
				gf = g
				same = math.Float64bits(g) == math.Float64bits(wf) || (math.IsNaN(wf) && g != g)
			}
			if !same {
				t.Errorf("%s: %v, want %v", where, gf, wf)
			}
		case "plaintext":
			if got != w {
				t.Errorf("%s: %q, want %q", where, got, w)
			}
		case "rawdata":
			g, _ := got.([]byte)
			if strings.ToUpper(hex.EncodeToString(g)) != w {
				t.Errorf("%s: rawdata differs", where)
			}
		case "message":
			gm, _ := got.(*Message)
			if w == nil {
				if gm != nil {
					t.Errorf("%s: %v, want no message", where, gm)
				}
				continue
			}
			wm := w.(map[string]any)
			if gm == nil {
				t.Errorf("%s: no message, want %v", where, wm["abbrev"])
				continue
			}
			checkMessage(t, gm, wm, special, where+".")
		case "message-list":
			gl, _ := got.([]*Message)
			wl := w.([]any)
			if len(gl) != len(wl) {
				t.Errorf("%s: %d messages, want %d", where, len(gl), len(wl))
				continue
			}
			for i := range wl {
				p := fmt.Sprintf("%s[%d]", where, i)
				if wl[i] == nil {
					if gl[i] != nil {
						t.Errorf("%s: want null", p)
					}
					continue
				}
				if gl[i] == nil {
					t.Errorf("%s: null", p)
					continue
				}
				checkMessage(t, gl[i], wl[i].(map[string]any), special, p+".")
			}
		default:
			if float64(ToInt64(got)) != w.(float64) || zeroValue(f.Type) == nil ||
				fmt.Sprintf("%T", got) != fmt.Sprintf("%T", zeroValue(f.Type)) {
				t.Errorf("%s: %v (%T), want %v", where, got, got, w)
			}
		}
	}
}

func checkMessage(t *testing.T, m *Message, want map[string]any, special map[string]string, path string) {
	t.Helper()
	abbrev := want["abbrev"]
	if def := testProto.Messages[m.Header.MGID]; def == nil || def.Abbrev != abbrev {
		t.Errorf("%s: message %d, want %v", path, m.Header.MGID, abbrev)
		return
	}
	fields := map[string]any{}
	for k, v := range want {
		if k != "abbrev" {
			fields[k] = v
		}
	}
	checkFields(t, m, fields, special, path)
}

func checkHeader(t *testing.T, h Header, want map[string]any) {
	t.Helper()
	got := map[string]float64{"mgid": float64(h.MGID), "size": float64(h.Size), "timestamp": h.Timestamp,
		"src": float64(h.Src), "src_ent": float64(h.SrcEnt), "dst": float64(h.Dst), "dst_ent": float64(h.DstEnt)}
	for k, v := range got {
		if want[k].(float64) != v {
			t.Errorf("header %s: %v, want %v", k, v, want[k])
		}
	}
}

func TestGoldenManifest(t *testing.T) {
	_, m := loadGolden(t)
	if m.IMCMD5 != IMCMD5 || m.IMCVersion != IMCVersion || m.Sync != fmt.Sprintf("0x%04X", SyncNumber) {
		t.Fatalf("corpus is IMC %s %s %s, code is %s %s 0x%04X", m.IMCVersion, m.IMCMD5, m.Sync, IMCVersion, IMCMD5, SyncNumber)
	}
	if testProto.MD5 != IMCMD5 {
		t.Fatalf("IMC.xml has MD5 %s, the typed messages %s", testProto.MD5, IMCMD5)
	}
}

func TestGoldenFrames(t *testing.T) {
	dir, man := loadGolden(t)
	n := 0
	for _, c := range man.Cases {
		for _, order := range []string{"le", "be"} {
			desc := c.LE
			if order == "be" {
				desc = c.BE
			}
			be := order == "be"
			name := strings.TrimSuffix(filepath.Base(desc.File), ".imc") + "/" + order
			t.Run(name, func(t *testing.T) {
				data := readGolden(t, dir, desc.File)
				if len(data) != desc.Length {
					t.Fatalf("length %d, want %d", len(data), desc.Length)
				}
				if desc.Hex != "" && strings.ToUpper(hex.EncodeToString(data)) != desc.Hex {
					t.Fatal("file differs from the manifest's hex")
				}
				// Typed codec.
				m, err := Decode(data)
				if err != nil {
					t.Fatalf("typed decode: %v", err)
				}
				if m.Abbrev() != c.Abbrev || m.GetMGID() != c.ID {
					t.Fatalf("decoded %s (%d), want %s (%d)", m.Abbrev(), m.GetMGID(), c.Abbrev, c.ID)
				}
				checkHeader(t, *m.GetHeader(), desc.Header)
				checkFields(t, m.ToMessage(), c.Fields, c.Special, "")
				out, err := AppendFrame(nil, m, be)
				if err != nil {
					t.Fatalf("typed encode: %v", err)
				}
				if !bytes.Equal(out, data) {
					t.Fatalf("typed re-encoding differs\n got %X\nwant %X", head(out), head(data))
				}
				// Typed -> dynamic -> typed keeps everything.
				back := NewMessage(m.GetMGID())
				if err := back.FromMessage(m.ToMessage()); err != nil {
					t.Fatal(err)
				}
				if out2, _ := AppendFrame(nil, back, be); !bytes.Equal(out2, data) {
					t.Fatal("typed -> dynamic -> typed changes the frame")
				}
				// Dynamic codec.
				dm, err := testProto.Unmarshal(data)
				if err != nil {
					t.Fatalf("dynamic decode: %v", err)
				}
				checkHeader(t, dm.Header, desc.Header)
				checkFields(t, dm, c.Fields, c.Special, "")
				dout, err := testProto.Encode(dm, be)
				if err != nil {
					t.Fatalf("dynamic encode: %v", err)
				}
				if !bytes.Equal(dout, data) {
					t.Fatalf("dynamic re-encoding differs\n got %X\nwant %X", head(dout), head(data))
				}
				// Through a FrameReader, in small reads.
				fr := NewFrameReader(&chunkReader{data: data, n: 7})
				f, err := fr.Next()
				if err != nil {
					t.Fatalf("reader: %v", err)
				}
				if !bytes.Equal(f.Raw, data) || f.BigEndian != be {
					t.Fatal("reader returned another frame")
				}
				if _, err := fr.Next(); err != io.EOF {
					t.Fatalf("reader: %v after the frame, want EOF", err)
				}
			})
			n++
		}
	}
	if n != 2*len(man.Cases) || n == 0 {
		t.Fatalf("%d frames checked", n)
	}
}

func head(b []byte) []byte {
	if len(b) > 96 {
		return b[:96]
	}
	return b
}

// chunkReader returns at most n bytes per Read.
type chunkReader struct {
	data []byte
	n    int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	k := min(r.n, len(p), len(r.data))
	copy(p, r.data[:k])
	r.data = r.data[k:]
	return k, nil
}

func TestGoldenLEandBEDecodeAlike(t *testing.T) {
	dir, man := loadGolden(t)
	for _, c := range man.Cases {
		le, err1 := Decode(readGolden(t, dir, c.LE.File))
		be, err2 := Decode(readGolden(t, dir, c.BE.File))
		if err1 != nil || err2 != nil {
			t.Fatal(err1, err2)
		}
		a, _ := AppendFrame(nil, le, false)
		b, _ := AppendFrame(nil, be, false)
		if !bytes.Equal(a, b) {
			t.Errorf("%s: big and little endian frames decode differently", c.LE.File)
		}
	}
}

func TestGoldenInvalid(t *testing.T) {
	dir, man := loadGolden(t)
	inv := map[string][]byte{}
	for _, x := range man.Invalid {
		inv[filepath.Base(x.File)] = readGolden(t, dir, x.File)
	}
	if len(inv) != 4 {
		t.Fatalf("%d invalid frames", len(inv))
	}
	var unk *UnknownMessageError
	if _, err := Decode(inv["bad-crc.imc"]); !errors.Is(err, ErrCRC) {
		t.Errorf("bad-crc: %v", err)
	}
	if _, err := Decode(inv["unknown-id.imc"]); !errors.As(err, &unk) || unk.MGID != 65000 {
		t.Errorf("unknown-id: %v", err)
	}
	if _, err := Decode(inv["truncated.imc"]); !errors.Is(err, ErrLength) {
		t.Errorf("truncated: %v", err)
	}
	if m, err := Decode(inv["trailing-byte.imc"]); err != nil || m.Abbrev() != "EntityState" {
		t.Errorf("trailing-byte: %v", err)
	}
	// Dynamic codec, strict.
	if _, err := testProto.Unmarshal(inv["bad-crc.imc"]); !errors.Is(err, ErrCRC) {
		t.Errorf("dynamic bad-crc: %v", err)
	}
	if _, err := testProto.Unmarshal(inv["unknown-id.imc"]); !errors.As(err, &unk) {
		t.Errorf("dynamic unknown-id: %v", err)
	}
	if _, err := testProto.Unmarshal(inv["truncated.imc"]); !errors.Is(err, ErrLength) {
		t.Errorf("dynamic truncated: %v", err)
	}
	if m, err := testProto.Unmarshal(inv["trailing-byte.imc"]); err != nil || m.Header.MGID != 1 {
		t.Errorf("dynamic trailing-byte: %v", err)
	}
	// Lenient: the bad CRC is ignored, as the old codec did.
	lenient := *testProto
	lenient.Lenient = true
	if _, err := lenient.Unmarshal(inv["bad-crc.imc"]); err != nil {
		t.Errorf("lenient bad-crc: %v", err)
	}

	// In a stream reader: frames, decoded messages, bytes left.
	expect := map[string]struct {
		frames, msgs int
		consumed     bool
	}{
		"bad-crc.imc":       {0, 0, true},
		"unknown-id.imc":    {1, 0, true},
		"truncated.imc":     {0, 0, false},
		"trailing-byte.imc": {1, 1, true},
	}
	for name, data := range inv {
		fr := NewFrameReader(bytes.NewReader(data))
		frames, msgs := 0, 0
		for {
			f, err := fr.Next()
			if err != nil {
				if err != io.EOF {
					t.Errorf("%s: %v", name, err)
				}
				break
			}
			frames++
			if _, err := f.Decode(); err == nil {
				msgs++
			}
		}
		e := expect[name]
		if frames != e.frames || msgs != e.msgs || (fr.Buffered() == 0) != e.consumed {
			t.Errorf("%s: %d frames, %d messages, %d bytes left; want %+v", name, frames, msgs, fr.Buffered(), e)
		}
		if !e.consumed && fr.Buffered() != len(data) {
			t.Errorf("%s: %d bytes kept, want all %d", name, fr.Buffered(), len(data))
		}
	}
}

func TestGoldenStream(t *testing.T) {
	dir, man := loadGolden(t)
	s := man.Stream
	data := readGolden(t, dir, s.File)
	if len(data) != s.Length {
		t.Fatal("stream length")
	}
	var want []string
	for _, m := range s.Messages {
		want = append(want, m.Abbrev)
	}
	for _, chunk := range []int{1, 2, 3, 7, 21, 64, 65536} {
		for _, push := range []bool{false, true} {
			var got []string
			var frames []Frame
			var fr *FrameReader
			collect := func() bool {
				f, err := fr.Next()
				if err != nil {
					return false
				}
				m, err := f.Decode()
				var unk *UnknownMessageError
				if errors.As(err, &unk) {
					return true // a valid frame of an unknown id: skipped whole
				}
				if err != nil {
					t.Fatalf("chunk %d: %v", chunk, err)
				}
				got = append(got, m.Abbrev())
				raw := append([]byte(nil), f.Raw...)
				frames = append(frames, Frame{Raw: raw, BigEndian: f.BigEndian})
				return true
			}
			if push {
				fr = NewFrameReader(nil)
				for i := 0; i < len(data); i += chunk {
					fr.Feed(data[i:min(i+chunk, len(data))])
					for collect() {
					}
				}
			} else {
				fr = NewFrameReader(&chunkReader{data: data, n: chunk})
				for collect() {
				}
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("chunk %d push %v: %v, want %v", chunk, push, got, want)
			}
			if fr.Buffered() != s.Length-s.Consumed {
				t.Fatalf("chunk %d: %d bytes left, want %d", chunk, fr.Buffered(), s.Length-s.Consumed)
			}
			for i, info := range s.Messages {
				if !bytes.Equal(frames[i].Raw, data[info.Offset:info.Offset+info.Length]) {
					t.Fatalf("message %d is not the frame at %d", i, info.Offset)
				}
			}
		}
	}
}
