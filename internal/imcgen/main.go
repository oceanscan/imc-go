// Command imcgen generates messages.go (typed IMC messages with direct
// encoders and decoders) from an IMC XML specification.
//
//	go run ./internal/imcgen -xml IMC.xml -out messages.go
//
// It uses only the standard library, so it runs even when the generated
// file of the package does not compile. The MD5 of the XML file (as read,
// byte for byte) is stamped into the output as IMCMD5: DUNE compares it
// with its own (DUNE_IMC_CONST_MD5, the MD5 of the original XML DUNE was
// generated from) when a peer connects. Pass -md5 to refuse an XML whose
// MD5 is not the expected one.
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"flag"
	"fmt"
	"go/format"
	"io"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
)

type xmlSpec struct {
	Name      string     `xml:"name,attr"`
	Version   string     `xml:"version,attr"`
	Enums     []xmlDef   `xml:"enumerations>def"`
	Bitfields []xmlDef   `xml:"bitfields>def"`
	Groups    []xmlGroup `xml:"message-groups>message-group"`
	Header    []xmlField `xml:"header>field"`
	Messages  []xmlMsg   `xml:"message"`
}

type xmlDef struct {
	Name   string     `xml:"name,attr"`
	Abbrev string     `xml:"abbrev,attr"`
	Prefix string     `xml:"prefix,attr"`
	Values []xmlValue `xml:"value"`
}

type xmlValue struct {
	ID     string `xml:"id,attr"`
	Name   string `xml:"name,attr"`
	Abbrev string `xml:"abbrev,attr"`
}

type xmlGroup struct {
	Name   string `xml:"name,attr"`
	Abbrev string `xml:"abbrev,attr"`
	Types  []struct {
		Abbrev string `xml:"abbrev,attr"`
	} `xml:"message-type"`
}

type xmlMsg struct {
	ID     int        `xml:"id,attr"`
	Name   string     `xml:"name,attr"`
	Abbrev string     `xml:"abbrev,attr"`
	Fields []xmlField `xml:"field"`
}

type xmlField struct {
	Name        string     `xml:"name,attr"`
	Abbrev      string     `xml:"abbrev,attr"`
	Type        string     `xml:"type,attr"`
	Unit        string     `xml:"unit,attr"`
	Value       string     `xml:"value,attr"`
	EnumDef     string     `xml:"enum-def,attr"`
	BitfieldDef string     `xml:"bitfield-def,attr"`
	MessageType string     `xml:"message-type,attr"`
	Values      []xmlValue `xml:"value"`
}

// Primitive IMC types: Go type, size, and the read/append helpers.
type prim struct {
	goType string
	size   int
}

var prims = map[string]prim{
	"int8_t":   {"int8", 1},
	"uint8_t":  {"uint8", 1},
	"int16_t":  {"int16", 2},
	"uint16_t": {"uint16", 2},
	"int32_t":  {"int32", 4},
	"uint32_t": {"uint32", 4},
	"int64_t":  {"int64", 8},
	"fp32_t":   {"float32", 4},
	"fp64_t":   {"float64", 8},
}

// Identifiers of the hand-written part of package imc that generated names
// must not take.
var reserved = map[string]bool{
	"Message": true, "Header": true, "Protocol": true, "Frame": true, "FrameReader": true,
	"IMessage": true, "XMLProtocol": true, "XMLMessage": true, "XMLField": true, "Transporter": true,
	"Marshal": true, "Decode": true, "NewMessage": true, "Options": true, "Error": true,
}

// Method names of generated messages (fields must not take them).
var methods = map[string]bool{
	"Header": true, "GetHeader": true, "GetMGID": true, "MGID": true, "Abbrev": true, "ToMessage": true,
	"FromMessage": true, "AppendPayload": true, "DecodePayload": true, "String": true,
}

type enumType struct {
	GoName   string // Go type name
	Base     string // underlying Go type
	Bitfield bool
	Values   []enumValue
	Doc      string
	Global   bool
}

type enumValue struct {
	Const  string
	Value  uint64
	Abbrev string
	Name   string
}

type field struct {
	Abbrev   string
	GoName   string
	IMCType  string
	GoType   string // type in the struct
	Base     string // underlying primitive Go type (fixed fields)
	Size     int    // 0 for variable size
	Enum     *enumType
	Concrete string // concrete message type of inline / list fields ("" = any)
	Name     string
	Unit     string
}

type message struct {
	ID     int
	Abbrev string
	Name   string
	Fields []*field
}

type gen struct {
	spec     *xmlSpec
	md5      string
	sync     uint64
	enums    []*enumType
	global   map[string]*enumType // enum-def / bitfield-def abbrev -> type
	msgs     []*message
	byAbbrev map[string]*xmlMsg
	names    map[string]string // Go identifier -> what took it
}

func main() {
	xmlPath := flag.String("xml", "IMC.xml", "IMC XML specification (or .xml.gz)")
	out := flag.String("out", "messages.go", "output file")
	pkg := flag.String("pkg", "imc", "package name")
	want := flag.String("md5", "", "refuse an XML whose MD5 differs from this one")
	flag.Parse()

	raw, err := os.ReadFile(*xmlPath)
	if err != nil {
		log.Fatal(err)
	}
	if strings.HasSuffix(*xmlPath, ".gz") {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			log.Fatal(err)
		}
		if raw, err = io.ReadAll(zr); err != nil {
			log.Fatal(err)
		}
	}
	sum := md5.Sum(raw)
	g := &gen{md5: hex.EncodeToString(sum[:]), global: map[string]*enumType{}, names: map[string]string{}}
	if *want != "" && !strings.EqualFold(*want, g.md5) {
		log.Fatalf("%s has MD5 %s, expected %s", *xmlPath, g.md5, *want)
	}
	var spec xmlSpec
	if err := xml.Unmarshal(raw, &spec); err != nil {
		log.Fatal(err)
	}
	g.spec = &spec
	g.build()
	src := g.emit(*pkg)
	formatted, err := format.Source(src)
	if err != nil {
		os.WriteFile(*out+".broken", src, 0o644)
		log.Fatalf("gofmt: %v (unformatted output in %s.broken)", err, *out)
	}
	if err := os.WriteFile(*out, formatted, 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s: IMC %s, MD5 %s, %d messages, %d enumerations/bitfields\n",
		*out, spec.Version, g.md5, len(g.msgs), len(g.enums))
}

func (g *gen) take(name, what string) {
	if reserved[name] {
		log.Fatalf("%s: Go name %s is reserved", what, name)
	}
	if other, ok := g.names[name]; ok {
		log.Fatalf("%s: Go name %s already taken by %s", what, name, other)
	}
	g.names[name] = what
}

// camel turns an IMC abbreviation into a Go name: sys_name -> SysName.
func camel(abbrev string) string {
	parts := strings.Split(abbrev, "_")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

func parseID(s string) uint64 {
	s = strings.TrimSpace(s)
	v, err := strconv.ParseUint(s, 0, 64)
	if err != nil {
		// Negative enumeration values (signed fields).
		n, err2 := strconv.ParseInt(s, 0, 64)
		if err2 != nil {
			log.Fatalf("bad value id %q: %v", s, err)
		}
		return uint64(n)
	}
	return v
}

func (g *gen) newEnum(goName, base string, bitfield, global bool, values []xmlValue, doc string) *enumType {
	g.take(goName, doc)
	e := &enumType{GoName: goName, Base: base, Bitfield: bitfield, Global: global, Doc: doc}
	seen := map[string]bool{}
	for _, v := range values {
		c := goName + "_" + strings.ToUpper(v.Abbrev)
		if seen[c] {
			log.Fatalf("%s: duplicate value %s", doc, v.Abbrev)
		}
		seen[c] = true
		g.take(c, doc)
		e.Values = append(e.Values, enumValue{Const: c, Value: parseID(v.ID), Abbrev: v.Abbrev, Name: v.Name})
	}
	g.enums = append(g.enums, e)
	return e
}

func (g *gen) build() {
	for _, h := range g.spec.Header {
		if h.Abbrev == "sync" {
			g.sync = parseID(h.Value)
		}
	}
	if g.sync == 0 {
		g.sync = 0xFE55
	}
	g.byAbbrev = map[string]*xmlMsg{}
	for i := range g.spec.Messages {
		m := &g.spec.Messages[i]
		g.byAbbrev[m.Abbrev] = m
		g.take(m.Abbrev, "message "+m.Abbrev)
	}
	// Underlying type of each global definition: from the fields that use it.
	defBase := map[string]string{}
	for _, m := range g.spec.Messages {
		for _, f := range m.Fields {
			def := f.EnumDef
			if def == "" {
				def = f.BitfieldDef
			}
			if def == "" {
				continue
			}
			p, ok := prims[f.Type]
			if !ok {
				log.Fatalf("%s.%s: enumeration on non primitive type %s", m.Abbrev, f.Abbrev, f.Type)
			}
			if b, ok := defBase[def]; ok && b != p.goType {
				log.Fatalf("definition %s used with types %s and %s", def, b, p.goType)
			}
			defBase[def] = p.goType
		}
	}
	for _, d := range g.spec.Enums {
		base := defBase[d.Abbrev]
		if base == "" {
			base = "uint8"
		}
		g.global[d.Abbrev] = g.newEnum(d.Abbrev, base, false, true, d.Values, "enumeration "+d.Name)
	}
	for _, d := range g.spec.Bitfields {
		base := defBase[d.Abbrev]
		if base == "" {
			base = "uint32"
		}
		g.global[d.Abbrev] = g.newEnum(d.Abbrev, base, true, true, d.Values, "bitfield "+d.Name)
	}
	for _, xm := range g.spec.Messages {
		m := &message{ID: xm.ID, Abbrev: xm.Abbrev, Name: xm.Name}
		seen := map[string]bool{}
		for _, xf := range xm.Fields {
			f := &field{Abbrev: xf.Abbrev, GoName: camel(xf.Abbrev), IMCType: xf.Type, Name: xf.Name, Unit: xf.Unit}
			if methods[f.GoName] || seen[f.GoName] {
				log.Fatalf("%s.%s: field name %s clashes", xm.Abbrev, xf.Abbrev, f.GoName)
			}
			seen[f.GoName] = true
			if p, ok := prims[xf.Type]; ok {
				f.Base, f.GoType, f.Size = p.goType, p.goType, p.size
				switch {
				case xf.EnumDef != "":
					f.Enum = g.global[xf.EnumDef]
				case xf.BitfieldDef != "":
					f.Enum = g.global[xf.BitfieldDef]
				case len(xf.Values) > 0 && (xf.Unit == "Enumerated" || xf.Unit == "Bitfield"):
					f.Enum = g.newEnum(xm.Abbrev+f.GoName, p.goType, xf.Unit == "Bitfield", false, xf.Values,
						fmt.Sprintf("%s of %s", xf.Name, xm.Abbrev))
				}
				if (xf.EnumDef != "" || xf.BitfieldDef != "") && f.Enum == nil {
					log.Fatalf("%s.%s: unknown definition", xm.Abbrev, xf.Abbrev)
				}
				if f.Enum != nil {
					f.GoType = f.Enum.GoName
				}
			} else {
				switch xf.Type {
				case "plaintext":
					f.GoType = "string"
				case "rawdata":
					f.GoType = "[]byte"
				case "message", "message-list":
					if _, ok := g.byAbbrev[xf.MessageType]; ok {
						f.Concrete = xf.MessageType
					}
					elem := "IMessage"
					if f.Concrete != "" {
						elem = "*" + f.Concrete
					}
					if xf.Type == "message" {
						f.GoType = elem
					} else {
						f.GoType = "[]" + elem
					}
				default:
					log.Fatalf("%s.%s: unknown type %s", xm.Abbrev, xf.Abbrev, xf.Type)
				}
			}
			m.Fields = append(m.Fields, f)
		}
		g.msgs = append(g.msgs, m)
	}
	sort.Slice(g.msgs, func(i, j int) bool { return g.msgs[i].ID < g.msgs[j].ID })
}

// -- emission ---------------------------------------------------------------

type writer struct{ bytes.Buffer }

func (w *writer) p(format string, args ...any) {
	fmt.Fprintf(&w.Buffer, format, args...)
	w.WriteByte('\n')
}

func (g *gen) emit(pkg string) []byte {
	w := &writer{}
	w.p("// Code generated by internal/imcgen from IMC.xml (IMC %s, MD5 %s). DO NOT EDIT.", g.spec.Version, g.md5)
	w.p("")
	w.p("package %s", pkg)
	w.p("")
	w.p("import (")
	w.p("\t\"fmt\"")
	w.p("\t\"math\"")
	w.p("\t\"strings\"")
	w.p(")")
	w.p("")
	w.p("var _ = math.Float32bits")
	w.p("var _ = strings.Join")
	w.p("")
	w.p("const (")
	w.p("\t// IMCMD5 is the MD5 of the IMC XML the typed messages were generated from (DUNE's DUNE_IMC_CONST_MD5).")
	w.p("\tIMCMD5 = %q", g.md5)
	w.p("\t// IMCVersion is the version of that IMC specification.")
	w.p("\tIMCVersion = %q", g.spec.Version)
	w.p("\t// SyncNumber is the synchronization number of that specification (little endian frames start with its low byte).")
	w.p("\tSyncNumber uint16 = 0x%04X", g.sync)
	w.p(")")
	w.p("")
	for _, e := range g.enums {
		g.emitEnum(w, e)
	}
	for _, m := range g.msgs {
		g.emitMessage(w, m)
	}
	g.emitRegistry(w)
	return w.Bytes()
}

func (g *gen) emitEnum(w *writer, e *enumType) {
	kind := "an enumeration"
	if e.Bitfield {
		kind = "a bitfield"
	}
	w.p("// %s is %s (%s).", e.GoName, kind, e.Doc)
	w.p("type %s %s", e.GoName, e.Base)
	w.p("")
	if len(e.Values) > 0 {
		w.p("const (")
		for _, v := range e.Values {
			w.p("\t// %s: %s.", v.Const, v.Name)
			if e.Base[0] == 'i' && int64(v.Value) < 0 {
				w.p("\t%s %s = %d", v.Const, e.GoName, int64(v.Value))
			} else if e.Bitfield {
				w.p("\t%s %s = 0x%X", v.Const, e.GoName, v.Value)
			} else {
				w.p("\t%s %s = %d", v.Const, e.GoName, v.Value)
			}
		}
		w.p(")")
		w.p("")
	}
	if !e.Bitfield {
		w.p("// String returns the IMC abbreviation of the value (or its number).")
		w.p("func (v %s) String() string {", e.GoName)
		w.p("\tswitch v {")
		done := map[uint64]bool{}
		for _, v := range e.Values {
			if done[v.Value] {
				continue
			}
			done[v.Value] = true
			w.p("\tcase %s:", v.Const)
			w.p("\t\treturn %q", v.Abbrev)
		}
		w.p("\t}")
		w.p("\treturn fmt.Sprintf(\"%s(%%d)\", %s(v))", e.GoName, e.Base)
		w.p("}")
	} else {
		w.p("// String returns the IMC abbreviations of the bits set, separated by '|'.")
		w.p("func (v %s) String() string {", e.GoName)
		w.p("\tvar parts []string")
		w.p("\trest := v")
		for _, v := range e.Values {
			if v.Value == 0 || v.Value&(v.Value-1) != 0 {
				continue // not a single bit (NONE, ALL)
			}
			w.p("\tif v&%s != 0 {", v.Const)
			w.p("\t\tparts = append(parts, %q)", v.Abbrev)
			w.p("\t\trest &^= %s", v.Const)
			w.p("\t}")
		}
		w.p("\tif rest != 0 {")
		w.p("\t\tparts = append(parts, fmt.Sprintf(\"0x%%X\", %s(rest)))", e.Base)
		w.p("\t}")
		w.p("\treturn strings.Join(parts, \"|\")")
		w.p("}")
	}
	w.p("")
}

// fixed runs: consecutive fixed size fields, checked once.
func runs(fields []*field) [][]*field {
	var out [][]*field
	var cur []*field
	for _, f := range fields {
		if f.Size > 0 {
			cur = append(cur, f)
			continue
		}
		if cur != nil {
			out = append(out, cur)
			cur = nil
		}
		out = append(out, []*field{f})
	}
	if cur != nil {
		out = append(out, cur)
	}
	return out
}

func (g *gen) emitMessage(w *writer, m *message) {
	w.p("// %s is IMC message %d (%s).", m.Abbrev, m.ID, m.Name)
	w.p("type %s struct {", m.Abbrev)
	w.p("\tHeader Header")
	for _, f := range m.Fields {
		unit := ""
		if f.Unit != "" && f.Unit != "Enumerated" && f.Unit != "Bitfield" {
			unit = " (" + f.Unit + ")"
		}
		w.p("\t%s %s // %s%s", f.GoName, f.GoType, f.Name, unit)
	}
	w.p("}")
	w.p("")
	w.p("// ID%s is the message id of %s.", m.Abbrev, m.Abbrev)
	w.p("const ID%s uint16 = %d", m.Abbrev, m.ID)
	w.p("")
	r := "m *" + m.Abbrev
	w.p("func (*%s) GetMGID() uint16 { return %d }", m.Abbrev, m.ID)
	w.p("func (*%s) Abbrev() string { return %q }", m.Abbrev, m.Abbrev)
	w.p("func (%s) GetHeader() *Header { return &m.Header }", r)
	w.p("")

	// AppendPayload.
	w.p("// AppendPayload appends the serialized fields of the message to b.")
	w.p("func (%s) AppendPayload(b []byte, be bool) ([]byte, error) {", r)
	hasErr := false
	for _, run := range runs(m.Fields) {
		f := run[0]
		if f.Size > 0 {
			for _, f := range run {
				w.p("\tb = %s", appendFixed(f, "m."+f.GoName))
			}
			continue
		}
		hasErr = true
		switch f.IMCType {
		case "plaintext":
			w.p("\tif len(m.%s) > 0xFFFF {", f.GoName)
			w.p("\t\treturn b, errTooLong(%q, len(m.%s))", m.Abbrev+"."+f.Abbrev, f.GoName)
			w.p("\t}")
			w.p("\tb = appendU16(b, uint16(len(m.%s)), be)", f.GoName)
			w.p("\tb = append(b, m.%s...)", f.GoName)
		case "rawdata":
			w.p("\tif len(m.%s) > 0xFFFF {", f.GoName)
			w.p("\t\treturn b, errTooLong(%q, len(m.%s))", m.Abbrev+"."+f.Abbrev, f.GoName)
			w.p("\t}")
			w.p("\tb = appendU16(b, uint16(len(m.%s)), be)", f.GoName)
			w.p("\tb = append(b, m.%s...)", f.GoName)
		case "message":
			w.p("\t{")
			w.p("\t\tvar err error")
			if f.Concrete != "" {
				w.p("\t\tif b, err = m.%s.appendInline(b, be); err != nil {", f.GoName)
			} else {
				w.p("\t\tif b, err = appendInline(b, m.%s, be); err != nil {", f.GoName)
			}
			w.p("\t\t\treturn b, err")
			w.p("\t\t}")
			w.p("\t}")
		case "message-list":
			w.p("\tif len(m.%s) > 0xFFFF {", f.GoName)
			w.p("\t\treturn b, errTooLong(%q, len(m.%s))", m.Abbrev+"."+f.Abbrev, f.GoName)
			w.p("\t}")
			w.p("\tb = appendU16(b, uint16(len(m.%s)), be)", f.GoName)
			w.p("\tfor _, item := range m.%s {", f.GoName)
			w.p("\t\tvar err error")
			if f.Concrete != "" {
				w.p("\t\tif b, err = item.appendInline(b, be); err != nil {")
			} else {
				w.p("\t\tif b, err = appendInline(b, item, be); err != nil {")
			}
			w.p("\t\t\treturn b, err")
			w.p("\t\t}")
			w.p("\t}")
		}
	}
	_ = hasErr
	w.p("\treturn b, nil")
	w.p("}")
	w.p("")

	// appendInline (sealed interface method; nil pointer = null message).
	w.p("func (%s) appendInline(b []byte, be bool) ([]byte, error) {", r)
	w.p("\tif m == nil {")
	w.p("\t\treturn appendU16(b, NullID, be), nil")
	w.p("\t}")
	w.p("\treturn m.AppendPayload(appendU16(b, %d, be), be)", m.ID)
	w.p("}")
	w.p("")

	// DecodePayload.
	w.p("// DecodePayload decodes the fields of the message from b and returns the")
	w.p("// number of bytes used (bytes after the fields are not an error).")
	w.p("func (%s) DecodePayload(b []byte, be bool) (int, error) {", r)
	w.p("\toff := 0")
	for _, run := range runs(m.Fields) {
		f := run[0]
		if f.Size > 0 {
			size := 0
			for _, f := range run {
				size += f.Size
			}
			w.p("\tif len(b)-off < %d {", size)
			w.p("\t\treturn off, errShort(%q)", m.Abbrev+"."+f.Abbrev)
			w.p("\t}")
			for _, f := range run {
				w.p("\tm.%s = %s", f.GoName, readFixed(f))
				w.p("\toff += %d", f.Size)
			}
			continue
		}
		switch f.IMCType {
		case "plaintext", "rawdata":
			w.p("\t{")
			w.p("\t\tif len(b)-off < 2 {")
			w.p("\t\t\treturn off, errShort(%q)", m.Abbrev+"."+f.Abbrev)
			w.p("\t\t}")
			w.p("\t\tn := int(readU16(b[off:], be))")
			w.p("\t\toff += 2")
			w.p("\t\tif len(b)-off < n {")
			w.p("\t\t\treturn off, errShort(%q)", m.Abbrev+"."+f.Abbrev)
			w.p("\t\t}")
			if f.IMCType == "plaintext" {
				w.p("\t\tm.%s = string(b[off : off+n])", f.GoName)
			} else {
				w.p("\t\tm.%s = cloneBytes(b[off : off+n])", f.GoName)
			}
			w.p("\t\toff += n")
			w.p("\t}")
		case "message":
			w.p("\t{")
			if f.Concrete != "" {
				w.p("\t\tv, n, err := decodeInlineAs[%s](b[off:], be, %d)", f.Concrete, g.byAbbrev[f.Concrete].ID)
			} else {
				w.p("\t\tv, n, err := decodeInline(b[off:], be)")
			}
			w.p("\t\toff += n")
			w.p("\t\tif err != nil {")
			w.p("\t\t\treturn off, wrapField(%q, err)", m.Abbrev+"."+f.Abbrev)
			w.p("\t\t}")
			w.p("\t\tm.%s = v", f.GoName)
			w.p("\t}")
		case "message-list":
			w.p("\t{")
			w.p("\t\tif len(b)-off < 2 {")
			w.p("\t\t\treturn off, errShort(%q)", m.Abbrev+"."+f.Abbrev)
			w.p("\t\t}")
			w.p("\t\tcount := int(readU16(b[off:], be))")
			w.p("\t\toff += 2")
			w.p("\t\tvar list %s", f.GoType)
			w.p("\t\tif count > 0 {")
			w.p("\t\t\tlist = make(%s, 0, min(count, (len(b)-off)/2))", f.GoType)
			w.p("\t\t}")
			w.p("\t\tfor i := 0; i < count; i++ {")
			if f.Concrete != "" {
				w.p("\t\t\tv, n, err := decodeInlineAs[%s](b[off:], be, %d)", f.Concrete, g.byAbbrev[f.Concrete].ID)
			} else {
				w.p("\t\t\tv, n, err := decodeInline(b[off:], be)")
			}
			w.p("\t\t\toff += n")
			w.p("\t\t\tif err != nil {")
			w.p("\t\t\t\treturn off, wrapField(%q, err)", m.Abbrev+"."+f.Abbrev)
			w.p("\t\t\t}")
			w.p("\t\t\tlist = append(list, v)")
			w.p("\t\t}")
			w.p("\t\tm.%s = list", f.GoName)
			w.p("\t}")
		}
	}
	w.p("\treturn off, nil")
	w.p("}")
	w.p("")

	// ToMessage / FromMessage (bridge to the dynamic codec).
	w.p("// ToMessage converts the message to the dynamic representation.")
	w.p("func (%s) ToMessage() *Message {", r)
	w.p("\tif m == nil {")
	w.p("\t\treturn nil")
	w.p("\t}")
	w.p("\tres := &Message{Header: m.Header, Fields: make(map[string]any, %d)}", len(m.Fields))
	w.p("\tres.Header.MGID = %d", m.ID)
	for _, f := range m.Fields {
		switch {
		case f.Enum != nil:
			w.p("\tres.Fields[%q] = %s(m.%s)", f.Abbrev, f.Base, f.GoName)
		case f.IMCType == "message":
			w.p("\tres.Fields[%q] = toGeneric(m.%s)", f.Abbrev, f.GoName)
		case f.IMCType == "message-list":
			w.p("\t{")
			w.p("\t\tlist := make([]*Message, len(m.%s))", f.GoName)
			w.p("\t\tfor i, item := range m.%s {", f.GoName)
			w.p("\t\t\tlist[i] = toGeneric(item)")
			w.p("\t\t}")
			w.p("\t\tres.Fields[%q] = list", f.Abbrev)
			w.p("\t}")
		default:
			w.p("\tres.Fields[%q] = m.%s", f.Abbrev, f.GoName)
		}
	}
	w.p("\treturn res")
	w.p("}")
	w.p("")
	w.p("// FromMessage sets the message from the dynamic representation (missing")
	w.p("// fields keep their values).")
	w.p("func (%s) FromMessage(msg *Message) error {", r)
	w.p("\tif msg.Header.MGID != %d {", m.ID)
	w.p("\t\treturn fmt.Errorf(\"invalid MGID: expected %d, got %%d\", msg.Header.MGID)", m.ID)
	w.p("\t}")
	w.p("\tm.Header = msg.Header")
	for _, f := range m.Fields {
		w.p("\tif val, ok := msg.Fields[%q]; ok {", f.Abbrev)
		switch {
		case f.Enum != nil:
			w.p("\t\tm.%s = %s(%s(val))", f.GoName, f.GoType, helper(f.Base))
		case f.Size > 0:
			w.p("\t\tm.%s = %s(val)", f.GoName, helper(f.Base))
		case f.IMCType == "plaintext":
			w.p("\t\tm.%s = ToString(val)", f.GoName)
		case f.IMCType == "rawdata":
			w.p("\t\tm.%s = ToByteSlice(val)", f.GoName)
		case f.IMCType == "message":
			w.p("\t\tv, err := fromGeneric(ToGenericMessage(val))")
			w.p("\t\tif err != nil {")
			w.p("\t\t\treturn err")
			w.p("\t\t}")
			if f.Concrete != "" {
				w.p("\t\tm.%s, _ = v.(*%s)", f.GoName, f.Concrete)
			} else {
				w.p("\t\tm.%s = v", f.GoName)
			}
		case f.IMCType == "message-list":
			w.p("\t\tsrc := ToGenericMessageList(val)")
			w.p("\t\tm.%s = make(%s, len(src))", f.GoName, f.GoType)
			w.p("\t\tfor i, item := range src {")
			w.p("\t\t\tv, err := fromGeneric(item)")
			w.p("\t\t\tif err != nil {")
			w.p("\t\t\t\treturn err")
			w.p("\t\t\t}")
			if f.Concrete != "" {
				w.p("\t\t\tm.%s[i], _ = v.(*%s)", f.GoName, f.Concrete)
			} else {
				w.p("\t\t\tm.%s[i] = v", f.GoName)
			}
			w.p("\t\t}")
		}
		w.p("\t}")
	}
	w.p("\treturn nil")
	w.p("}")
	w.p("")
}

func helper(base string) string {
	return "To" + strings.ToUpper(base[:1]) + base[1:]
}

func appendFixed(f *field, v string) string {
	if f.Enum != nil {
		v = f.Base + "(" + v + ")"
	}
	switch f.Base {
	case "int8":
		return fmt.Sprintf("append(b, byte(%s))", v)
	case "uint8":
		return fmt.Sprintf("append(b, %s)", v)
	case "int16":
		return fmt.Sprintf("appendU16(b, uint16(%s), be)", v)
	case "uint16":
		return fmt.Sprintf("appendU16(b, %s, be)", v)
	case "int32":
		return fmt.Sprintf("appendU32(b, uint32(%s), be)", v)
	case "uint32":
		return fmt.Sprintf("appendU32(b, %s, be)", v)
	case "int64":
		return fmt.Sprintf("appendU64(b, uint64(%s), be)", v)
	case "float32":
		return fmt.Sprintf("appendU32(b, math.Float32bits(%s), be)", v)
	case "float64":
		return fmt.Sprintf("appendU64(b, math.Float64bits(%s), be)", v)
	}
	panic(f.Base)
}

func readFixed(f *field) string {
	var e string
	switch f.Base {
	case "int8":
		e = "int8(b[off])"
	case "uint8":
		e = "b[off]"
	case "int16":
		e = "int16(readU16(b[off:], be))"
	case "uint16":
		e = "readU16(b[off:], be)"
	case "int32":
		e = "int32(readU32(b[off:], be))"
	case "uint32":
		e = "readU32(b[off:], be)"
	case "int64":
		e = "int64(readU64(b[off:], be))"
	case "float32":
		e = "math.Float32frombits(readU32(b[off:], be))"
	case "float64":
		e = "math.Float64frombits(readU64(b[off:], be))"
	}
	if f.Enum != nil {
		return f.GoType + "(" + e + ")"
	}
	return e
}

func (g *gen) emitRegistry(w *writer) {
	w.p("// NewMessage returns a new, zero message of the given id, or nil if the id is unknown.")
	w.p("func NewMessage(mgid uint16) IMessage {")
	w.p("\tswitch mgid {")
	for _, m := range g.msgs {
		w.p("\tcase %d:", m.ID)
		w.p("\t\treturn &%s{}", m.Abbrev)
	}
	w.p("\t}")
	w.p("\treturn nil")
	w.p("}")
	w.p("")
	w.p("// MessageID returns the id of a message by abbreviation (false if unknown).")
	w.p("func MessageID(abbrev string) (uint16, bool) {")
	w.p("\tswitch abbrev {")
	for _, m := range g.msgs {
		w.p("\tcase %q:", m.Abbrev)
		w.p("\t\treturn %d, true", m.ID)
	}
	w.p("\t}")
	w.p("\treturn 0, false")
	w.p("}")
	w.p("")
	w.p("// MessageIDs lists the ids of all typed messages, in increasing order.")
	w.p("var MessageIDs = []uint16{")
	for _, m := range g.msgs {
		w.p("\t%d,", m.ID)
	}
	w.p("}")
	w.p("")
	w.p("// CreateTyped returns a new typed message by abbreviation, set from msg when it is not nil.")
	w.p("func (p *Protocol) CreateTyped(abbrev string, msg *Message) (IMessage, error) {")
	w.p("\tid, ok := MessageID(abbrev)")
	w.p("\tif !ok {")
	w.p("\t\treturn nil, fmt.Errorf(\"unknown typed message: %%s\", abbrev)")
	w.p("\t}")
	w.p("\tm := NewMessage(id)")
	w.p("\tif msg != nil {")
	w.p("\t\treturn m, m.FromMessage(msg)")
	w.p("\t}")
	w.p("\treturn m, nil")
	w.p("}")
	w.p("")
	// Groups.
	w.p("// MessageGroups maps the IMC message groups to their members.")
	w.p("var MessageGroups = map[string][]string{")
	for _, gr := range g.spec.Groups {
		var names []string
		for _, t := range gr.Types {
			names = append(names, strconv.Quote(t.Abbrev))
		}
		w.p("\t%q: {%s},", gr.Abbrev, strings.Join(names, ", "))
	}
	w.p("}")
}
