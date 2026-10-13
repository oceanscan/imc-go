// Package dunetest provides a fake DUNE DistributedBus server, for testing
// dunetask tasks (and any other bus peer) without DUNE:
//
//	srv, _ := dunetest.Start(dunetest.Config{Entities: []string{"Navigation"}})
//	defer srv.Close()
//	go dunetask.Run(ctx, dunetask.Options{Bus: srv.URL()}, task)
//	srv.WaitEntity("My Task", 5*time.Second)
//	srv.Send(&imc.EstimatedState{Depth: 3}, "Navigation", "")
//	m, _ := srv.WaitMessage(dunetest.Of[*imc.LogBookEntry](), 5*time.Second)
//
// It behaves like DUNE's task in server mode, with one entity table: it
// sends its report when a client connects and when asked, mirrors the
// client's entities (prefixing "Remote " to taken labels, or refusing them
// when Strict or the client sends #strict=1), translates entity ids both
// ways, drops a client's messages until its report, refuses a client whose
// #imc differs from its own, and (protocol 3) forwards to each client only
// the message ids of its #sub (EntityList always).
package dunetest

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	imc "github.com/oceanscan/imc-go"
	"github.com/oceanscan/imc-go/dunetask"
)

const unk = 255

// Config of a fake DUNE. The zero value is a protocol 3 server on a free
// TCP port of 127.0.0.1 with entities Daemon, Navigation and Logger.
type Config struct {
	// Entities of the server (default Daemon, Navigation, Logger).
	Entities []string
	// System id (default 0x1234).
	System uint16
	// Proto is the protocol version announced (default 3).
	Proto int
	// IMCMD5 is the MD5 announced (default imc.IMCMD5).
	IMCMD5 string
	// Strict refuses taken labels instead of renaming them.
	Strict bool
	// Addr is the TCP address to listen on (default 127.0.0.1:0).
	Addr string
	// Path, when set, listens on this Unix socket instead of TCP.
	Path string
	// Name is the server's #name (default "dune:fake").
	Name string
}

// Client is a connection to the fake server.
type Client struct {
	srv      *FakeDune
	conn     net.Conn
	keys     map[string]string
	ready    bool
	sysid    uint16
	r2l      [256]int
	mirrors  map[string]int // client label -> local id
	cids     map[string]int // client label -> client id
	reports  []string
	refused  []string
	sub      map[uint16]bool
	hasSub   bool
	TxBytes  int
	TxMsgs   int
	Filtered int
	wmu      sync.Mutex
}

// FakeDune is a fake DUNE bus server.
type FakeDune struct {
	cfg      Config
	instance uint32
	ln       net.Listener

	mu              sync.Mutex
	cond            *sync.Cond
	entities        map[int]string
	owner           map[int]*Client
	clients         []*Client
	received        []imc.IMessage
	refusedIMC      int
	reportsReceived int
	paused          bool
	closed          bool
	wg              sync.WaitGroup
}

// Start starts a fake DUNE.
func Start(cfg Config) (*FakeDune, error) {
	if cfg.Entities == nil {
		cfg.Entities = []string{"Daemon", "Navigation", "Logger"}
	}
	if cfg.System == 0 {
		cfg.System = 0x1234
	}
	if cfg.Proto == 0 {
		cfg.Proto = 3
	}
	if cfg.IMCMD5 == "" {
		cfg.IMCMD5 = imc.IMCMD5
	}
	if cfg.Name == "" {
		cfg.Name = "dune:fake"
	}
	s := &FakeDune{cfg: cfg, instance: rand.Uint32() | 1, entities: map[int]string{}, owner: map[int]*Client{}}
	s.cond = sync.NewCond(&s.mu)
	for i, e := range cfg.Entities {
		s.entities[i] = e
	}
	var err error
	if cfg.Path != "" {
		os.Remove(cfg.Path)
		s.ln, err = net.Listen("unix", cfg.Path)
	} else {
		addr := cfg.Addr
		if addr == "" {
			addr = "127.0.0.1:0"
		}
		s.ln, err = net.Listen("tcp", addr)
	}
	if err != nil {
		return nil, err
	}
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

// URL returns the bus URL of the server.
func (s *FakeDune) URL() string {
	if s.cfg.Path != "" {
		return "unix://" + s.cfg.Path
	}
	return "tcp://" + s.ln.Addr().String()
}

// Addr returns the listening address.
func (s *FakeDune) Addr() string { return s.ln.Addr().String() }

// Instance returns the server's #instance.
func (s *FakeDune) Instance() uint32 { return s.instance }

// Close stops listening and drops every client (like DUNE stopping).
func (s *FakeDune) Close() {
	s.mu.Lock()
	s.closed = true
	s.paused = false
	s.cond.Broadcast()
	s.mu.Unlock()
	s.ln.Close()
	s.DropClients()
	s.wg.Wait()
	if s.cfg.Path != "" {
		os.Remove(s.cfg.Path)
	}
}

// DropClients closes every client connection.
func (s *FakeDune) DropClients() {
	s.mu.Lock()
	clients := append([]*Client(nil), s.clients...)
	s.mu.Unlock()
	for _, c := range clients {
		c.conn.Close()
	}
}

// Pause stops (or resumes) reading from the clients, as a stalled DUNE.
func (s *FakeDune) Pause(paused bool) {
	s.mu.Lock()
	s.paused = paused
	s.cond.Broadcast()
	s.mu.Unlock()
}

// Clients returns the connected clients.
func (s *FakeDune) Clients() []*Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Client(nil), s.clients...)
}

// RefusedIMC counts clients refused for an IMC mismatch.
func (s *FakeDune) RefusedIMC() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refusedIMC
}

// Reports returns the reports the client sent.
func (c *Client) Reports() []string {
	c.srv.mu.Lock()
	defer c.srv.mu.Unlock()
	return append([]string(nil), c.reports...)
}

// Keys returns the reserved keys of the client's last report.
func (c *Client) Keys() map[string]string {
	c.srv.mu.Lock()
	defer c.srv.mu.Unlock()
	out := map[string]string{}
	for k, v := range c.keys {
		out[k] = v
	}
	return out
}

// Sub returns the client's subscriptions (nil: everything).
func (c *Client) Sub() map[uint16]bool {
	c.srv.mu.Lock()
	defer c.srv.mu.Unlock()
	if !c.hasSub {
		return nil
	}
	return c.sub
}

// Refused returns the labels refused at the last report.
func (c *Client) Refused() []string {
	c.srv.mu.Lock()
	defer c.srv.mu.Unlock()
	return append([]string(nil), c.refused...)
}

// Stats returns bytes and messages written to the client and messages not
// forwarded because of #sub.
func (c *Client) Stats() (txBytes, txMsgs, filtered int) {
	c.srv.mu.Lock()
	defer c.srv.mu.Unlock()
	return c.TxBytes, c.TxMsgs, c.Filtered
}

// -- entities ------------------------------------------------------------------

// Resolve returns the server id of an entity by label (-1 if unknown).
func (s *FakeDune) Resolve(label string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolve(label)
}

func (s *FakeDune) resolve(label string) int {
	for id, l := range s.entities {
		if l == label {
			return id
		}
	}
	return -1
}

// Report returns the server's current report.
func (s *FakeDune) Report() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.report()
}

func (s *FakeDune) report() string {
	parts := []string{fmt.Sprintf("#instance=%x", s.instance)}
	if s.cfg.Proto >= 2 {
		parts = append(parts, fmt.Sprintf("#proto=%d", s.cfg.Proto), "#imc="+s.cfg.IMCMD5,
			fmt.Sprintf("#sys=%d", s.cfg.System), "#name="+s.cfg.Name)
	}
	ids := make([]int, 0, len(s.entities))
	for id := range s.entities {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		label := s.entities[id]
		if c := s.owner[id]; c != nil {
			var cid int
			for k, v := range c.mirrors {
				if v == id {
					cid = c.cids[k]
				}
			}
			inst, _ := strconv.ParseUint(c.keys["#instance"], 16, 32)
			parts = append(parts, fmt.Sprintf("%s=%d/%x/%d", label, id, inst, cid))
		} else {
			parts = append(parts, fmt.Sprintf("%s=%d", label, id))
		}
	}
	return strings.Join(parts, ";")
}

func (s *FakeDune) freeID() int {
	for i := 0; i < unk; i++ {
		if _, ok := s.entities[i]; !ok {
			return i
		}
	}
	return unk
}

func (s *FakeDune) apply(c *Client, m *imc.EntityList) bool {
	keys, entries := dunetask.ParseReport(m.List)
	c.keys = keys
	c.reports = append(c.reports, m.List)
	s.reportsReceived++
	c.sysid = m.Header.Src
	proto, err := strconv.Atoi(keys["#proto"])
	if err != nil {
		proto = 1
	}
	if v, ok := keys["#imc"]; proto >= 2 && ok && v != s.cfg.IMCMD5 {
		s.refusedIMC++
		return false
	}
	if v, ok := keys["#sub"]; ok && s.cfg.Proto >= 3 {
		c.sub, c.hasSub = dunetask.ParseSub(v), true
		if c.sub == nil {
			c.hasSub = false
		}
	} else {
		c.sub, c.hasSub = nil, false
	}
	strict := s.cfg.Strict || (proto >= 2 && keys["#strict"] == "1")
	c.ready = true
	for i := range c.r2l {
		c.r2l[i] = unk
	}
	old := c.mirrors
	c.mirrors, c.cids, c.refused = map[string]int{}, map[string]int{}, nil
	for _, e := range entries {
		if e.ID >= unk {
			continue
		}
		if e.HasOrigin && e.Origin == s.instance {
			if _, ok := s.entities[e.OriginID]; ok {
				c.r2l[e.ID] = e.OriginID
			}
			continue
		}
		var id int
		if oid, ok := old[e.Label]; ok {
			id = oid
			delete(old, e.Label)
		} else {
			label := e.Label
			if s.resolve(label) >= 0 {
				if strict {
					c.refused = append(c.refused, label)
					continue
				}
				for s.resolve(label) >= 0 {
					label = "Remote " + label
				}
			}
			id = s.freeID()
			s.entities[id] = label
			s.owner[id] = c
		}
		c.mirrors[e.Label] = id
		c.cids[e.Label] = e.ID
		c.r2l[e.ID] = id
	}
	for _, id := range old {
		delete(s.entities, id)
		delete(s.owner, id)
	}
	return true
}

func (s *FakeDune) release(c *Client) {
	for _, id := range c.mirrors {
		delete(s.entities, id)
		delete(s.owner, id)
	}
	c.mirrors = map[string]int{}
}

// -- network --------------------------------------------------------------------

func (s *FakeDune) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		c := &Client{srv: s, conn: conn, mirrors: map[string]int{}, cids: map[string]int{}}
		for i := range c.r2l {
			c.r2l[i] = unk
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			conn.Close()
			return
		}
		s.clients = append(s.clients, c)
		s.write(c, &imc.EntityList{Op: imc.EntityListOp_REPORT, List: s.report()})
		s.mu.Unlock()
		s.wg.Add(1)
		go s.serve(c)
	}
}

func (s *FakeDune) serve(c *Client) {
	defer s.wg.Done()
	fr := imc.NewFrameReader(&gatedReader{s: s, c: c})
	for {
		f, err := fr.Next()
		if err != nil {
			break
		}
		m, err := f.Decode()
		if err != nil {
			continue
		}
		s.mu.Lock()
		closeIt := s.onMessage(c, m)
		s.mu.Unlock()
		if closeIt {
			break
		}
	}
	c.conn.Close()
	s.mu.Lock()
	s.release(c)
	for i, x := range s.clients {
		if x == c {
			s.clients = append(s.clients[:i], s.clients[i+1:]...)
			break
		}
	}
	s.broadcastReport()
	s.cond.Broadcast()
	s.mu.Unlock()
}

// gatedReader blocks reads while the server is paused.
type gatedReader struct {
	s *FakeDune
	c *Client
}

func (g *gatedReader) Read(p []byte) (int, error) {
	g.s.mu.Lock()
	for g.s.paused && !g.s.closed {
		g.s.cond.Wait()
	}
	g.s.mu.Unlock()
	return g.c.conn.Read(p)
}

func (s *FakeDune) onMessage(c *Client, m imc.IMessage) bool {
	if el, ok := m.(*imc.EntityList); ok {
		switch el.Op {
		case imc.EntityListOp_REPORT:
			if !s.apply(c, el) {
				return true
			}
			s.broadcastReport()
		case imc.EntityListOp_QUERY:
			s.write(c, &imc.EntityList{Op: imc.EntityListOp_REPORT, List: s.report()})
		}
		s.cond.Broadcast()
		return false
	}
	if !c.ready {
		return false
	}
	h := m.GetHeader()
	if h.Src == c.sysid {
		h.Src = s.cfg.System
	}
	if h.Dst == c.sysid {
		h.Dst = s.cfg.System
	}
	h.SrcEnt = uint8(c.r2l[h.SrcEnt])
	h.DstEnt = uint8(c.r2l[h.DstEnt])
	s.received = append(s.received, m)
	for _, o := range s.clients {
		if o != c {
			s.forward(o, m)
		}
	}
	s.cond.Broadcast()
	return false
}

func (s *FakeDune) broadcastReport() {
	rep := s.report()
	for _, c := range s.clients {
		s.write(c, &imc.EntityList{Op: imc.EntityListOp_REPORT, List: rep})
	}
}

// write sends a message to a client (mu held). Writes are asynchronous
// for clients that do not read (a goroutine per blocked write).
func (s *FakeDune) write(c *Client, m imc.IMessage) {
	if el, ok := m.(*imc.EntityList); ok {
		el.Header = imc.Header{Src: s.cfg.System, SrcEnt: unk, Dst: 0xFFFF, DstEnt: unk,
			Timestamp: float64(time.Now().UnixNano()) / 1e9}
	}
	frame, err := imc.Marshal(m)
	if err != nil {
		return
	}
	c.TxBytes += len(frame)
	c.TxMsgs++
	c.wmu.Lock()
	c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	c.conn.Write(frame)
	c.wmu.Unlock()
}

// WriteRaw writes bytes to every client as they are (garbage, broken
// frames, ...).
func (s *FakeDune) WriteRaw(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.clients {
		c.wmu.Lock()
		c.conn.Write(data)
		c.wmu.Unlock()
	}
}

func (s *FakeDune) forward(c *Client, m imc.IMessage) {
	id := m.GetMGID()
	if id == imc.IDEntityList || !c.hasSub || c.sub[id] {
		s.write(c, m)
	} else {
		c.Filtered++
	}
}

// Send dispatches a message from DUNE to every client. srcLabel and
// dstLabel name entities of the server ("" for none: 255).
func (s *FakeDune) Send(m imc.IMessage, srcLabel, dstLabel string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := m.GetHeader()
	h.Src = s.cfg.System
	h.SrcEnt, h.DstEnt = unk, unk
	h.Dst = 0xFFFF
	if id := s.resolve(srcLabel); srcLabel != "" && id >= 0 {
		h.SrcEnt = uint8(id)
	}
	if id := s.resolve(dstLabel); dstLabel != "" && id >= 0 {
		h.DstEnt = uint8(id)
	}
	if h.Timestamp == 0 {
		h.Timestamp = float64(time.Now().UnixNano()) / 1e9
	}
	for _, c := range s.clients {
		s.forward(c, m)
	}
}

// -- waiting --------------------------------------------------------------------------

// ErrTimeout is returned by the Wait functions.
var ErrTimeout = errors.New("dunetest: timeout")

// WaitFor waits until cond (called with the server locked) is true.
func (s *FakeDune) WaitFor(cond func() bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	timer := time.AfterFunc(timeout, func() {
		s.mu.Lock()
		s.cond.Broadcast()
		s.mu.Unlock()
	})
	defer timer.Stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	for !cond() {
		if time.Now().After(deadline) {
			return ErrTimeout
		}
		s.cond.Wait()
	}
	return nil
}

// WaitEntity waits until a client's entity is mirrored and returns its id.
func (s *FakeDune) WaitEntity(label string, timeout time.Duration) (int, error) {
	id := -1
	err := s.WaitFor(func() bool { id = s.resolve(label); return id >= 0 && s.owner[id] != nil }, timeout)
	return id, err
}

// WaitClients waits until n clients are connected.
func (s *FakeDune) WaitClients(n int, timeout time.Duration) error {
	return s.WaitFor(func() bool { return len(s.clients) == n }, timeout)
}

// Of matches messages of type M.
func Of[M imc.IMessage]() func(imc.IMessage) bool {
	return func(m imc.IMessage) bool { _, ok := m.(M); return ok }
}

// Where matches messages of type M for which pred is true.
func Where[M imc.IMessage](pred func(M) bool) func(imc.IMessage) bool {
	return func(m imc.IMessage) bool { x, ok := m.(M); return ok && pred(x) }
}

// Received returns the number of messages received from clients so far
// (to wait only for later ones: WaitMessageSince).
func (s *FakeDune) Received() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.received)
}

// Messages returns the messages received from clients (translated to the
// server's ids) that match, from index since.
func (s *FakeDune) Messages(match func(imc.IMessage) bool, since int) []imc.IMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.messages(match, since)
}

func (s *FakeDune) messages(match func(imc.IMessage) bool, since int) []imc.IMessage {
	var out []imc.IMessage
	for _, m := range s.received[min(since, len(s.received)):] {
		if match == nil || match(m) {
			out = append(out, m)
		}
	}
	return out
}

// WaitMessage waits for a message from a client that matches.
func (s *FakeDune) WaitMessage(match func(imc.IMessage) bool, timeout time.Duration) (imc.IMessage, error) {
	return s.WaitMessageSince(match, 0, timeout)
}

// WaitMessageSince waits for a matching message received at index since or
// later.
func (s *FakeDune) WaitMessageSince(match func(imc.IMessage) bool, since int, timeout time.Duration) (imc.IMessage, error) {
	var found imc.IMessage
	err := s.WaitFor(func() bool {
		if l := s.messages(match, since); len(l) > 0 {
			found = l[0]
			return true
		}
		return false
	}, timeout)
	return found, err
}
