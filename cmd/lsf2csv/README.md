# lsf2csv

Convert DUNE/IMC Log Storage Format (LSF) files to CSV. Each message type is
written to its own CSV file, one row per message.

Supports plain `Data.lsf` files and gzip-compressed variants (`.lsf.gz`).
The matching `IMC.xml` protocol definition is auto-detected next to the LSF
file if not provided explicitly.

---

## Requirements

- Go 1.21 or later
- The `imc-go` module (already in `go.mod`)

---

## Compiling

From the repository root:

```sh
git clone https://github.com/oceanscan/imc-go
cd imc-go
go build -o lsf2csv ./cmd/lsf2csv
```

Or install directly (Go 1.21+):

```sh
go install github.com/oceanscan/imc-go/cmd/lsf2csv@latest
```

Cross-compile for a different platform (example: Linux ARM64):

```sh
GOOS=linux GOARCH=arm64 go build -o lsf2csv-arm64 ./cmd/lsf2csv
```

---

## Synopsis

```
lsf2csv -lsf <path> [options]
lsf2csv -R -lsf <directory> [options]
```

---

## Options

| Flag | Default | Description |
|------|---------|-------------|
| `-lsf <path>` | *(required)* | Path to `Data.lsf`, or a root directory when `-R` is set. |
| `-xml <path>` | `IMC.xml` | Path to the IMC protocol definition file. Auto-detected if omitted (see below). |
| `-R` | off | Recursively find and convert every `Data.lsf` under `-lsf`. |
| `-out <dir>` | `.` | Output directory for CSV files. Created if it does not exist. |
| `-msg <list>` | *(all)* | Comma-separated message abbreviations to export (e.g. `EstimatedState,GpsFix`). |
| `-entity <list>` | *(all)* | Comma-separated entity names to include (e.g. `Navigation,GPS`). |
| `-src <list>` | *(all)* | Comma-separated source IDs (decimal integers) to include. |
| `-start <time>` | *(none)* | Earliest timestamp to include. Unix epoch seconds or RFC3339 (e.g. `2024-06-01T00:00:00Z`). |
| `-end <time>` | *(none)* | Latest timestamp to include. Same formats as `-start`. |
| `-fields <list>` | *(all)* | Comma-separated field names to export. When set, every output CSV uses exactly these columns. |

### IMC.xml auto-detection order

When `-xml` is not supplied (or left at the default `IMC.xml`), the tool
searches in this order:

1. `IMC.xml` in the current working directory
2. `IMC.xml` in the same directory as the LSF file
3. `IMC.xml.gz` in the same directory as the LSF file

---

## Output

One CSV file is created per message type that passes the filters. Files are
named `<MessageAbbrev>.csv` and placed in the output directory.

Every CSV starts with a header row. Default columns:

```
timestamp, src, src_ent, dst, dst_ent, <message-specific fields...>
```

`timestamp` is a Unix epoch float (seconds with sub-second precision).

---

## Examples

### Single file, all messages

```sh
lsf2csv -lsf /path/to/Data.lsf
```

### Single file, specific messages, custom output directory

```sh
lsf2csv -lsf /path/to/Data.lsf \
        -msg EstimatedState,GpsFix \
        -out /tmp/csv
```

### Filter by entity and time window

```sh
lsf2csv -lsf /path/to/Data.lsf \
        -entity Navigation \
        -start 2024-06-01T10:00:00Z \
        -end   2024-06-01T11:00:00Z \
        -out   /tmp/csv
```

### Export only specific fields

```sh
lsf2csv -lsf /path/to/Data.lsf \
        -msg EstimatedState \
        -fields timestamp,lat,lon,depth,psi \
        -out /tmp/csv
```

### Recursive batch conversion

Convert every `Data.lsf` found under the `Logs/` tree, preserving the
directory structure under `csv_out/`:

```sh
lsf2csv -R -lsf Logs/ -out csv_out/
```

Result layout:

```
csv_out/
  lauv-ualg-1/20240912/091520/EstimatedState.csv
  lauv-ualg-1/20240912/091520/GpsFix.csv
  buv-petinga/20251217/090754/EstimatedState.csv
  ...
```

Errors on individual files are logged but do not abort the batch.

### Explicit IMC.xml

```sh
lsf2csv -lsf /path/to/Data.lsf -xml /opt/imc/IMC.xml -out /tmp/csv
```

---

## Notes

- Unknown message IDs (not present in the XML) are silently skipped.
- Gzip-compressed LSF files (`Data.lsf.gz`) are supported transparently.
- The tool does two passes over the LSF file: one quick scan to build an
  entity-ID → name map, then the main conversion pass.
- Progress is reported on a single updating line every 10 000 messages.
