package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/oceanscan/imc-go"
)

var (
	lsfPath   = flag.String("lsf", "", "Path to Data.lsf (required)")
	xmlPath   = flag.String("xml", "IMC.xml", "Path to IMC.xml")
	msgFilter = flag.String("msg", "", "Comma-separated message types to export (first of each will be printed)")
)

func main() {
	flag.Parse()

	if *lsfPath == "" {
		fmt.Println("Error: -lsf is required")
		flag.Usage()
		os.Exit(1)
	}

	xmlFile := *xmlPath
	if xmlFile == "IMC.xml" {
		if _, err := os.Stat(xmlFile); os.IsNotExist(err) {
			alt := filepath.Join(filepath.Dir(*lsfPath), "IMC.xml")
			if _, err := os.Stat(alt); err == nil {
				xmlFile = alt
			} else {
				altGz := alt + ".gz"
				if _, err := os.Stat(altGz); err == nil {
					xmlFile = altGz
				}
			}
		}
	}

	xmlR, _, err := openFile(xmlFile)
	if err != nil {
		log.Fatalf("Failed to open XML: %v", err)
	}
	xmlProto, err := imc.ParseReader(xmlR)
	xmlR.Close()
	if err != nil {
		log.Fatalf("Failed to parse IMC XML: %v", err)
	}
	proto := imc.NewProtocol(xmlProto)

	// Pre-scan for entities and source names
	fmt.Fprintf(os.Stderr, "Scanning for systems and entities...\n")
	sysNames, entNames := buildMaps(*lsfPath, proto)
	fmt.Fprintf(os.Stderr, "Found %d systems and %d entities\n", len(sysNames), len(entNames))

	msgsToExport := parseMap(*msgFilter)
	foundMsgs := make(map[string]bool)

	// Open LSF
	f, _, err := openFile(*lsfPath)
	if err != nil {
		log.Fatalf("Failed to open LSF: %v", err)
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 64*1024)

	for {
		header, err := proto.UnmarshalHeader(br)
		if err != nil {
			if err == io.EOF {
				break
			}
			break
		}

		msgDef := proto.Messages[header.MGID]
		if msgDef == nil {
			io.CopyN(io.Discard, br, int64(header.Size)+2)
			continue
		}

		abbrev := msgDef.Abbrev

		// Determine if we should process this message type
		shouldProcess := false
		if len(msgsToExport) == 0 {
			if !foundMsgs[abbrev] {
				shouldProcess = true
			}
		} else if msgsToExport[abbrev] && !foundMsgs[abbrev] {
			shouldProcess = true
		}

		if !shouldProcess {
			io.CopyN(io.Discard, br, int64(header.Size)+2)
			continue
		}

		// Decode Payload
		msg, err := proto.UnmarshalFields(br, header.MGID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error decoding payload for %s: %v\n", abbrev, err)
			break
		}
		msg.Header = *header
		io.CopyN(io.Discard, br, 2) // Skip CRC

		foundMsgs[abbrev] = true

		// Format JSON
		out := messageToMap(msg, proto, sysNames, entNames, true)

		jsonData, _ := json.Marshal(out)
		fmt.Println(string(jsonData))

		// If we've found everything we were looking for, we can stop
		if len(msgsToExport) > 0 && len(foundMsgs) == len(msgsToExport) {
			break
		}
	}
}

func messageToMap(msg *imc.Message, proto *imc.Protocol, sysNames map[uint16]string, entNames map[uint8]string, isTop bool) map[string]any {
	out := make(map[string]any)

	msgDef := proto.Messages[msg.Header.MGID]
	if msgDef != nil {
		out["abbrev"] = msgDef.Abbrev
	} else {
		out["mgid"] = msg.Header.MGID
	}

	if isTop {
		out["timestamp"] = msg.Header.Timestamp

		// Source resolution
		if name, ok := sysNames[msg.Header.Src]; ok {
			out["src"] = name
		} else {
			out["src"] = msg.Header.Src
		}

		if name, ok := entNames[msg.Header.SrcEnt]; ok {
			out["src_ent"] = name
		} else {
			out["src_ent"] = msg.Header.SrcEnt
		}

		// Destination resolution
		if name, ok := sysNames[msg.Header.Dst]; ok {
			out["dst"] = name
		} else {
			out["dst"] = msg.Header.Dst
		}

		if name, ok := entNames[msg.Header.DstEnt]; ok {
			out["dst_ent"] = name
		} else {
			out["dst_ent"] = msg.Header.DstEnt
		}
	}

	// Fields
	for field, val := range msg.Fields {
		out[field] = convertValue(val, proto, sysNames, entNames)
	}

	return out
}

func convertValue(val any, proto *imc.Protocol, sysNames map[uint16]string, entNames map[uint8]string) any {
	switch v := val.(type) {
	case *imc.Message:
		if v == nil {
			return nil
		}
		return messageToMap(v, proto, sysNames, entNames, false)
	case []*imc.Message:
		res := make([]any, len(v))
		for i, m := range v {
			if m == nil {
				res[i] = nil
				continue
			}
			res[i] = messageToMap(m, proto, sysNames, entNames, false)
		}
		return res
	case []byte:
		// Example shows byte slices as JSON strings or hex?
		// Go's default is base64. Let's keep it for now.
		return v
	default:
		return v
	}
}

func buildMaps(path string, proto *imc.Protocol) (map[uint16]string, map[uint8]string) {
	f, totalSize, err := openFile(path)
	if err != nil {
		return nil, nil
	}
	defer f.Close()

	cr := &CountingReader{r: f}
	br := bufio.NewReaderSize(cr, 64*1024)

	sysNames := make(map[uint16]string)
	entNames := make(map[uint8]string)

	count := 0
	for {
		header, err := proto.UnmarshalHeader(br)
		if err != nil {
			break
		}
		count++

		if header.MGID == 3 { // EntityInfo
			msg, err := proto.UnmarshalFields(br, header.MGID)
			if err == nil {
				id := uint8(imc.ToInt64(msg.Fields["id"]))
				label := imc.ToString(msg.Fields["label"])
				entNames[id] = label
			}
			io.CopyN(io.Discard, br, 2) // Skip CRC
		} else if header.MGID == 151 { // Announce
			msg, err := proto.UnmarshalFields(br, header.MGID)
			if err == nil {
				name := imc.ToString(msg.Fields["sys_name"])
				sysNames[header.Src] = name
			}
			io.CopyN(io.Discard, br, 2) // Skip CRC
		} else {
			io.CopyN(io.Discard, br, int64(header.Size)+2)
		}

		if count%20000 == 0 {
			pct := float64(cr.Count()) / float64(totalSize) * 100
			fmt.Fprintf(os.Stderr, "\rScanning... %.1f%%", pct)
		}
	}
	fmt.Fprintf(os.Stderr, "\rScanning... 100.0%%\n")
	return sysNames, entNames
}

func openFile(path string) (io.ReadCloser, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}

	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	size := fi.Size()

	if strings.HasSuffix(path, ".gz") {
		gr, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return nil, 0, err
		}
		return &gzipReadCloser{Reader: gr, f: f}, size, nil
	}

	return f, size, nil
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

type CountingReader struct {
	r     io.Reader
	count int64
}

func (cr *CountingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.count += int64(n)
	return n, err
}

func (cr *CountingReader) Count() int64 {
	return cr.count
}

func parseMap(s string) map[string]bool {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	m := make(map[string]bool)
	for _, part := range parts {
		m[strings.TrimSpace(part)] = true
	}
	return m
}
