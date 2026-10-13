# Go IMC Module

A Go implementation of the Inter-Module Communication (IMC) protocol of DUNE,
Neptus and the LSTS toolchain, and an SDK to write DUNE tasks in Go.

- **Typed messages** (`messages.go`, generated from the bundled `IMC.xml`, the
  one DUNE is generated from: IMC 5.5.8, MD5 `IMCMD5`): one struct per
  message, enumeration and bitfield types with constants, typed message
  lists, and direct encoders/decoders (no reflection): `Marshal`,
  `AppendFrame`, `Decode`.
- **Stream reader** (`FrameReader`) with DUNE's resynchronization rules, for
  TCP, Unix sockets and logs.
- **Dynamic codec** (`Protocol`, `Message`): load any `IMC.xml` at run time and
  work with fields in a `map[string]any` (what the log tools use).
- **Strict by default**: sync number, CRC, frame length and sizes checked;
  big endian frames decoded as big endian; a lenient mode for log forensics.
- **`dunetask`**: DUNE tasks in Go, as peers of DUNE's distributed bus
  (entities, parameters, activation, logging, subscriptions, reconnection),
  with a fake DUNE for tests. See [dunetask/README.md](dunetask/README.md).
- **UDP Transporter**: unicast and multicast (`SO_REUSEADDR`/`SO_REUSEPORT`).
- **Tools** (`cmd/`, a module of its own): LSF log converters, mergers,
  splitters, log synchronization (TUI).
- Tested byte for byte against DUNE's golden frames (both byte orders).

## Importing as a Library

To use `imc-go` in your own project:

```bash
go get github.com/oceanscan/imc-go
```

```go
import imc "github.com/oceanscan/imc-go"
```

For a local checkout, `go mod edit -replace github.com/oceanscan/imc-go=../imc-go`.
The library module depends only on `golang.org/x/net` and `golang.org/x/sys`;
the tools in `cmd/` are a separate module (`github.com/oceanscan/imc-go/cmd`)
with their TUI dependencies.

## Getting Started

### Prerequisites

- Go 1.24 or later
- Access to an `IMC.xml` specification file.

### Installation

```bash
go mod tidy
```

## Usage

### 1. Initialize the Protocol

```go
import "github.com/oceanscan/imc-go"

// Parse the XML specification
xmlProto, _ := imc.ParseXML("IMC.xml")
// Create a protocol engine
proto := imc.NewProtocol(xmlProto)
```

### 2. Creating and Sending Messages

```go
// Create a new UDP transporter
trans, _ := imc.NewUDPTransporter(proto, "0.0.0.0:0")
defer trans.Close()

// Create an Announce message
msg, _ := proto.CreateMessage("Announce")
msg.Fields["sys_name"] = "My-Go-Node"
msg.Fields["sys_type"] = uint8(0) // CCU

// Send to a specific address
trans.Send(msg, "127.0.0.1:6002")
```

### 3. Receiving Multicast Messages

```go
// Listen on the standard IMC multicast port
trans, _ := imc.NewUDPTransporter(proto, "0.0.0.0:30101")
defer trans.Close()

// Join the IMC multicast group
trans.JoinMulticast("224.0.75.69:30101")

for {
    msg, addr, err := trans.Receive()
    if err == nil {
        abbrev := proto.Messages[msg.Header.MGID].Abbrev
        fmt.Printf("Received %s from %v\n", abbrev, addr)
    }
}
```

### 4. Typed Messages

Typed messages have their own codec (no XML needed at run time):

```go
m := &imc.EntityState{State: imc.EntityStateState_FAULT, Description: "too deep"}
frame, err := imc.Marshal(m)      // little endian frame
back, err := imc.Decode(frame)    // strict: sync, length, CRC
es := back.(*imc.EntityState)

fr := imc.NewFrameReader(conn)    // a stream: TCP, Unix socket, log
for {
	f, err := fr.Next()           // a frame with a valid CRC
	if err != nil {
		break
	}
	if f.Header.MGID == imc.IDEstimatedState {
		m, _ := f.Decode()
		_ = m.(*imc.EstimatedState)
	}
}
```

They convert to and from dynamic messages:

```go
import "github.com/oceanscan/imc-go"

// Receiving and converting to typed
msg, addr, _ := trans.Receive()
abbrev := proto.Messages[msg.Header.MGID].Abbrev

// Use CreateTyped for easy conversion
typedMsg, err := proto.CreateTyped(abbrev, msg)
if err == nil {
    switch m := typedMsg.(type) {
    case *imc.Announce:
        fmt.Printf("Received Announce from %s: Name=%s, Type=%d\n", 
            addr, m.SysName, m.SysType)
    case *imc.EstimatedState:
        fmt.Printf("Vehicle Position: Lat=%f, Lon=%f\n", m.Lat, m.Lon)
    }
}

// Creating a typed message for sending
ann := imc.Announce{
    SysName: "MyNode",
    SysType: 2, // UUV
}
trans.Send(ann.ToMessage(), "127.0.0.1:6001")
```

### 5. Strict and Lenient Decoding

`Protocol` is strict by default: only the specification's sync number, in
either byte order (a byte swapped sync number is a big endian frame, decoded
as big endian), frames whose CRC and length are right, and, when encoding,
every field present, of the right kind and in range (`CreateMessage` fills
every field with its zero value). For log forensics, `proto.Lenient = true`
keeps the behaviour of imc-go 1.3 (any sync number with an 0xFE byte, CRC
ignored, missing fields written as zero); the tools in `cmd/` set it. Sizes
over 65535 bytes are refused in both modes.

## Regenerating Messages

`messages.go` is generated from `IMC.xml` by `internal/imcgen` (standard
library only):

```bash
go generate .        # go run ./internal/imcgen -xml IMC.xml -out messages.go
```

Use the original XML DUNE was generated from (its MD5 is DUNE's
`DUNE_IMC_CONST_MD5`, stamped in `IMCMD5`; `-md5` refuses another one). DUNE
refuses a bus peer whose IMC MD5 differs. The `IMC.xml.gz` stored in DUNE's
logs has its descriptions stripped, so its MD5 differs: fine for the dynamic
codec, not for the typed messages of a DUNE peer.

### From imc-go 1.3 (IMC 5.5.5)

- The bundled XML is now IMC 5.5.8 (DUNE's). Messages and fields added or
  changed since 5.5.5 change the typed structs accordingly; the dynamic
  codec still reads any XML.
- Enumerated and bitfield fields have their own types (`EntityStateState`,
  `CLoopsMask`); untyped constants and conversions (`uint8(x)`) still work.
- Inline message fields are `IMessage` or `*T`, message lists `[]*T` or
  `[]IMessage` (were `*Message` and `[]*Message`); typed structs no longer
  have an `Extra` map.
- The dynamic codec is strict by default (above); set `Lenient` for the old
  behaviour. `CreateMessage` now fills every field.
- `cmd/gen_messages` is replaced by `internal/imcgen`, and the tools are a
  module of their own (`cd cmd && go build ./...`).

## Running Examples

The project includes several command-line tools in the `cmd/` directory to demonstrate the library's capabilities. 

### 1. Multicast Listener
Joins the standard LSTS multicast group and prints received `Announce` messages from other systems on the network.
```bash
cd cmd && go run ./multicast_listener
```

### 2. LSF Log Parser
Parses a binary `.lsf` log file and prints message statistics.
```bash
# Make sure the test data is available
cd cmd && go run ./parse_lsf
```

### 3. EstimatedState Positions
Extracts absolute latitude/longitude (in degrees) for every `EstimatedState` message in an LSF log by displacing the geodetic reference `(Lat, Lon)` by the NED offset `(X, Y)` on the WGS84 ellipsoid. Output is CSV.
```bash
cd cmd && go run ./estate_positions ../IMC.xml Data.lsf > positions.csv
```

### 4. DUNE Tasks
`dunetask` and its examples (`examples/depth_watcher`, `examples/conformance`):
see [dunetask/README.md](dunetask/README.md).

## Cross-Compilation (Raspberry Pi)

To compile the tools for a Raspberry Pi or other ARM devices from your development machine:

### For Raspberry Pi 4/5 (64-bit)
```bash
cd cmd && GOOS=linux GOARCH=arm64 go build -o listener-arm64 ./multicast_listener
```

### For Raspberry Pi 3 or Zero 2 (32-bit)
```bash
cd cmd && GOOS=linux GOARCH=arm GOARM=7 go build -o listener-arm ./multicast_listener
```

## Windows Support

The library is cross-platform and supports Windows. To compile for Windows from Linux or macOS:

```bash
cd cmd && GOOS=windows GOARCH=amd64 go build -o listener.exe ./multicast_listener
```
