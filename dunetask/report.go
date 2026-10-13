package dunetask

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ReportEntry is an entity item of an EntityList report:
// "label=id" or "label=id/origin/origin-id".
type ReportEntry struct {
	Label     string
	ID        int
	HasOrigin bool
	Origin    uint32
	OriginID  int
}

const ws = " \t\r\n"

// ParseReport parses an entity report (doc/specs/Transports.DistributedBus.md,
// Report grammar) as DUNE does: white space around keys and values is
// ignored, items whose value does not parse are skipped, the last of
// repeated keys wins. It returns the reserved '#' keys and the entities
// sorted by label.
func ParseReport(text string) (map[string]string, []ReportEntry) {
	items := map[string]string{}
	for _, part := range strings.Split(text, ";") {
		label, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		label = strings.Trim(label, ws)
		if label != "" {
			items[label] = strings.Trim(value, ws)
		}
	}
	labels := make([]string, 0, len(items))
	for l := range items {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	keys := map[string]string{}
	var entries []ReportEntry
	for _, label := range labels {
		value := items[label]
		if strings.HasPrefix(label, "#") {
			keys[label] = value
			continue
		}
		parts := strings.Split(value, "/")
		if len(parts) != 1 && len(parts) != 3 {
			continue
		}
		id, ok := number(parts[0], 10, 0xFFFF)
		if !ok {
			continue
		}
		e := ReportEntry{Label: label, ID: int(id)}
		if len(parts) == 3 {
			origin, ok1 := number(parts[1], 16, 0xFFFFFFFF)
			oid, ok2 := number(parts[2], 10, 0xFFFF)
			if !ok1 || !ok2 {
				continue
			}
			e.HasOrigin, e.Origin, e.OriginID = true, uint32(origin), int(oid)
		}
		entries = append(entries, e)
	}
	return keys, entries
}

// number parses digits only (no sign, no prefix, no spaces).
func number(text string, base int, top uint64) (uint64, bool) {
	if text == "" {
		return 0, false
	}
	for _, c := range text {
		ok := c >= '0' && c <= '9'
		if base == 16 {
			ok = ok || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		}
		if !ok {
			return 0, false
		}
	}
	v, err := strconv.ParseUint(text, base, 64)
	if err != nil || v > top {
		return 0, false
	}
	return v, true
}

// CheckLabel returns an error for labels DUNE cannot report: empty, with
// ';' or '=', starting with '#', or with surrounding white space.
func CheckLabel(label string) error {
	if label == "" || label != strings.Trim(label, ws) || strings.ContainsAny(label, ";=") || strings.HasPrefix(label, "#") {
		return fmt.Errorf("invalid entity label %q: no \";\", \"=\", leading \"#\" or surrounding spaces", label)
	}
	return nil
}

// ParseURL parses a bus URL: "tcp://host:port", "host:port",
// "unix:///abs/path" (also "unix:/abs/path" and "unix:rel/path"). It returns
// the network ("tcp" or "unix") and the address.
func ParseURL(url string) (network, address string, err error) {
	url = strings.TrimSpace(url)
	if rest, ok := strings.CutPrefix(url, "unix:"); ok {
		path := rest
		if p, ok := strings.CutPrefix(path, "//"); ok {
			path = p
			if !strings.HasPrefix(path, "/") {
				return "", "", fmt.Errorf("bus URL %q: use unix:///absolute/path or unix:relative/path", url)
			}
		}
		if path == "" {
			return "", "", fmt.Errorf("bus URL %q: no socket path", url)
		}
		return "unix", path, nil
	}
	if rest, ok := strings.CutPrefix(url, "tcp://"); ok {
		url = rest
	} else if strings.Contains(url, "://") {
		return "", "", fmt.Errorf("bus URL %q: unknown scheme (tcp:// or unix://)", url)
	}
	i := strings.LastIndex(url, ":")
	if i < 0 {
		return "", "", fmt.Errorf("bus URL %q: expected tcp://host:port or unix:///path", url)
	}
	host, port := url[:i], url[i+1:]
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 0 || p > 65535 {
		return "", "", fmt.Errorf("bus URL %q: expected tcp://host:port or unix:///path", url)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if strings.Contains(host, ":") {
		return "tcp", "[" + host + "]:" + port, nil
	}
	return "tcp", host + ":" + port, nil
}

// FormatSub formats a #sub value: sorted decimal ids, or "*" for nil
// (everything).
func FormatSub(ids map[uint16]bool) string {
	if ids == nil {
		return "*"
	}
	list := make([]int, 0, len(ids))
	for id := range ids {
		list = append(list, int(id))
	}
	sort.Ints(list)
	parts := make([]string, len(list))
	for i, id := range list {
		parts[i] = strconv.Itoa(id)
	}
	return strings.Join(parts, ",")
}

// ParseSub parses a #sub value: nil means everything ("*").
func ParseSub(text string) map[uint16]bool {
	ids := map[uint16]bool{}
	for _, item := range strings.Split(text, ",") {
		item = strings.Trim(item, ws)
		if item == "*" {
			return nil
		}
		if n, ok := number(item, 10, 0xFFFF); ok {
			ids[uint16(n)] = true
		}
	}
	return ids
}

// Env holds the defaults Supervisors.Process gives the programs it starts.
type Env struct {
	// Bus is $DUNE_BUS, or tcp://127.0.0.1:6060.
	Bus string
	// Name is $DUNE_TASK_NAME ("" if unset).
	Name string
	// System is $DUNE_SYSTEM when it is a number (decimal or 0x hexadecimal), or -1.
	System int
	// SystemName is $DUNE_SYSTEM when it is not a number (only checked
	// against the server's #name).
	SystemName string
}

// DefaultBus is the bus URL when neither the program nor $DUNE_BUS gives one.
const DefaultBus = "tcp://127.0.0.1:6060"

// EnvDefaults reads DUNE_BUS, DUNE_TASK_NAME and DUNE_SYSTEM (empty
// variables count as unset).
func EnvDefaults() Env {
	return envFrom(os.Getenv)
}

func envFrom(get func(string) string) Env {
	e := Env{Bus: strings.TrimSpace(get("DUNE_BUS")), Name: strings.TrimSpace(get("DUNE_TASK_NAME")), System: -1}
	if e.Bus == "" {
		e.Bus = DefaultBus
	}
	e.System, e.SystemName = ParseSystem(get("DUNE_SYSTEM"))
	return e
}

// ParseSystem interprets a system given as text: a number (decimal, 0x
// hexadecimal, 0 octal) below 0xFFFF is a system id; anything else non
// empty is a system name. It returns -1 for the id when it is not one.
func ParseSystem(s string) (int, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return -1, ""
	}
	v, err := strconv.ParseInt(s, 0, 64)
	if err != nil || v < 0 || v >= 0xFFFF {
		return -1, s
	}
	return int(v), ""
}

func cleanName(name string) string {
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == ';' || r == '=' {
			return -1
		}
		return r
	}, name))
	if name == "" {
		return "go"
	}
	return name
}

func programName() string {
	if len(os.Args) > 0 && os.Args[0] != "" {
		return filepath.Base(os.Args[0])
	}
	return "go"
}
