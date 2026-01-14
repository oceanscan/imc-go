package main

import (
	"bufio"
	"compress/gzip"
	"encoding/csv"
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
	lsfPath = flag.String("lsf", "", "Path to Data.lsf (required)")
	xmlPath = flag.String("xml", "IMC.xml", "Path to IMC.xml")
	outPath = flag.String("out", "positions.csv", "Output CSV file path")
)

const (
	// WGS-84 semi-major axis (equatorial radius) in meters
	earthRadius = 6378137.0
)

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

func main() {
	flag.Parse()

	if *lsfPath == "" {
		fmt.Println("Error: -lsf is required")
		flag.Usage()
		os.Exit(1)
	}

	xmlFile := *xmlPath
	if xmlFile == "IMC.xml" {
		// If default, check if it exists in the current directory
		if _, err := os.Stat(xmlFile); os.IsNotExist(err) {
			// Try same directory as LSF
			alt := filepath.Join(filepath.Dir(*lsfPath), "IMC.xml")
			if _, err := os.Stat(alt); err == nil {
				xmlFile = alt
			} else {
				// Try .gz version
				altGz := alt + ".gz"
				if _, err := os.Stat(altGz); err == nil {
					xmlFile = altGz
				}
			}
		}
	}

	fmt.Printf("Loading protocol from %s...\n", xmlFile)
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

	// Open LSF
	f, totalSize, err := openFile(*lsfPath)
	if err != nil {
		log.Fatalf("Failed to open LSF: %v", err)
	}
	defer f.Close()

	cr := &CountingReader{r: f}
	br := bufio.NewReaderSize(cr, 64*1024) // 64KB buffer

	// Create output CSV file
	outFile, err := os.Create(*outPath)
	if err != nil {
		log.Fatalf("Failed to create output file: %v", err)
	}
	defer outFile.Close()

	bw := bufio.NewWriter(outFile)
	csvWriter := csv.NewWriter(bw)
	defer func() {
		csvWriter.Flush()
		if err := csvWriter.Error(); err != nil {
			log.Fatalf("CSV writer error: %v", err)
		}
		if err := bw.Flush(); err != nil {
			log.Fatalf("Buffer writer error: %v", err)
		}
	}()

	// Write CSV header
	if err := csvWriter.Write([]string{"timestamp", "latitude_deg", "longitude_deg", "depth_m", "altitude_m"}); err != nil {
		log.Fatalf("Failed to write CSV header: %v", err)
	}

	fmt.Println("Processing EstimatedState messages...")
	count := 0
	exportedCount := 0
	lastExportedTime := -1.0

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

		// Only process EstimatedState messages (ID 350)
		if header.MGID != 350 {
			io.CopyN(io.Discard, br, int64(header.Size)+2)
			continue
		}

		// Check if we should export this message (1 second separation)
		if lastExportedTime >= 0 && header.Timestamp-lastExportedTime < 1.0 {
			io.CopyN(io.Discard, br, int64(header.Size)+2)
			continue
		}

		// Decode Payload
		msg, err := proto.UnmarshalFields(br, header.MGID)
		if err != nil {
			fmt.Printf("\nError decoding EstimatedState at msg %d: %v\n", count, err)
			break
		}
		msg.Header = *header
		// Skip CRC
		io.CopyN(io.Discard, br, 2)

		// Extract fields
		latRad := imc.ToFloat64(msg.Fields["lat"])
		lonRad := imc.ToFloat64(msg.Fields["lon"])
		x := imc.ToFloat64(msg.Fields["x"])      // offset north (meters)
		y := imc.ToFloat64(msg.Fields["y"])      // offset east (meters)
		depth := imc.ToFloat64(msg.Fields["depth"])
		alt := imc.ToFloat64(msg.Fields["alt"])

		// Apply WGS-84 offset translation
		// x = north offset, y = east offset
		// Convert offsets to lat/lon changes
		finalLat, finalLon := translateWGS84(latRad, lonRad, x, y)

		// Convert final position to degrees
		finalLatDeg := finalLat * 180.0 / math.Pi
		finalLonDeg := finalLon * 180.0 / math.Pi

		// Write row
		row := []string{
			fmt.Sprintf("%.3f", header.Timestamp),
			fmt.Sprintf("%.8f", finalLatDeg),
			fmt.Sprintf("%.8f", finalLonDeg),
			fmt.Sprintf("%.2f", depth),
			fmt.Sprintf("%.2f", alt),
		}
		if err := csvWriter.Write(row); err != nil {
			log.Fatalf("Failed to write CSV row: %v", err)
		}
		exportedCount++
		lastExportedTime = header.Timestamp

		if count%10000 == 0 {
			pct := float64(cr.Count()) / float64(totalSize) * 100
			fmt.Printf("\rProcessed %d messages, exported %d positions (%.1f%%)...", count, exportedCount, pct)
		}
	}

	// Final flush and error check handled by defer
	fmt.Printf("\rProcessed %d messages, exported %d positions (100.0%%)   \n", count, exportedCount)
	fmt.Printf("Done. Positions exported to %s\n", *outPath)
}

// translateWGS84 translates a WGS-84 lat/lon position by north/east offsets in meters
// Returns the new latitude and longitude in radians
func translateWGS84(latRad, lonRad, northMeters, eastMeters float64) (float64, float64) {
	// Calculate the change in latitude (north offset)
	// Using small angle approximation: deltaLat (radians) = distance / radius
	deltaLat := northMeters / earthRadius

	// Calculate the change in longitude (east offset)
	// Longitude distance varies with latitude due to Earth's spherical shape
	deltaLon := eastMeters / (earthRadius * math.Cos(latRad))

	newLat := latRad + deltaLat
	newLon := lonRad + deltaLon

	return newLat, newLon
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
