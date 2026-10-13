# dunetask: DUNE tasks in Go

`dunetask` lets a Go program join a DUNE system as a peer of DUNE's
distributed bus (`Transports.DistributedBus`). Each task of the program is
a DUNE entity: it is in DUNE's entity list and logs, reports its state, has
parameters that Neptus can read and set by label, honours `Active`, and
exchanges IMC messages with DUNE's bus in both directions. It does not run
inside DUNE: a crash of the Go process cannot take DUNE down, and DUNE drops
the task's entities until it comes back.

- Pure Go (Go 1.24 or later), no cgo: one static binary per target,
  cross-compiled for the vehicles' ARM CPUs with `GOARCH`.
- Typed IMC messages generated from the `IMC.xml` DUNE was built with
  (package `imc`, the root of this module) and checked against DUNE's IMC
  MD5 when connecting.
- Protocol: `doc/specs/Transports.DistributedBus.md` in DUNE (version 3;
  version 2 servers work too, without subscriptions, and version 1 servers
  without the MD5 check).
- TCP (`tcp://host:port`, the default): the program may run on another CPU
  of the vehicle than DUNE. Unix domain sockets (`unix:///path/to.sock`) are
  an option for programs on DUNE's own CPU. Every feature works on both.
- DUNE can start, watch and restart the program (`Supervisors.Process`).
- The same behaviour as DUNE's Python SDK (`sdk/python/dune_task`), checked
  by the same language-neutral conformance suites
  (`doc/specs/SDK.Conformance.md`).

## Quick start

DUNE side: enable a bus server, e.g. with `etc/templates/distbus-server.ini`
(port 6060):

```ini
[Require ../templates/distbus-server.ini]
```

or run the test configuration `build/dune -c testing/distbus-main -p Simulation`.

Go side (`go get github.com/oceanscan/imc-go`):

```go
package main

import (
	imc "github.com/oceanscan/imc-go"
	"github.com/oceanscan/imc-go/dunetask"
)

func main() {
	t := dunetask.New("Depth Watcher", dunetask.HonoursActive()) // unique in the vehicle
	maxDepth := dunetask.NewParam(t, "Maximum Depth", 5.0, dunetask.Units("m"), dunetask.Min(0))

	dunetask.Consume(t, func(m *imc.EstimatedState) {
		nav, ok := t.EidOf("Navigation")
		if !t.Active() || !ok || m.Header.SrcEnt != nav {
			return
		}
		if float64(m.Depth) > maxDepth.Get() {
			t.State(imc.EntityStateState_FAULT, "too deep")
			t.War("depth %.1f m", m.Depth)               // LogBookEntry
			t.Dispatch(&imc.TextMessage{Text: "too deep"})
		}
	})

	dunetask.Main(dunetask.Options{Bus: "tcp://127.0.0.1:6060"}, t)
}
```

A complete example is `examples/depth_watcher` (a port of DUNE's
`examples/python/depth_watcher.py`):

```sh
go run ./examples/depth_watcher --bus tcp://127.0.0.1:6060
DUNE_BUS=unix:///run/dune/bus.sock go run ./examples/depth_watcher
```

`examples/conformance` is the conformance task of
`doc/specs/SDK.Conformance.md`, a small task that uses most of the API.

## API

### Task

`dunetask.New(label, options...)` creates a task: one entity. A program may
run several tasks on one connection (`dunetask.Main(opts, a, b)`); they see
each other's messages.

| Option | Meaning |
|---|---|
| `HonoursActive()` | Adds the `Active` parameter and runs DUNE's activation state machine (initially inactive). |
| `ActiveDefault(true)` | `Active` defaults to true: activated at start. |

| Function | Meaning |
|---|---|
| `Consume(t, func(m *imc.X))` | Call the function with every message of type `X` (from DUNE or from other tasks of the process). Also at run time: DUNE is told at once (subscriptions). |
| `t.Subscribe(id, func(imc.IMessage))` | The same by message id. |
| `t.Periodic(d, fn)` | Call `fn` every `d` (skipped when the task is busy). |
| `NewParam(t, name, default, options...)` | A parameter (below). |

Handlers, periodic functions and hooks of a task run one at a time, in
order, on the task's own goroutine: they need no locks for the task's state
(see `examples/conformance`, whose counter is a plain variable). A handler
that blocks (I/O, heavy computation) blocks its task: messages queue up
(1000 per task) and then the oldest are dropped. Received messages are
shared between the tasks of the process: do not modify them.

| Method | Meaning |
|---|---|
| `Dispatch(m, options...)` | Send a message on DUNE's bus from this entity. The source system (when 0 or 0xFFFF) and entity are set, the time stamp when it is 0; the destination is everyone unless `To(system)`, `ToEntity(eid)` or `ToLabel(label)` say otherwise. Returns false when it is not sent (disconnected, entity refused by DUNE, unknown label): messages are not queued while disconnected. Other tasks of the process that consume the message get a copy. |
| `Reply(request, m)` | Dispatch to the sender of `request` (`Dst = request.Src`, `DstEnt = request.SrcEnt`), as DUNE's `dispatchReply`. |
| `State(state, description)` | Entity state (`imc.EntityStateState_NORMAL`, `_FAULT`, `_ERROR`, ...). Sent at once when it changes and every second. Until the first call the SDK reports NORMAL `idle`/`active`. |
| `Inf`, `War`, `Err`, `Cri(format, args...)` | Log locally and send a `LogBookEntry` (context = label). `Debug` logs locally only. |
| `Eid()` | This entity's id (as seen by this process). |
| `EidOf(label)`, `LabelOf(eid)` | Entity ids of DUNE (and of this process) by label, and back; false when unknown. Incoming `SrcEnt`/`DstEnt` are in these ids. |
| `Active()`, `ActivationState()` | Activation state (`Active` is always true when the task does not honour Active). |
| `Connected()` | The bus handshake is done. |
| `RequestActivation()`, `RequestDeactivation()` | Activate or deactivate as if `Active` was set. |
| `ParamText(name)`, `ParamNames()` | Parameters as DUNE text. |
| `Label()`, `Logger()`, `Bus()` | The label, the task's local logger, the bus. |

| Hook (field, set before `Run`) | Called |
|---|---|
| `OnStart func() error` | Before connecting; an error stops `Run` (`Main` exits with 1). |
| `OnStop func()` | When stopping (after deactivation). |
| `OnReady func()`, `OnDisconnect func()` | Bus handshake done (also after reconnecting) / connection lost. |
| `OnUpdateParameters func(changed []string)` | After `SetEntityParameters` or `PopEntityParameters`, with the names whose values changed. |
| `OnActivation`, `OnDeactivation func() error` | Activation and deactivation; an error fails it (`ACT_FAIL` / `DEACT_FAIL` with the error text). |

The SDK answers, for each task's entity: `QueryEntityState`,
`QueryEntityInfo` (component = process name), `QueryEntityActivationState`,
`VersionInfo` queries, `QueryEntityParameters`, `SetEntityParameters`
(then answers with `EntityParameters`) and `Push`/`PopEntityParameters`,
all by label as DUNE's own tasks do. Activation follows DUNE's
`StatefulEntity`: `ACT_IP`, then `ACT_DONE`+`ACTIVE` or `ACT_FAIL`+`INACTIVE`,
and the same for deactivation, with requests during a transition queued.
After every handshake each entity reports its `EntityState` and
`EntityActivationState`, and an `EntityInfo` once DUNE mirrors it (so DUNE's
logs know its label).

### Parameters

```go
speed := dunetask.NewParam(t, "Cruise Speed", 1.5, dunetask.Units("m/s"),
	dunetask.Desc("Cruise speed"), dunetask.Min(0), dunetask.Max(3))
modes := dunetask.NewParam(t, "Modes", []string{"a", "b"})        // list parameter
debug := dunetask.NewParam(t, "Debug Mode", false)
mode := dunetask.NewParam(t, "Mode", "fast", dunetask.Values("fast", "slow"))
gain := dunetask.NewParam(t, "Gain", 1.0, dunetask.Scope("maneuver")) // saved by PushEntityParameters

v := speed.Get()     // from any goroutine
err := speed.Set(2)  // validated
```

Types: `bool`, `int`, `int64`, `float64`, `string` and slices of them. The
name is DUNE's parameter name (words: `"Maximum Depth"`;
`dunetask.LabelOf("max_depth")` gives it). Values travel as text with
DUNE's syntax: `true`/`false` (also `True`, `False`, `1`, `0`), integers with
an optional `0x` (a leading `0` is octal, as `strtol`), lists separated by
`,`. A value's text is kept exactly as it was set (`2.50` stays `2.50`);
defaults are formatted as Python's `repr` does (`1.0`, `0.1`, `1e+16`), as
DUNE's Python SDK reports them. Invalid values are refused with a warning
(`updating entity parameters: ...`, also a `LogBookEntry`). `Visibility`
and `Scope` are kept; only `Scope` other than `global` matters, to
`PushEntityParameters`, as in DUNE.

### Running

- `dunetask.Main(opts, tasks...)`: run until SIGTERM/SIGINT and exit: 0
  when stopped by a signal, 1 when a task fails to start (or the options are
  wrong), 2 on an IMC mismatch. `MainCode` returns the status instead.
- `dunetask.Run(ctx, opts, tasks...)`: run until `ctx` is done; returns nil,
  `ErrIMCMismatch`, or an error wrapping `ErrStart`.
- `dunetask.NewBus(opts)` then `bus.Run(ctx, tasks...)`: the same, with the
  bus at hand (`bus.Stats()`, `bus.Sub()`, `bus.Ready()`).

On SIGTERM (or when `ctx` ends) the tasks are deactivated, `OnStop` is
called, the entities report BOOT `stopping`, the outgoing queue is flushed
(at most 1 s) and the connection is closed: well within 2 s. On a lost
connection the SDK reconnects with backoff and redoes the handshake; DUNE
mirrors the entities again. Parameters and activation are kept.

Options (`dunetask.Options`; the zero value is fine):

| Field | Default | Meaning |
|---|---|---|
| `Bus` | `$DUNE_BUS`, then `tcp://127.0.0.1:6060` | `tcp://host:port`, `host:port`, `unix:///abs/path` (also `unix:/abs/path`, `unix:rel/path`). |
| `Name` | `$DUNE_TASK_NAME`, then the program's file name | Process name, sent as `#name`; DUNE uses it as the `component` of the entities. `;` and `=` are removed. |
| `System` | `$DUNE_SYSTEM` | A number (decimal or `0x`) forces the source system; a name is only checked against the server's (a warning when they differ). Otherwise the server's. |
| `AllowRename` | false | `#strict=1` is sent unless set: DUNE refuses a taken label instead of renaming it `Remote <label>`. |
| `ReconnectMin`, `ReconnectMax` | 0.5 s, 5 s | Reconnection backoff (doubling, ±20 %, reset after a handshake). |
| `OnMismatch` | `MismatchExit` | On an IMC mismatch: `MismatchExit` (`Run` returns `ErrIMCMismatch`, `Main` exits with 2), or `MismatchRetry`. |
| `MismatchRetry` | 30 s | Time between attempts with `MismatchRetry` (at least 5 s, `MinMismatchRetry`). |
| `SubscribeAll` | false | Ask a protocol 3 server for everything (`#sub=*`) instead of only what the tasks consume. |
| `QueueSize` | 4 MiB | Bound of the outgoing queue, in bytes (Outgoing queue). |
| `SocketBuffer` | 256 KiB | Kernel send and receive buffer of a TCP connection, set before connecting so a stalled reader cannot hide behind buffers the kernel grew to tens of MB (negative: automatic). |
| `Output` | stderr, level `$DUNE_LOG` (INFO) | Where log lines go (`dunetask.NewOutput(w, level, timestamps)`). |

The handshake ends when DUNE answers the query the SDK sends after its own
report (so it knows which of its entities DUNE mirrored); until then, and
while disconnected, messages are dropped.

Log lines go to stderr as `LEVEL logger: text` (`INFO dunetask: connected
to ...`, `WARNING dunetask.Depth_Watcher: depth 7.0 m`), with a time stamp
first unless `DUNE_TASK_NAME` is set.

### Run by DUNE (Supervisors.Process)

DUNE can start the program, restart it when it dies and check that its
entities appear (`doc/specs/Supervisors.Process.md`). It passes no
arguments, only the environment:

| Variable | Use |
|---|---|
| `DUNE_BUS` | Bus URL, e.g. `unix:///run/dune/bus.sock` or `tcp://127.0.0.1:6060`. |
| `DUNE_TASK_NAME` | Process name (`#name`), so DUNE's entity list says which task launched it. |
| `DUNE_SYSTEM` | A system id (decimal or `0x` hexadecimal) forces the source system; a system name is only checked against the server's. |
| `DUNE_LOG` | Log level (`DEBUG`, `INFO`, ...). |

so a program that calls `dunetask.Main(dunetask.Options{}, t)` (and has no
required arguments) needs nothing else; `examples/depth_watcher` is one:

```ini
[Supervisors.Process/Depth Watcher]
Enabled           = Always
Entity Label      = Depth Watcher Supervisor
Command           = /opt/dune-ext/bin/depth_watcher
Restart Policy    = on-failure
Expected Entities = Depth Watcher
```

With `DUNE_TASK_NAME` set, log lines start with their level, as
`Error Output Level = From Prefix` expects. A program on another CPU is
supervised there (by that CPU's DUNE or by systemd with
`Environment=DUNE_BUS=tcp://<main cpu>:6060`).

### Subscriptions

DUNE forwards its whole bus to every peer unless told otherwise. With a
protocol 3 server the SDK sends, in its report, `#sub=<ids>`: the messages
its tasks consume (`Consume`, `Subscribe`), the requests it answers for
their entities (`QueryEntityState`, `QueryEntityInfo`,
`QueryEntityActivationState`, `Query`/`Set`/`Push`/`PopEntityParameters`,
`VersionInfo`) and `EntityList`. A later `Consume` of a new message re-sends
the report. DUNE then forwards only those, from its own tasks and from other
peers. `Bus.Sub()` is the current set (nil: everything). A protocol 2 server
sends everything; the SDK still skips, after the CRC, what no task consumes
(without decoding it).

### Outgoing queue

Messages are written to the socket by a writer goroutine, in batches (one
`write` for many frames). When DUNE reads slower than the program writes, up
to 256 KiB wait in the socket's buffer and the rest in a queue bounded by
`QueueSize` (4 MiB): when it is full, the **oldest** messages are dropped
(whole frames; handshake reports are never dropped), so memory stays
bounded and what arrives later is the newest. The SDK logs a warning when
dropping starts, every 5 s while it goes on, and a summary when the queue
drains. Counters in `Bus.Stats()`: `RxBytes`, `Rx`, `Tx`, `DroppedTx` (not
sent: not connected, refused entity or dropped from the queue),
`QueueDrops`, `QueueDroppedBytes`, `QueuePeak`, `QueuedBytes`, `Filtered`
(skipped, no task consumes them), `CRCErrors`, `SkippedBytes`,
`DecodeErrors`, `TaskQueueDrops`, `Connects`. DUNE does the same on its side
for each peer (`Peer Queue Size`) and disconnects a peer that reads nothing
for `Peer Stall Timeout`.

## IMC

The root package `imc` (`github.com/oceanscan/imc-go`) has two codecs built
from the same XML.

**Typed messages** (generated, `messages.go`): one struct per message with
a `Header`, enumeration and bitfield types with constants and `String`
methods, typed message lists, and direct encoders and decoders (no
reflection, no maps):

```go
m := &imc.EntityState{State: imc.EntityStateState_FAULT, Description: "too deep"}
frame, err := imc.Marshal(m)            // little endian frame, as DUNE writes
back, err := imc.Decode(frame)          // strict: sync, length and CRC checked
es := back.(*imc.EntityState)
p := &imc.SetEntityParameters{Name: "X", Params: []*imc.EntityParameter{{Name: "A", Value: "1"}}}
imc.CLoopsMask_DEPTH | imc.CLoopsMask_YAW // "DEPTH|YAW"
imc.IMCMD5, imc.IMCVersion, imc.SyncNumber, imc.NewMessage(id), imc.MessageID("EstimatedState")
```

Inline message fields are `*T` (the field's message type) or `imc.IMessage`
(any message, for groups such as `Maneuver`); `nil` is the empty message.
Unknown enumeration values from the wire are kept as they are.

**Stream reader**: `imc.NewFrameReader(conn)` splits a byte stream with
DUNE's rules (skips garbage to the next sync byte, resynchronizes one byte
after a bad CRC, decodes big endian frames, waits for partial frames, the
largest frame is 65557 bytes); `Next()` returns frames with a valid CRC,
and the caller decodes those it wants (`f.Decode()`, or skips by
`f.Header.MGID`). One fixed buffer, no allocation per frame.

```go
fr := imc.NewFrameReader(conn)
for {
	f, err := fr.Next()
	if err != nil {
		break
	}
	if m, err := f.Decode(); err == nil { ... }
}
```

**Dynamic codec** (`imc.Protocol`, `imc.Message` with fields in a map,
driven by any IMC XML at run time): what log tools use, since every LSF log
carries the `IMC.xml.gz` it was written with. It is **strict** by default:
only the sync number in either byte order (big endian frames are decoded as
big endian), CRC and length checked; when encoding, a missing field, a value
of the wrong kind or an integer out of range is an error (`CreateMessage`
fills every field with its zero value). `p.Lenient = true` keeps the old
behaviour of imc-go for log forensics (any sync number with an 0xFE byte,
CRC ignored, missing fields written as zero); the tools of `cmd/` set it.
Sizes over 65535 are refused in both modes.

The bundled `IMC.xml` is the one DUNE is generated from (IMC 5.5.8, MD5
`50cbd21833afb6b5d20bb3c619f61184`). To follow another DUNE, replace it with
the original XML DUNE was generated from (DUNE's `DUNE_IMC_CONST_MD5` is its
MD5; the `IMC.xml.gz` in DUNE's logs has its descriptions stripped, so its
MD5 differs) and run `go generate` (`internal/imcgen`). Tasks built against
another IMC are refused by DUNE (and refuse it): the MD5 check.

## Testing tasks

`dunetask/dunetest.FakeDune` is a fake bus server (protocol 3 by default,
`Proto: 1` or `2`) for unit tests of tasks: it mirrors the client's
entities, translates ids, honours `#sub`, records what the client sends and
sends it messages from named entities. `Config{Path: "x.sock"}` listens on
a Unix socket; `Pause(true)` stops reading, like a stalled DUNE;
`DropClients()` and `Close()` simulate DUNE going away.

```go
srv, _ := dunetest.Start(dunetest.Config{Entities: []string{"Navigation"}})
defer srv.Close()
go dunetask.Run(ctx, dunetask.Options{Bus: srv.URL()}, task)
srv.WaitEntity("Depth Watcher", 5*time.Second)
srv.Send(&imc.EstimatedState{Depth: 7}, "Navigation", "")
m, err := srv.WaitMessage(dunetest.Where(func(m *imc.LogBookEntry) bool {
	return m.Type == imc.LogBookEntryType_WARNING
}), 5*time.Second)
```

See `task_test.go` and `example_test.go`. The tests of this module:

```sh
go test -race ./...                                   # codec, SDK with the fake DUNE, examples
DUNE_GOLDEN=<dune>/tests/golden go test .             # golden frames of a DUNE tree (default: testdata/golden)
go test -run XXX -bench . -benchmem .                 # codec benchmarks
```

In DUNE's tree, `tests/sdk/go/run-tests.sh` runs them with DUNE's golden
corpus and `tests/sdk/go/conformance.sh` runs the conformance suites
(`tests/sdk/conformance`) against the Go conformance task, on x86_64 and,
under qemu-user, on arm64 and armv7.

## Cross-compiling for the vehicles

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' ./examples/depth_watcher  # Raspberry Pi 4, manta
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags='-s -w' ./examples/depth_watcher  # armv7 hard-float
```

The binaries are static (about 5 MB stripped) and need nothing on the
vehicle. To try them on a PC without installing anything system wide, run
them under qemu-user in a container (`apt-get install qemu-user` in a
`debian:bookworm-slim` container, then `qemu-aarch64 ./depth_watcher`;
`--network host` lets them reach a DUNE on the host).

## Performance

On a laptop CPU (Intel i7-8550U, Go 1.26), for `EstimatedState`:

| | encode | decode |
|---|---|---|
| typed (`AppendFrame` into a buffer / `Decode`) | 0.10 µs, 0 allocations | 0.16 µs, 1 allocation |
| dynamic, this version | 0.85 µs | 2.1 µs |
| dynamic, imc-go 1.3.0 (`binary.Read`, reflection) | 2.8 µs | 5.2 µs |

A stream of frames through `FrameReader` (CRC checked) and the typed
decoder runs at about 600 MB/s (5.6 M EstimatedState/s); frames no task
wants cost 0.1 µs (CRC only).

The conformance task (`examples/conformance`, fake-DUNE suite): 1.7 % of
one core at 50 Hz of `EstimatedState` while active (each one answered with
a `DevDataText`), about 120 000 `EstimatedState`/s handled in a flood, 5 MiB
RSS at idle, RSS growth 12 MiB while DUNE stops reading; SIGTERM exits in
about 10 ms. Against `lauv-simulator-1` (20 Hz `EstimatedState`, with
`#sub`): 0.4 to 1 % CPU, 2.2 kB/s received instead of 56 kB/s (4 %). Under
qemu-user (emulated, so slower than the real CPU): arm64 3.4 %, armv7 7.4 %
at 50 Hz.

## Limitations

- Subscriptions are by message id only: a task that consumes
  `EstimatedState` gets every `EstimatedState` on the bus, whatever its
  source or destination.
- A protocol 2 server has no subscriptions: it sends everything that passes
  its `Messages to Forward`/`Messages to Ignore`/`Rate Limiters`.
- No `SaveEntityParameters`, `QueryTypedEntityParameters` or `HealthCheck`.
  Neptus does not know the types of the parameters (they are not in DUNE's
  parameters XML).
- A protocol 1 server does not report its IMC MD5: the SDK warns and cannot
  check it.
- Labels must not contain `;` or `=` nor start with `#`; they must be unique
  in the vehicle. By default DUNE refuses a taken label: the SDK logs an
  error naming it and sends nothing from that entity until DUNE mirrors it.
  With `AllowRename`, DUNE renames it `Remote <label>` and the SDK logs an
  error too.
- Messages sent while disconnected are dropped, as in DUNE.
- The typed messages are those of the bundled XML: a DUNE generated from
  another IMC needs a regenerated `messages.go` (the MD5 check refuses the
  mismatch).
