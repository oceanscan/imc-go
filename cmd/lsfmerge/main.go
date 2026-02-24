package main

import (
	"bufio"
	"compress/gzip"
	"container/heap"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/oceanscan/imc-go"
)

var (
	xmlPath = flag.String("xml", "IMC.xml", "Path to IMC.xml (auto-detected from LSF directory if omitted)")
	outPath = flag.String("o", "", "Output file path (.lsf or .lsf.gz)")
)

// year2000 is the Unix timestamp for 2000-01-01T00:00:00Z.
const year2000 = 946684800.0

func main() {
	flag.Parse()

	if *outPath == "" {
		fmt.Fprintln(os.Stderr, "Usage: lsfmerge -o <output.lsf> [flags] <file1.lsf> [file2.lsf ...]")
		flag.PrintDefaults()
		os.Exit(1)
	}
	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "Usage: lsfmerge -o <output.lsf> [flags] <file1.lsf> [file2.lsf ...]")
		flag.PrintDefaults()
		os.Exit(1)
	}

	// Resolve IMC.xml from the first input file's directory.
	xmlFile := resolveXML(filepath.Dir(flag.Arg(0)), *xmlPath)
	fmt.Fprintf(os.Stderr, "Loading protocol from %s...\n", xmlFile)
	xmlR, err := openFile(xmlFile)
	if err != nil {
		log.Fatalf("Failed to open XML: %v", err)
	}
	xmlProto, err := imc.ParseReader(xmlR)
	xmlR.Close()
	if err != nil {
		log.Fatalf("Failed to parse IMC XML: %v", err)
	}
	proto := imc.NewProtocol(xmlProto)

	if err := merge(proto, flag.Args(), *outPath); err != nil {
		log.Fatalf("%v", err)
	}
}

// lsfSource represents one open input LSF file for the k-way merge.
type lsfSource struct {
	path    string
	reader  *bufio.Reader
	closer  io.ReadCloser
	proto   *imc.Protocol
	mainSrc uint16      // Src of the first valid message (the log's main system)
	gotMain bool        // whether mainSrc has been determined
	header  *imc.Header // current main-system message header (heap key)
	rawMsg  []byte      // serialized header + payload + CRC of current main-system msg
	pending [][]byte    // non-main-system messages buffered before the current main msg
	done    bool
}

// next reads forward in the LSF stream, skipping messages before year 2000,
// buffering non-main-system messages into pending, until a main-system message
// is found. Returns true if a main-system message is available.
func (s *lsfSource) next() bool {
	s.pending = s.pending[:0]
	s.header = nil
	s.rawMsg = nil

	for {
		h, err := s.proto.UnmarshalHeader(s.reader)
		if err != nil {
			s.done = true
			return false
		}

		// Read raw payload + CRC.
		rawBody := make([]byte, int(h.Size)+2)
		if _, err := io.ReadFull(s.reader, rawBody); err != nil {
			s.done = true
			return false
		}

		// Drop messages before year 2000.
		if h.Timestamp < year2000 {
			continue
		}

		// Build full raw message (header + payload + CRC).
		raw := serializeHeader(h)
		raw = append(raw, rawBody...)

		// Determine main system from the first valid message.
		if !s.gotMain {
			s.mainSrc = h.Src
			s.gotMain = true
		}

		if h.Src == s.mainSrc {
			// This is a main-system message — use it as the heap key.
			s.header = h
			s.rawMsg = raw
			return true
		}

		// Non-main-system message — buffer it.
		s.pending = append(s.pending, raw)
	}
}

// --- Min-heap of lsfSource by main-system timestamp ---

type sourceHeap []*lsfSource

func (h sourceHeap) Len() int           { return len(h) }
func (h sourceHeap) Less(i, j int) bool { return h[i].header.Timestamp < h[j].header.Timestamp }
func (h sourceHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *sourceHeap) Push(x any)        { *h = append(*h, x.(*lsfSource)) }
func (h *sourceHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return item
}

func merge(proto *imc.Protocol, inputs []string, output string) error {
	// Open all input files and prime each source.
	var sources []*lsfSource
	for _, path := range inputs {
		rc, err := openFile(path)
		if err != nil {
			return fmt.Errorf("failed to open %s: %w", path, err)
		}
		s := &lsfSource{
			path:   path,
			reader: bufio.NewReaderSize(rc, 64*1024),
			closer: rc,
			proto:  proto,
		}
		if s.next() {
			sources = append(sources, s)
		} else {
			rc.Close()
			fmt.Fprintf(os.Stderr, "  %s: no valid messages, skipping\n", path)
		}
	}
	defer func() {
		for _, s := range sources {
			if s.closer != nil {
				s.closer.Close()
			}
		}
	}()

	if len(sources) == 0 {
		return fmt.Errorf("no valid messages found in any input file")
	}

	fmt.Fprintf(os.Stderr, "Merging %d file(s)...\n", len(sources))

	// Build heap. Copy into a new slice so Pop doesn't nil entries in sources.
	sh := make(sourceHeap, len(sources))
	copy(sh, sources)
	heap.Init(&sh)

	// Open output file.
	outFile, err := os.Create(output)
	if err != nil {
		return fmt.Errorf("failed to create output: %w", err)
	}
	defer outFile.Close()

	var w io.Writer
	bw := bufio.NewWriterSize(outFile, 64*1024)

	if strings.HasSuffix(output, ".gz") {
		gw := gzip.NewWriter(bw)
		defer func() {
			gw.Close()
			bw.Flush()
		}()
		w = gw
	} else {
		defer bw.Flush()
		w = bw
	}

	// K-way merge loop.
	count := 0
	for sh.Len() > 0 {
		s := heap.Pop(&sh).(*lsfSource)

		// Write buffered non-main-system messages.
		for _, raw := range s.pending {
			if _, err := w.Write(raw); err != nil {
				return fmt.Errorf("write error: %w", err)
			}
			count++
		}

		// Write the main-system message.
		if _, err := w.Write(s.rawMsg); err != nil {
			return fmt.Errorf("write error: %w", err)
		}
		count++

		if count%50000 == 0 {
			fmt.Fprintf(os.Stderr, "\r  %d messages written...", count)
		}

		// Advance this source.
		if s.next() {
			heap.Push(&sh, s)
		} else {
			// Source exhausted — flush any trailing non-main messages.
			for _, raw := range s.pending {
				if _, err := w.Write(raw); err != nil {
					return fmt.Errorf("write error: %w", err)
				}
				count++
			}
			s.closer.Close()
			s.closer = nil
		}
	}

	fmt.Fprintf(os.Stderr, "\r  %d messages written.       \n", count)
	return nil
}

// serializeHeader writes a Header to a 20-byte slice (little-endian).
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

// --- File helpers ---

func openFile(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(path, ".gz") {
		gr, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		return &gzipReadCloser{Reader: gr, f: f}, nil
	}
	return f, nil
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
