# AGENTS.md

Single-module Go app (`module AdamModuleSimulator`, Go 1.27): a simulator of an Advantech
ADAM-6050 module (Modbus TCP server + web control panel + UDP discovery/ASCII
emulation) so the Advantech .NET utility and browsers can talk to it.

## Commands

- Build: `go build ./...` (output binary `AdamModuleSimulator.exe` lands in the
  repo root; `AdamModuleSimulator`/`*.exe` are existing build artifacts, not
  source).
- Verify: `go vet ./...` and `gofmt -l .` — there is no lint/typecheck/test
  config, no CI, no Makefile, and no tests. If you add tests: `go test ./...`.
- Run: `go run .` — flags: `-addr` (default `:5002`, web panel), `-ip`
  (advertised IP, auto-detected), `-password` (default `00000000`), `-name`
  (default `6050`).
- Runtime needs free ports: TCP 502 (Modbus), UDP 5048 (discovery), UDP 1025
  (ASCII), TCP 5002 (web). Bind failures are deliberately non-fatal: they are
  recorded/logged and the web panel keeps running — do not "fix" this by making
  them fatal.

## Structure (one `package main`, three files)

- `main.go` — HTTP routes, template rendering, flag/identity setup.
- `adam.go` — `AdamServer`: coil state (`[18]int`, guarded by `coilMu`) and the
  Modbus TCP handler.
- `discovery.go` — UDP 5048 MADA discovery replies, UDP 1025 ASCII channel, and
  `handleASCII` shared with the Modbus ASCII path.
- `templates/*.html` and `assets/**` are `//go:embed`-ed; there is **no hot
  reload** — rebuild/restart after changing them. `index.html` is the page;
  `coils.html`, `status.html`, `listenbtn.html` are HTMX fragment partials
  swapped in by `/fragments/*` and `POST` routes — keep each fragment
  self-contained.

## Protocol constraints (verified against decompiled Advantech.Adam.dll — do not "clean up")

- Advertised `FwVer` must stay **4.02** (binary bytes at DeviceInfo[56..60],
  not ASCII). The utility gates on version >= 5 / >= 3.01 to download the
  flexible-modbus XML table and enable access control; changing this breaks the
  intended code path.
- ASCII `$` commands are **fixed length 14**: the obfuscated password bytes may
  contain `\r` or NUL, so slice — never split on or strip them.
- Every `$`-query reply must be longer than 4 bytes or the utility reports
  "Connect module failed!".
- Modbus word-alignment: odd-length ASCII payloads arrive NUL-padded — trim
  `\r`/`\x00` from the command tail before dispatch.
- Coil addressing quirk: FC1/FC2/FC15 do `start -= 4` when `start > 11`, FC5
  uses `data[9] - 4`. Coil indices are 0..17 (DI0-11 = 0-11, DO0-5 = 12-17);
  keep these offsets consistent across function codes when editing.
- ASCII-over-Modbus window: FC16 write to wire address **9999** (holding reg
  10000) executes the ADAM command and stores the reply for the follow-up FC3
  read; FC3/FC4 reads outside that window must return zeros, not errors.
- Web panel and Modbus share the same `coils` array (`coilMu`) — any new writer
  must take that lock.

## Conventions

- Comments here document wire-format facts reverse-engineered from the vendor
  client; preserve them when refactoring.
- Output goes to stdout via `fmt.Print*` for protocol events and `log` for
  HTTP — match the existing split.

## Cursor Cloud specific instructions

- The base image ships Go 1.22. This module sets `go 1.27`, and that compiler
  reports `toolchain not available` instead of downloading Go 1.27. The
  environment install unpacks Go 1.27.3 into `/usr/local/go` and symlinks `go`
  and `gofmt` into `/usr/local/bin`, which login shells search before `/usr/bin`.
- On boot, `start` sets `net.ipv4.ip_unprivileged_port_start` to 502 (kernel
  state, not part of the disk snapshot) and runs `go run .`. The web panel is
  already on port 5002. `http.ListenAndServe` exits if that port is taken, so
  stop the existing process before starting another copy. Modbus TCP 502, UDP
  discovery 5048, and UDP ASCII 1025 still treat bind failures as non-fatal.
- DI0–DI11 power up ON and DO0–DO5 power up OFF. `curl -s
  http://127.0.0.1:5002/fragments/status` reports the listener;
  `POST /coils/12/on` forces DO0.
