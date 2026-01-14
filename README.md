# Go IMC Module

A dynamic Go implementation of the Inter-Module Communication (IMC) protocol. This library allows for communication with LSTS systems (Neptus, DUNE, etc.) by reading the protocol specification directly from an `IMC.xml` file.

## Features

- **Dynamic Protocol Loading**: Load Any `IMC.xml` version at runtime.
- **Generic Message Handling**: Work with messages using a simple `map[string]any` interface.
- **UDP Transporter**: Support for unicast and multicast communication.
- **Multicast Coexistence**: Implements `SO_REUSEADDR` and `SO_REUSEPORT` to allow multiple processes to share the same IMC ports.
- **Typed Message Helpers**: Auto-generated Go structs for type-safe field access and IDE autocomplete.
- **Nested Message Support**: Full recursion for `message` and `message-list` types.
- **Strict Validation**: CRC16 checks and synchronization number verification.

## Getting Started

### Prerequisites

- Go 1.22 or later
- Access to an `IMC.xml` specification file.

### Installation

```bash
go mod tidy
```

## Usage

### 1. Initialize the Protocol

```go
import "imc-go"

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

### 4. Using Typed Message Helpers

For a more convenient development experience, you can use generated structs to avoid raw map access:

```go
import "imc-go"

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

## Regenerating Messages

If the `IMC.xml` specification changes or you want to update the generated helpers, use the included generator tool:

1. Place the new `IMC.xml` in the root of the project.
2. Run the generator:

```bash
go run cmd/gen_messages/main.go
```

This will update `messages.go` with structs for all messages defined in the XML.

## Running Examples

The project includes several command-line tools in the `cmd/` directory to demonstrate the library's capabilities. 

### 1. Verification Tool (Unicast Loopback)
Sends and receives an `Announce` message locally to verify serialization and basic networking.
```bash
go run cmd/verify/main.go
```

### 2. Multicast Listener
Joins the standard LSTS multicast group and prints received `Announce` messages from other systems on the network.
```bash
go run cmd/multicast_listener/main.go
```

### 3. LSF Log Parser
Parses a binary `.lsf` log file and prints message statistics.
```bash
# Make sure the test data is available
go run cmd/parse_lsf/main.go
```

### 4. Code Generator
Updates the typed message structs in `messages.go` based on the current `IMC.xml`.
```bash
go run cmd/gen_messages/main.go
```

## License

MIT (or as specified in the project)
