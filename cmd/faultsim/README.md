# faultsim

A console client for `api/scada/faultsim.proto`'s `FaultSimulator` gRPC
service. It injects one three-phase measurement into a named breaker of a
running `cmd/scada-simulator` instance, driving that breaker's protection
evaluation exactly like calling `pkg/breaker.EmulatorFeed.InjectMeasurement`
from Go directly — a current above the breaker's configured pickup trips
it, which can in turn start its autoreclose sequence.

It talks to the same gRPC server/port `cmd/scada-simulator` serves
`TelemetryStream` on (`Dms.Api.Scada.Grpc.{Host,Port}` in its config, default
`50051`) — `FaultSimulator` is a second service registered on that one
server, not a separate listener.

## Build

```
go build -o faultsim ./cmd/faultsim/
```

## Usage

```
faultsim -breaker <name> [flags]
```

| Flag              | Default          | Meaning                                                                          |
|-------------------|------------------|-----------------------------------------------------------------------------------|
| `-addr`           | `127.0.0.1:50051`| scada-simulator gRPC server address                                              |
| `-breaker`        | *(required)*     | breaker name to inject into, e.g. `feeder-1` — must match its config's `name`    |
| `-current`        | `""` (all zero)  | phase current(s), amps RMS: one number for all three phases, or `"a,b,c"`        |
| `-voltage`        | `""` (all zero)  | phase voltage(s), volts RMS phase-to-neutral: one number for all three, or `"a,b,c"` |
| `-active-power`   | `0`              | three-phase active power, W                                                      |
| `-reactive-power` | `0`              | three-phase reactive power, VAR                                                  |
| `-apparent-power` | `0`              | three-phase apparent power, VA                                                   |
| `-frequency`      | `0`              | power-system frequency, Hz                                                      |
| `-timeout`        | `5s`             | gRPC call timeout                                                                |

Only `-current` matters for tripping protection — the rest exist so a
telemetry consumer watching `Subscribe` sees a complete, realistic
measurement rather than zeros on every other analog tag. Every call prints
the `InjectMeasurementResult` (`OK` or `ERROR_BREAKER_NOT_FOUND`) and exits
non-zero on anything but `OK`, so it composes in scripts.

**A fault only trips a breaker that's currently `Closed`.** A breaker that
isn't carries no current for a real relay to see, so `-current` on an
already-`Open` breaker (e.g. `feeder-2` before it's ever been closed, or
any breaker sitting `Open` after a trip/`Lockout`) is a no-op — nothing
picks up, nothing trips. This matters after clearing a `Lockout`
(`SetMode`'s `On`, or `ResetLockout` — see `pkg/breaker/README.md`'s
Autoreclose section): the breaker sits `Open`/`Normal`/`Ready`, and a fault
you never cleared stays silently pending until the breaker is actually
closed again — only then does it trip, starting a fresh reclose cycle.

**A single call is not a one-shot event.** The server keeps re-feeding
whatever you last injected for a breaker (every 200ms — a simulated sample
rate) until a later call for that same breaker replaces it — see
`internal/faultsimserver`'s package doc comment. So `faultsim -breaker
feeder-1 -current 3000` doesn't just cause one trip: it sets feeder-1's
*ongoing* condition to "3000A fault," which persists across every
autoreclose attempt through to `Lockout`, exactly as a real fault would
stay on the line until it's actually cleared. To clear a fault, inject a
new value back below pickup, e.g. `faultsim -breaker feeder-1 -current 0`
— there's no separate "stop"/"clear" command, since setting the current
value back down *is* clearing it.

The examples below assume `config/scada-simulator.yaml`'s two sample
breakers:

| Breaker    | Stage 51 pickup (`pickup_current`) | Stage 50 pickup (`instantaneous_pickup`) | `auto_reclose` |
|------------|-------------------------------------|-------------------------------------------|----------------|
| `feeder-1` | 400 A (IEC standard inverse, TMS 0.3) | 2000 A | 2 attempts, dead times 5s/15s |
| `feeder-2` | 250 A (IEC very inverse, TMS 0.2)     | 1500 A | 3 attempts, dead times 5s/15s/60s |

Start the server first, in another terminal:

```
go build -o scada-simulator ./cmd/scada-simulator/
./scada-simulator -config config/scada-simulator.yaml
```

Its own stdout is the easiest way to watch the effect of every scenario
below — `breaker[<name>]: protection trip: ...` /
`breaker[<name>]: autoreclose attempt: ...` /
`breaker[<name>]: autoreclose lockout: ...` are logged at Warning/Info/Error
as they happen. A real SCADA client (or a throwaway one against
`TelemetryStream.Subscribe`) would see the same events as tag updates.

## Scenarios

### 1. Instantaneous trip, cleared after the first reclose attempt

A current above `instantaneous_pickup` trips almost immediately (a short,
fixed delay — a couple of power-system cycles). Because the fault is
*sustained*, it survives the first reclose attempt too — to let it succeed,
clear the fault (inject a value below pickup) once you've seen it reclose:

```
faultsim -breaker feeder-1 -current 3000
# ... wait ~5.1s for the first reclose attempt (dead time + mechanical operate) ...
faultsim -breaker feeder-1 -current 0
```

Server log (real run):

```
breaker[feeder-1]: protection trip: {Cause:Instantaneous Current:3000 OperateTime:30ms}
breaker[feeder-1]: autoreclose attempt: {Attempt:1 DeadTime:5s}
```

— and no further trip after clearing: the breaker stays `Closed`,
`protection.state` back to `Normal`, `autoreclose.state` back to `Ready`.
If you skip the clear step, feeder-1 re-trips as soon as the fault is
re-fed during that first `Reclaim` window and moves on to attempt 2 —
see scenario 3.

### 2. Inverse-time (stage 51) trip

A current between `pickup_current` and `instantaneous_pickup` trips only
once the IEC 60255 inverse-time curve has "run out" — the accumulated
exposure-to-time ratio, integrated across real wall-clock time while the
fault is sustained. One injection is enough; the server's own re-feeding
(every 200ms) is what advances it over time. For `feeder-1` at 800A (2x
`pickup_current`, IEC standard inverse, TMS 0.3), that curve computes to
`OperateTime` ≈ 3.0s:

```
faultsim -breaker feeder-1 -current 800
```

Server log (real run, ~3s later):

```
breaker[feeder-1]: protection trip: {Cause:TimeOvercurrent Current:800 OperateTime:3.008708106s}
```

(`OperateTime` for a given curve/TMS/pickup is computed by
`pkg/breaker.OperateTime`, not hardcoded.) A current below `pickup_current`
never picks up at all. To let it pick up without ever tripping, clear it
before `OperateTime` elapses:

```
faultsim -breaker feeder-1 -current 800
sleep 1.5
faultsim -breaker feeder-1 -current 0
```

— confirmed no trip line appears at all; `protection.state` was briefly
`PickedUp`, then decays back to `Normal` over `ResetTime` once the clearing
injection (current back below pickup) starts being sustained instead.

### 3. Sustained fault through to lockout

Since a fault is sustained automatically, one injection left uncleared is
enough — the same scenario `pkg/breaker/example_test.go`'s `Example` walks
through programmatically, but here just from a single CLI call:

```
faultsim -breaker feeder-1 -current 3000
```

Server log (real run, `feeder-1`'s `max_attempts: 2`, dead times 5s/15s —
~20s from injection to lockout):

```
breaker[feeder-1]: protection trip: {Cause:Instantaneous Current:3000 OperateTime:30ms}
breaker[feeder-1]: autoreclose attempt: {Attempt:1 DeadTime:5s}
breaker[feeder-1]: protection trip: {Cause:Instantaneous Current:3000 OperateTime:30ms}
breaker[feeder-1]: autoreclose attempt: {Attempt:2 DeadTime:15s}
breaker[feeder-1]: protection trip: {Cause:Instantaneous Current:3000 OperateTime:30ms}
breaker[feeder-1]: autoreclose lockout: {Attempts:2}
```

Once in `Lockout`, protection evaluation stops altogether (`Tripped`/
`Lockout` both short-circuit `pkg/breaker`'s `evaluateProtection`), so
further sustained injections are silently no-ops — the breaker stays open
regardless of `-current`. `BreakerController.ResetLockout` (not exposed by
this CLI; only reachable from Go, or by extending `faultsim.proto`) is what
an operator would call to clear it and return to `Normal`.

### 4. Asymmetric (single-/two-phase) fault

`-current`/`-voltage` also accept `"a,b,c"` for a fault that isn't
symmetric across all three phases — protection evaluates on the *maximum*
of the three phase currents, so this trips the same way a symmetric fault
of that magnitude would, but is useful for exercising a telemetry
consumer's per-phase display. Uses `feeder-1` here specifically because it
starts `Closed` — protection only ever evaluates while a breaker is
actually `Closed` (a breaker that isn't carries no current for a real relay
to see), so injecting a fault into `feeder-2` before it's ever been closed
does nothing:

```
faultsim -breaker feeder-1 -current 100,200,3000
```

```
OK
```

Trips `feeder-1` on phase C alone (3000A ≫ its 2000A instantaneous pickup)
while phases A/B stay at load-level values on
`feeder-1.current.a`/`.current.b`.

### 5. Benign telemetry-only injection

A measurement entirely below every configured pickup never trips anything
— useful for pushing realistic analog values (voltage, power, frequency)
through `Subscribe` without touching breaker state, e.g. to test a
dashboard's normal-operation rendering. Like any injection it's sustained
(re-fed every 200ms) until replaced, so it keeps refreshing those tags for
as long as the server runs:

```
faultsim -breaker feeder-1 -current 50 -voltage 230 -active-power 15000 -reactive-power 3000 -apparent-power 15300 -frequency 50.0
```

### 6. Scripting / error handling

```
$ faultsim -breaker no-such-breaker -current 3000
ERROR_BREAKER_NOT_FOUND
$ echo $?
1

$ faultsim -current 100
missing required -breaker flag
Usage of faultsim:
  ...
$ echo $?
2
```

`ERROR_BREAKER_NOT_FOUND` is the only rejection this service returns —
`sim.InjectMeasurement` itself never fails (see its doc comment in
`pkg/breaker`), so any other non-zero exit means the RPC couldn't complete
at all (connection refused, timeout, etc.), not a rejected request.
