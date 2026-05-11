package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/oceanscan/imc-go"
)

type gzReadCloser struct {
	*gzip.Reader
	f *os.File
}

func (g *gzReadCloser) Close() error {
	err := g.Reader.Close()
	if cerr := g.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// openMaybeGz opens a file, transparently decompressing it if the path ends in .gz.
func openMaybeGz(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(path, ".gz") {
		return f, nil
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &gzReadCloser{Reader: gz, f: f}, nil
}

func main() {
	if len(os.Args) != 3 {
		log.Fatalf("usage: %s <IMC.xml[.gz]> <Data.lsf[.gz]>", os.Args[0])
	}
	xmlPath, dataPath := os.Args[1], os.Args[2]

	fmt.Printf("Loading protocol from %s...\n", xmlPath)
	xmlReader, err := openMaybeGz(xmlPath)
	if err != nil {
		log.Fatalf("Failed to open IMC.xml: %v", err)
	}
	xmlProto, err := imc.ParseReader(xmlReader)
	xmlReader.Close()
	if err != nil {
		log.Fatalf("Failed to parse IMC.xml: %v", err)
	}
	proto := imc.NewProtocol(xmlProto)

	fmt.Printf("Opening data file %s...\n", dataPath)
	f, err := openMaybeGz(dataPath)
	if err != nil {
		log.Fatalf("Failed to open data file: %v", err)
	}
	defer f.Close()

	count := 0
	stats := make(map[string]int)

	for {
		msg, err := proto.UnmarshalReader(f)
		if err != nil {
			if err == io.EOF {
				break
			}
			// Some LSF files might have gaps or sync issues, but for now we stop on error
			fmt.Printf("\nError at message %d: %v\n", count, err)
			break
		}

		count++
		abbrev := proto.Messages[msg.Header.MGID].Abbrev
		stats[abbrev]++

		if count%1000 == 0 {
			fmt.Printf("\rProcessed %d messages...", count)
		}

		// Optional: Print first 10 messages
		if count <= 10 {
			fmt.Printf("\nMessage %d: %s (Size: %d, Src: %d)", count, abbrev, msg.Header.Size, msg.Header.Src)
		}
	}

	fmt.Printf("\n\nParsing complete. Total messages: %d\n", count)
	fmt.Println("Message Statistics:")
	for abbrev, c := range stats {
		fmt.Printf("  %-20s: %d\n", abbrev, c)
	}
}
