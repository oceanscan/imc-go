package dunetask_test

// The SDK against the fake DUNE of package dunetest (in-process, over real
// TCP and Unix sockets): the same checks as the Python SDK's
// tests/sdk/python/test_task.py and test_proto3.py.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	imc "github.com/oceanscan/imc-go"
	"github.com/oceanscan/imc-go/dunetask"
	"github.com/oceanscan/imc-go/dunetask/dunetest"
)

const wait = 5 * time.Second

var quiet = dunetask.NewOutput(os.Stderr, dunetask.LevelError, false)

func init() {
	if os.Getenv("DUNETASK_TEST_LOG") != "" {
		quiet = dunetask.NewOutput(os.Stderr, dunetask.LevelDebug, true)
	}
}

// watcher is the test task of the Python tests.
type watcher struct {
	*dunetask.Task
	maxDepth *dunetask.Param[float64]
	count    *dunetask.Param[int]
	names    *dunetask.Param[[]string]
	verbose  *dunetask.Param[bool]

	mu             sync.Mutex
	seen           []*imc.EstimatedState
	updates        [][]string
	activations    int
	deactivations  int
	failActivation bool
}

func newWatcher(label string, opts ...dunetask.TaskOption) *watcher {
	w := &watcher{Task: dunetask.New(label, append([]dunetask.TaskOption{dunetask.HonoursActive()}, opts...)...)}
	w.maxDepth = dunetask.NewParam(w.Task, "Max Depth", 10.0, dunetask.Units("m"), dunetask.Desc("Maximum depth"))
	w.count = dunetask.NewParam(w.Task, "Count", 3, dunetask.Min(0), dunetask.Max(10), dunetask.Scope("plan"))
	w.names = dunetask.NewParam(w.Task, "Names", []string{"a", "b"})
	w.verbose = dunetask.NewParam(w.Task, "Verbose", false)
	dunetask.Consume(w.Task, func(m *imc.EstimatedState) {
		w.mu.Lock()
		w.seen = append(w.seen, m)
		w.mu.Unlock()
		if w.Active() && float64(m.Depth) > w.maxDepth.Get() {
			w.War("too deep: %.1f m", m.Depth)
			w.Dispatch(&imc.TextMessage{Origin: w.Label(), Text: "too deep"})
		}
	})
	w.OnUpdateParameters = func(changed []string) {
		w.mu.Lock()
		w.updates = append(w.updates, changed)
		w.mu.Unlock()
	}
	w.OnActivation = func() error {
		time.Sleep(10 * time.Millisecond)
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.failActivation {
			return errors.New("no sensor")
		}
		w.activations++
		return nil
	}
	w.OnDeactivation = func() error {
		w.mu.Lock()
		w.deactivations++
		w.mu.Unlock()
		return nil
	}
	return w
}

func (w *watcher) nSeen() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.seen)
}

type env struct {
	t      *testing.T
	srv    *dunetest.FakeDune
	bus    *dunetask.Bus
	cancel context.CancelFunc
	done   chan error
}

func run(t *testing.T, srv *dunetest.FakeDune, opts dunetask.Options, tasks ...*dunetask.Task) *env {
	t.Helper()
	if opts.Bus == "" {
		opts.Bus = srv.URL()
	}
	if opts.Name == "" {
		opts.Name = "test.go"
	}
	if opts.ReconnectMin == 0 {
		opts.ReconnectMin, opts.ReconnectMax = 100*time.Millisecond, 300*time.Millisecond
	}
	opts.Output = quiet
	bus, err := dunetask.NewBus(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &env{t: t, srv: srv, bus: bus, cancel: cancel, done: make(chan error, 1)}
	go func() { e.done <- bus.Run(ctx, tasks...) }()
	t.Cleanup(e.stop)
	return e
}

func (e *env) stop() {
	if e.cancel == nil {
		return
	}
	e.cancel()
	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
		e.t.Error("Run did not return")
	}
	e.cancel = nil
}

func (e *env) waitReady() {
	e.t.Helper()
	if !eventually(func() bool { return e.bus.Ready() }, wait) {
		e.t.Fatal("handshake not done")
	}
}

func eventually(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func start(t *testing.T, cfg dunetest.Config) *dunetest.FakeDune {
	t.Helper()
	srv, err := dunetest.Start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return srv
}

func setup(t *testing.T, cfg dunetest.Config, w *watcher) (*dunetest.FakeDune, *env, *watcher, int) {
	t.Helper()
	srv := start(t, cfg)
	if w == nil {
		w = newWatcher("Depth Watcher")
	}
	e := run(t, srv, dunetask.Options{}, w.Task)
	eid, err := srv.WaitEntity(w.Label(), wait)
	if err != nil {
		t.Fatal(err)
	}
	e.waitReady()
	// The task introduced itself (after its state and activation state).
	waitMsg(t, srv, 0, func(m *imc.EntityInfo) bool { return m.Label == w.Label() })
	return srv, e, w, eid
}

func lastKeys(t *testing.T, srv *dunetest.FakeDune) map[string]string {
	reps := srv.Clients()[0].Reports()
	keys, _ := dunetask.ParseReport(reps[len(reps)-1])
	return keys
}

func since(srv *dunetest.FakeDune) int { return srv.Received() }

func waitMsg[M imc.IMessage](t *testing.T, srv *dunetest.FakeDune, from int, pred func(M) bool) M {
	t.Helper()
	if pred == nil {
		pred = func(M) bool { return true }
	}
	m, err := srv.WaitMessageSince(dunetest.Where(pred), from, wait)
	if err != nil {
		var zero M
		t.Fatalf("no %T: %v", zero, err)
	}
	return m.(M)
}

func messages[M imc.IMessage](srv *dunetest.FakeDune, from int, pred func(M) bool) []M {
	if pred == nil {
		pred = func(M) bool { return true }
	}
	var out []M
	for _, m := range srv.Messages(dunetest.Where(pred), from) {
		out = append(out, m.(M))
	}
	return out
}

func param(name, value string) *imc.EntityParameter {
	return &imc.EntityParameter{Name: name, Value: value}
}

// -- handshake ------------------------------------------------------------------------

func TestHandshake(t *testing.T) {
	srv, e, w, eid := setup(t, dunetest.Config{}, nil)
	keys := lastKeys(t, srv)
	want := map[string]string{"#proto": "3", "#imc": imc.IMCMD5, "#sys": strconv.Itoa(0x1234), "#name": "test.go", "#strict": "1"}
	for k, v := range want {
		if keys[k] != v {
			t.Errorf("%s = %q, want %q", k, keys[k], v)
		}
	}
	if keys["#instance"] != fmt.Sprintf("%x", e.bus.Instance()) {
		t.Error("#instance")
	}
	reps := srv.Clients()[0].Reports()
	rep := reps[len(reps)-1]
	var names []string
	for _, item := range strings.Split(rep, ";") {
		names = append(names, strings.SplitN(item, "=", 2)[0])
	}
	if strings.Join(names[:7], ",") != "#instance,#proto,#imc,#sys,#name,#strict,#sub" {
		t.Errorf("key order: %v", names)
	}
	_, entries := dunetask.ParseReport(rep)
	tagged := map[string]dunetask.ReportEntry{}
	for _, en := range entries {
		if en.HasOrigin {
			tagged[en.Label] = en
		} else if en.Label != "Depth Watcher" || en.ID != int(w.Eid()) {
			t.Errorf("own entity %+v", en)
		}
	}
	for _, l := range []string{"Daemon", "Navigation", "Logger"} {
		if en := tagged[l]; en.Origin != srv.Instance() || en.OriginID != srv.Resolve(l) {
			t.Errorf("%s tagged %+v", l, en)
		}
	}
	if e.bus.System() != 0x1234 {
		t.Error("system id")
	}
	info := waitMsg(t, srv, 0, func(m *imc.EntityInfo) bool { return m.Label == "Depth Watcher" })
	if int(info.Header.SrcEnt) != eid || int(info.Id) != eid || info.Component != "test.go" {
		t.Errorf("EntityInfo %+v", info)
	}
	waitMsg[*imc.EntityState](t, srv, 0, nil)
	waitMsg[*imc.EntityActivationState](t, srv, 0, nil)
}

func TestHandshakeProto1SendsNoV2Keys(t *testing.T) {
	srv, _, _, _ := setup(t, dunetest.Config{Proto: 1}, nil)
	for _, rep := range srv.Clients()[0].Reports() {
		keys, entries := dunetask.ParseReport(rep)
		if len(keys) != 1 || keys["#instance"] == "" {
			t.Errorf("keys to a protocol 1 server: %v", keys)
		}
		for _, en := range entries {
			if strings.HasPrefix(en.Label, "#") {
				t.Error(en)
			}
		}
	}
}

func TestIDsTranslatedBothWays(t *testing.T) {
	srv, _, w, eid := setup(t, dunetest.Config{}, nil)
	nav, ok := w.EidOf("Navigation")
	if !ok || int(nav) != srv.Resolve("Navigation") {
		t.Fatal("mirrors take the server's id when it is free")
	}
	srv.Send(&imc.EstimatedState{Depth: 1}, "Navigation", "Depth Watcher")
	if !eventually(func() bool { return w.nSeen() == 1 }, wait) {
		t.Fatal("no EstimatedState")
	}
	w.mu.Lock()
	m := w.seen[0]
	w.mu.Unlock()
	if m.Header.SrcEnt != nav || m.Header.DstEnt != w.Eid() || m.Header.Src != 0x1234 {
		t.Errorf("header %+v", m.Header)
	}
	// Unknown ids become 255.
	es := &imc.EstimatedState{Depth: 2}
	srv.Send(es, "", "")
	if !eventually(func() bool { return w.nSeen() == 2 }, wait) {
		t.Fatal("second EstimatedState")
	}
	w.mu.Lock()
	if w.seen[1].Header.SrcEnt != 255 {
		t.Error("unknown source entity")
	}
	w.mu.Unlock()
	// Outgoing: our entity and a DUNE entity by label.
	n := since(srv)
	if !w.Dispatch(&imc.DevDataText{Value: "hi"}, dunetask.ToLabel("Logger")) {
		t.Fatal("dispatch")
	}
	d := waitMsg[*imc.DevDataText](t, srv, n, nil)
	if int(d.Header.SrcEnt) != eid || int(d.Header.DstEnt) != srv.Resolve("Logger") || d.Header.Src != 0x1234 {
		t.Errorf("outgoing header %+v", d.Header)
	}
	if w.Dispatch(&imc.DevDataText{}, dunetask.ToLabel("Nobody")) {
		t.Error("unknown label sent")
	}
}

func TestEntitiesOfOtherPeers(t *testing.T) {
	srv, _, w, _ := setup(t, dunetest.Config{}, nil)
	other := dunetask.New("Other Thing")
	e2 := run(t, srv, dunetask.Options{Name: "other.go"}, other)
	if _, err := srv.WaitEntity("Other Thing", wait); err != nil {
		t.Fatal(err)
	}
	if !eventually(func() bool { _, ok := w.EidOf("Other Thing"); return ok }, wait) {
		t.Fatal("other peer's entity not mirrored")
	}
	e2.stop()
	if !eventually(func() bool { _, ok := w.EidOf("Other Thing"); return !ok }, wait) {
		t.Fatal("other peer's entity not released")
	}
	if _, ok := w.EidOf("Navigation"); !ok {
		t.Fatal("Navigation lost")
	}
}

func TestLabelCollisionRefusedWithStrict(t *testing.T) {
	srv := start(t, dunetest.Config{})
	task := dunetask.New("Navigation")
	e := run(t, srv, dunetask.Options{}, task)
	e.waitReady()
	if err := srv.WaitFor(func() bool { return true }, 0); err != nil {
		t.Fatal(err)
	}
	if !eventually(func() bool {
		c := srv.Clients()
		return len(c) == 1 && len(c[0].Refused()) == 1
	}, wait) {
		t.Fatal("not refused")
	}
	if !eventually(func() bool { u := e.bus.Unmirrored(); return len(u) == 1 && u[0] == "Navigation" }, wait) {
		t.Fatal("refusal not detected")
	}
	n := since(srv)
	task.Inf("hello")
	time.Sleep(1500 * time.Millisecond)
	for _, m := range srv.Messages(nil, n) {
		if m.GetHeader().SrcEnt == 255 {
			t.Fatalf("sent from the refused entity: %s", m.Abbrev())
		}
	}
}

// A report the server sent before it applied ours is not taken for a
// refusal: the handshake ends on an answer that reflects our report.
func TestReportCrossingOursIsNotARefusal(t *testing.T) {
	srv := start(t, dunetest.Config{CrossingReport: true})
	task := dunetask.New("Watcher")
	atReady := make(chan []string, 4)
	task.OnReady = func() { atReady <- task.Bus().Unmirrored() }
	run(t, srv, dunetask.Options{}, task)
	select {
	case u := <-atReady:
		if len(u) != 0 {
			t.Fatalf("ready with entities taken as refused: %v", u)
		}
	case <-time.After(wait):
		t.Fatal("handshake not done")
	}
	n := since(srv)
	task.Inf("hello")
	waitMsg(t, srv, n, func(m *imc.LogBookEntry) bool { return m.Text == "hello" })
}

func TestLabelCollisionPrefixedWithoutStrict(t *testing.T) {
	srv := start(t, dunetest.Config{})
	task := dunetask.New("Navigation")
	e := run(t, srv, dunetask.Options{AllowRename: true}, task)
	e.waitReady()
	if _, err := srv.WaitEntity("Remote Navigation", wait); err != nil {
		t.Fatal(err)
	}
	if lastKeys(t, srv)["#strict"] != "" {
		t.Fatal("#strict sent")
	}
	n := since(srv)
	task.Inf("still sent")
	waitMsg(t, srv, n, func(m *imc.LogBookEntry) bool { return m.Text == "still sent" })
}

// -- entity services ---------------------------------------------------------------

func TestEntityServices(t *testing.T) {
	srv, _, _, eid := setup(t, dunetest.Config{}, nil)
	logger := srv.Resolve("Logger")
	n := since(srv)
	srv.Send(&imc.QueryEntityState{}, "Logger", "Depth Watcher")
	st := waitMsg(t, srv, n, func(m *imc.EntityState) bool { return int(m.Header.DstEnt) == logger })
	if int(st.Header.SrcEnt) != eid || st.State != imc.EntityStateState_NORMAL || st.Description != "idle" {
		t.Errorf("EntityState %+v", st)
	}
	n = since(srv)
	srv.Send(&imc.QueryEntityInfo{Id: uint8(eid)}, "Logger", "Depth Watcher")
	info := waitMsg[*imc.EntityInfo](t, srv, n, nil)
	if int(info.Header.DstEnt) != logger || info.Header.Dst != 0x1234 || info.Label != "Depth Watcher" {
		t.Errorf("EntityInfo %+v", info)
	}
	n = since(srv)
	srv.Send(&imc.QueryEntityActivationState{}, "Logger", "Depth Watcher")
	if as := waitMsg[*imc.EntityActivationState](t, srv, n, nil); as.State != imc.EntityActivationStateState_INACTIVE {
		t.Error(as.State)
	}
	n = since(srv)
	srv.Send(&imc.VersionInfo{Op: imc.VersionInfoOp_QUERY}, "Logger", "Depth Watcher")
	v := waitMsg[*imc.VersionInfo](t, srv, n, nil)
	if v.Op != imc.VersionInfoOp_REPLY || int(v.Header.DstEnt) != logger || !strings.HasPrefix(v.Description, "dunetask") {
		t.Errorf("VersionInfo %+v", v)
	}
	// Queries for other entities are not answered.
	n = since(srv)
	srv.Send(&imc.QueryEntityInfo{}, "Logger", "Logger")
	time.Sleep(200 * time.Millisecond)
	if len(messages[*imc.EntityInfo](srv, n, nil)) != 0 {
		t.Error("answered a query for another entity")
	}
}

func TestHeartbeat(t *testing.T) {
	srv, _, _, eid := setup(t, dunetest.Config{}, nil)
	n := since(srv)
	time.Sleep(2300 * time.Millisecond)
	if l := messages(srv, n, func(m *imc.EntityState) bool { return int(m.Header.SrcEnt) == eid }); len(l) < 2 {
		t.Fatalf("%d EntityState in 2.3 s", len(l))
	}
}

func TestParameters(t *testing.T) {
	srv, _, w, _ := setup(t, dunetest.Config{}, nil)
	n := since(srv)
	srv.Send(&imc.QueryEntityParameters{Name: "Depth Watcher"}, "Logger", "")
	p := waitMsg[*imc.EntityParameters](t, srv, n, nil)
	values := map[string]string{}
	var order []string
	for _, x := range p.Params {
		values[x.Name] = x.Value
		order = append(order, x.Name)
	}
	want := map[string]string{"Active": "false", "Count": "3", "Max Depth": "10.0", "Names": "a, b", "Verbose": "false"}
	if fmt.Sprint(values) != fmt.Sprint(want) || !sort.StringsAreSorted(order) {
		t.Fatalf("parameters %v", values)
	}
	if int(p.Header.DstEnt) != srv.Resolve("Logger") {
		t.Error("reply destination")
	}
	// Other labels are ignored.
	srv.Send(&imc.SetEntityParameters{Name: "Navigation", Params: []*imc.EntityParameter{param("Count", "5")}}, "", "")
	n = since(srv)
	srv.Send(&imc.SetEntityParameters{Name: "Depth Watcher", Params: []*imc.EntityParameter{
		param("Max Depth", "2.5"), param("Count", "99"), param("Names", "x,y , z"), param("Nope", "1")}}, "", "")
	reply := waitMsg[*imc.EntityParameters](t, srv, n, nil)
	for _, x := range reply.Params {
		if x.Name == "Max Depth" && x.Value != "2.5" {
			t.Error(x.Value)
		}
	}
	if w.maxDepth.Get() != 2.5 || w.count.Get() != 3 || fmt.Sprint(w.names.Get()) != "[x y z]" {
		t.Errorf("values %v %v %v", w.maxDepth.Get(), w.count.Get(), w.names.Get())
	}
	w.mu.Lock()
	if fmt.Sprint(w.updates) != "[[Max Depth Names]]" {
		t.Errorf("updates %v", w.updates)
	}
	w.mu.Unlock()
	// Invalid values are refused with a warning.
	waitMsg(t, srv, n, func(m *imc.LogBookEntry) bool {
		return m.Type == imc.LogBookEntryType_WARNING && strings.Contains(m.Text, "Count: value is above maximum")
	})
	waitMsg(t, srv, n, func(m *imc.LogBookEntry) bool { return strings.Contains(m.Text, `unknown parameter "Nope"`) })
	// Push / pop (Count has scope plan).
	srv.Send(&imc.PushEntityParameters{Name: "Depth Watcher"}, "", "")
	srv.Send(&imc.SetEntityParameters{Name: "Depth Watcher", Params: []*imc.EntityParameter{param("Count", "1")}}, "", "")
	if !eventually(func() bool { return w.count.Get() == 1 }, wait) {
		t.Fatal("set")
	}
	srv.Send(&imc.PopEntityParameters{Name: "Depth Watcher"}, "", "")
	if !eventually(func() bool { return w.count.Get() == 3 }, wait) {
		t.Fatal("pop")
	}
}

func actStates(srv *dunetest.FakeDune, n int) string {
	var s []string
	for _, m := range messages[*imc.EntityActivationState](srv, n, nil) {
		s = append(s, m.State.String())
	}
	return strings.Join(s, ",")
}

func TestActivation(t *testing.T) {
	srv, _, w, _ := setup(t, dunetest.Config{}, nil)
	if w.Active() {
		t.Fatal("active at start")
	}
	set := func(v string) {
		srv.Send(&imc.SetEntityParameters{Name: "Depth Watcher", Params: []*imc.EntityParameter{param("Active", v)}}, "", "")
	}
	n := since(srv)
	set("true")
	if !eventually(func() bool { return actStates(srv, n) == "ACT_IP,ACT_DONE,ACTIVE" }, wait) {
		t.Fatalf("activation: %s", actStates(srv, n))
	}
	if !w.Active() || w.ParamText("Active") != "true" {
		t.Fatal("not active")
	}
	waitMsg(t, srv, n, func(m *imc.EntityState) bool { return m.Description == "active" })
	srv.Send(&imc.EstimatedState{Depth: 20}, "Navigation", "")
	waitMsg[*imc.TextMessage](t, srv, n, nil)
	log := waitMsg(t, srv, n, func(m *imc.LogBookEntry) bool { return m.Context == "Depth Watcher" })
	if log.Type != imc.LogBookEntryType_WARNING || !strings.Contains(log.Text, "too deep") {
		t.Errorf("log %+v", log)
	}
	n = since(srv)
	set("false")
	if !eventually(func() bool { return actStates(srv, n) == "DEACT_IP,DEACT_DONE,INACTIVE" }, wait) {
		t.Fatalf("deactivation: %s", actStates(srv, n))
	}
	w.mu.Lock()
	if w.activations != 1 || w.deactivations != 1 {
		t.Error(w.activations, w.deactivations)
	}
	w.failActivation = true
	w.mu.Unlock()
	n = since(srv)
	set("true")
	if !eventually(func() bool { return actStates(srv, n) == "ACT_IP,ACT_FAIL,INACTIVE" }, wait) {
		t.Fatalf("failed activation: %s", actStates(srv, n))
	}
	if l := messages[*imc.EntityActivationState](srv, n, nil); l[1].Error != "no sensor" {
		t.Error(l[1].Error)
	}
	if w.ParamText("Active") != "false" || w.Active() {
		t.Error("still active")
	}
	// Setting Active to its current value reports the activation state.
	n = since(srv)
	set("false")
	if as := waitMsg[*imc.EntityActivationState](t, srv, n, nil); as.State != imc.EntityActivationStateState_INACTIVE {
		t.Error(as.State)
	}
}

func TestActiveByDefault(t *testing.T) {
	w := newWatcher("W2", dunetask.ActiveDefault(true))
	_, _, w, _ = setup(t, dunetest.Config{}, w)
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.Active() || w.activations != 1 {
		t.Fatal("not active by default")
	}
}

func TestLogbookAndLocalDelivery(t *testing.T) {
	srv := start(t, dunetest.Config{})
	talker := dunetask.New("Talker")
	listener := dunetask.New("Listener")
	var mu sync.Mutex
	var got []*imc.TextMessage
	dunetask.Consume(listener, func(m *imc.TextMessage) {
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	})
	e := run(t, srv, dunetask.Options{}, talker, listener)
	e.waitReady()
	if _, err := srv.WaitEntity("Listener", wait); err != nil {
		t.Fatal(err)
	}
	talker.Inf("hello")
	talker.Err("bad %d", 1)
	msg := &imc.TextMessage{Text: "local"}
	talker.Dispatch(msg)
	msg.Text = "changed after dispatch" // the listener has a copy
	le := waitMsg(t, srv, 0, func(m *imc.LogBookEntry) bool { return m.Type == imc.LogBookEntryType_ERROR })
	if le.Context != "Talker" || le.Text != "bad 1" || int(le.Header.SrcEnt) != srv.Resolve("Talker") {
		t.Errorf("%+v", le)
	}
	waitMsg(t, srv, 0, func(m *imc.LogBookEntry) bool { return m.Type == imc.LogBookEntryType_INFO && m.Text == "hello" })
	waitMsg(t, srv, 0, func(m *imc.TextMessage) bool { return m.Text == "local" })
	if !eventually(func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 1 }, wait) {
		t.Fatal("not delivered locally")
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Text != "local" || got[0].Header.SrcEnt != talker.Eid() {
		t.Fatalf("local delivery %+v", got)
	}
}

// -- connection -----------------------------------------------------------------------

func TestNothingSentBeforeHandshake(t *testing.T) {
	task := dunetask.New("Lonely")
	if task.Dispatch(&imc.DevDataText{}) {
		t.Fatal("sent without a bus")
	}
}

func TestReconnectAfterServerRestart(t *testing.T) {
	srv, e, w, _ := setup(t, dunetest.Config{}, nil)
	addr := srv.Addr()
	srv.Close()
	if !eventually(func() bool { return !w.Connected() }, wait) {
		t.Fatal("still connected")
	}
	if _, ok := w.EidOf("Navigation"); ok {
		t.Fatal("mirrors kept")
	}
	w.maxDepth.Set(7.5)
	srv2 := start(t, dunetest.Config{Addr: addr, Entities: []string{"Daemon", "Logger", "Navigation", "Extra"}})
	if _, err := srv2.WaitEntity("Depth Watcher", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	waitMsg(t, srv2, 0, func(m *imc.EntityInfo) bool { return m.Label == "Depth Watcher" })
	e.waitReady()
	if _, ok := w.EidOf("Extra"); !ok {
		t.Fatal("new server entity")
	}
	if e.bus.Stats().Connects != 2 {
		t.Fatal(e.bus.Stats().Connects)
	}
	// Parameters are kept.
	n := since(srv2)
	srv2.Send(&imc.QueryEntityParameters{Name: "Depth Watcher"}, "", "")
	p := waitMsg[*imc.EntityParameters](t, srv2, n, nil)
	for _, x := range p.Params {
		if x.Name == "Max Depth" && x.Value != "7.5" {
			t.Fatal(x.Value)
		}
	}
}

func TestIMCMismatchRefused(t *testing.T) {
	srv := start(t, dunetest.Config{IMCMD5: strings.Repeat("0", 32)})
	w := newWatcher("Depth Watcher")
	e := run(t, srv, dunetask.Options{OnMismatch: dunetask.MismatchRetry, MismatchRetry: time.Second}, w.Task)
	if !eventually(func() bool { return e.bus.Stats().Connects == 1 }, wait) {
		t.Fatal("no connection")
	}
	time.Sleep(2500 * time.Millisecond)
	// Not mirrored, retried slowly (at least 5 s apart), refused before
	// sending its report.
	if srv.Resolve("Depth Watcher") >= 0 || e.bus.Stats().Connects != 1 || w.Connected() {
		t.Fatalf("connects %d", e.bus.Stats().Connects)
	}
	if !eventually(func() bool { return e.bus.Stats().Connects == 2 }, 5*time.Second) {
		t.Fatal("no retry after 5 s")
	}
}

func TestIMCMismatchExits(t *testing.T) {
	srv := start(t, dunetest.Config{IMCMD5: strings.Repeat("0", 32)})
	err := dunetask.Run(context.Background(), dunetask.Options{Bus: srv.URL(), Output: quiet}, newWatcher("W").Task)
	if !errors.Is(err, dunetask.ErrIMCMismatch) {
		t.Fatal(err)
	}
}

func TestStartFailure(t *testing.T) {
	task := dunetask.New("Broken")
	task.OnStart = func() error { return errors.New("no device") }
	err := dunetask.Run(context.Background(), dunetask.Options{Bus: "tcp://127.0.0.1:1", Output: quiet}, task)
	if !errors.Is(err, dunetask.ErrStart) {
		t.Fatal(err)
	}
}

func TestStopReportsStopping(t *testing.T) {
	srv, e, w, eid := setup(t, dunetest.Config{}, newWatcher("Stopper", dunetask.ActiveDefault(true)))
	stopped := make(chan struct{})
	w.OnStop = func() { close(stopped) }
	n := since(srv)
	t0 := time.Now()
	e.stop()
	if time.Since(t0) > 2*time.Second {
		t.Fatalf("stopping took %v", time.Since(t0))
	}
	<-stopped
	waitMsg(t, srv, n, func(m *imc.EntityState) bool {
		return int(m.Header.SrcEnt) == eid && m.State == imc.EntityStateState_BOOT && m.Description == "stopping"
	})
	if actStates(srv, n) != "DEACT_IP,DEACT_DONE,INACTIVE" {
		t.Fatal(actStates(srv, n))
	}
	if srv.WaitClients(0, wait) != nil {
		t.Fatal("connection not closed")
	}
}

// -- protocol 3: subscriptions ---------------------------------------------------------

var serviceIDs = []uint16{imc.IDQueryEntityState, imc.IDQueryEntityInfo, imc.IDQueryEntityActivationState,
	imc.IDQueryEntityParameters, imc.IDSetEntityParameters, imc.IDPushEntityParameters, imc.IDPopEntityParameters,
	imc.IDVersionInfo}

func TestSubFromHandlers(t *testing.T) {
	srv, e, _, _ := setup(t, dunetest.Config{}, nil)
	want := map[uint16]bool{imc.IDEntityList: true, imc.IDEstimatedState: true}
	for _, id := range serviceIDs {
		want[id] = true
	}
	keys := lastKeys(t, srv)
	if dunetask.FormatSub(dunetask.ParseSub(keys["#sub"])) != dunetask.FormatSub(want) {
		t.Fatalf("#sub=%s", keys["#sub"])
	}
	got := map[uint16]bool{}
	for _, id := range e.bus.Sub() {
		got[id] = true
	}
	if dunetask.FormatSub(got) != keys["#sub"] {
		t.Fatal("Bus.Sub")
	}
}

func TestSubHonouredAndResentOnSubscribe(t *testing.T) {
	srv, e, w, _ := setup(t, dunetest.Config{}, nil)
	c := srv.Clients()[0]
	srv.Send(&imc.TextMessage{Text: "one"}, "Navigation", "")
	srv.Send(&imc.EstimatedState{Depth: 1}, "Navigation", "")
	if !eventually(func() bool { return w.nSeen() == 1 }, wait) {
		t.Fatal("EstimatedState")
	}
	if _, _, f := c.Stats(); f != 1 {
		t.Fatalf("filtered %d", f)
	}
	nrep := len(c.Reports())
	var mu sync.Mutex
	var got []string
	dunetask.Consume(w.Task, func(m *imc.TextMessage) { mu.Lock(); got = append(got, m.Text); mu.Unlock() })
	if !eventually(func() bool { return len(c.Reports()) > nrep && c.Sub()[imc.IDTextMessage] }, wait) {
		t.Fatal("#sub not re-sent")
	}
	srv.Send(&imc.TextMessage{Text: "two"}, "Navigation", "")
	if !eventually(func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 1 && got[0] == "two" }, wait) {
		t.Fatal(got)
	}
	// Every byte the server wrote is counted.
	if !eventually(func() bool { b, _, _ := c.Stats(); return e.bus.Stats().RxBytes == uint64(b) }, wait) {
		b, _, _ := c.Stats()
		t.Fatalf("rx %d, server wrote %d", e.bus.Stats().RxBytes, b)
	}
}

func TestSubscribeAll(t *testing.T) {
	srv := start(t, dunetest.Config{})
	e := run(t, srv, dunetask.Options{SubscribeAll: true}, newWatcher("Depth Watcher").Task)
	e.waitReady()
	if lastKeys(t, srv)["#sub"] != "*" || srv.Clients()[0].Sub() != nil {
		t.Fatal("#sub=*")
	}
}

func TestNoSubToProto2Server(t *testing.T) {
	srv, _, w, _ := setup(t, dunetest.Config{Proto: 2}, nil)
	for _, rep := range srv.Clients()[0].Reports() {
		keys, _ := dunetask.ParseReport(rep)
		if _, ok := keys["#sub"]; ok || keys["#proto"] != "3" {
			t.Fatalf("keys %v", keys)
		}
	}
	srv.Send(&imc.DevDataText{Value: "x"}, "Navigation", "")
	srv.Send(&imc.EstimatedState{Depth: 1}, "Navigation", "")
	if !eventually(func() bool { return w.nSeen() == 1 }, wait) {
		t.Fatal("EstimatedState")
	}
	if _, _, f := srv.Clients()[0].Stats(); f != 0 {
		t.Fatal("protocol 2 server filtered")
	}
}

func TestUnixSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "dt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "bus.sock")
	srv := start(t, dunetest.Config{Path: path})
	if srv.URL() != "unix://"+path {
		t.Fatal(srv.URL())
	}
	w := newWatcher("Depth Watcher")
	e := run(t, srv, dunetask.Options{}, w.Task)
	if _, err := srv.WaitEntity("Depth Watcher", wait); err != nil {
		t.Fatal(err)
	}
	e.waitReady()
	srv.Send(&imc.EstimatedState{Depth: 2}, "Navigation", "Depth Watcher")
	if !eventually(func() bool { return w.nSeen() == 1 }, wait) {
		t.Fatal("EstimatedState")
	}
	srv.Close()
	time.Sleep(200 * time.Millisecond)
	srv2 := start(t, dunetest.Config{Path: path})
	if _, err := srv2.WaitEntity("Depth Watcher", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if e.bus.Stats().Connects != 2 {
		t.Fatal(e.bus.Stats().Connects)
	}
}

func TestOutgoingQueueBounded(t *testing.T) {
	srv := start(t, dunetest.Config{})
	task := dunetask.New("Flooder")
	limit := 256 << 10
	e := run(t, srv, dunetask.Options{QueueSize: limit}, task)
	e.waitReady()
	if _, err := srv.WaitEntity("Flooder", wait); err != nil {
		t.Fatal(err)
	}
	srv.Pause(true)
	payload := strings.Repeat("x", 1000)
	for i := 0; i < 40000; i++ {
		task.Dispatch(&imc.DevDataText{Value: fmt.Sprintf("%d %s", i, payload)})
		if q := e.bus.Stats().QueuedBytes; q > uint64(limit+2048) {
			t.Fatalf("queue %d bytes", q)
		}
	}
	st := e.bus.Stats()
	if st.QueueDrops == 0 || st.QueuePeak > uint64(limit+2048) || st.QueueDroppedBytes < 1000*(st.QueueDrops-100) {
		t.Fatalf("stats %+v", st)
	}
	task.Dispatch(&imc.DevDataText{Value: "last"})
	srv.Pause(false)
	if _, err := srv.WaitMessage(dunetest.Where(func(m *imc.DevDataText) bool { return m.Value == "last" }), 20*time.Second); err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, m := range messages[*imc.DevDataText](srv, 0, nil) {
		if m.Value != "last" {
			n, _ := strconv.Atoi(strings.Fields(m.Value)[0])
			got = append(got, n)
		}
	}
	// In order, the oldest dropped: a prefix (in the socket buffers) and
	// the newest (in the queue).
	if !sort.IntsAreSorted(got) || got[len(got)-1] != 39999 || len(got) >= 40000 {
		t.Fatalf("%d received, last %d", len(got), got[len(got)-1])
	}
	if !eventually(func() bool { return e.bus.Stats().QueuedBytes == 0 }, wait) {
		t.Fatal("queue not drained")
	}
	if !srv.Clients()[0].Sub()[imc.IDEntityList] {
		t.Fatal("client state lost")
	}
}

// Run leaves no goroutine behind, whether it ends connected, while
// reconnecting, or after a server restart.
func TestNoGoroutineLeak(t *testing.T) {
	base := runtime.NumGoroutine()
	srv := start(t, dunetest.Config{})
	for i := 0; i < 3; i++ {
		w := newWatcher(fmt.Sprintf("Leak %d", i))
		e := run(t, srv, dunetask.Options{}, w.Task)
		e.waitReady()
		if i == 1 {
			srv.DropClients()
			time.Sleep(50 * time.Millisecond)
		}
		e.stop()
	}
	// A bus that never connects.
	e := run(t, srv, dunetask.Options{Bus: "tcp://127.0.0.1:1"}, newWatcher("Nowhere").Task)
	time.Sleep(300 * time.Millisecond)
	e.stop()
	srv.Close()
	if !eventually(func() bool { return runtime.NumGoroutine() <= base }, 3*time.Second) {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("%d goroutines, %d before\n%s", runtime.NumGoroutine(), base, buf[:n])
	}
}
