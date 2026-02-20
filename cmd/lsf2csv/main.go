package main

import (
	"bufio"
	"compress/gzip"
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
	lsfPath    = flag.String("lsf", "", "Path to Data.lsf or directory when using -R (required)")
	xmlPath    = flag.String("xml", "IMC.xml", "Path to IMC.xml (auto-detected per directory in recursive mode)")
	recursive  = flag.Bool("R", false, "Recursively find and convert all Data.lsf files under -lsf directory")
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

	msgsToExport := parseMap(*msgFilter)
	entsToFilter := parseMap(*entFilter)
	srcsToFilter := parseMap(*srcFilter)
	fieldFilter := parseList(*fieldsFlag)
	start, _ := parseTime(*startTime)
	end, _ := parseTime(*endTime)

	if *recursive {
		root := *lsfPath
		var lsfFiles []string
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() && strings.EqualFold(info.Name(), "Data.lsf") {
				lsfFiles = append(lsfFiles, path)
			}
			return nil
		})
		if err != nil {
			log.Fatalf("Error walking directory: %v", err)
		}
		if len(lsfFiles) == 0 {
			log.Fatalf("No Data.lsf files found under %s", root)
		}
		fmt.Printf("Found %d LSF file(s) to convert.\n", len(lsfFiles))
		for i, lsf := range lsfFiles {
			fmt.Printf("\n[%d/%d] %s\n", i+1, len(lsfFiles), lsf)
			rel, _ := filepath.Rel(root, filepath.Dir(lsf))
			dest := filepath.Join(*outDir, rel)
			xmlFile := resolveXML(filepath.Dir(lsf), *xmlPath)
			if err := convertLSF(lsf, xmlFile, dest, msgsToExport, entsToFilter, srcsToFilter, fieldFilter, start, end); err != nil {
				log.Printf("Error processing %s: %v", lsf, err)
			}
		}
	} else {
		xmlFile := resolveXML(filepath.Dir(*lsfPath), *xmlPath)
		if err := convertLSF(*lsfPath, xmlFile, *outDir, msgsToExport, entsToFilter, srcsToFilter, fieldFilter, start, end); err != nil {
			log.Fatalf("%v", err)
		}
	}
}

func resolveXML(lsfDir, xmlFlag string) string {
	// If the user gave an explicit non-default path, use it as-is.
	if xmlFlag != "IMC.xml" {
		return xmlFlag
	}
	// Try current working directory first.
	if _, err := os.Stat(xmlFlag); err == nil {
		return xmlFlag
	}
	// Try same directory as the LSF file.
	alt := filepath.Join(lsfDir, "IMC.xml")
	if _, err := os.Stat(alt); err == nil {
		return alt
	}
	// Try gzip version next to the LSF file.
	if _, err := os.Stat(alt + ".gz"); err == nil {
		return alt + ".gz"
	}
	return xmlFlag
}

func convertLSF(lsfFilePath, xmlFile, outputDir string,
	msgsToExport, entsToFilter, srcsToFilter map[string]bool,
	fieldFilter []string, start, end float64) error {

	fmt.Printf("Loading protocol from %s...\n", xmlFile)
	xmlR, _, err := openFile(xmlFile)
	if err != nil {
		return fmt.Errorf("failed to open XML: %w", err)
	}
	xmlProto, err := imc.ParseReader(xmlR)
	xmlR.Close()
	if err != nil {
		return fmt.Errorf("failed to parse IMC XML: %w", err)
	}
	proto := imc.NewProtocol(xmlProto)

	fmt.Println("Scanning for entities...")
	entityMap := buildEntityMap(lsfFilePath, proto)
	fmt.Printf("Found %d entities\n", len(entityMap))

	f, totalSize, err := openFile(lsfFilePath)
	if err != nil {
		return fmt.Errorf("failed to open LSF: %w", err)
	}
	defer f.Close()

	cr := &CountingReader{r: f}
	br := bufio.NewReaderSize(cr, 64*1024)

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
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
			io.CopyN(io.Discard, br, int64(header.Size)+2)
			continue
		}

		abbrev := msgDef.Abbrev
		filterOut := false

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
			msg, err := proto.UnmarshalFields(br, header.MGID)
			if err != nil {
				fmt.Printf("\nError decoding payload for %s at msg %d: %v\n", abbrev, count, err)
				break
			}
			msg.Header = *header
			io.CopyN(io.Discard, br, 2)

			exp, ok := exports[abbrev]
			if !ok {
				exp, err = createExport(abbrev, msgDef, fieldFilter, outputDir, proto)
				if err != nil {
					return fmt.Errorf("failed to create export for %s: %w", abbrev, err)
				}
				exports[abbrev] = exp
			}

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
	fmt.Printf("Done. Exported to %d files in %s\n", len(exports), outputDir)
	return nil
}

func buildEntityMap(path string, proto *imc.Protocol) map[uint8]string {
	f, totalSize, err := openFile(path)
	if err != nil {
		return nil
	}
	defer f.Close()

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
		// Wrap to ensure f is also closed when gr is closed
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

func createExport(abbrev string, msgDef *imc.XMLMessage, fieldFilter []string, outputDir string, proto *imc.Protocol) (*CsvExport, error) {
	path := filepath.Join(outputDir, abbrev+".csv")
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
