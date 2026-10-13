package dunetask

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	imc "github.com/oceanscan/imc-go"
)

// Protocol is the version of the DistributedBus protocol the SDK speaks
// (#proto): 3 adds subscriptions (#sub).
const Protocol = 3

// UNK is entity id 255: unknown, or all entities as a destination.
const UNK = 255

const (
	// DefaultQueueSize bounds the outgoing queue, in bytes.
	DefaultQueueSize = 4 << 20
	// DefaultSocketBuffer is the kernel send and receive buffer of TCP
	// connections, set before connecting.
	DefaultSocketBuffer = 256 << 10
	// remotePrefix is prefixed to mirrored entities whose label is taken
	// (as the C++ task does).
	remotePrefix = "Remote "
	// batchSize bounds the bytes written per write call.
	batchSize = 256 << 10
	// dropReport is the time between two warnings about dropped frames.
	dropReport = 5 * time.Second
)

// MinMismatchRetry is the shortest time between two attempts after an IMC
// mismatch (the bus spec forbids reconnecting in a tight loop).
const MinMismatchRetry = 5 * time.Second

// ErrIMCMismatch is returned by Run when the server was built with another
// IMC (and Options.OnMismatch is MismatchExit).
var ErrIMCMismatch = errors.New("IMC mismatch")

// MismatchPolicy says what to do when the server's IMC MD5 differs.
type MismatchPolicy int

const (
	// MismatchExit stops: Run returns ErrIMCMismatch (Main exits with 2).
	MismatchExit MismatchPolicy = iota
	// MismatchRetry keeps trying every Options.MismatchRetry (at least 5 s).
	MismatchRetry
)

// Options of a bus connection. The zero value is usable: every field has a
// default.
type Options struct {
	// Bus is the server URL: tcp://host:port, host:port or unix:///path.
	// Default: $DUNE_BUS, then tcp://127.0.0.1:6060.
	Bus string
	// Name is the process name sent as #name (DUNE uses it as the
	// component of the entities). Default: $DUNE_TASK_NAME, then the
	// program's name.
	Name string
	// System forces the source system id ("" or a number; a name is only
	// checked against the server's). Default: $DUNE_SYSTEM.
	System string
	// AllowRename lets DUNE rename a taken label to "Remote <label>"
	// instead of refusing it (#strict=1 is sent unless it is set).
	AllowRename bool
	// ReconnectMin and ReconnectMax bound the reconnection backoff
	// (default 0.5 s and 5 s).
	ReconnectMin, ReconnectMax time.Duration
	// OnMismatch is what to do on an IMC mismatch (default MismatchExit).
	OnMismatch MismatchPolicy
	// MismatchRetry is the time between attempts with MismatchRetry
	// (default 30 s, at least 5 s).
	MismatchRetry time.Duration
	// SubscribeAll asks a protocol 3 server for every message (#sub=*)
	// instead of only what the tasks consume.
	SubscribeAll bool
	// QueueSize bounds the outgoing queue in bytes (default 4 MiB): when it
	// is full the oldest messages are dropped.
	QueueSize int
	// SocketBuffer is the kernel send and receive buffer of TCP
	// connections (default 256 KiB; negative: let the kernel choose).
	SocketBuffer int
	// Output receives the log lines (default: stderr, level $DUNE_LOG).
	Output *Output
}

type entity struct {
	label       string
	id          int
	owner       *Task
	serverID    int // id at the server, -1 until mirrored
	serverLabel string
	refused     bool
	warnedLabel string
}

type mirror struct {
	label    string // local label (may be "Remote <server label>")
	id       int
	remoteID int
}

// Stats are counters of a bus.
type Stats struct {
	Rx, RxBytes, Tx, DroppedRx, DroppedTx, Connects    uint64
	QueueDrops, QueueDroppedBytes, QueuePeak, Filtered uint64
	DecodeErrors, CRCErrors, SkippedBytes, QueuedBytes uint64
	TaskQueueDrops                                     uint64
}

// Bus is a connection to a DistributedBus server, shared by the tasks of a
// process.
type Bus struct {
	url, network, address string
	name                  string
	systemName            string
	instance              uint32
	strict                bool
	reconnectMin          time.Duration
	reconnectMax          time.Duration
	onMismatch            MismatchPolicy
	mismatchRetry         time.Duration
	subscribeAll          bool
	queueSize             int
	socketBuffer          int
	log                   *Logger
	out                   *Output

	mu            sync.Mutex
	sysid         int // -1 unknown
	fixedSysid    bool
	tasks         []*Task
	entities      map[int]*entity
	mirrors       map[string]*mirror // server label -> mirror
	r2l           [256]uint8
	peer          map[string]string
	peerProto     int // 0: no report yet
	peerReport    string
	connected     bool
	ready         bool
	reported      bool
	confirming    bool // a first answer lacked one of our entities: asked once more
	reportPending bool
	lastReport    string
	newMirrored   []*entity
	idHints       map[int]bool
	sub           map[uint16]bool // nil: everything
	accept        map[uint16]bool // nil: everything
	imcMismatch   string
	fatal         error
	unmirrored    []string
	conn          net.Conn
	q             *outQueue
	backoff       time.Duration

	rx, rxBytes, tx, droppedRx, droppedTx, connects atomic.Uint64
	filtered, decodeErrors, crcErrors, skipped      atomic.Uint64
	taskDrops                                       atomic.Uint64
}

// NewBus prepares a connection (Run connects it).
func NewBus(opts Options) (*Bus, error) {
	env := EnvDefaults()
	url := opts.Bus
	if url == "" {
		url = env.Bus
	}
	network, address, err := ParseURL(url)
	if err != nil {
		return nil, err
	}
	name := opts.Name
	if name == "" {
		name = env.Name
	}
	if name == "" {
		name = programName()
	}
	b := &Bus{
		url: url, network: network, address: address, name: cleanName(name),
		instance:     rand.Uint32(),
		strict:       !opts.AllowRename,
		reconnectMin: opts.ReconnectMin, reconnectMax: opts.ReconnectMax,
		onMismatch: opts.OnMismatch, mismatchRetry: opts.MismatchRetry,
		subscribeAll: opts.SubscribeAll, queueSize: opts.QueueSize, socketBuffer: opts.SocketBuffer,
		out: opts.Output, sysid: -1, entities: map[int]*entity{}, mirrors: map[string]*mirror{},
	}
	for b.instance == 0 {
		b.instance = rand.Uint32()
	}
	if b.out == nil {
		b.out = defaultOutput
	}
	b.log = b.out.Logger("dunetask")
	if b.reconnectMin <= 0 {
		b.reconnectMin = 500 * time.Millisecond
	}
	if b.reconnectMax < b.reconnectMin {
		b.reconnectMax = max(5*time.Second, b.reconnectMin)
	}
	if b.mismatchRetry == 0 {
		b.mismatchRetry = 30 * time.Second
	}
	b.mismatchRetry = max(b.mismatchRetry, MinMismatchRetry)
	if b.queueSize <= 0 {
		b.queueSize = DefaultQueueSize
	}
	if b.socketBuffer == 0 {
		b.socketBuffer = DefaultSocketBuffer
	}
	sys := opts.System
	if strings.TrimSpace(sys) == "" {
		if env.System >= 0 {
			sys = strconv.Itoa(env.System)
		} else {
			sys = env.SystemName
		}
	}
	id, sysName := ParseSystem(sys)
	if id >= 0 {
		b.sysid, b.fixedSysid = id, true
	}
	b.systemName = sysName
	for i := range b.r2l {
		b.r2l[i] = UNK
	}
	return b, nil
}

// URL returns the server URL.
func (b *Bus) URL() string { return b.url }

// Name returns the process name (#name).
func (b *Bus) Name() string { return b.name }

// Instance returns the bus instance (#instance).
func (b *Bus) Instance() uint32 { return b.instance }

// Ready tells whether the handshake with the server is done.
func (b *Bus) Ready() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ready
}

// System returns the system id (-1 while unknown).
func (b *Bus) System() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sysid
}

// Sub returns the message ids sent as #sub (nil: everything).
func (b *Bus) Sub() []uint16 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sub == nil {
		return nil
	}
	out := make([]uint16, 0, len(b.sub))
	for id := range b.sub {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// PeerReport returns the server's last report.
func (b *Bus) PeerReport() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.peerReport
}

// Unmirrored returns the labels of our entities the server did not mirror.
func (b *Bus) Unmirrored() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.unmirrored...)
}

// Stats returns the counters.
func (b *Bus) Stats() Stats {
	s := Stats{Rx: b.rx.Load(), RxBytes: b.rxBytes.Load(), Tx: b.tx.Load(), DroppedRx: b.droppedRx.Load(),
		DroppedTx: b.droppedTx.Load(), Connects: b.connects.Load(), Filtered: b.filtered.Load(),
		DecodeErrors: b.decodeErrors.Load(), CRCErrors: b.crcErrors.Load(), SkippedBytes: b.skipped.Load(),
		TaskQueueDrops: b.taskDrops.Load()}
	b.mu.Lock()
	q := b.q
	b.mu.Unlock()
	if q != nil {
		q.mu.Lock()
		s.QueueDrops, s.QueueDroppedBytes, s.QueuePeak = q.drops, q.droppedBytes, uint64(q.peak)
		s.QueuedBytes = uint64(q.bytes)
		q.mu.Unlock()
	}
	return s
}

// -- entities ------------------------------------------------------------------

func (b *Bus) addEntity(label string, owner *Task) (*entity, error) {
	if err := CheckLabel(label); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.entities {
		if e.label == label {
			return nil, fmt.Errorf("entity %q already registered", label)
		}
	}
	id, err := b.freeID()
	if err != nil {
		return nil, err
	}
	e := &entity{label: label, id: id, owner: owner, serverID: -1}
	b.entities[id] = e
	b.reportPending = true
	b.kick()
	return e, nil
}

func (b *Bus) freeID() (int, error) {
	used := map[int]bool{}
	for id := range b.entities {
		used[id] = true
	}
	for _, m := range b.mirrors {
		used[m.id] = true
	}
	for i := 0; i < UNK; i++ {
		if !used[i] {
			return i, nil
		}
	}
	return 0, errors.New("no entity ids left")
}

// Resolve returns the local id of an entity (ours or the server's) by
// label.
func (b *Bus) Resolve(label string) (uint8, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.resolve(label)
}

func (b *Bus) resolve(label string) (uint8, bool) {
	for _, e := range b.entities {
		if e.label == label {
			return uint8(e.id), true
		}
	}
	if m, ok := b.mirrors[label]; ok {
		return uint8(m.id), true
	}
	for _, m := range b.mirrors {
		if m.label == label {
			return uint8(m.id), true
		}
	}
	return UNK, false
}

// LabelOf returns the label of a local entity id (ours or the server's).
func (b *Bus) LabelOf(eid uint8) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e, ok := b.entities[int(eid)]; ok {
		return e.label, true
	}
	for label, m := range b.mirrors {
		if m.id == int(eid) {
			return label, true
		}
	}
	return "", false
}

// -- sending -------------------------------------------------------------------------

// publish delivers a message to the other tasks of the process and sends
// it to the server.
func (b *Bus) publish(m imc.IMessage, sender *Task) bool {
	frame, err := imc.AppendFrame(nil, m, false)
	if err != nil {
		b.log.Warnf("cannot send %s: %v", m.Abbrev(), err)
		return false
	}
	b.mu.Lock()
	tasks := b.tasks
	b.mu.Unlock()
	var copyMsg imc.IMessage
	id := m.GetMGID()
	for _, t := range tasks {
		if t == sender || !t.wants(id) {
			continue
		}
		if copyMsg == nil {
			// The sender may change its message after Dispatch: the
			// other tasks get a copy (decoded from the frame).
			if copyMsg, err = imc.Decode(frame); err != nil {
				break
			}
		}
		t.deliver(copyMsg)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if e, ok := b.entities[int(m.GetHeader().SrcEnt)]; ok && e.refused {
		b.droppedTx.Add(1)
		return false
	}
	if !b.ready || b.q == nil {
		b.droppedTx.Add(1)
		return false
	}
	b.tx.Add(1)
	b.q.push(frame, true)
	return true
}

// stamp fills the source of an outgoing message: the system (when 0 or
// 0xFFFF), the entity and the time stamp (when 0).
func (b *Bus) stamp(m imc.IMessage, ent *entity) {
	h := m.GetHeader()
	b.mu.Lock()
	sysid := b.sysid
	b.mu.Unlock()
	if (h.Src == 0 || h.Src == imc.Broadcast) && sysid >= 0 {
		h.Src = uint16(sysid)
	} else if h.Src == 0 {
		h.Src = imc.Broadcast
	}
	if ent != nil {
		h.SrcEnt = uint8(ent.id)
	}
	if h.Timestamp == 0 {
		h.Timestamp = now()
	}
}

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// control sends a handshake message (never dropped from the queue). Called
// with mu held.
func (b *Bus) control(m imc.IMessage) {
	if b.q == nil {
		return
	}
	h := m.GetHeader()
	h.Src = imc.Broadcast
	if b.sysid >= 0 {
		h.Src = uint16(b.sysid)
	}
	h.SrcEnt, h.Dst, h.DstEnt, h.Timestamp = UNK, imc.Broadcast, UNK, now()
	frame, err := imc.Marshal(m)
	if err != nil {
		b.log.Warnf("cannot send %s: %v", m.Abbrev(), err)
		return
	}
	b.tx.Add(1)
	b.q.push(frame, false)
}

// -- report ----------------------------------------------------------------------------

func (b *Bus) buildReport() string {
	parts := []string{fmt.Sprintf("#instance=%x", b.instance)}
	peerProto := max(b.peerProto, 1)
	if peerProto >= 2 {
		parts = append(parts, fmt.Sprintf("#proto=%d", Protocol), "#imc="+imc.IMCMD5)
		if b.sysid >= 0 {
			parts = append(parts, fmt.Sprintf("#sys=%d", b.sysid))
		}
		parts = append(parts, "#name="+b.name)
		if b.strict {
			parts = append(parts, "#strict=1")
		}
		if peerProto >= 3 {
			parts = append(parts, "#sub="+FormatSub(b.sub))
		}
	}
	type row struct {
		id   int
		text string
	}
	var rows []row
	for _, e := range b.entities {
		rows = append(rows, row{e.id, fmt.Sprintf("%s=%d", e.label, e.id)})
	}
	pi := b.peerInstance()
	for _, m := range b.mirrors {
		rows = append(rows, row{m.id, fmt.Sprintf("%s=%d/%x/%d", m.label, m.id, pi, m.remoteID)})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].id != rows[j].id {
			return rows[i].id < rows[j].id
		}
		return rows[i].text < rows[j].text
	})
	for _, r := range rows {
		parts = append(parts, r.text)
	}
	return strings.Join(parts, ";")
}

func (b *Bus) peerInstance() uint32 {
	v, err := strconv.ParseUint(b.peer["#instance"], 16, 32)
	if err != nil {
		return 0
	}
	return uint32(v)
}

func (b *Bus) sendReport(force bool) {
	report := b.buildReport()
	if !force && report == b.lastReport {
		return
	}
	b.lastReport = report
	b.log.Debugf("sending entity report: %s", report)
	b.control(&imc.EntityList{Op: imc.EntityListOp_REPORT, List: report})
}

// applyReport applies the server's report. It returns whether our mirrors
// changed, and false in ok to refuse the server (IMC mismatch).
func (b *Bus) applyReport(m *imc.EntityList) (changed, ok bool) {
	keys, entries := ParseReport(m.List)
	first := b.peerProto == 0
	b.peer = keys
	b.peerReport = m.List
	b.peerProto = 1
	if v, ok := keys["#proto"]; ok {
		if p, err := strconv.Atoi(v); err == nil {
			b.peerProto = max(p, 1)
		}
	}
	if !b.fixedSysid {
		if v, err := strconv.Atoi(keys["#sys"]); err == nil && keys["#sys"] != "" {
			b.sysid = v
		} else if m.Header.Src != imc.Broadcast {
			b.sysid = int(m.Header.Src)
		}
	}
	peerIMC, hasIMC := keys["#imc"]
	if hasIMC && strings.ToLower(peerIMC) != imc.IMCMD5 {
		b.imcMismatch = peerIMC
		b.log.Errorf("IMC mismatch: the server was built with IMC MD5 %s, this SDK with %s; "+
			"regenerate imc-go's messages from the server's IMC.xml", peerIMC, imc.IMCMD5)
		return false, false
	}
	if first {
		if !hasIMC {
			b.log.Warnf("server speaks protocol %d and does not report its IMC MD5: cannot check that it uses IMC %s (MD5 %s)",
				b.peerProto, imc.IMCVersion, imc.IMCMD5)
		}
		inst := keys["#instance"]
		if inst == "" {
			inst = "?"
		}
		extra := ""
		if n, ok := keys["#name"]; ok {
			extra = ", " + n
		}
		b.log.Infof("connected to %s (server instance %s, protocol %d, system 0x%04X%s)", b.url, inst,
			b.peerProto, max(b.sysid, 0), extra)
		if pn, ok := keys["#name"]; ok && b.systemName != "" && strings.HasPrefix(pn, "dune:") && pn[5:] != b.systemName {
			b.log.Warnf("DUNE_SYSTEM is %q but the server is %q", b.systemName, pn[5:])
		}
	}

	me := b.instance
	var r2l [256]uint8
	for i := range r2l {
		r2l[i] = UNK
	}
	mirrors := map[string]*mirror{}
	known := map[int]int{}
	for _, e := range b.entities {
		known[e.id] = e.serverID
	}
	b.idHints = map[int]bool{}
	for _, e := range entries {
		if !(e.HasOrigin && e.Origin == me) {
			b.idHints[e.ID] = true
		}
	}
	for _, e := range b.entities {
		e.serverID, e.serverLabel = -1, ""
	}
	for _, entry := range entries {
		if entry.ID >= UNK {
			continue
		}
		if entry.HasOrigin && entry.Origin == me {
			if ent, ok := b.entities[entry.OriginID]; ok {
				r2l[entry.ID] = uint8(ent.id)
				ent.serverID, ent.serverLabel = entry.ID, entry.Label
			}
			continue
		}
		if old, ok := b.mirrors[entry.Label]; ok {
			delete(b.mirrors, entry.Label)
			old.remoteID = entry.ID
			mirrors[entry.Label] = old
			r2l[entry.ID] = uint8(old.id)
			continue
		}
		mr := b.reserve(entry, mirrors)
		if mr == nil {
			continue
		}
		mirrors[entry.Label] = mr
		r2l[entry.ID] = uint8(mr.id)
		changed = true
	}
	if len(b.mirrors) > 0 {
		changed = true // entities the server no longer has
	}
	b.mirrors = mirrors
	b.r2l = r2l
	b.newMirrored = nil
	for _, e := range b.sortedEntities() {
		if e.serverID >= 0 && known[e.id] != e.serverID {
			b.newMirrored = append(b.newMirrored, e)
		}
	}
	return changed, true
}

func (b *Bus) sortedEntities() []*entity {
	out := make([]*entity, 0, len(b.entities))
	for _, e := range b.entities {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// reserve gives a server entity a local id and label (C++: reserveMirror).
func (b *Bus) reserve(entry ReportEntry, mirrors map[string]*mirror) *mirror {
	taken := map[string]bool{}
	for _, e := range b.entities {
		taken[e.label] = true
	}
	for _, m := range mirrors {
		taken[m.label] = true
	}
	for _, m := range b.mirrors {
		taken[m.label] = true
	}
	label := entry.Label
	for i := 0; ; i++ {
		if !taken[label] {
			break
		}
		if i == 5 {
			b.log.Warnf("cannot mirror server entity %q: label taken", entry.Label)
			return nil
		}
		label = remotePrefix + label
	}
	id, err := b.freeIDExcluding(mirrors, entry.ID)
	if err != nil {
		b.log.Warnf("no entity ids left for server entity %q", entry.Label)
		return nil
	}
	return &mirror{label: label, id: id, remoteID: entry.ID}
}

// freeIDExcluding returns a free local id: prefer (the server's id, so ids
// match DUNE's when possible), or the lowest.
func (b *Bus) freeIDExcluding(mirrors map[string]*mirror, prefer int) (int, error) {
	used := map[int]bool{}
	for id := range b.entities {
		used[id] = true
	}
	for _, m := range mirrors {
		used[m.id] = true
	}
	for _, m := range b.mirrors {
		used[m.id] = true
	}
	if prefer >= 0 && prefer < UNK && !used[prefer] {
		return prefer, nil
	}
	for _, avoid := range []map[int]bool{b.idHints, nil} {
		for i := 0; i < UNK; i++ {
			if !used[i] && !avoid[i] {
				return i, nil
			}
		}
	}
	return 0, errors.New("no entity ids left")
}

// checkMirrored says which of our entities the server did not mirror, or
// renamed (at every report once it has ours).
func (b *Bus) checkMirrored() {
	b.unmirrored = nil
	for _, e := range b.sortedEntities() {
		if e.serverID < 0 {
			b.unmirrored = append(b.unmirrored, e.label)
		}
	}
	sort.Strings(b.unmirrored)
	for _, e := range b.sortedEntities() {
		refused := e.serverID < 0
		if refused && !e.refused {
			b.log.Errorf("the server did not mirror entity %q: its label is probably taken (see the server log); "+
				"its messages are not sent, and parameters and activation will not reach it", e.label)
		} else if e.refused && !refused {
			b.log.Infof("the server now mirrors entity %q", e.label)
		}
		e.refused = refused
		if e.serverLabel != "" && e.serverLabel != e.label && e.serverLabel != e.warnedLabel {
			e.warnedLabel = e.serverLabel
			b.log.Errorf("the server mirrors entity %q as %q (label taken): parameters and activation by label "+
				"will not reach it", e.label, e.serverLabel)
		}
	}
}

// -- receiving -------------------------------------------------------------------------

// anyUnmirrored tells whether the server's last report lacks one of our
// entities.
func (b *Bus) anyUnmirrored() bool {
	for _, e := range b.entities {
		if e.serverID < 0 {
			return true
		}
	}
	return false
}

// onMessage handles a message from the server. It returns functions to
// call once mu is released, and whether to close the connection.
func (b *Bus) onMessage(m imc.IMessage) (after []func(), closeConn bool) {
	if el, ok := m.(*imc.EntityList); ok {
		switch el.Op {
		case imc.EntityListOp_REPORT:
			changed, ok := b.applyReport(el)
			if !ok {
				return nil, true
			}
			if b.reported && !b.ready && !b.confirming && b.anyUnmirrored() {
				// It looks like a refusal, but a report the server sent
				// before it applied ours (a broadcast crossing our report)
				// looks the same: ask once more, and the next answer tells.
				b.confirming = true
				b.control(&imc.EntityList{Op: imc.EntityListOp_QUERY})
			} else if b.reported && !b.ready {
				// The answer to the query that followed our report: the
				// server's mirrors of our entities (or refusals) are known.
				b.ready = true
				b.backoff = b.reconnectMin
				b.checkMirrored()
				after = append(after, b.emitReady)
			} else if b.ready {
				b.checkMirrored()
			}
			if !b.reported || changed || b.reportPending {
				b.reportPending = false
				b.sendReport(true)
			}
			if !b.reported {
				b.reported = true
				b.control(&imc.EntityList{Op: imc.EntityListOp_QUERY})
			}
			if b.ready {
				for _, e := range b.newMirrored {
					if e.owner != nil {
						owner := e.owner
						after = append(after, func() { owner.post(owner.announceInfo) })
					}
				}
			}
		case imc.EntityListOp_QUERY:
			if b.peerProto != 0 {
				b.sendReport(true)
			} else {
				b.reportPending = true
			}
		}
		return after, false
	}
	if b.peerProto == 0 {
		b.droppedRx.Add(1)
		return nil, false
	}
	h := m.GetHeader()
	h.SrcEnt = b.r2l[h.SrcEnt]
	h.DstEnt = b.r2l[h.DstEnt]
	switch x := m.(type) {
	case *imc.QueryEntityInfo:
		x.Id = b.r2l[x.Id]
	case *imc.EntityInfo:
		if h.SrcEnt != UNK {
			x.Id = h.SrcEnt
		}
	}
	b.rx.Add(1)
	tasks := b.tasks
	after = append(after, func() {
		for _, t := range tasks {
			t.deliver(m)
		}
	})
	return after, false
}

func (b *Bus) emitReady() {
	b.mu.Lock()
	tasks := b.tasks
	b.mu.Unlock()
	for _, t := range tasks {
		t.post(t.onBusReady)
	}
}

// accepts tells whether some task consumes the message id (others are
// skipped after the CRC check, without decoding).
func (b *Bus) accepts(mgid uint16) bool {
	return b.accept == nil || b.accept[mgid]
}

// refreshFilter recomputes the messages the tasks consume: the receive
// filter and #sub (re-sent if it changed).
func (b *Bus) refreshFilter() {
	b.mu.Lock()
	defer b.mu.Unlock()
	var accept map[uint16]bool
	if len(b.tasks) > 0 {
		accept = map[uint16]bool{}
		for _, t := range b.tasks {
			for id := range t.consumedIDs() {
				accept[id] = true
			}
		}
	}
	b.accept = accept
	var sub map[uint16]bool
	if accept != nil && !b.subscribeAll {
		sub = map[uint16]bool{imc.IDEntityList: true}
		for id := range accept {
			sub[id] = true
		}
	}
	if !sameSet(sub, b.sub) {
		b.sub = sub
		b.kick()
	}
}

func sameSet(a, b map[uint16]bool) bool {
	if (a == nil) != (b == nil) || len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// kick sends our report promptly when it changed (mu held).
func (b *Bus) kick() {
	if b.ready && b.q != nil {
		b.sendReport(false)
	}
}

// -- connection --------------------------------------------------------------------

func (b *Bus) dial(ctx context.Context) (net.Conn, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	if b.network == "tcp" && b.socketBuffer > 0 {
		size := b.socketBuffer
		// Buffer sizes must be set before connect() to bound the window
		// the kernel advertises.
		d.Control = func(network, address string, c syscall.RawConn) error {
			var serr error
			err := c.Control(func(fd uintptr) { serr = setBuffers(fd, size) })
			if err != nil {
				return err
			}
			return serr
		}
	}
	return d.DialContext(ctx, b.network, b.address)
}

// run connects, and reconnects with backoff, until ctx is done or the bus
// gives up (IMC mismatch with MismatchExit).
func (b *Bus) run(ctx context.Context) error {
	b.mu.Lock()
	b.backoff = b.reconnectMin
	b.mu.Unlock()
	for ctx.Err() == nil {
		conn, err := b.dial(ctx)
		if err != nil {
			b.log.Debugf("cannot connect to %s: %v", b.url, err)
			sleep(ctx, b.nextBackoff())
			continue
		}
		b.session(ctx, conn)
		if ctx.Err() != nil {
			break
		}
		b.mu.Lock()
		mismatch := b.imcMismatch != ""
		b.mu.Unlock()
		if mismatch {
			if b.onMismatch == MismatchExit {
				b.mu.Lock()
				b.fatal = ErrIMCMismatch
				b.mu.Unlock()
				return ErrIMCMismatch
			}
			sleep(ctx, b.mismatchRetry)
		} else {
			sleep(ctx, b.nextBackoff())
		}
	}
	return nil
}

func (b *Bus) nextBackoff() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	d := b.backoff
	b.backoff = min(b.backoff*2, b.reconnectMax)
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (b *Bus) session(ctx context.Context, conn net.Conn) {
	b.connects.Add(1)
	q := newOutQueue(b.queueSize, b.log)
	b.mu.Lock()
	b.conn, b.q = conn, q
	b.connected, b.ready = true, false
	b.peer, b.peerProto, b.peerReport = map[string]string{}, 0, ""
	b.imcMismatch, b.lastReport = "", ""
	b.reportPending, b.reported, b.confirming = false, false, false
	for _, e := range b.entities {
		e.refused, e.warnedLabel = false, ""
	}
	b.mu.Unlock()
	b.log.Debugf("connected to %s", b.url)

	sctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { // close the connection when stopping
		defer wg.Done()
		<-sctx.Done()
		conn.Close()
	}()
	go func() {
		defer wg.Done()
		defer cancel()
		q.writeLoop(sctx, conn)
	}()
	go func() {
		defer wg.Done()
		b.hello(sctx)
	}()

	fr := imc.NewFrameReader(conn)
	var lastRead, lastSkipped, lastCRC uint64
	for {
		f, err := fr.Next()
		b.rxBytes.Add(fr.ReadBytes - lastRead)
		lastRead = fr.ReadBytes
		b.skipped.Add(fr.SkippedBytes - lastSkipped)
		lastSkipped = fr.SkippedBytes
		b.crcErrors.Add(fr.CRCErrors - lastCRC)
		lastCRC = fr.CRCErrors
		if err != nil {
			if sctx.Err() == nil {
				b.log.Debugf("connection error: %v", err)
			}
			break
		}
		mgid := f.Header.MGID
		b.mu.Lock()
		accepted := mgid == imc.IDEntityList || b.accepts(mgid)
		b.mu.Unlock()
		if !accepted {
			b.filtered.Add(1)
			continue
		}
		m, err := f.Decode()
		if err != nil {
			b.decodeErrors.Add(1)
			continue
		}
		b.mu.Lock()
		after, closeConn := b.onMessage(m)
		b.mu.Unlock()
		for _, fn := range after {
			fn()
		}
		if closeConn {
			break
		}
	}
	cancel()
	conn.Close()
	wg.Wait()

	b.mu.Lock()
	wasReady := b.ready
	dropped := q.discard()
	b.droppedTx.Add(uint64(dropped))
	b.connected, b.ready = false, false
	b.conn, b.q = nil, nil
	b.mirrors = map[string]*mirror{}
	for i := range b.r2l {
		b.r2l[i] = UNK
	}
	for _, e := range b.entities {
		e.serverID, e.serverLabel = -1, ""
	}
	tasks := b.tasks
	b.mu.Unlock()
	if wasReady && ctx.Err() == nil {
		b.log.Warnf("connection to %s lost", b.url)
	}
	for _, t := range tasks {
		t.post(t.onBusDisconnect)
	}
}

// hello queries the server until the handshake is done (its report, then
// its answer to our report), then keeps our report up to date.
func (b *Bus) hello(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		b.mu.Lock()
		if !b.ready {
			// The server sends its report on connection: query only if
			// it is late.
			if b.reported {
				b.sendReport(true)
			}
			b.control(&imc.EntityList{Op: imc.EntityListOp_QUERY})
		} else {
			b.reportPending = false
			b.sendReport(false)
		}
		b.mu.Unlock()
	}
}

// drain waits (at most timeout) until the outgoing queue is written.
func (b *Bus) drain(timeout time.Duration) {
	b.mu.Lock()
	q := b.q
	b.mu.Unlock()
	if q != nil {
		q.waitEmpty(timeout)
	}
}

// -- outgoing queue ----------------------------------------------------------------------

type outFrame struct {
	b         []byte
	droppable bool
}

// outQueue holds frames waiting for the socket, bounded in bytes: when it
// is full the oldest droppable frames are dropped (whole frames; the batch
// being written is never touched).
type outQueue struct {
	mu           sync.Mutex
	frames       []outFrame
	head         int
	bytes        int
	limit        int
	peak         int
	drops        uint64
	droppedBytes uint64
	notify       chan struct{}
	empty        *sync.Cond
	writing      bool
	log          *Logger
	// While dropping: start, drops, bytes, last warning.
	dropStart, dropLast time.Time
	dropN, dropBytes    uint64
	dropping            bool
}

func newOutQueue(limit int, log *Logger) *outQueue {
	q := &outQueue{limit: limit, notify: make(chan struct{}, 1), log: log}
	q.empty = sync.NewCond(&q.mu)
	return q
}

func (q *outQueue) push(frame []byte, droppable bool) {
	q.mu.Lock()
	q.frames = append(q.frames, outFrame{frame, droppable})
	q.bytes += len(frame)
	if q.bytes > q.limit {
		q.trim()
	}
	q.peak = max(q.peak, q.bytes)
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *outQueue) trim() {
	keep := make([]outFrame, 0, len(q.frames)-q.head)
	var drops, nbytes uint64
	i := q.head
	for ; i < len(q.frames) && q.bytes > q.limit; i++ {
		f := q.frames[i]
		if f.droppable {
			q.bytes -= len(f.b)
			drops++
			nbytes += uint64(len(f.b))
		} else {
			keep = append(keep, f)
		}
	}
	keep = append(keep, q.frames[i:]...)
	q.frames, q.head = keep, 0
	if drops == 0 {
		return
	}
	q.drops += drops
	q.droppedBytes += nbytes
	now := time.Now()
	if !q.dropping {
		q.dropping, q.dropStart, q.dropLast, q.dropN, q.dropBytes = true, now, now, 0, 0
		q.log.Warnf("the server does not read fast enough: outgoing queue full (%d bytes), dropping the oldest messages", q.limit)
	}
	q.dropN += drops
	q.dropBytes += nbytes
	if now.Sub(q.dropLast) >= dropReport {
		q.dropLast = now
		q.log.Warnf("still dropping: %d messages (%d bytes) dropped in %.0f s", q.dropN, q.dropBytes, now.Sub(q.dropStart).Seconds())
	}
}

// take moves up to max bytes of whole frames (at least one) into batch.
func (q *outQueue) take(batch []byte, limit int) []byte {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.head < len(q.frames) {
		f := q.frames[q.head]
		if len(batch) > 0 && len(batch)+len(f.b) > limit {
			break
		}
		batch = append(batch, f.b...)
		q.frames[q.head] = outFrame{}
		q.head++
		q.bytes -= len(f.b)
	}
	if q.head == len(q.frames) {
		q.frames, q.head = q.frames[:0], 0
	} else if q.head > 1024 && q.head > len(q.frames)/2 {
		q.frames = append(q.frames[:0], q.frames[q.head:]...)
		q.head = 0
	}
	q.writing = len(batch) > 0
	return batch
}

func (q *outQueue) drained() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.writing = false
	if q.head == len(q.frames) {
		if q.dropping {
			q.dropping = false
			q.log.Warnf("outgoing queue drained: %d messages (%d bytes) were dropped in %.1f s",
				q.dropN, q.dropBytes, time.Since(q.dropStart).Seconds())
		}
		q.empty.Broadcast()
	}
}

func (q *outQueue) writeLoop(ctx context.Context, conn net.Conn) {
	batch := make([]byte, 0, batchSize)
	for {
		select {
		case <-ctx.Done():
			q.drained()
			return
		case <-q.notify:
		}
		for {
			batch = q.take(batch[:0], batchSize)
			if len(batch) == 0 {
				break
			}
			if _, err := conn.Write(batch); err != nil {
				q.drained()
				return
			}
		}
		q.drained()
	}
}

func (q *outQueue) waitEmpty(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		q.mu.Lock()
		for q.head < len(q.frames) || q.writing {
			q.empty.Wait()
		}
		q.mu.Unlock()
		close(done)
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		q.mu.Lock()
		q.writing = false
		q.frames, q.head = nil, 0 // unblock the waiter
		q.empty.Broadcast()
		q.mu.Unlock()
		<-done
	}
}

// discard drops what is left (the connection is gone). @return the number
// of frames.
func (q *outQueue) discard() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.frames) - q.head
	q.frames, q.head, q.bytes = nil, 0, 0
	q.writing = false
	if q.dropping {
		q.dropping = false
		q.log.Warnf("outgoing queue drained: %d messages (%d bytes) were dropped in %.1f s",
			q.dropN, q.dropBytes, time.Since(q.dropStart).Seconds())
	}
	q.empty.Broadcast()
	return n
}
