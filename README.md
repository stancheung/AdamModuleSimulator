# AdamModuleSimulator

A pure-Go simulator of an Advantech **ADAM-6050** DI/DO module. It speaks the
three protocols the official Advantech .NET utility and a browser expect, so
you can develop and demo against a "real" module without hardware:

- **Modbus TCP** server on port **502** (18 coils: DI0–DI11, DO0–DO5)
- **UDP discovery** on port **5048** (`MADA` broadcast → NetConfig/DeviceInfo
  replies, so the module shows up in the utility's device list)
- **ASCII command channel** on UDP **1025**, plus the Modbus-tunneled ASCII
  path (FC16 write to holding register 10000 → FC3 read of the reply)
- **Web control panel** on port **5002** (server-rendered HTMX fragments) to
  toggle coils and start/stop the Modbus listener

Templates and static assets are embedded in the binary (`go:embed`) — no
runtime files needed.

## Requirements

- Go 1.27+
- Free ports: TCP 502, TCP 5002, UDP 5048, UDP 1025

Port bind failures are non-fatal: whatever failed is logged and shown in the
web panel's status bar while everything else keeps running.

## Usage

```sh
go run .
```

Then open http://localhost:5002 and/or run the Advantech ADAM utility on the
same machine — it should discover the simulated module.

Flags:

| Flag        | Default     | Meaning                                   |
| ----------- | ----------- | ----------------------------------------- |
| `-addr`     | `:5002`     | Web panel listen address                  |
| `-ip`       | auto-detect | IP advertised in discovery replies         |
| `-password` | `00000000`  | Module password (8 chars) for the utility |
| `-name`     | `6050`      | Module name (6050 → displayed as ADAM-6050) |

Build a standalone binary:

```sh
go build ./...
```

## HTTP API

| Route                       | Method | Purpose                                   |
| --------------------------- | ------ | ----------------------------------------- |
| `/`                         | GET    | Full page                                  |
| `/fragments/coils`          | GET    | Coil grid (`?model=6060` for 6 DI variant) |
| `/fragments/status`         | GET    | Listener status badge                      |
| `/listen/toggle`            | POST   | Start/stop the Modbus listener             |
| `/coils/{idx}/{value}`      | POST   | Force coil `idx` (`on`/`off`), 0–17        |
| `/assets/…`                 | GET    | Embedded CSS/JS/fonts                      |

## Project layout

```
main.go        HTTP routes, templates, flags, module identity
adam.go        AdamServer: coil state + Modbus TCP function codes
discovery.go   UDP 5048 discovery, UDP 1025 ASCII, handleASCII
templates/     index.html + HTMX partials (coils, status, listen button)
assets/        style.css, htmx.min.js, fonts (all embedded)
```

## Protocol notes

The wire format was reverse-engineered from the decompiled `Advantech.Adam.dll`
and is documented inline next to the code — firmware version quirks, fixed
length ASCII commands, and coil address offsets are intentional. See
`AGENTS.md` for the constraints that must not be "cleaned up" if you modify
the protocol code.

## Development

```sh
go vet ./...   # verify
gofmt -l .     # formatting (should print nothing)
```

There is no test suite yet; `go test ./...` works if you add tests.
