# pkg/breaker

An in-memory simulator of a high-voltage (HV) circuit breaker and its
protection relay (IED), exposing a SCADA-facing Go interface plus a
runnable [`Simulator`](simulator.go) that drives the model with injected
three-phase analog values.

This is a domain simulation for engineering and demonstration purposes, not
a real protocol stack: it deliberately implements no IEC 61850, IEC
60870-5-104, DNP3, Modbus or OPC UA — the interfaces in [`scada.go`](scada.go)
are pure Go, and a real SCADA gateway would implement protocol adapters on
top of them. It also does not model arc physics or waveform-level behaviour,
and holds no persistent state. See [`doc.go`](doc.go) for the full list of
non-goals.

## SCADA requirement to interface mapping

| # | Requirement                              | Interface                 |
|---|-------------------------------------------|----------------------------|
| 1 | Reading breaker position                  | `PositionReader`           |
| 2 | Reading three-phase analog measurements    | `AnalogReader`              |
| 3 | Reading/setting protection relay settings  | `ProtectionConfigurator`    |
| 4 | Configuring automatic reclosing            | `AutoRecloseConfigurator`   |
| 5 | Remote control (select-before-operate, block/unblock) | `BreakerController` |
| 6 | Observing protection/autoreclose status    | `ProtectionStatusReader`    |
| 7 | Feeding plant values into the simulation   | `EmulatorFeed`              |
| — | Push-based notifications                   | `EventSubscriber`           |

`Simulator` (see [`simulator.go`](simulator.go)) implements every interface
above. A single internal goroutine owns all mutable state and serializes
injected measurements, control commands, protection evaluation and the
autoreclose sequence through it, so the whole package is race-free without
exposing a mutex.

`BreakerController` also has `Block`/`Unblock`, e.g. for an operator's
maintenance block/tag: while blocked, `Select` and `Operate` are rejected
with `*InterlockError`. The blocked state is tracked internally
(`simState.blocked`/`blockReason`) and reflected as `qds.QdsBlocked` on the
quality `Position()` returns, so it rides on the same measurement SCADA
already polls, rather than being a separate status only visible some other
way.

## Protection

`OperateTime` (in [`protection.go`](protection.go)) implements the IEC
60255 inverse-time overcurrent formula for the stage-51 (time-overcurrent)
element; `InstantaneousOperateTime` models the stage-50 (instantaneous)
element as a short fixed delay. Both are pure functions, independent of the
simulator's timing machinery, and are unit tested directly in
[`protection_test.go`](protection_test.go).

## Autoreclose

The ANSI device 79 automatic reclose sequence
(`Ready → DeadTime → Closing → Reclaim → Ready`, or `→ Lockout` after
`MaxAttempts`) is implemented as a pure, event-driven state machine in
[`autoreclose.go`](autoreclose.go), independently unit tested in
[`autoreclose_test.go`](autoreclose_test.go). `Simulator` drives it with
real timers; the state machine itself owns none.

## Configuration

`Config` (see [`config.go`](config.go)) describes a breaker's initial state
and fixed operating characteristics — name, position, protection settings,
autoreclose behaviour, mechanical operate time, nominal frequency — and can
be loaded directly from YAML. `Name` has no effect on simulation behaviour;
it prefixes this `Simulator`'s internal log messages and identifies it in
SCADA tag names (see below) so multiple breakers are distinguishable. In
this repository more than one breaker can be emulated at once: each entry
in `Configuration.Breakers` (`internal/configuration`) is one `Config`,
populated from the `breakers:` list in `config/scada-simulator.yaml` — the
number of breakers emulated is simply the length of that list.

## SCADA tag naming

Implemented by `internal/telemetryserver.TelemetryServer` for `Subscribe`
and `SupervisoryControl` (see its package doc and `control.go`), and
listable ahead of time via `internal/restserver`'s `GET
/api/external/telemetry/tags` (`telemetryserver.TagsFor`). Every tag is
hierarchical and dot-separated, `<breaker-name>.<category>.<leaf>`, never an
underscore-joined compound like `active_power` — e.g. `power.active`, not
`active_power`. `<breaker-name>` is `Config.Name`, so tags stay unambiguous
across multiple emulated breakers.

| Tag | Source | Values | SCADA type |
|---|---|---|---|
| `<name>.position` | `Position()` (read-only) | `Intermediate` / `Open` / `Closed` / `Bad` | integer (2-bit DPI) |
| `<name>.blocked` | `Position()`'s quality (`qds.QdsBlocked`) | boolean | boolean (1-bit DI) |
| `<name>.control` | *(control-only — see below; listed by `GET /api/external/telemetry/tags`, but never read via Subscribe)* | `0` = Open, `1` = Close | boolean (1-bit DI) |
| `<name>.current.a` / `.current.b` / `.current.c` | `Measurement()` | amps | float |
| `<name>.voltage.a` / `.voltage.b` / `.voltage.c` | `Measurement()` | volts | float |
| `<name>.power.active` / `.power.reactive` / `.power.apparent` | `Measurement()` | W / VAR / VA | float |
| `<name>.frequency` | `Measurement()` | Hz | float |
| `<name>.protection.state` | `ProtectionStatus()` | `Normal` / `PickedUp` / `Tripped` / `Lockout` | boolean (1-bit DI) |
| `<name>.protection.group` | `ProtectionStatus()` | 1–4 | boolean (1-bit DI) |
| `<name>.autoreclose.state` | `AutoRecloseStatus()` | `Ready` / `DeadTime` / `Closing` / `Reclaim` / `Lockout` | boolean (1-bit DI) |
| `<name>.autoreclose.attempt` | `AutoRecloseStatus()` | 0, 1, 2, … | boolean (1-bit DI) |

`<name>.position` is the only tag reported as a 2-bit integer: it is this
package's one point with true double-point semantics (four states,
mirroring a real breaker's 52a/52b auxiliary contacts). Every other
non-analog tag — even one carrying more than two possible values, like
`protection.state` or `autoreclose.attempt` — is reported as an ordinary
1-bit `boolean` DataPoint instead, still carrying its real numeric `Value`
(`internal/telemetryserver`'s `diPoint`, as opposed to `intPoint`, reserved
for `position`).

`<name>.control` is deliberately a separate tag from `<name>.position`: in
`api/scada/telemetry.proto`'s `ScadaSupervisoryControlRequest`, `key`
identifies the point being controlled, and a control point is a distinct
SCADA object from the read-only status point reporting the same physical
breaker's position (standard IEC 60870/61850 practice) — a client writes
`<name>.control`, never `<name>.position`. `select`/`execute` follow
`BreakerController`'s select-before-operate semantics (a select-only
request arms the command; a later execute-only request for the same `key`
carries it out — the server tracks the pending selection itself, since the
request has no field to echo a token back); a request with both set
performs both in one call. `test` always performs an immediate Select+Execute
(regardless of `select`/`execute`'s own values) and puts the breaker into
test mode: every tag this server reports for it via `Subscribe` carries
`qds.QdsTest` quality until a later call explicitly sets `test` back to
`false`, which clears it.

## Running the example

```
go test ./pkg/breaker/ -run Example -v
```

[`example_test.go`](example_test.go) constructs a `Simulator`, injects a
sustained fault current via `EmulatorFeed`, and observes: the protection
relay trip the breaker, an autoreclose sequence attempt to recover it, and
— because the fault never clears — the sequence exhaust its attempts and
reach `Lockout`.
