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
it only prefixes this `Simulator`'s internal log messages so logs from
multiple breakers are distinguishable. In this repository it is embedded as
`Configuration.Breaker` in `internal/configuration`, populated from the
`breaker:` section of `config/scada-simulator.yaml`.

## Running the example

```
go test ./pkg/breaker/ -run Example -v
```

[`example_test.go`](example_test.go) constructs a `Simulator`, injects a
sustained fault current via `EmulatorFeed`, and observes: the protection
relay trip the breaker, an autoreclose sequence attempt to recover it, and
— because the fault never clears — the sequence exhaust its attempts and
reach `Lockout`.
