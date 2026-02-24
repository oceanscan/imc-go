package main

import (
	"bufio"
	"compress/gzip"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/oceanscan/imc-go"
)

var (
	xmlPath = flag.String("xml", "IMC.xml", "Path to IMC.xml (auto-detected from LSF directory if omitted)")
	outDir  = flag.String("o", "", "Output base directory (default: directory of input file)")
	mode    = flag.String("mode", "day", "Split mode: day or hour")
)

const (
	mgidEntityInfo        = 3
	mgidEntityList        = 5
	mgidLoggingControl    = 102
	mgidAnnounce          = 151
	mgidPlanSpecification = 551
)

// CountingReader tracks bytes read from an io.Reader.
type CountingReader struct {
	r     io.Reader
	count int64
}

func (cr *CountingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.count += int64(n)
	return n, err
}

// bucketFile tracks an open output file for a time bucket.
type bucketFile struct {
	File *os.File
	BW   *bufio.Writer
}

func main() {
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "Usage: lsfsplit [flags] <file.lsf>")
		flag.PrintDefaults()
		os.Exit(1)
	}

	if *mode != "day" && *mode != "hour" {
		log.Fatalf("Invalid mode %q: must be 'day' or 'hour'", *mode)
	}

	lsfPath := flag.Arg(0)

	if *outDir == "" {
		*outDir = filepath.Dir(lsfPath)
	}

	xmlFile := resolveXML(filepath.Dir(lsfPath), *xmlPath)
	fmt.Printf("Loading protocol from %s...\n", xmlFile)
	xmlR, _, _, err := openFile(xmlFile)
	if err != nil {
		log.Fatalf("Failed to open XML: %v", err)
	}
	xmlProto, err := imc.ParseReader(xmlR)
	xmlR.Close()
	if err != nil {
		log.Fatalf("Failed to parse IMC XML: %v", err)
	}
	proto := imc.NewProtocol(xmlProto)

	if err := splitLSF(lsfPath, proto); err != nil {
		log.Fatalf("%v", err)
	}
}

func splitLSF(lsfPath string, proto *imc.Protocol) error {
	// --- Pass 1: Collect preamble data ---
	fmt.Println("Scanning for preamble data...")
	preamble, err := collectPreamble(lsfPath, proto)
	if err != nil {
		return err
	}
	if preamble.loggingCtrl != nil {
		fmt.Printf("  LoggingControl: %s\n", imc.ToString(preamble.loggingCtrl.Fields["name"]))
	}
	if preamble.announceRaw != nil {
		fmt.Println("  Announce: found")
	}
	fmt.Printf("  Entities: %d\n", len(preamble.entityMap))
	if preamble.planSpecRaw != nil {
		fmt.Println("  PlanSpecification: found")
	}

	// --- Pass 2: Split messages into time-bucketed files ---
	f, totalSize, cr, err := openFile(lsfPath)
	if err != nil {
		return fmt.Errorf("failed to open LSF: %w", err)
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 64*1024)

	buckets := make(map[string]*bucketFile)
	defer func() {
		for _, bf := range buckets {
			bf.BW.Flush()
			bf.File.Close()
		}
	}()

	fmt.Println("Splitting messages...")
	count := 0

	for {
		header, err := proto.UnmarshalHeader(br)
		if err != nil {
			if err == io.EOF {
				break
			}
			fmt.Printf("\nError at message %d: %v\n", count, err)
			break
		}
		count++

		// Read raw payload + CRC.
		rawBody := make([]byte, int(header.Size)+2)
		if _, err := io.ReadFull(br, rawBody); err != nil {
			return fmt.Errorf("failed to read payload at message %d: %w", count, err)
		}

		// Build full raw message bytes (header + payload + CRC).
		rawMsg := serializeHeader(header)
		rawMsg = append(rawMsg, rawBody...)

		// Determine time bucket.
		key := bucketKey(header.Timestamp, *mode)

		bf, ok := buckets[key]
		if !ok {
			bf, err = openBucket(key, *outDir)
			if err != nil {
				return fmt.Errorf("failed to create output for %s: %w", key, err)
			}
			buckets[key] = bf

			// Write preamble.
			if err := writePreamble(bf.BW, proto, preamble, key); err != nil {
				return fmt.Errorf("failed to write preamble for %s: %w", key, err)
			}
		}

		// Write raw message to bucket.
		if _, err := bf.BW.Write(rawMsg); err != nil {
			return fmt.Errorf("failed to write message to %s: %w", key, err)
		}

		if count%10000 == 0 {
			pct := min(float64(cr.count)/float64(totalSize)*100, 100)
			fmt.Printf("\rSplitting: %d messages (%.1f%%)...", count, pct)
		}
	}

	// Flush all.
	for _, bf := range buckets {
		bf.BW.Flush()
	}

	fmt.Printf("\rSplitting: %d messages (100.0%%)   \n", count)
	fmt.Printf("Split into %d file(s):\n", len(buckets))

	// Sort keys for orderly output.
	keys := make([]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fi, _ := buckets[k].File.Stat()
		size := int64(0)
		if fi != nil {
			size = fi.Size()
		}
		fmt.Printf("  %s/Data.lsf (%d bytes)\n", k, size)
	}

	return nil
}

// preambleData holds all preamble information collected during pass 1.
type preambleData struct {
	loggingCtrl *imc.Message
	announceRaw []byte
	announceTs  float64
	announceSrc uint16
	entityMap   map[uint8]string
	planSpecRaw []byte
}

// collectPreamble scans the first ~10 minutes of the LSF file to collect preamble data.
func collectPreamble(lsfPath string, proto *imc.Protocol) (*preambleData, error) {
	f, _, _, err := openFile(lsfPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open LSF: %w", err)
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 64*1024)

	p := &preambleData{
		entityMap: make(map[uint8]string),
	}

	var firstTs float64
	const scanWindow = 10 * 60 // 10 minutes in seconds

	for {
		header, err := proto.UnmarshalHeader(br)
		if err != nil {
			break
		}

		// Track first timestamp and stop after 10 minutes of data.
		if firstTs == 0 {
			firstTs = header.Timestamp
		} else if header.Timestamp-firstTs > scanWindow {
			break
		}

		switch header.MGID {
		case mgidLoggingControl:
			if p.loggingCtrl == nil {
				msg, err := proto.UnmarshalFields(br, header.MGID)
				if err == nil {
					msg.Header = *header
					p.loggingCtrl = msg
				}
				io.CopyN(io.Discard, br, 2) // skip CRC
				continue
			}
		case mgidAnnounce:
			if p.announceRaw == nil {
				rawBody := make([]byte, int(header.Size)+2)
				if _, err := io.ReadFull(br, rawBody); err == nil {
					raw := serializeHeader(header)
					raw = append(raw, rawBody...)
					p.announceRaw = raw
					p.announceSrc = header.Src
					p.announceTs = header.Timestamp
				}
				continue
			}
		case mgidEntityInfo:
			msg, err := proto.UnmarshalFields(br, header.MGID)
			if err == nil {
				id := uint8(imc.ToInt64(msg.Fields["id"]))
				label := imc.ToString(msg.Fields["label"])
				p.entityMap[id] = label
			}
			io.CopyN(io.Discard, br, 2) // skip CRC
			continue
		case mgidPlanSpecification:
			if p.planSpecRaw == nil {
				rawBody := make([]byte, int(header.Size)+2)
				if _, err := io.ReadFull(br, rawBody); err == nil {
					raw := serializeHeader(header)
					raw = append(raw, rawBody...)
					p.planSpecRaw = raw
				}
				continue
			}
		}

		// Skip payload + CRC for non-preamble messages.
		io.CopyN(io.Discard, br, int64(header.Size)+2)
	}

	return p, nil
}

// bucketKey returns the time-bucket key for a given unix timestamp.
func bucketKey(timestamp float64, mode string) string {
	t := time.Unix(0, int64(timestamp*1e9)).UTC()
	switch mode {
	case "hour":
		return t.Format("20060102") + "/" + t.Format("15")
	default: // day
		return t.Format("20060102")
	}
}

// openBucket creates the output directory and file for a time bucket.
func openBucket(key, baseDir string) (*bucketFile, error) {
	dir := filepath.Join(baseDir, key)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "Data.lsf")
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	bw := bufio.NewWriterSize(f, 64*1024)
	return &bucketFile{File: f, BW: bw}, nil
}

// writePreamble writes the preamble messages to an output file.
func writePreamble(w io.Writer, proto *imc.Protocol, p *preambleData, bucketKey string) error {
	// 1. LoggingControl with modified name.
	if p.loggingCtrl != nil {
		msg := cloneMessage(p.loggingCtrl)
		origName := imc.ToString(msg.Fields["name"])
		msg.Fields["name"] = origName + "/" + bucketKey
		data, err := msg.Serialize(proto)
		if err != nil {
			return fmt.Errorf("failed to serialize LoggingControl: %w", err)
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
	}

	// 2. Announce (raw bytes, unmodified).
	if len(p.announceRaw) > 0 {
		if _, err := w.Write(p.announceRaw); err != nil {
			return err
		}
	}

	// 3. Synthesized EntityList from collected EntityInfo messages.
	if len(p.entityMap) > 0 {
		entityListData, err := buildEntityList(proto, p.entityMap, p.announceTs, p.announceSrc)
		if err != nil {
			return fmt.Errorf("failed to build EntityList: %w", err)
		}
		if _, err := w.Write(entityListData); err != nil {
			return err
		}
	}

	// 4. PlanSpecification (raw bytes, unmodified).
	if len(p.planSpecRaw) > 0 {
		if _, err := w.Write(p.planSpecRaw); err != nil {
			return err
		}
	}

	return nil
}

// buildEntityList creates a serialized EntityList message from a map of entity IDs to labels.
func buildEntityList(proto *imc.Protocol, entities map[uint8]string, ts float64, src uint16) ([]byte, error) {
	msg, err := proto.CreateMessage("EntityList")
	if err != nil {
		return nil, err
	}

	msg.Header.Timestamp = ts
	msg.Header.Src = src
	msg.Header.SrcEnt = 0
	msg.Header.Dst = 0xFFFF
	msg.Header.DstEnt = 0xFF

	msg.Fields["op"] = uint8(0) // REPORT

	// Build TupleList: "id=label;id=label;..."
	ids := make([]int, 0, len(entities))
	for id := range entities {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)

	var parts []string
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("%s=%d", entities[uint8(id)], id))
	}
	msg.Fields["list"] = strings.Join(parts, ";")

	return msg.Serialize(proto)
}

// cloneMessage creates a shallow clone of a Message suitable for modifying fields.
func cloneMessage(m *imc.Message) *imc.Message {
	clone := &imc.Message{
		Header: m.Header,
		Fields: make(map[string]any, len(m.Fields)),
	}
	for k, v := range m.Fields {
		clone.Fields[k] = v
	}
	return clone
}

// serializeHeader writes a Header to a 20-byte slice (little-endian, field-by-field).
func serializeHeader(h *imc.Header) []byte {
	buf := make([]byte, 20)
	binary.LittleEndian.PutUint16(buf[0:2], h.Sync)
	binary.LittleEndian.PutUint16(buf[2:4], h.MGID)
	binary.LittleEndian.PutUint16(buf[4:6], h.Size)
	binary.LittleEndian.PutUint64(buf[6:14], math.Float64bits(h.Timestamp))
	binary.LittleEndian.PutUint16(buf[14:16], h.Src)
	buf[16] = h.SrcEnt
	binary.LittleEndian.PutUint16(buf[17:19], h.Dst)
	buf[19] = h.DstEnt
	return buf
}

// --- File opening helpers (supporting .gz) ---

func openFile(path string) (io.ReadCloser, int64, *CountingReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, nil, err
	}
	size := fi.Size()

	// Wrap raw file to track compressed (or raw) bytes read.
	cr := &CountingReader{r: f}

	if strings.HasSuffix(path, ".gz") {
		gr, err := gzip.NewReader(cr)
		if err != nil {
			f.Close()
			return nil, 0, nil, err
		}
		return &gzipReadCloser{Reader: gr, f: f}, size, cr, nil
	}
	return &fileReadCloser{f: f, r: cr}, size, cr, nil
}

// fileReadCloser wraps a file with a CountingReader for uncompressed files.
type fileReadCloser struct {
	f *os.File
	r *CountingReader
}

func (fc *fileReadCloser) Read(p []byte) (int, error) {
	return fc.r.Read(p)
}

func (fc *fileReadCloser) Close() error {
	return fc.f.Close()
}

type gzipReadCloser struct {
	*gzip.Reader
	f *os.File
}

func (g *gzipReadCloser) Close() error {
	err1 := g.Reader.Close()
	err2 := g.f.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// resolveXML finds the IMC.xml file, checking next to the LSF then falling back.
func resolveXML(lsfDir, xmlFlag string) string {
	if xmlFlag != "IMC.xml" {
		return xmlFlag
	}
	if _, err := os.Stat(xmlFlag); err == nil {
		return xmlFlag
	}
	alt := filepath.Join(lsfDir, "IMC.xml")
	if _, err := os.Stat(alt); err == nil {
		return alt
	}
	if _, err := os.Stat(alt + ".gz"); err == nil {
		return alt + ".gz"
	}
	return xmlFlag
}
