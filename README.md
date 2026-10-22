# imc-go: IMC log tools

Command-line tools for IMC (the Inter-Module Communication protocol of
DUNE, Neptus and the LSTS toolchain) and LSF logs: converters (`lsf2csv`,
`lsf2json`), mergers and splitters (`lsfmerge`, `lsfsplit`), log
synchronization (`logsync`, `auto-logsync`, a TUI), `estate_positions`,
`parse_lsf`, `multicast_listener` and `verify`. `dist.sh` builds them for
Linux, Windows and macOS (and `.deb` packages).

## The codec moved to dune-sdk

The Go IMC codec that was this repository's root package (`imc`: typed
messages generated from DUNE's `IMC.xml`, the stream reader, the dynamic
codec, the UDP transporter) is now package `imc` of the Go module of
[dune-sdk](https://github.com/oceanscan/dune-sdk), the repository of DUNE's
task SDKs, with its history:

```go
import "github.com/oceanscan/dune-sdk/go/imc"     // was "github.com/oceanscan/imc-go"
```

The package name and API are unchanged (see `go/imc/README.md` there).
This repository keeps only the tools, which import it (module
`github.com/oceanscan/dune-sdk/go`, tag `go/v1.0.0`). dune-sdk is private, so
tell Go not to use the public proxy for it, and let git fetch it over SSH:

```bash
export GOPRIVATE=github.com/oceanscan/*
git config --global url."git@github.com:".insteadOf https://github.com/
cd cmd && go build ./...
```

`IMC.xml` (IMC 5.5.8, DUNE's) stays here as the tools' default
specification; LSF logs carry their own `IMC.xml.gz`, which the tools find
next to the log.

## The Tools

The command-line tools in `cmd/` (a module of its own):

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
DUNE tasks in Go (package `dunetask`, on the same codec) are in dune-sdk:
`go/dunetask` of module `github.com/oceanscan/dune-sdk/go`.

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
