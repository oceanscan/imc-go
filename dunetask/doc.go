// Package dunetask lets a Go program join a DUNE system as a peer of DUNE's
// distributed bus (Transports.DistributedBus). Each Task of the program is a
// DUNE entity: it is in DUNE's entity list and logs, reports its state, has
// parameters that Neptus can read and set by label, honours Active, and
// exchanges IMC messages with DUNE's bus in both directions. It does not run
// inside DUNE: a crash of the program cannot take DUNE down, and DUNE drops
// the task's entities until it comes back.
//
//	t := dunetask.New("Depth Watcher", dunetask.HonoursActive())
//	maxDepth := dunetask.NewParam(t, "Maximum Depth", 5.0, dunetask.Units("m"))
//	dunetask.Consume(t, func(m *imc.EstimatedState) {
//		if t.Active() && float64(m.Depth) > maxDepth.Get() {
//			t.State(imc.EntityStateState_FAULT, "too deep")
//			t.War("depth %.1f m", m.Depth)              // LogBookEntry
//		}
//	})
//	dunetask.Main(dunetask.Options{}, t)               // $DUNE_BUS, SIGTERM, ...
//
// The SDK speaks protocol 3 of the bus (doc/specs/Transports.DistributedBus.md
// in DUNE): handshake with the IMC MD5 check, subscriptions (#sub: DUNE
// sends only what the tasks consume), entity id translation, label refusal,
// reconnection, and a bounded outgoing queue. It behaves like DUNE's Python
// SDK (sdk/python/dune_task) and passes the same language-neutral
// conformance suites (doc/specs/SDK.Conformance.md; the conformance task is
// examples/conformance).
//
// Handlers, periodic jobs and hooks of a task run one at a time on the
// task's own goroutine; Dispatch, Inf/War/Err, State, Param.Get and the
// lookups are safe from any goroutine. Package dunetest has a fake DUNE for
// unit tests. See README.md for the whole guide.
package dunetask
