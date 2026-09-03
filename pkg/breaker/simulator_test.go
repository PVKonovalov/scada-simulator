package breaker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"scada-simulator/pkg/qds"
)

// waitForEvent reads from ch until it sees an event of kind, failing the
// test if none arrives before the deadline embedded in ctx.
func waitForEvent(t *testing.T, ctx context.Context, ch <-chan Event, kind EventKind) Event {
	t.Helper()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("event channel closed while waiting for %v", kind)
			}
			if ev.Kind == kind {
				return ev
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %v", kind)
		}
	}
}

// TestSimulator_TripAndReclose injects a sustained overcurrent fault and
// checks the breaker trips via the instantaneous stage, then successfully
// autorecloses once, returning to a healthy Normal/Closed state.
func TestSimulator_TripAndReclose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sim := New(ctx, Config{
		InitialPosition: PositionClosed,
		SettingsGroups: []SettingsGroupConfig{
			{Group: SettingsGroup1, Settings: ProtectionSettings{InstantaneousPickup: 1000}},
		},
		AutoReclose: AutoRecloseConfig{
			Enabled:     true,
			MaxAttempts: 1,
			DeadTimes:   []time.Duration{5 * time.Millisecond},
			ReclaimTime: 5 * time.Millisecond,
		},
		MechanicalOperateTime: 5 * time.Millisecond,
		NominalFrequencyHz:    1000, // shrinks the instantaneous delay to 1.5ms for a fast test
	})
	defer sim.Close()

	events, err := sim.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	fault := Measurement{Current: PhaseValues{A: 2000, B: 2000, C: 2000}}
	if err := sim.InjectMeasurement(fault); err != nil {
		t.Fatalf("InjectMeasurement: %v", err)
	}

	trip := waitForEvent(t, ctx, events, EventTrip)
	detail := trip.Detail.(TripDetail)
	if detail.Cause != CauseInstantaneous {
		t.Errorf("trip cause = %v, want Instantaneous", detail.Cause)
	}

	waitForEvent(t, ctx, events, EventAutoRecloseAttempt)

	// Wait for the breaker to settle Closed again after the reclose.
	for {
		ev := waitForEvent(t, ctx, events, EventPositionChanged)
		if ev.Detail.(PositionChangedDetail).Position == PositionClosed {
			break
		}
	}

	pos, _ := sim.Position()
	if pos != PositionClosed {
		t.Errorf("Position() = %v, want Closed", pos)
	}
	if status := sim.ProtectionStatus(); status.State != ProtectionNormal {
		t.Errorf("ProtectionStatus().State = %v, want Normal", status.State)
	}

	succeeded := waitForEvent(t, ctx, events, EventAutoRecloseSucceeded)
	if attempts := succeeded.Detail.(AutoRecloseSucceededDetail).Attempts; attempts != 1 {
		t.Errorf("AutoRecloseSucceededDetail.Attempts = %d, want 1", attempts)
	}
	if status := sim.AutoRecloseStatus(); status.State != AutoRecloseReady {
		t.Errorf("AutoRecloseStatus().State = %v, want Ready", status.State)
	}
}

// TestSimulator_MeasurementChangedOnlyOnRealChange checks that
// EventMeasurementChanged is only published when an injected measurement
// actually differs from the previously stored one — repeating the exact
// same reading (as internal/faultsimserver's sustain loop does for a
// persistent fault) must not flood Subscribe clients with duplicate
// updates, but a genuinely different reading must still be reported.
func TestSimulator_MeasurementChangedOnlyOnRealChange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sim := New(ctx, Config{InitialPosition: PositionClosed})
	defer sim.Close()

	events, err := sim.Subscribe(ctx, EventMeasurementChanged)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	m := Measurement{Current: PhaseValues{A: 100, B: 100, C: 100}}
	if err := sim.InjectMeasurement(m); err != nil {
		t.Fatalf("InjectMeasurement: %v", err)
	}
	waitForEvent(t, ctx, events, EventMeasurementChanged)

	// Same reading again: must not publish a second event.
	if err := sim.InjectMeasurement(m); err != nil {
		t.Fatalf("InjectMeasurement (repeat): %v", err)
	}
	select {
	case ev := <-events:
		t.Errorf("unexpected event for an unchanged measurement: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}

	// A genuinely different reading must still be published.
	m.Current.A = 150
	if err := sim.InjectMeasurement(m); err != nil {
		t.Fatalf("InjectMeasurement (changed): %v", err)
	}
	changed := waitForEvent(t, ctx, events, EventMeasurementChanged)
	if got := changed.Detail.(Measurement).Current.A; got != 150 {
		t.Errorf("changed event Current.A = %v, want 150", got)
	}
}

// TestSimulator_LockoutRejectsManualClose checks that Select/Operate reject
// a manual command while the autoreclose sequence is in Lockout, per
// ResetLockout's documented ANSI 79 requirement that clearing Lockout takes
// an explicit operator action — a manual Close must not be able to bypass
// that and leave protectionState stuck at Lockout forever (completeOperate
// only ever clears Tripped, deliberately not Lockout). It also checks that
// reaching Lockout automatically puts the breaker into ModeBlocked, and
// that ResetLockout clears both Lockout and ModeBlocked together, after
// which the same command succeeds.
func TestSimulator_LockoutRejectsManualClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sim := New(ctx, Config{
		InitialPosition: PositionClosed,
		SettingsGroups: []SettingsGroupConfig{
			{Group: SettingsGroup1, Settings: ProtectionSettings{InstantaneousPickup: 1000}},
		},
		AutoReclose: AutoRecloseConfig{
			Enabled:     true,
			MaxAttempts: 1,
			DeadTimes:   []time.Duration{2 * time.Millisecond},
			// Comfortably longer than the instantaneous delay (1.5ms) plus
			// MechanicalOperateTime (5ms), so the re-injected fault's own
			// trip is guaranteed to reach Open (and so reach autoReclose's
			// OnTrip) before this reclaim timer would otherwise elapse and
			// reset the attempt count — see pkg/breaker/example_test.go's
			// Example, which documents the same margin requirement.
			ReclaimTime: 50 * time.Millisecond,
		},
		MechanicalOperateTime: 5 * time.Millisecond,
		NominalFrequencyHz:    1000, // shrinks the instantaneous delay to 1.5ms for a fast test
	})
	defer sim.Close()

	events, err := sim.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	fault := Measurement{Current: PhaseValues{A: 2000, B: 2000, C: 2000}}
	if err := sim.InjectMeasurement(fault); err != nil {
		t.Fatalf("InjectMeasurement: %v", err)
	}

	// Wait for the single configured attempt to reclose, then re-inject the
	// still-present fault so it re-trips during Reclaim and the sequence
	// reaches Lockout (MaxAttempts exceeded).
	for {
		ev := waitForEvent(t, ctx, events, EventPositionChanged)
		if ev.Detail.(PositionChangedDetail).Position == PositionClosed {
			break
		}
	}
	if err := sim.InjectMeasurement(fault); err != nil {
		t.Fatalf("InjectMeasurement (re-fault): %v", err)
	}
	waitForEvent(t, ctx, events, EventLockout)

	if status := sim.ProtectionStatus(); status.State != ProtectionLockout {
		t.Fatalf("ProtectionStatus().State = %v, want Lockout", status.State)
	}
	posBefore, _ := sim.Position()
	if posBefore != PositionOpen {
		t.Fatalf("Position() = %v, want Open", posBefore)
	}
	if mode := sim.Mode(); mode != ModeBlocked {
		t.Fatalf("Mode() = %v, want ModeBlocked (auto-applied on Lockout)", mode)
	}

	cmd := Command{Target: PositionClosed, Source: "test"}
	if _, err := sim.Select(ctx, cmd); !isInterlockError(err) {
		t.Errorf("Select() during Lockout err = %v, want *InterlockError", err)
	}
	if pos, _ := sim.Position(); pos != PositionOpen {
		t.Errorf("Position() after rejected Select = %v, want unchanged Open", pos)
	}
	if status := sim.ProtectionStatus(); status.State != ProtectionLockout {
		t.Errorf("ProtectionStatus().State after rejected Select = %v, want unchanged Lockout", status.State)
	}

	if err := sim.ResetLockout(ctx); err != nil {
		t.Fatalf("ResetLockout: %v", err)
	}
	if status := sim.ProtectionStatus(); status.State != ProtectionNormal {
		t.Fatalf("ProtectionStatus().State after ResetLockout = %v, want Normal", status.State)
	}
	if mode := sim.Mode(); mode != ModeOn {
		t.Fatalf("Mode() after ResetLockout = %v, want ModeOn (ResetLockout clears the auto-applied ModeBlocked too)", mode)
	}

	id, err := sim.Select(ctx, cmd)
	if err != nil {
		t.Fatalf("Select() after ResetLockout: %v", err)
	}
	if err := sim.Operate(ctx, id, cmd); err != nil {
		t.Fatalf("Operate() after ResetLockout: %v", err)
	}
	for {
		ev := waitForEvent(t, ctx, events, EventPositionChanged)
		if ev.Detail.(PositionChangedDetail).Position == PositionClosed {
			break
		}
	}
}

// TestSimulator_SetModeOnClearsLockout checks that SetMode(ModeOn) is a
// second, equivalent way to recover from Lockout — not just ResetLockout —
// since a caller might have no other way to reach it (e.g. ResetLockout
// isn't exposed over gRPC at all in this repo, only SetMode is).
func TestSimulator_SetModeOnClearsLockout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sim := New(ctx, Config{
		InitialPosition: PositionClosed,
		SettingsGroups: []SettingsGroupConfig{
			{Group: SettingsGroup1, Settings: ProtectionSettings{InstantaneousPickup: 1000}},
		},
		AutoReclose: AutoRecloseConfig{
			Enabled:     true,
			MaxAttempts: 1,
			DeadTimes:   []time.Duration{2 * time.Millisecond},
			ReclaimTime: 50 * time.Millisecond, // see TestSimulator_LockoutRejectsManualClose's comment on this margin
		},
		MechanicalOperateTime: 5 * time.Millisecond,
		NominalFrequencyHz:    1000,
	})
	defer sim.Close()

	events, err := sim.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	fault := Measurement{Current: PhaseValues{A: 2000, B: 2000, C: 2000}}
	if err := sim.InjectMeasurement(fault); err != nil {
		t.Fatalf("InjectMeasurement: %v", err)
	}
	for {
		ev := waitForEvent(t, ctx, events, EventPositionChanged)
		if ev.Detail.(PositionChangedDetail).Position == PositionClosed {
			break
		}
	}
	if err := sim.InjectMeasurement(fault); err != nil {
		t.Fatalf("InjectMeasurement (re-fault): %v", err)
	}
	waitForEvent(t, ctx, events, EventLockout)

	if status := sim.AutoRecloseStatus(); status.State != AutoRecloseLockout {
		t.Fatalf("AutoRecloseStatus().State = %v, want Lockout", status.State)
	}

	if err := sim.SetMode(ctx, ModeOn, ""); err != nil {
		t.Fatalf("SetMode(On): %v", err)
	}
	if status := sim.AutoRecloseStatus(); status.State != AutoRecloseReady {
		t.Fatalf("AutoRecloseStatus().State after SetMode(On) = %v, want Ready (SetMode(On) must clear Lockout too)", status.State)
	}
	if status := sim.ProtectionStatus(); status.State != ProtectionNormal {
		t.Fatalf("ProtectionStatus().State after SetMode(On) = %v, want Normal", status.State)
	}

	cmd := Command{Target: PositionClosed, Source: "test"}
	id, err := sim.Select(ctx, cmd)
	if err != nil {
		t.Fatalf("Select() after SetMode(On): %v", err)
	}
	if err := sim.Operate(ctx, id, cmd); err != nil {
		t.Fatalf("Operate() after SetMode(On): %v", err)
	}
}

// TestSimulator_RecoveryDoesNotRetripWhileOpen checks a real reported bug:
// after SetMode(ModeOn)/ResetLockout clears Lockout, the breaker is left
// sitting physically Open (neither function touches position) — a still-
// ongoing fault (e.g. internal/faultsimserver's sustained injection) must
// not immediately re-arm and trip an already-open breaker, kicking off a
// brand-new autoreclose cycle nothing actually closed into. A fresh trip
// must only ever follow a genuine Close of the breaker.
func TestSimulator_RecoveryDoesNotRetripWhileOpen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sim := New(ctx, Config{
		InitialPosition: PositionClosed,
		SettingsGroups: []SettingsGroupConfig{
			{Group: SettingsGroup1, Settings: ProtectionSettings{InstantaneousPickup: 1000}},
		},
		AutoReclose: AutoRecloseConfig{
			Enabled:     true,
			MaxAttempts: 1,
			DeadTimes:   []time.Duration{2 * time.Millisecond},
			ReclaimTime: 50 * time.Millisecond, // see TestSimulator_LockoutRejectsManualClose's comment on this margin
		},
		MechanicalOperateTime: 5 * time.Millisecond,
		NominalFrequencyHz:    1000,
	})
	defer sim.Close()

	events, err := sim.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	fault := Measurement{Current: PhaseValues{A: 2000, B: 2000, C: 2000}}
	if err := sim.InjectMeasurement(fault); err != nil {
		t.Fatalf("InjectMeasurement: %v", err)
	}
	for {
		ev := waitForEvent(t, ctx, events, EventPositionChanged)
		if ev.Detail.(PositionChangedDetail).Position == PositionClosed {
			break
		}
	}
	if err := sim.InjectMeasurement(fault); err != nil {
		t.Fatalf("InjectMeasurement (re-fault): %v", err)
	}
	waitForEvent(t, ctx, events, EventLockout)

	if err := sim.SetMode(ctx, ModeOn, ""); err != nil {
		t.Fatalf("SetMode(On): %v", err)
	}

	// The fault is still "present" (nothing cleared it) — feeding it again,
	// as a sustain loop would, must not re-trip the still-Open breaker.
	for range 5 {
		if err := sim.InjectMeasurement(fault); err != nil {
			t.Fatalf("InjectMeasurement (still-present fault): %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pos, _ := sim.Position(); pos != PositionOpen {
		t.Errorf("Position() = %v, want unchanged Open (no close ever happened)", pos)
	}
	if status := sim.ProtectionStatus(); status.State != ProtectionNormal {
		t.Errorf("ProtectionStatus().State = %v, want unchanged Normal (no spurious re-trip)", status.State)
	}
	if status := sim.AutoRecloseStatus(); status.State != AutoRecloseReady {
		t.Errorf("AutoRecloseStatus().State = %v, want unchanged Ready (no spurious autoreclose cycle)", status.State)
	}

	// Only a genuine Close lets the still-present fault trip it again, from
	// a fresh attempt 1.
	cmd := Command{Target: PositionClosed, Source: "test"}
	id, err := sim.Select(ctx, cmd)
	if err != nil {
		t.Fatalf("Select(): %v", err)
	}
	if err := sim.Operate(ctx, id, cmd); err != nil {
		t.Fatalf("Operate(): %v", err)
	}
	for {
		ev := waitForEvent(t, ctx, events, EventPositionChanged)
		if ev.Detail.(PositionChangedDetail).Position == PositionClosed {
			break
		}
	}
	if err := sim.InjectMeasurement(fault); err != nil {
		t.Fatalf("InjectMeasurement (fault after manual close): %v", err)
	}
	trip := waitForEvent(t, ctx, events, EventTrip)
	if trip.Detail.(TripDetail).Cause != CauseInstantaneous {
		t.Errorf("trip cause = %v, want Instantaneous", trip.Detail.(TripDetail).Cause)
	}
	attempt := waitForEvent(t, ctx, events, EventAutoRecloseAttempt)
	if got := attempt.Detail.(AutoRecloseAttemptDetail).Attempt; got != 1 {
		t.Errorf("autoreclose attempt = %d, want 1 (a fresh cycle)", got)
	}
}

// TestSimulator_SelectOperateCancel exercises the basic select-before-
// operate flow and its interlocks/errors, using errors.AsType as
// documented on InterlockError and UnknownSelectionError.
func TestSimulator_SelectOperateCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sim := New(ctx, Config{InitialPosition: PositionOpen, MechanicalOperateTime: time.Millisecond})
	defer sim.Close()

	cmd := Command{Target: PositionClosed, Source: "test"}
	id, err := sim.Select(ctx, cmd)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}

	// A second Select while one is pending must be interlocked.
	_, err = sim.Select(ctx, cmd)
	if !isInterlockError(err) {
		t.Errorf("second Select() err = %v, want *InterlockError", err)
	}

	if err := sim.Operate(ctx, id, cmd); err != nil {
		t.Fatalf("Operate: %v", err)
	}

	// The selection was consumed by Operate; using it again is unknown.
	if err := sim.Operate(ctx, id, cmd); !isUnknownSelection(err) {
		t.Errorf("replayed Operate() err = %v, want *UnknownSelectionError", err)
	}

	// Wait for the mechanism to settle before the Cancel-on-unknown-id check.
	time.Sleep(10 * time.Millisecond)

	if err := sim.Cancel(ctx, "never-issued"); !isUnknownSelection(err) {
		t.Errorf("Cancel(unknown) err = %v, want *UnknownSelectionError", err)
	}
}

// TestSimulator_IdempotentOperateEmitsConfirmation checks that a successful
// Operate matching the breaker's current position — a no-op, since nothing
// physically moves — still emits an EventPositionChanged confirming it,
// rather than silently doing nothing observable. Without this, a SCADA
// client that (re)issues a command matching the breaker's current state
// gets no positive response on the position tag.
func TestSimulator_IdempotentOperateEmitsConfirmation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sim := New(ctx, Config{InitialPosition: PositionClosed, MechanicalOperateTime: time.Millisecond})
	defer sim.Close()

	events, err := sim.Subscribe(ctx, EventPositionChanged)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	cmd := Command{Target: PositionClosed, Source: "test"} // already Closed: idempotent
	id, err := sim.Select(ctx, cmd)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if err := sim.Operate(ctx, id, cmd); err != nil {
		t.Fatalf("Operate: %v", err)
	}

	ev := waitForEvent(t, ctx, events, EventPositionChanged)
	detail := ev.Detail.(PositionChangedDetail)
	if detail.Position != PositionClosed {
		t.Errorf("confirmation Position = %v, want Closed (unchanged)", detail.Position)
	}

	// The breaker must not have gone through Intermediate: this was a
	// genuine no-op, not a real mechanical operation.
	pos, _ := sim.Position()
	if pos != PositionClosed {
		t.Errorf("Position() = %v, want Closed", pos)
	}
}

// isUnknownSelection reports whether err is an *UnknownSelectionError.
func isUnknownSelection(err error) bool {
	_, ok := errors.AsType[*UnknownSelectionError](err)
	return ok
}

// TestSimulator_SelectionExpires checks that an un-Operated selection frees
// itself after its timeout, and that Operate on the expired id reports
// SelectionExpiredError specifically (not UnknownSelectionError).
func TestSimulator_SelectionExpires(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sim := New(ctx, Config{InitialPosition: PositionOpen, SelectionTimeout: 10 * time.Millisecond})
	defer sim.Close()

	cmd := Command{Target: PositionClosed, Source: "test"}
	id, err := sim.Select(ctx, cmd)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}

	time.Sleep(50 * time.Millisecond) // well past the 10ms selection timeout

	err = sim.Operate(ctx, id, cmd)
	if _, ok := errors.AsType[*SelectionExpiredError](err); !ok {
		t.Errorf("Operate(expired) err = %v, want *SelectionExpiredError", err)
	}
}

// TestSimulator_SetMode_Blocked checks that SetMode(ModeBlocked) is
// reflected as qds.QdsBlocked on the quality Position() returns, that it
// rejects both a new Select and an Operate on an already-pending selection,
// and that SetMode(ModeOn) clears the quality flag and lets that same
// (still-pending) selection succeed.
func TestSimulator_SetMode_Blocked(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sim := New(ctx, Config{InitialPosition: PositionOpen, MechanicalOperateTime: time.Millisecond})
	defer sim.Close()

	events, err := sim.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	cmd := Command{Target: PositionClosed, Source: "test"}
	id, err := sim.Select(ctx, cmd)
	if err != nil {
		t.Fatalf("Select before block: %v", err)
	}

	if err := sim.SetMode(ctx, ModeBlocked, "maintenance"); err != nil {
		t.Fatalf("SetMode(Blocked): %v", err)
	}
	modeEv := waitForEvent(t, ctx, events, EventModeChanged)
	detail := modeEv.Detail.(ModeChangedDetail)
	if detail.Mode != ModeBlocked || detail.Reason != "maintenance" {
		t.Errorf("ModeChangedDetail = %+v, want {ModeBlocked maintenance}", detail)
	}

	if _, q := sim.Position(); !q.Has(qds.QdsBlocked) {
		t.Errorf("Position() quality = %v, want QdsBlocked set", q)
	}
	if mode := sim.Mode(); mode != ModeBlocked {
		t.Errorf("Mode() = %v, want ModeBlocked", mode)
	}

	if _, err := sim.Select(ctx, cmd); !isInterlockError(err) {
		t.Errorf("Select() while blocked err = %v, want *InterlockError", err)
	}
	if err := sim.Operate(ctx, id, cmd); !isInterlockError(err) {
		t.Errorf("Operate() while blocked err = %v, want *InterlockError", err)
	}

	if err := sim.SetMode(ctx, ModeOn, ""); err != nil {
		t.Fatalf("SetMode(On): %v", err)
	}
	waitForEvent(t, ctx, events, EventModeChanged)

	if _, q := sim.Position(); q.Has(qds.QdsBlocked) {
		t.Errorf("Position() quality = %v, want QdsBlocked cleared", q)
	}

	// The selection made before Block survived it and still works.
	if err := sim.Operate(ctx, id, cmd); err != nil {
		t.Errorf("Operate() after unblock: %v", err)
	}
}

// TestSimulator_SetMode_Off checks that ModeOff rejects control the same as
// ModeBlocked, and that InjectMeasurement stores the reading but stops
// evaluating protection and reporting events while Off.
func TestSimulator_SetMode_Off(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sim := New(ctx, Config{InitialPosition: PositionClosed})
	defer sim.Close()

	events, err := sim.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := sim.SetMode(ctx, ModeOff, "decommissioned"); err != nil {
		t.Fatalf("SetMode(Off): %v", err)
	}
	waitForEvent(t, ctx, events, EventModeChanged)
	waitForEvent(t, ctx, events, EventPositionChanged) // SetMode's own quality-refresh confirmation

	cmd := Command{Target: PositionOpen, Source: "test"}
	if _, err := sim.Select(ctx, cmd); !isInterlockError(err) {
		t.Errorf("Select() while off err = %v, want *InterlockError", err)
	}
	waitForEvent(t, ctx, events, EventControlRejected)

	if err := sim.InjectMeasurement(Measurement{Current: PhaseValues{A: 10000, B: 10000, C: 10000}}); err != nil {
		t.Fatalf("InjectMeasurement: %v", err)
	}
	select {
	case ev := <-events:
		t.Errorf("unexpected event while off: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
	if got := sim.Measurement().Current.A; got != 10000 {
		t.Errorf("Measurement().Current.A = %v, want 10000 (still latched while off)", got)
	}
}

// TestSimulator_SetMode_BlockedInhibitsTrip checks that a fault current
// exceeding the instantaneous pickup while ModeBlocked still trips
// protectionState (the function still decides to trip) but does not
// actually open the breaker (no physical output).
func TestSimulator_SetMode_BlockedInhibitsTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sim := New(ctx, Config{
		InitialPosition: PositionClosed,
		SettingsGroups: []SettingsGroupConfig{{
			Group:    0,
			Settings: ProtectionSettings{InstantaneousPickup: 1000},
		}},
	})
	defer sim.Close()

	if err := sim.SetMode(ctx, ModeBlocked, "maintenance"); err != nil {
		t.Fatalf("SetMode(Blocked): %v", err)
	}

	if err := sim.InjectMeasurement(Measurement{Current: PhaseValues{A: 5000, B: 5000, C: 5000}}); err != nil {
		t.Fatalf("InjectMeasurement: %v", err)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if sim.ProtectionStatus().State == ProtectionTripped {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := sim.ProtectionStatus().State; got != ProtectionTripped {
		t.Fatalf("ProtectionStatus().State = %v, want ProtectionTripped", got)
	}
	if pos, _ := sim.Position(); pos != PositionClosed {
		t.Errorf("Position() = %v, want Closed (Blocked must inhibit the physical trip)", pos)
	}
}

// TestSimulator_SetMode_TestBlocked checks ModeTestBlocked's distinguishing
// behavior against both ModeBlocked and ModeTest: like Blocked, a trip is
// inhibited physically (the breaker contact never moves) even though the
// protection decision still stands; unlike Blocked, Select/Operate are
// accepted, not rejected — but even an accepted, confirmed Operate still
// does not move the breaker. Quality carries both QdsTest and QdsBlocked.
func TestSimulator_SetMode_TestBlocked(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sim := New(ctx, Config{
		InitialPosition: PositionClosed,
		SettingsGroups: []SettingsGroupConfig{{
			Group:    0,
			Settings: ProtectionSettings{InstantaneousPickup: 1000},
		}},
	})
	defer sim.Close()

	if err := sim.SetMode(ctx, ModeTestBlocked, "commissioning"); err != nil {
		t.Fatalf("SetMode(TestBlocked): %v", err)
	}
	if got := sim.Mode(); got != ModeTestBlocked {
		t.Fatalf("Mode() = %v, want TestBlocked", got)
	}
	if _, quality := sim.Position(); !quality.Has(qds.QdsTest) || !quality.Has(qds.QdsBlocked) {
		t.Errorf("Position() quality = %v, want both QdsTest and QdsBlocked set", quality)
	}

	// Like Blocked: the protection decision still stands, but the breaker
	// contact never moves.
	if err := sim.InjectMeasurement(Measurement{Current: PhaseValues{A: 5000, B: 5000, C: 5000}}); err != nil {
		t.Fatalf("InjectMeasurement: %v", err)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if sim.ProtectionStatus().State == ProtectionTripped {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := sim.ProtectionStatus().State; got != ProtectionTripped {
		t.Fatalf("ProtectionStatus().State = %v, want ProtectionTripped", got)
	}
	if pos, _ := sim.Position(); pos != PositionClosed {
		t.Errorf("Position() = %v, want Closed (TestBlocked must inhibit the physical trip)", pos)
	}

	// Unlike Blocked: Select/Operate are accepted (confirmed OK), but still
	// do not move the breaker.
	cmd := Command{Target: PositionOpen, Source: "test"}
	id, err := sim.Select(ctx, cmd)
	if err != nil {
		t.Fatalf("Select() while TestBlocked = %v, want accepted", err)
	}
	if err := sim.Operate(ctx, id, cmd); err != nil {
		t.Fatalf("Operate() while TestBlocked = %v, want accepted", err)
	}
	if pos, _ := sim.Position(); pos != PositionClosed {
		t.Errorf("Position() after Operate() while TestBlocked = %v, want still Closed (no physical output)", pos)
	}
}

// isInterlockError reports whether err is an *InterlockError.
func isInterlockError(err error) bool {
	_, ok := errors.AsType[*InterlockError](err)
	return ok
}

// TestSimulator_Concurrency injects measurements and issues control
// commands from many goroutines concurrently; it exists to be run under
// `go test -race`.
func TestSimulator_Concurrency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sim := New(ctx, Config{
		InitialPosition: PositionClosed,
		SettingsGroups: []SettingsGroupConfig{
			{Group: SettingsGroup1, Settings: ProtectionSettings{InstantaneousPickup: 5000}},
		},
		MechanicalOperateTime: time.Millisecond,
		SelectionTimeout:      5 * time.Millisecond,
	})
	defer sim.Close()

	events, err := sim.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	go func() {
		for range events {
			// drain, discarding — this test only cares that nothing races or panics
		}
	}()

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range 50 {
				_ = sim.InjectMeasurement(Measurement{Current: PhaseValues{A: float64(i * j)}})
			}
		}(i)
	}
	for range 4 {
		wg.Go(func() {
			for range 50 {
				cmd := Command{Target: PositionOpen, Source: "concurrency-test"}
				id, err := sim.Select(ctx, cmd)
				if err != nil {
					continue // interlocked by a concurrent Select — expected
				}
				_ = sim.Operate(ctx, id, cmd)
				_, _ = sim.Position()
				_ = sim.ProtectionStatus()
			}
		})
	}
	wg.Wait()
}
