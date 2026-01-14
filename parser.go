package imc

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
)

type XMLProtocol struct {
	XMLName   xml.Name          `xml:"messages"`
	Name      string            `xml:"name,attr"`
	Version   string            `xml:"version,attr"`
	Types     []XMLType         `xml:"types>type"`
	Enums     []XMLEnum         `xml:"enumerations>def"`
	Bitfields []XMLBitfield     `xml:"bitfields>def"`
	Groups    []XMLMessageGroup `xml:"message-groups>message-group"`
	Header    []XMLField        `xml:"header>field"`
	Footer    []XMLField        `xml:"footer>field"`
	Messages  []XMLMessage      `xml:"message"`
}

type XMLType struct {
	Name string `xml:"name,attr"`
	Size int    `xml:"size,attr"`
}

type XMLEnum struct {
	Name   string     `xml:"name,attr"`
	Abbrev string     `xml:"abbrev,attr"`
	Prefix string     `xml:"prefix,attr"`
	Values []XMLValue `xml:"value"`
}

type XMLBitfield struct {
	Name   string     `xml:"name,attr"`
	Abbrev string     `xml:"abbrev,attr"`
	Prefix string     `xml:"prefix,attr"`
	Values []XMLValue `xml:"value"`
}

type XMLValue struct {
	ID     string `xml:"id,attr"`
	Name   string `xml:"name,attr"`
	Abbrev string `xml:"abbrev,attr"`
}

type XMLMessageGroup struct {
	Name     string           `xml:"name,attr"`
	Abbrev   string           `xml:"abbrev,attr"`
	MsgTypes []XMLMessageType `xml:"message-type"`
}

type XMLMessageType struct {
	Abbrev string `xml:"abbrev,attr"`
}

type XMLMessage struct {
	ID     uint16     `xml:"id,attr"`
	Name   string     `xml:"name,attr"`
	Abbrev string     `xml:"abbrev,attr"`
	Fields []XMLField `xml:"field"`
}

type XMLField struct {
	Name     string     `xml:"name,attr"`
	Abbrev   string     `xml:"abbrev,attr"`
	Type     string     `xml:"type,attr"`
	Unit     string     `xml:"unit,attr"`
	Value    string     `xml:"value,attr"`
	Fixed    bool       `xml:"fixed,attr"`
	EnumDef  string     `xml:"enum-def,attr"`
	Bitfield string     `xml:"bitfield-def,attr"` // Check if it's bitfield or bitfield-def in XML
	Values   []XMLValue `xml:"value"`             // Inline values for enums/bitfields
}

func ParseXML(path string) (*XMLProtocol, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	return ParseReader(file)
}

func ParseReader(r io.Reader) (*XMLProtocol, error) {
	var proto XMLProtocol
	decoder := xml.NewDecoder(r)
	if err := decoder.Decode(&proto); err != nil {
		return nil, fmt.Errorf("failed to decode IMC XML: %w", err)
	}
	return &proto, nil
}
