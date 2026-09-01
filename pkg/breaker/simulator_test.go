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

// TestSimulator_Block checks that Block is reflected as qds.QdsBlocked on
// the quality Position() returns, that it rejects both a new Select and an
// Operate on an already-pending selection, and that Unblock clears the
// quality flag and lets that same (still-pending) selection succeed.
func TestSimulator_Block(t *testing.T) {
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

	if err := sim.Block(ctx, "maintenance"); err != nil {
		t.Fatalf("Block: %v", err)
	}
	blockedEv := waitForEvent(t, ctx, events, EventControlBlocked)
	if reason := blockedEv.Detail.(ControlBlockedDetail).Reason; reason != "maintenance" {
		t.Errorf("ControlBlockedDetail.Reason = %q, want %q", reason, "maintenance")
	}

	if _, q := sim.Position(); !q.Has(qds.QdsBlocked) {
		t.Errorf("Position() quality = %v, want QdsBlocked set", q)
	}

	if _, err := sim.Select(ctx, cmd); !isInterlockError(err) {
		t.Errorf("Select() while blocked err = %v, want *InterlockError", err)
	}
	if err := sim.Operate(ctx, id, cmd); !isInterlockError(err) {
		t.Errorf("Operate() while blocked err = %v, want *InterlockError", err)
	}

	if err := sim.Unblock(ctx); err != nil {
		t.Fatalf("Unblock: %v", err)
	}
	waitForEvent(t, ctx, events, EventControlUnblocked)

	if _, q := sim.Position(); q.Has(qds.QdsBlocked) {
		t.Errorf("Position() quality = %v, want QdsBlocked cleared", q)
	}

	// The selection made before Block survived it and still works.
	if err := sim.Operate(ctx, id, cmd); err != nil {
		t.Errorf("Operate() after unblock: %v", err)
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
