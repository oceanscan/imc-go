package imc

import (
	"crypto/md5"
	"encoding/hex"
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
	// MD5 of the XML document as read (hexadecimal, lower case).
	MD5 string `xml:"-"`
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
	Bitfield string     `xml:"bitfield-def,attr"`
	MsgType  string     `xml:"message-type,attr"`
	Values   []XMLValue `xml:"value"` // Inline values for enums/bitfields
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
	h := md5.New()
	decoder := xml.NewDecoder(io.TeeReader(r, h))
	if err := decoder.Decode(&proto); err != nil {
		return nil, fmt.Errorf("failed to decode IMC XML: %w", err)
	}
	// Hash what follows the root element too (the MD5 is of the file).
	io.Copy(h, r)
	proto.MD5 = hex.EncodeToString(h.Sum(nil))
	return &proto, nil
}
