package main

import (
	"fmt"
	"io"
	"log"
	"os"

	"github.com/oceanscan/imc-go"
)

func main() {
	xmlPath := "data/test1/IMC.xml"
	dataPath := "data/test1/Data.lsf"

	fmt.Printf("Loading protocol from %s...\n", xmlPath)
	xmlProto, err := imc.ParseXML(xmlPath)
	if err != nil {
		log.Fatalf("Failed to parse IMC.xml: %v", err)
	}
	proto := imc.NewProtocol(xmlProto)

	fmt.Printf("Opening data file %s...\n", dataPath)
	f, err := os.Open(dataPath)
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
