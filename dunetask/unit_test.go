package dunetask

import (
	"context"
	"net"

	"golang.org/x/sys/unix"
	"reflect"
	"testing"
)

func TestParseReportLikeCxx(t *testing.T) {
	keys, entries := ParseReport("#instance=1a2b;#proto=2;#imc=50cb;#new=x;Zed=3; A b = 1/ff/7 ;Bad=x;=4;Big=300;" +
		"Half=5/ab;C=2/ff/x;D=12abc;E=-1;F=4/fffffffff/1;Zed=6")
	want := map[string]string{"#instance": "1a2b", "#proto": "2", "#imc": "50cb", "#new": "x"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys %v", keys)
	}
	got := []ReportEntry{{"A b", 1, true, 0xff, 7}, {"Big", 300, false, 0, 0}, {"Zed", 6, false, 0, 0}}
	if !reflect.DeepEqual(entries, got) {
		t.Fatalf("entries %+v", entries)
	}
}

func TestParseURL(t *testing.T) {
	cases := map[string][2]string{
		"tcp://127.0.0.1:6060": {"tcp", "127.0.0.1:6060"},
		"10.0.0.1:6000":        {"tcp", "10.0.0.1:6000"},
		":6000":                {"tcp", "127.0.0.1:6000"},
		"tcp://[::1]:6000":     {"tcp", "[::1]:6000"},
		"unix:///run/bus.sock": {"unix", "/run/bus.sock"},
		"unix:/run/bus.sock":   {"unix", "/run/bus.sock"},
		"unix:rel.sock":        {"unix", "rel.sock"},
	}
	for url, want := range cases {
		n, a, err := ParseURL(url)
		if err != nil || n != want[0] || a != want[1] {
			t.Errorf("%s: %s %s %v", url, n, a, err)
		}
	}
	for _, bad := range []string{"udp://x:1", "unix://rel", "unix:", "host", "tcp://h:x"} {
		if _, _, err := ParseURL(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestSubSyntax(t *testing.T) {
	if ParseSub(" 5, 350 ,x,,70000,1") == nil || len(ParseSub("5, 350 ,x,,70000,1")) != 3 {
		t.Fatal(ParseSub("5, 350 ,x,,70000,1"))
	}
	if ParseSub("1,*") != nil || FormatSub(nil) != "*" || FormatSub(map[uint16]bool{350: true, 5: true}) != "5,350" {
		t.Fatal("sub format")
	}
}

func TestEnv(t *testing.T) {
	env := func(m map[string]string) Env { return envFrom(func(k string) string { return m[k] }) }
	e := env(nil)
	if e.Bus != DefaultBus || e.System != -1 || e.Name != "" {
		t.Fatalf("%+v", e)
	}
	e = env(map[string]string{"DUNE_BUS": " unix:///x ", "DUNE_SYSTEM": "0x2b01", "DUNE_TASK_NAME": "Sup/Ct"})
	if e.Bus != "unix:///x" || e.System != 0x2b01 || e.Name != "Sup/Ct" {
		t.Fatalf("%+v", e)
	}
	if e = env(map[string]string{"DUNE_SYSTEM": "lauv-xplore-1"}); e.System != -1 || e.SystemName != "lauv-xplore-1" {
		t.Fatalf("%+v", e)
	}
}

func TestParamSyntax(t *testing.T) {
	if v, err := parseParam[bool]("true"); err != nil || !v {
		t.Fatal(v, err)
	}
	if v, err := parseParam[bool]("0"); err != nil || v {
		t.Fatal(v, err)
	}
	if _, err := parseParam[bool]("yes"); err == nil {
		t.Fatal("yes is not a boolean")
	}
	if v, _ := parseParam[int]("0x10"); v != 16 {
		t.Fatal(v)
	}
	if v, _ := parseParam[int](" 7 "); v != 7 {
		t.Fatal(v)
	}
	if v, _ := parseParam[int]("010"); v != 8 {
		t.Fatal("strtol base 0: octal", v)
	}
	if v, _ := parseParam[[]float64]("1, 2.5,3"); !reflect.DeepEqual(v, []float64{1, 2.5, 3}) {
		t.Fatal(v)
	}
	if v, _ := parseParam[[]string]("a, b c"); !reflect.DeepEqual(v, []string{"a", "b c"}) {
		t.Fatal(v)
	}
	if v, _ := parseParam[[]int](""); len(v) != 0 {
		t.Fatal(v)
	}
	if formatParam(true) != "true" || formatParam([]float64{1, 2.5}) != "1.0, 2.5" {
		t.Fatal(formatParam([]float64{1, 2.5}))
	}
	floats := map[float64]string{1: "1.0", 100: "100.0", 0.1: "0.1", 2.5: "2.5", 1e16: "1e+16", 1e-5: "1e-05",
		123456789012345.0: "123456789012345.0", -0.0001: "-0.0001", 1.5e300: "1.5e+300"}
	for v, s := range floats {
		if FormatFloat(v) != s {
			t.Errorf("FormatFloat(%v) = %s, want %s", v, FormatFloat(v), s)
		}
	}
	if LabelOf("max_depth") != "Max Depth" || LabelOf("maxDepth") != "Max Depth" {
		t.Fatal(LabelOf("maxDepth"))
	}
	task := New("T")
	p := NewParam(task, "Count", 3, Min(0), Max(10))
	if _, err := task.params.setString("Count", "11"); err == nil || err.Error() != "Count: value is above maximum" {
		t.Fatal(err)
	}
	if ch, err := task.params.setString("Count", "4"); !ch || err != nil || p.Get() != 4 {
		t.Fatal(ch, err)
	}
	if ch, _ := task.params.setString("Count", "0x4"); ch || task.ParamText("Count") != "0x4" {
		t.Fatal("same value: not a change; the text is kept as given")
	}
	if err := p.Set(20); err == nil {
		t.Fatal("Set validates")
	}
	v := NewParam(task, "Mode", "a", Values("a", "b"))
	if _, err := task.params.setString("Mode", "c"); err == nil || v.Get() != "a" {
		t.Fatal("values set")
	}
	if _, err := task.params.setString("Nope", "1"); err == nil {
		t.Fatal("unknown parameter")
	}
}

func TestBadLabels(t *testing.T) {
	for _, l := range []string{"", " A", "A ", "a;b", "a=b", "#x"} {
		if CheckLabel(l) == nil {
			t.Errorf("%q accepted", l)
		}
	}
	if CheckLabel("Depth Watcher") != nil {
		t.Fatal("good label refused")
	}
}

func TestSocketBuffersFixedBeforeConnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	for _, size := range []int{128 << 10, -1} {
		b, err := NewBus(Options{Bus: "tcp://" + ln.Addr().String(), SocketBuffer: size})
		if err != nil {
			t.Fatal(err)
		}
		c, err := b.dial(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := c.(*net.TCPConn).SyscallConn()
		var rcv, snd int
		raw.Control(func(fd uintptr) {
			rcv, _ = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF)
			snd, _ = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUF)
		})
		c.Close()
		if size > 0 && ((rcv != size && rcv != 2*size) || (snd != size && snd != 2*size)) {
			t.Fatalf("buffers %d %d, want %d", rcv, snd, size)
		}
		if size < 0 && rcv == 2*DefaultSocketBuffer && snd == 2*DefaultSocketBuffer {
			t.Fatal("automatic buffers were fixed")
		}
	}
}
