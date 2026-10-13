package dunetask

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	imc "github.com/oceanscan/imc-go"
)

// Version of the SDK (reported in VersionInfo replies).
const Version = "0.1.0"

// TaskQueueSize is the number of messages queued per task before the
// oldest are dropped (a handler that is too slow).
const TaskQueueSize = 1000

type (
	asState = imc.EntityActivationStateState
	esState = imc.EntityStateState
)

// Short names of the states, as DUNE's.
const (
	asInactive  = imc.EntityActivationStateState_INACTIVE
	asActive    = imc.EntityActivationStateState_ACTIVE
	asActIP     = imc.EntityActivationStateState_ACT_IP
	asActDone   = imc.EntityActivationStateState_ACT_DONE
	asActFail   = imc.EntityActivationStateState_ACT_FAIL
	asDeactIP   = imc.EntityActivationStateState_DEACT_IP
	asDeactDone = imc.EntityActivationStateState_DEACT_DONE
	asDeactFail = imc.EntityActivationStateState_DEACT_FAIL
)

// services are the messages every task answers for its entity.
var services = []uint16{
	imc.IDQueryEntityState, imc.IDQueryEntityInfo, imc.IDQueryEntityActivationState, imc.IDQueryEntityParameters,
	imc.IDSetEntityParameters, imc.IDPushEntityParameters, imc.IDPopEntityParameters, imc.IDVersionInfo,
}

func isService(id uint16) bool {
	for _, s := range services {
		if s == id {
			return true
		}
	}
	return false
}

// TaskOption configures a task.
type TaskOption func(*Task)

// HonoursActive adds the "Active" parameter and runs DUNE's activation
// state machine (the task starts inactive unless ActiveDefault(true)).
func HonoursActive() TaskOption { return func(t *Task) { t.honoursActive = true } }

// ActiveDefault sets the default of "Active" (activated at start when true).
func ActiveDefault(active bool) TaskOption {
	return func(t *Task) { t.honoursActive = true; t.activeDefault = active }
}

// Task is one DUNE entity of the process, with the services of
// DUNE::Tasks::Task: it answers QueryEntityState, QueryEntityInfo,
// QueryEntityActivationState, VersionInfo queries and Query/Set/Push/Pop
// EntityParameters by label; reports EntityState every second and on
// change; runs the activation state machine when it honours Active; and
// turns Inf/War/Err/Cri into LogBookEntry messages.
//
// Handlers, periodic functions and hooks of a task run one at a time, in
// order, on the task's own goroutine: they need no locks for the task's
// state. They must not block for long (messages queue up, then the oldest
// are dropped). Received messages are shared between the tasks of the
// process: do not modify them.
type Task struct {
	label         string
	honoursActive bool
	activeDefault bool
	params        params
	activeParam   *Param[bool]
	log           *Logger

	// Hooks: set them before Run. They run on the task's goroutine.
	//
	// OnStart is called before connecting; an error stops Run.
	OnStart func() error
	// OnStop is called when stopping (after deactivation).
	OnStop func()
	// OnReady is called when the bus handshake is done (also after a
	// reconnection).
	OnReady func()
	// OnDisconnect is called when the connection is lost.
	OnDisconnect func()
	// OnUpdateParameters is called after SetEntityParameters (or
	// PopEntityParameters) with the names of the parameters whose values
	// changed (possibly none).
	OnUpdateParameters func(changed []string)
	// OnActivation is called to activate; an error fails the activation
	// (ACT_FAIL with the error).
	OnActivation func() error
	// OnDeactivation is called to deactivate; an error fails it.
	OnDeactivation func() error

	mu         sync.Mutex
	handlers   map[uint16][]func(imc.IMessage)
	periodic   []periodicJob
	bus        *Bus
	ent        *entity
	actState   asState
	actError   string
	pendingAct int // 0 none, else 1 + asActive/asInactive
	state      esState
	stateDesc  string
	autoState  bool
	started    bool
	inbox      mailbox[imc.IMessage]
	ctl        mailbox[func()]
	tick       chan func()
	cancel     context.CancelFunc
	done       chan struct{}
}

type periodicJob struct {
	period time.Duration
	fn     func()
}

// New creates a task whose entity has the given label (unique in the
// vehicle: parameters, activation and Neptus address entities by label).
func New(label string, opts ...TaskOption) *Task {
	t := &Task{label: label, handlers: map[uint16][]func(imc.IMessage){}, state: imc.EntityStateState_BOOT,
		stateDesc: "starting", autoState: true}
	for _, o := range opts {
		o(t)
	}
	if t.honoursActive {
		t.activeParam = NewParam(t, "Active", t.activeDefault, Desc("True to activate task, false otherwise"))
	}
	t.inbox.limit = TaskQueueSize
	t.inbox.init()
	t.ctl.init()
	t.tick = make(chan func(), 16)
	t.log = defaultOutput.Logger("dunetask." + strings.ReplaceAll(label, " ", "_"))
	return t
}

// Consume calls fn with every message of type M (from DUNE, or dispatched
// by the other tasks of the process). Call it before Run, or later (DUNE is
// then told at once).
//
//	dunetask.Consume(t, func(m *imc.EstimatedState) { ... })
func Consume[M any, PM interface {
	*M
	imc.IMessage
}](t *Task, fn func(PM)) {
	id := PM(new(M)).GetMGID()
	t.Subscribe(id, func(m imc.IMessage) { fn(m.(PM)) })
}

// Subscribe calls fn with every message of the given id.
func (t *Task) Subscribe(mgid uint16, fn func(imc.IMessage)) {
	t.mu.Lock()
	t.handlers[mgid] = append(t.handlers[mgid], fn)
	bus := t.bus
	t.mu.Unlock()
	if bus != nil {
		bus.refreshFilter()
	}
}

// Periodic calls fn every period on the task's goroutine (from Run).
func (t *Task) Periodic(period time.Duration, fn func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.periodic = append(t.periodic, periodicJob{period, fn})
}

// Label returns the entity label.
func (t *Task) Label() string { return t.label }

// Logger returns the task's logger (local log lines only).
func (t *Task) Logger() *Logger { return t.log }

// Bus returns the bus the task runs on (nil before Run).
func (t *Task) Bus() *Bus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.bus
}

// Eid returns the task's entity id, as seen by this process (UNK before Run).
func (t *Task) Eid() uint8 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ent == nil {
		return UNK
	}
	return uint8(t.ent.id)
}

// EidOf returns the id (in this process) of an entity of DUNE or of this
// process, by label; false if unknown (yet). Incoming SrcEnt/DstEnt are in
// these ids.
func (t *Task) EidOf(label string) (uint8, bool) {
	if b := t.Bus(); b != nil {
		return b.Resolve(label)
	}
	return UNK, false
}

// LabelOf returns the label of an entity id.
func (t *Task) LabelOf(eid uint8) (string, bool) {
	if b := t.Bus(); b != nil {
		return b.LabelOf(eid)
	}
	return "", false
}

// Active tells whether the task is active (always true when it does not
// honour Active).
func (t *Task) Active() bool {
	if !t.honoursActive {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.actState == asActive
}

// ActivationState returns the activation state and its error text.
func (t *Task) ActivationState() (imc.EntityActivationStateState, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.actState, t.actError
}

// Connected tells whether the bus handshake is done.
func (t *Task) Connected() bool {
	b := t.Bus()
	return b != nil && b.Ready()
}

// ParamText returns a parameter's value as DUNE text ("" if unknown).
func (t *Task) ParamText(name string) string { return t.params.text(name) }

// ParamNames returns the names of the parameters, sorted.
func (t *Task) ParamNames() []string { return t.params.names() }

// DispatchOption sets the destination of a dispatched message.
type DispatchOption func(*imc.Header, *Task) bool

// To sets the destination system.
func To(system uint16) DispatchOption {
	return func(h *imc.Header, _ *Task) bool { h.Dst = system; return true }
}

// ToEntity sets the destination entity (an id of this process, see EidOf).
func ToEntity(eid uint8) DispatchOption {
	return func(h *imc.Header, _ *Task) bool { h.DstEnt = eid; return true }
}

// ToLabel sets the destination entity by label; the message is not sent if
// the label is unknown.
func ToLabel(label string) DispatchOption {
	return func(h *imc.Header, t *Task) bool {
		eid, ok := t.EidOf(label)
		if !ok {
			t.log.Warnf("cannot send to %q: unknown entity", label)
			return false
		}
		h.DstEnt = eid
		return true
	}
}

// Dispatch sends a message on DUNE's bus from this task's entity: the
// source is set (the system when Src is 0 or 0xFFFF, the entity always),
// the time stamp when it is 0, and the destination from the options (by
// default everyone: 0xFFFF, 255). It returns false when the message is not
// sent (not connected, entity refused by DUNE, unknown label). Other tasks
// of the process that consume the message get a copy.
func (t *Task) Dispatch(m imc.IMessage, opts ...DispatchOption) bool {
	h := m.GetHeader()
	h.Dst, h.DstEnt = imc.Broadcast, UNK
	for _, o := range opts {
		if !o(h, t) {
			return false
		}
	}
	return t.send(m)
}

func (t *Task) send(m imc.IMessage) bool {
	t.mu.Lock()
	bus, ent := t.bus, t.ent
	t.mu.Unlock()
	if bus == nil {
		return false
	}
	bus.stamp(m, ent)
	return bus.publish(m, t)
}

// Reply dispatches m to the sender of request (dst = request.Src, dst_ent =
// request.SrcEnt), as DUNE's dispatchReply.
func (t *Task) Reply(request, m imc.IMessage) bool {
	rh := request.GetHeader()
	h := m.GetHeader()
	h.Dst, h.DstEnt = rh.Src, rh.SrcEnt
	return t.send(m)
}

// State sets the entity state (sent at once when it changes, and every
// second). Until the first call the SDK reports NORMAL "idle" or "active".
func (t *Task) State(state imc.EntityStateState, description string) {
	t.mu.Lock()
	t.autoState = false
	t.mu.Unlock()
	t.setState(state, description)
}

// Inf logs locally and sends a LogBookEntry INFO (context = label).
func (t *Task) Inf(format string, args ...any) {
	t.logbook(imc.LogBookEntryType_INFO, LevelInfo, format, args...)
}

// War logs locally and sends a LogBookEntry WARNING.
func (t *Task) War(format string, args ...any) {
	t.logbook(imc.LogBookEntryType_WARNING, LevelWarn, format, args...)
}

// Err logs locally and sends a LogBookEntry ERROR.
func (t *Task) Err(format string, args ...any) {
	t.logbook(imc.LogBookEntryType_ERROR, LevelError, format, args...)
}

// Cri logs locally and sends a LogBookEntry CRITICAL.
func (t *Task) Cri(format string, args ...any) {
	t.logbook(imc.LogBookEntryType_CRITICAL, LevelCrit, format, args...)
}

// Debug logs locally only.
func (t *Task) Debug(format string, args ...any) { t.log.Log(LevelDebug, format, args...) }

func (t *Task) logbook(kind imc.LogBookEntryType, level Level, format string, args ...any) {
	text := format
	if len(args) > 0 {
		text = fmt.Sprintf(format, args...)
	}
	t.log.Log(level, "%s", text)
	t.Dispatch(&imc.LogBookEntry{Type: kind, Htime: now(), Context: t.label, Text: text})
}

// RequestActivation activates the task as when Active becomes true (from
// any goroutine; it runs on the task's goroutine).
func (t *Task) RequestActivation() { t.post(t.requestActivation) }

// RequestDeactivation deactivates the task as when Active becomes false.
func (t *Task) RequestDeactivation() { t.post(t.requestDeactivation) }

// -- internals (task goroutine unless said otherwise) --------------------------------

func (t *Task) attach(b *Bus) error {
	ent, err := b.addEntity(t.label, t)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.bus, t.ent = b, ent
	t.mu.Unlock()
	b.mu.Lock()
	b.tasks = append(b.tasks, t)
	b.mu.Unlock()
	t.log = b.out.Logger("dunetask." + strings.ReplaceAll(t.label, " ", "_"))
	b.refreshFilter()
	return nil
}

func (t *Task) consumedIDs() map[uint16]bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ids := map[uint16]bool{}
	for id := range t.handlers {
		ids[id] = true
	}
	for _, id := range services {
		ids[id] = true
	}
	return ids
}

func (t *Task) wants(id uint16) bool {
	t.mu.Lock()
	_, ok := t.handlers[id]
	t.mu.Unlock()
	return ok || isService(id)
}

// deliver queues a message for the task (any goroutine).
func (t *Task) deliver(m imc.IMessage) {
	if !t.wants(m.GetMGID()) {
		return
	}
	if t.inbox.put(m) {
		if b := t.Bus(); b != nil {
			b.taskDrops.Add(1)
		}
		t.log.Warnf("handlers too slow: dropping messages")
	}
}

// post runs fn on the task's goroutine (any goroutine).
func (t *Task) post(fn func()) { t.ctl.put(fn) }

func (t *Task) setState(state esState, desc string) {
	t.mu.Lock()
	changed := state != t.state || desc != t.stateDesc
	t.state, t.stateDesc = state, desc
	t.mu.Unlock()
	if changed {
		t.sendState(nil)
	}
}

func (t *Task) updateAutoState() {
	t.mu.Lock()
	auto, started := t.autoState, t.started
	idle := t.honoursActive && t.actState != asActive
	t.mu.Unlock()
	if auto && started {
		desc := "active"
		if idle {
			desc = "idle"
		}
		t.setState(imc.EntityStateState_NORMAL, desc)
	}
}

func (t *Task) sendState(replyTo imc.IMessage) {
	t.mu.Lock()
	m := &imc.EntityState{State: t.state, Description: t.stateDesc}
	t.mu.Unlock()
	if replyTo != nil {
		t.Reply(replyTo, m)
	} else {
		t.Dispatch(m)
	}
}

func (t *Task) setAct(state asState, errText string) {
	t.mu.Lock()
	t.actState, t.actError = state, errText
	t.mu.Unlock()
	t.Dispatch(&imc.EntityActivationState{State: state, Error: errText})
}

func (t *Task) sendActState() {
	st, e := t.ActivationState()
	t.Dispatch(&imc.EntityActivationState{State: st, Error: e})
}

func (t *Task) setParamText(name, text string) {
	if t.params.has(name) {
		t.params.setString(name, text)
	}
}

func (t *Task) requestActivation() {
	if !t.honoursActive {
		return
	}
	t.mu.Lock()
	st := t.actState
	if st != asInactive {
		switch st {
		case asDeactIP, asDeactDone, asDeactFail, asActFail, asActIP:
			t.pendingAct = 1 + int(asActive)
		}
		t.mu.Unlock()
		return
	}
	t.pendingAct = 0
	t.mu.Unlock()
	t.setAct(asActIP, "")
	var err error
	if t.OnActivation != nil {
		err = safeCall(t, "activation", t.OnActivation)
	}
	if err != nil {
		t.War("activation failed: %v", err)
		t.setParamText("Active", "false")
		t.setAct(asActFail, err.Error())
		t.setAct(asInactive, "")
	} else {
		t.setParamText("Active", "true")
		t.setAct(asActDone, "")
		t.setAct(asActive, "")
		t.mu.Lock()
		again := t.pendingAct == 1+int(asInactive)
		t.mu.Unlock()
		if again {
			t.requestDeactivation()
		}
	}
	t.updateAutoState()
}

func (t *Task) requestDeactivation() {
	if !t.honoursActive {
		return
	}
	t.mu.Lock()
	st := t.actState
	if st != asActive {
		switch st {
		case asDeactFail, asActIP, asActDone, asActFail, asDeactIP:
			t.pendingAct = 1 + int(asInactive)
		}
		t.mu.Unlock()
		return
	}
	t.pendingAct = 0
	t.mu.Unlock()
	t.setAct(asDeactIP, "")
	var err error
	if t.OnDeactivation != nil {
		err = safeCall(t, "deactivation", t.OnDeactivation)
	}
	if err != nil {
		t.War("deactivation failed: %v", err)
		t.setAct(asDeactFail, err.Error())
		t.setAct(asActive, "")
	} else {
		t.setParamText("Active", "false")
		t.setAct(asDeactDone, "")
		t.setAct(asInactive, "")
		t.mu.Lock()
		again := t.pendingAct == 1+int(asActive)
		t.mu.Unlock()
		if again {
			t.requestActivation()
		}
	}
	t.updateAutoState()
}

// safeCall runs a hook, turning a panic into an error (logged).
func safeCall(t *Task, what string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			t.log.Errorf("%s panicked: %v", what, r)
			err = fmt.Errorf("%v", r)
		}
	}()
	return fn()
}

func (t *Task) guard(what string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			t.log.Errorf("%s failed: %v", what, r)
		}
	}()
	fn()
}

func (t *Task) entityParameters() *imc.EntityParameters {
	ep := &imc.EntityParameters{Name: t.label}
	for _, n := range t.params.names() {
		ep.Params = append(ep.Params, &imc.EntityParameter{Name: n, Value: t.params.text(n)})
	}
	return ep
}

func (t *Task) entityInfo() *imc.EntityInfo {
	t.mu.Lock()
	bus, ent := t.bus, t.ent
	t.mu.Unlock()
	bus.mu.Lock()
	id := ent.id
	if ent.serverID >= 0 {
		id = ent.serverID
	}
	bus.mu.Unlock()
	return &imc.EntityInfo{Id: uint8(id), Label: t.label, Component: bus.name}
}

func (t *Task) handle(m imc.IMessage) {
	id := m.GetMGID()
	if isService(id) {
		t.guard("service "+m.Abbrev(), func() { t.service(m) })
	}
	t.mu.Lock()
	hs := t.handlers[id]
	t.mu.Unlock()
	for _, h := range hs {
		t.guard("handler of "+m.Abbrev(), func() { h(m) })
	}
}

func (t *Task) service(m imc.IMessage) {
	eid := t.Eid()
	dst := m.GetHeader().DstEnt
	mine, anyone := dst == eid, dst == UNK
	switch x := m.(type) {
	case *imc.QueryEntityState:
		if mine {
			t.sendState(m)
		} else if anyone {
			t.sendState(nil)
		}
	case *imc.QueryEntityInfo:
		if mine || anyone {
			t.Reply(m, t.entityInfo())
		}
	case *imc.QueryEntityActivationState:
		if mine {
			t.sendActState()
		}
	case *imc.VersionInfo:
		if mine && x.Op == imc.VersionInfoOp_QUERY {
			t.Reply(m, &imc.VersionInfo{Op: imc.VersionInfoOp_REPLY, Version: Version,
				Description: fmt.Sprintf("dunetask %s, %s", Version, t.Bus().name)})
		}
	case *imc.QueryEntityParameters:
		if x.Name == t.label {
			t.Reply(m, t.entityParameters())
		}
	case *imc.SetEntityParameters:
		if x.Name == t.label {
			var items [][2]string
			for _, p := range x.Params {
				if p != nil {
					items = append(items, [2]string{p.Name, p.Value})
				}
			}
			t.setParams(items)
			t.Reply(m, t.entityParameters())
		}
	case *imc.PushEntityParameters:
		if x.Name == t.label {
			t.params.push()
		}
	case *imc.PopEntityParameters:
		if x.Name == t.label {
			if saved, ok := t.params.pop(); ok {
				var items [][2]string
				for n, v := range saved {
					items = append(items, [2]string{n, v})
				}
				sort.Slice(items, func(i, j int) bool { return items[i][0] < items[j][0] })
				t.setParams(items)
			}
		}
	}
}

func (t *Task) setParams(items [][2]string) {
	changed := map[string]bool{}
	for _, it := range items {
		ch, err := t.params.setString(it[0], it[1])
		if err != nil {
			t.War("updating entity parameters: %v", err)
			continue
		}
		if ch {
			changed[it[0]] = true
		}
	}
	if t.OnUpdateParameters != nil {
		names := make([]string, 0, len(changed))
		for n := range changed {
			names = append(names, n)
		}
		sort.Strings(names)
		t.guard("on update parameters", func() { t.OnUpdateParameters(names) })
	}
	if t.honoursActive {
		if changed["Active"] {
			if t.activeParam.Get() {
				t.requestActivation()
			} else {
				t.requestDeactivation()
			}
		} else {
			t.sendActState()
		}
	}
}

// onBusReady: report state and activation state, then OnReady.
func (t *Task) onBusReady() {
	t.sendState(nil)
	if t.honoursActive {
		t.sendActState()
	}
	if t.OnReady != nil {
		t.guard("on ready", t.OnReady)
	}
}

func (t *Task) onBusDisconnect() {
	if t.OnDisconnect != nil {
		t.guard("on disconnect", t.OnDisconnect)
	}
}

// announceInfo: once the server mirrors our entity, EntityInfo with its id
// there.
func (t *Task) announceInfo() { t.Dispatch(t.entityInfo()) }

// start runs OnStart, activates when Active is true by default, and starts
// the task's goroutine.
func (t *Task) start(ctx context.Context) error {
	if t.OnStart != nil {
		if err := safeCall(t, "start", t.OnStart); err != nil {
			return fmt.Errorf("%s: %w", t.label, err)
		}
	}
	t.mu.Lock()
	t.started = true
	jobs := append([]periodicJob(nil), t.periodic...)
	t.mu.Unlock()
	t.updateAutoState()
	if t.honoursActive && t.activeParam.Get() {
		t.requestActivation()
	}
	lctx, cancel := context.WithCancel(ctx)
	t.cancel = cancel
	t.done = make(chan struct{})
	var wg sync.WaitGroup
	// Heartbeat: EntityState every second.
	jobs = append(jobs, periodicJob{time.Second, func() {
		if t.Connected() {
			t.sendState(nil)
		}
	}})
	for _, j := range jobs {
		wg.Add(1)
		go func(j periodicJob) {
			defer wg.Done()
			tk := time.NewTicker(j.period)
			defer tk.Stop()
			for {
				select {
				case <-lctx.Done():
					return
				case <-tk.C:
					select {
					case t.tick <- j.fn:
					default: // the task is busy: skip this tick
					}
				}
			}
		}(j)
	}
	go func() {
		defer close(t.done)
		t.loop(lctx)
		wg.Wait()
	}()
	return nil
}

func (t *Task) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.ctl.notify:
			for _, fn := range t.ctl.takeAll() {
				t.guard("event", fn)
			}
		case <-t.inbox.notify:
			for {
				msgs := t.inbox.takeAll()
				if len(msgs) == 0 {
					break
				}
				for _, m := range msgs {
					t.handle(m)
					if ctx.Err() != nil {
						return
					}
				}
				// Control events between batches.
				for _, fn := range t.ctl.takeAll() {
					t.guard("event", fn)
				}
			}
		case fn := <-t.tick:
			t.guard("periodic", fn)
		}
	}
}

// stop stops the goroutine, deactivates, calls OnStop and reports BOOT
// "stopping".
func (t *Task) stop() {
	if t.cancel != nil {
		t.cancel()
		<-t.done
	}
	if t.honoursActive {
		if st, _ := t.ActivationState(); st == asActive {
			t.requestDeactivation()
		}
	}
	if t.OnStop != nil {
		t.guard("on stop", t.OnStop)
	}
	t.setState(imc.EntityStateState_BOOT, "stopping")
}

// -- mailbox -------------------------------------------------------------------------------

// mailbox is an unbounded (or bounded, dropping the oldest) queue with a
// notification channel.
type mailbox[T any] struct {
	mu     sync.Mutex
	items  []T
	limit  int
	notify chan struct{}
}

func (m *mailbox[T]) init() { m.notify = make(chan struct{}, 1) }

// put adds an item. @return true if the oldest was dropped.
func (m *mailbox[T]) put(v T) bool {
	m.mu.Lock()
	dropped := false
	if m.limit > 0 && len(m.items) >= m.limit {
		var zero T
		m.items[0] = zero
		m.items = m.items[1:]
		dropped = true
	}
	m.items = append(m.items, v)
	m.mu.Unlock()
	select {
	case m.notify <- struct{}{}:
	default:
	}
	return dropped
}

func (m *mailbox[T]) takeAll() []T {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.items
	m.items = nil
	return out
}
