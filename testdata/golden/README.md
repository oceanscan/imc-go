# Golden IMC frames

Copied from DUNE's `tests/golden` (OceanScan DUNE, IMC 5.5.8): the generator
(`dune-imc-golden.cpp`) and the instructions to regenerate it live there.
Replace this copy whenever the bundled `IMC.xml` changes.

Binary IMC frames written by DUNE's own serializer, for implementations of
IMC in other languages (the SDKs that join a DUNE system over
`Transports.DistributedBus`, see `doc/specs/Transports.DistributedBus.md`) to
test their codecs and stream readers against.

| Path | Content |
| --- | --- |
| `frames/le/` | One frame per file, as DUNE writes it: little endian, sync bytes `55 FE`. |
| `frames/be/` | The same messages as big endian frames: sync bytes `FE 55`, every number (header, fields, lengths, inline message ids, CRC) byte swapped. DUNE reads them back to the same message. |
| `frames/invalid/` | A frame with a bad CRC, one with an unknown message id, a truncated one, and one whose payload is a byte longer than its fields. |
| `frames/stream.bin` | Garbage, valid frames of both byte orders, invalid frames and an incomplete frame at the end, in one byte stream. |
| `manifest.json` | For every file: header, CRC, the fields as DUNE decodes them (in DUNE's JSON form), and for the invalid frames and the stream what DUNE's reader makes of them. |
| `dune-imc-golden.cpp` | The generator. |

The corpus covers every field type the IMC specification uses (`int8_t`,
`uint8_t`, `int16_t`, `uint16_t`, `int32_t`, `uint32_t`, `fp32_t`, `fp64_t`,
`rawdata`, `plaintext`, `message`, `message-list`), their limits, enums and
bitfields (8, 16 and 32 bit), NaN, infinities, -0 and denormals, UTF-8 and
NUL bytes in plaintext, empty inline messages and lists, null list
elements, nested inline messages, and the largest possible frame (65535
bytes). `manifest.json` records the IMC version and the MD5 of the IMC XML
the build was generated from; a codec generated from another XML must not
be tested against it.

## Regenerating

The generator is built with the tree (`dune-imc-golden`, not built by
default unless `TESTS=ON`):

```sh
cmake --build build --target dune-imc-golden
build/dune-imc-golden tests/golden          # rewrite the corpus
build/dune-imc-golden --check tests/golden  # compare (ctest: test_golden_imc_frames)
```

Regenerate and commit the corpus whenever the IMC definition changes. The
generator must run on a little endian host.
