package main

import (
	"bufio"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/oceanscan/imc-go"
)

var (
	lsfPath    = flag.String("lsf", "", "Path to Data.lsf (required)")
	xmlPath    = flag.String("xml", "IMC.xml", "Path to IMC.xml")
	msgFilter  = flag.String("msg", "", "Comma-separated message types to export")
	entFilter  = flag.String("entity", "", "Comma-separated entity names to filter by")
	srcFilter  = flag.String("src", "", "Comma-separated source IDs to filter by")
	startTime  = flag.String("start", "", "Start timestamp (Unix epoch or RFC3339)")
	endTime    = flag.String("end", "", "End timestamp")
	fieldsFlag = flag.String("fields", "", "Specific fields to export (comma-separated). If empty, exports all.")
	outDir     = flag.String("out", ".", "Output directory for CSV files")
)

type CsvExport struct {
	File   *os.File
	Writer *csv.Writer
	Fields []string
}

// CountingReader tracks the number of bytes read from an io.Reader.
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

func main() {
	flag.Parse()

	if *lsfPath == "" {
		fmt.Println("Error: -lsf is required")
		flag.Usage()
		os.Exit(1)
	}

	fmt.Printf("Loading protocol from %s...\n", *xmlPath)
	xmlProto, err := imc.ParseXML(*xmlPath)
	if err != nil {
		log.Fatalf("Failed to parse IMC.xml: %v", err)
	}
	proto := imc.NewProtocol(xmlProto)

	// Pre-scan for entities
	fmt.Println("Scanning for entities...")
	entityMap := buildEntityMap(*lsfPath, proto)
	fmt.Printf("Found %d entities\n", len(entityMap))

	// Parse filters into maps for O(1) lookup
	msgsToExport := parseMap(*msgFilter)
	entsToFilter := parseMap(*entFilter)
	srcsToFilter := parseMap(*srcFilter)
	fieldFilter := parseList(*fieldsFlag)

	start, _ := parseTime(*startTime)
	end, _ := parseTime(*endTime)

	// Open LSF
	f, err := os.Open(*lsfPath)
	if err != nil {
		log.Fatalf("Failed to open LSF: %v", err)
	}
	defer f.Close()

	fi, _ := f.Stat()
	totalSize := fi.Size()

	cr := &CountingReader{r: f}
	br := bufio.NewReaderSize(cr, 64*1024) // 64KB buffer

	if err := os.MkdirAll(*outDir, 0755); err != nil {
		log.Fatalf("Failed to create output directory: %v", err)
	}

	exports := make(map[string]*CsvExport)
	defer func() {
		for _, exp := range exports {
			exp.Writer.Flush()
			exp.File.Close()
		}
	}()

	fmt.Println("Processing messages...")
	count := 0
	exportedCount := 0

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

		msgDef := proto.Messages[header.MGID]
		if msgDef == nil {
			// Skip unknown message payload + CRC
			io.CopyN(io.Discard, br, int64(header.Size)+2)
			continue
		}

		abbrev := msgDef.Abbrev
		filterOut := false

		// Fast Filtering
		if len(msgsToExport) > 0 && !msgsToExport[abbrev] {
			filterOut = true
		} else if len(entsToFilter) > 0 && !entsToFilter[entityMap[uint8(header.SrcEnt)]] {
			filterOut = true
		} else if len(srcsToFilter) > 0 && !srcsToFilter[strconv.Itoa(int(header.Src))] {
			filterOut = true
		} else if (start > 0 && header.Timestamp < start) || (end > 0 && header.Timestamp > end) {
			filterOut = true
		}

		if filterOut {
			io.CopyN(io.Discard, br, int64(header.Size)+2)
		} else {
			// Decode Payload
			msg, err := proto.UnmarshalFields(br, header.MGID)
			if err != nil {
				fmt.Printf("\nError decoding payload for %s at msg %d: %v\n", abbrev, count, err)
				break
			}
			msg.Header = *header
			// Skip CRC
			io.CopyN(io.Discard, br, 2)

			// Get or create CSV export
			exp, ok := exports[abbrev]
			if !ok {
				exp, err = createExport(abbrev, msgDef, fieldFilter, proto)
				if err != nil {
					log.Fatalf("Failed to create export for %s: %v", abbrev, err)
				}
				exports[abbrev] = exp
			}

			// Write row
			row := make([]string, len(exp.Fields))
			for i, fName := range exp.Fields {
				val := getFieldValue(msg, fName, proto)
				row[i] = formatValue(val)
			}
			exp.Writer.Write(row)
			exportedCount++
		}

		if count%10000 == 0 {
			pct := float64(cr.Count()) / float64(totalSize) * 100
			fmt.Printf("\rProcessed %d messages, exported %d (%.1f%%)...", count, exportedCount, pct)
		}
	}

	fmt.Printf("\rProcessed %d messages, exported %d (100.0%%)   \n", count, exportedCount)
	fmt.Printf("Done. Exported to %d files.\n", len(exports))
}

func buildEntityMap(path string, proto *imc.Protocol) map[uint8]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	fi, _ := f.Stat()
	totalSize := fi.Size()

	cr := &CountingReader{r: f}
	br := bufio.NewReaderSize(cr, 64*1024)

	entities := make(map[uint8]string)
	count := 0
	for {
		header, err := proto.UnmarshalHeader(br)
		if err != nil {
			if err == io.EOF {
				break
			}
			break
		}
		count++

		if header.MGID == 3 { // EntityInfo
			msg, err := proto.UnmarshalFields(br, header.MGID)
			if err != nil {
				break
			}
			id := uint8(imc.ToInt64(msg.Fields["id"]))
			label := imc.ToString(msg.Fields["label"])
			entities[id] = label
			// Skip CRC
			io.CopyN(io.Discard, br, 2)
		} else {
			// Skip payload and CRC
			io.CopyN(io.Discard, br, int64(header.Size)+2)
		}

		if count%20000 == 0 {
			pct := float64(cr.Count()) / float64(totalSize) * 100
			fmt.Printf("\rScanning... %.1f%%", pct)
		}
	}
	fmt.Printf("\rScanning... 100.0%%\n")
	return entities
}

func createExport(abbrev string, msgDef *imc.XMLMessage, fieldFilter []string, proto *imc.Protocol) (*CsvExport, error) {
	path := filepath.Join(*outDir, abbrev+".csv")
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}

	// Buffered writer for CSV
	bw := bufio.NewWriterSize(f, 64*1024)
	w := csv.NewWriter(bw)

	var fields []string
	if len(fieldFilter) > 0 {
		fields = fieldFilter
	} else {
		fields = []string{"timestamp", "src", "src_ent", "dst", "dst_ent"}
		for _, fd := range msgDef.Fields {
			fields = append(fields, fd.Abbrev)
		}
	}

	w.Write(fields)

	return &CsvExport{
		File:   f,
		Writer: w,
		Fields: fields,
	}, nil
}

func getFieldValue(msg *imc.Message, fieldName string, proto *imc.Protocol) any {
	switch fieldName {
	case "timestamp":
		return msg.Header.Timestamp
	case "src":
		return msg.Header.Src
	case "src_ent":
		return msg.Header.SrcEnt
	case "dst":
		return msg.Header.Dst
	case "dst_ent":
		return msg.Header.DstEnt
	default:
		return msg.Fields[fieldName]
	}
}

func formatValue(val any) string {
	if val == nil {
		return ""
	}

	switch v := val.(type) {
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32)
	case int, int8, int16, int32, int64:
		return fmt.Sprintf("%d", v)
	case uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%d", v)
	case string:
		return v
	case []byte:
		return fmt.Sprintf("%x", v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func parseList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func parseMap(s string) map[string]bool {
	list := parseList(s)
	if list == nil {
		return nil
	}
	m := make(map[string]bool)
	for _, item := range list {
		m[item] = true
	}
	return m
}

func parseTime(s string) (float64, error) {
	if s == "" {
		return 0, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err == nil {
		return float64(t.UnixNano()) / 1e9, nil
	}
	return 0, err
}

func contains(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}
