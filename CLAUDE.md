# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Common requirements

- Before implementing anything, always first briefly describe what you'll be doing and ask for my consent. After
  implementation, write down in RELEASE.md file what was implemented in the format "- Date: Short-Label Implementation
  Description"
- Use Go 1.26
- Comment all function, structs, vars
- This is a 24/7 service. Interaction with external services, such as a database, RTDB via API must support reconnection when the connection is broken and connection attempts at a specified interval during initial initialization


## Project

A SCADA simulator for testing purposes, written in Go. `cmd/main.go` runs a gRPC server (the `TelemetryStream` service in `api/scada/telemetry.proto`) that clients — chiefly the DMS backend (see `api/scada/openapi.json`) — connect to: they Subscribe to points/tags and can send telecontrol (SupervisoryControl) commands, which is meant to be backed by `pkg/breaker.Simulator`. The service handlers are not implemented yet (registered with the generated `UnimplementedTelemetryStreamServer`, so every RPC currently reports `Unimplemented`) — bridging them to a `Simulator` is the next step.

Module: `scada-simulator`, Go 1.26 (`go.mod`).

## Commands

```
go build ./...
go vet ./...
gofmt -l .              # must produce no output
go test ./...
go test -race ./...
go test ./pkg/qds/ -run TestQuality_Has   # run a single test
```

There is no Makefile or CI config in this repo; the commands above are the whole workflow.

## Architecture

- `cmd/main.go` — parses flags/config, constructs a `pkg/breaker.Simulator` from `config.Config.Breaker`, and runs a `google.golang.org/grpc` server on `Dms.Api.Scada.Grpc.{Host,Port}` with `pkg/telemetry`'s `TelemetryStream` service registered (currently `UnimplementedTelemetryStreamServer` — no RPC logic yet). Follows the shutdown pattern from the sibling `rdss-dms-rtdb` project: `signal.NotifyContext` → `GracefulStop` with a timeout fallback to `Stop()`. gRPC reflection is registered only when the log level is Debug or above.
- `internal/configuration/` — the application's own config schema (`Configuration` struct: `Logging`, `Dms.Api.Database.Dsn`, `Dms.Api.Scada.{Username,Password,Grpc.Host,Grpc.Port}`, `Breaker` (a `pkg/breaker.Config`)), read via `pkg/configuration`. `config/scada-simulator.yaml` is a sample config file matching this schema, including a populated `breaker:` section.
- `pkg/configuration/` — a generic, standalone (own LICENSE) YAML + environment-variable configuration loader. Struct fields opt into env-var overrides via an `env:"true"` tag alongside their `yaml:"..."` tag; `ReadFromEnv`/`ListEnv` walk the struct with reflection to derive env var names from the nested yaml tags (parent tag + `_` + field tag, upper-cased, optionally prefixed with `EnvVarPrefix`). `Read(file, s)` loads the YAML file then applies env overrides on top. Only scalar fields (string/bool/int/[]string) support `env:"true"`; nested structs/arrays like `pkg/breaker.Config` are YAML-only.
- `pkg/llog/` — a standalone (own LICENSE) leveled logger wrapping the standard `log` package (`Trace`/`Debug`/`Info`/`Warning`/`Error`), with a package-level `Logger` singleton set up in `init()`. Log level is configured via `internal/configuration`'s `Logging.Level` field. `pkg/breaker` does not import this package directly — it declares its own minimal `breaker.Logger` interface (`Warnf`/`Errorf`/`Infof`) and defaults to a no-op logger; attach `llog.Logger` (or any other logger with that method set) via `breaker.Config{}.WithConfig(llog.Logger)`.
- `pkg/qds/` — SCADA point-quality bitflags (`Quality`, a `uint32`), e.g. `QdsInvalid`, `QdsNoSource`, `QdsSubstituted`, `QdsAlarmLl`/`QdsAlarmUl`, etc. `GetPriority()` picks a single representative flag from a set bitmask by priority order (for things like choosing a badge/border color). Has real unit tests in `qds_test.go` — follow this package's table-driven style when adding tests elsewhere. `pkg/breaker` reuses `qds.Quality` rather than defining its own.

### `api/scada/`

`telemetry.proto` is the gRPC contract `cmd/main.go`'s server implements (client/substation-scoped `Subscribe` point stream, plus `SupervisoryControl` for telecontrol commands); its generated Go code lives in `pkg/telemetry/` (`telemetry.pb.go`, `telemetry_grpc.pb.go` — `DO NOT EDIT`, regenerate from the `.proto` instead). `openapi.json` is the broader external RDSS SCADA/DMS backend's full REST API (250+ endpoints: telemetry, alarms, select/confirm/execute/cancel control, DMS status/LFA, etc.), not yet consumed by any Go code here — grep it rather than reading it whole, it's large.

### `pkg/breaker`

An in-memory HV circuit breaker + protection relay (IED) simulator exposing a SCADA-facing interface (`scada.go`), implemented per the design spec in `tmp/hvcbagentprompt.md` (gitignored, present in this checkout only — consult it before extending the protection/autoreclose model). See `pkg/breaker/README.md` for the SCADA-requirement-to-interface mapping and how to run its `Example`. Key pieces: `types.go` (`Position`, `Measurement`), `protection.go` (IEC 60255 curves, `OperateTime`), `autoreclose.go` (pure ANSI-79 state machine), `control.go` (select-before-operate, `errors.AsType`-friendly error types), `config.go` (`Config`, YAML-loadable), `events.go`/`scada.go` (interfaces), `simulator.go` (the concrete `Simulator` — a single run-loop goroutine owns all state; external calls go through `do`/`doCtx` closures posted over a channel, which is what keeps it race-free without a mutex).

## Conventions

- MIT-licensed subpackages under `pkg/` (`pkg/llog`, `pkg/qds`, `pkg/configuration`) each carry their own `LICENSE` file and are written to be usable independently of this application — avoid introducing dependencies from these packages back onto `internal/` or `cmd/`.
- Table-driven tests, following `pkg/qds/qds_test.go`.
