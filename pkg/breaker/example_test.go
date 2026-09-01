package breaker

import (
	"context"
	"fmt"
	"time"
)

// Example constructs a simulator, injects a sustained fault current via
// the EmulatorFeed, and observes the protection relay trip the breaker, an
// autoreclose sequence attempt to recover it, and — because the fault
// never clears — the sequence eventually reach Lockout.
//
// The fault is re-injected only in reaction to observing the breaker
// settle back Closed (simulating a plant feed that keeps sampling a still-
// faulted line): every step is driven by blocking on the next Event from
// Subscribe, so the sequence below does not depend on precise wall-clock
// scheduling. ReclaimTime is still configured generously longer than the
// instantaneous-stage delay plus the mechanical operate time, so that a
// persisting fault is guaranteed to re-trip before the reclaim timer would
// otherwise reset the attempt count — the same margin a real deployment
// would want between those settings.
func Example() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sim := New(ctx, Config{
		InitialPosition: PositionClosed,
		SettingsGroups: []SettingsGroupConfig{
			{Group: SettingsGroup1, Settings: ProtectionSettings{InstantaneousPickup: 1000}},
		},
		AutoReclose: AutoRecloseConfig{
			Enabled:     true,
			MaxAttempts: 2,
			DeadTimes:   []time.Duration{2 * time.Millisecond, 2 * time.Millisecond},
			ReclaimTime: 50 * time.Millisecond,
		},
		MechanicalOperateTime: 5 * time.Millisecond,
		NominalFrequencyHz:    1000, // shrinks the instantaneous stage's delay for a fast example
	})
	defer sim.Close()

	events, err := sim.Subscribe(ctx)
	if err != nil {
		fmt.Println("subscribe error:", err)
		return
	}

	fault := Measurement{Current: PhaseValues{A: 2000, B: 2000, C: 2000}}
	if err := sim.InjectMeasurement(fault); err != nil {
		fmt.Println("inject error:", err)
		return
	}

	for ev := range events {
		switch ev.Kind {
		case EventProtectionPickedUp:
			fmt.Println("picked up:", ev.Detail.(PickedUpDetail).Cause)
		case EventTrip:
			fmt.Println("trip:", ev.Detail.(TripDetail).Cause)
		case EventPositionChanged:
			d := ev.Detail.(PositionChangedDetail)
			fmt.Println("position:", d.Position)
			if d.Position == PositionClosed {
				// The fault is still present on the line; the plant feed's
				// next sample will see it and the relay will pick up again.
				_ = sim.InjectMeasurement(fault)
			}
		case EventAutoRecloseAttempt:
			fmt.Println("autoreclose attempt:", ev.Detail.(AutoRecloseAttemptDetail).Attempt)
		case EventLockout:
			fmt.Println("lockout")
			return
		}
	}

	// Output:
	// picked up: Instantaneous
	// trip: Instantaneous
	// position: Intermediate
	// position: Open
	// autoreclose attempt: 1
	// position: Intermediate
	// position: Closed
	// picked up: Instantaneous
	// trip: Instantaneous
	// position: Intermediate
	// position: Open
	// autoreclose attempt: 2
	// position: Intermediate
	// position: Closed
	// picked up: Instantaneous
	// trip: Instantaneous
	// position: Intermediate
	// position: Open
	// lockout
}
