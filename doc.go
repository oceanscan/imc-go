// Package imc implements IMC, the Inter-Module Communication protocol of
// DUNE, Neptus and the LSTS toolchain, in two layers built from the same
// IMC XML specification:
//
//   - typed messages (messages.go, generated from the bundled IMC.xml by
//     internal/imcgen): one struct per message, enumeration and bitfield
//     types with their constants, typed message lists, and direct encoders
//     and decoders (no reflection, no maps) for both byte orders. Marshal,
//     AppendFrame, Decode and FrameReader work with them. This is what
//     programs that talk to DUNE use (see package dunetask);
//   - the dynamic codec (Protocol, Message): fields in a map, driven by any
//     IMC XML at run time. This is what log tools use, since every LSF log
//     carries the IMC.xml it was written with.
//
// Both are strict by default (sync number, CRC, lengths, sizes; see
// Protocol for the lenient mode of log forensics) and byte for byte
// identical to DUNE's serializer (tested against DUNE's golden frames).
//
// The bundled IMC.xml is the one DUNE is generated from (IMC 5.5.8, MD5
// IMCMD5): DUNE refuses a peer of its distributed bus whose IMC MD5
// differs. To follow another DUNE, replace IMC.xml with the original XML
// DUNE was generated from (not the stripped IMC.xml.gz of its logs, whose
// MD5 differs) and run go generate.
package imc

//go:generate go run ./internal/imcgen -xml IMC.xml -out messages.go
