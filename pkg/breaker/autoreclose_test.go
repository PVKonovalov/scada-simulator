package breaker

import (
	"testing"
	"time"
)

// twoShotConfig is a small enabled autoreclose configuration shared by the
// tests below: 2 attempts, distinct dead times so attempt order is visible,
// and a nominal reclaim time.
func twoShotConfig() AutoRecloseConfig {
	return AutoRecloseConfig{
		Enabled:     true,
		MaxAttempts: 2,
		DeadTimes:   []time.Duration{5 * time.Second, 10 * time.Second},
		ReclaimTime: 30 * time.Second,
	}
}

// TestAutoReclose_SingleSuccessfulReclose drives Ready -> DeadTime ->
// Closing -> Reclaim -> Ready for a fault that clears on the first attempt.
func TestAutoReclose_SingleSuccessfulReclose(t *testing.T) {
	ar := newAutoReclose(twoShotConfig())

	deadTime, willReclose := ar.OnTrip()
	if !willReclose || deadTime != 5*time.Second {
		t.Fatalf("OnTrip() = (%v, %v), want (5s, true)", deadTime, willReclose)
	}
	if ar.state != AutoRecloseDeadTime || ar.attempt != 1 {
		t.Fatalf("state = %v, attempt = %d, want DeadTime, 1", ar.state, ar.attempt)
	}

	if !ar.OnDeadTimeElapsed() {
		t.Fatalf("OnDeadTimeElapsed() = false, want true")
	}
	if ar.state != AutoRecloseClosing {
		t.Fatalf("state = %v, want Closing", ar.state)
	}

	reclaim := ar.OnCloseSucceeded()
	if reclaim != 30*time.Second || ar.state != AutoRecloseReclaim {
		t.Fatalf("OnCloseSucceeded() = %v, state = %v, want 30s, Reclaim", reclaim, ar.state)
	}

	if !ar.OnReclaimElapsed() {
		t.Fatalf("OnReclaimElapsed() = false, want true")
	}
	if ar.state != AutoRecloseReady || ar.attempt != 0 {
		t.Fatalf("state = %v, attempt = %d, want Ready, 0", ar.state, ar.attempt)
	}
}

// TestAutoReclose_MultipleAttemptsToLockout drives a persisting fault
// through both configured attempts and confirms the sequence locks out
// rather than trying a third time.
func TestAutoReclose_MultipleAttemptsToLockout(t *testing.T) {
	ar := newAutoReclose(twoShotConfig())

	// Attempt 1: trip, dead time, close, then the fault reappears before
	// reclaim finishes.
	deadTime, willReclose := ar.OnTrip()
	if !willReclose || deadTime != 5*time.Second || ar.attempt != 1 {
		t.Fatalf("attempt 1 OnTrip() = (%v, %v), attempt=%d", deadTime, willReclose, ar.attempt)
	}
	ar.OnDeadTimeElapsed()
	ar.OnCloseSucceeded() // now in Reclaim

	// Fault persists: a new trip arrives while still reclaiming.
	deadTime, willReclose = ar.OnTrip()
	if !willReclose || deadTime != 10*time.Second || ar.attempt != 2 {
		t.Fatalf("attempt 2 OnTrip() = (%v, %v), attempt=%d, want (10s, true, 2)", deadTime, willReclose, ar.attempt)
	}
	ar.OnDeadTimeElapsed()
	ar.OnCloseSucceeded() // now in Reclaim again

	// Fault persists a third time: attempts are exhausted.
	deadTime, willReclose = ar.OnTrip()
	if willReclose || deadTime != 0 {
		t.Fatalf("attempt 3 OnTrip() = (%v, %v), want (0, false)", deadTime, willReclose)
	}
	if ar.state != AutoRecloseLockout {
		t.Fatalf("state = %v, want Lockout", ar.state)
	}

	// Lockout refuses any further reclose.
	if _, willReclose := ar.OnTrip(); willReclose {
		t.Errorf("OnTrip() during Lockout willReclose = true, want false")
	}
}

// TestAutoReclose_ReclaimTimeReset checks that a successful close which
// survives past ReclaimTime returns to Ready with the attempt counter
// cleared, and a subsequent trip is treated as a fresh attempt 1 rather
// than continuing the old count.
func TestAutoReclose_ReclaimTimeReset(t *testing.T) {
	ar := newAutoReclose(twoShotConfig())

	ar.OnTrip()
	ar.OnDeadTimeElapsed()
	ar.OnCloseSucceeded()
	if !ar.OnReclaimElapsed() {
		t.Fatalf("OnReclaimElapsed() = false, want true")
	}
	if ar.state != AutoRecloseReady || ar.attempt != 0 {
		t.Fatalf("state = %v, attempt = %d, want Ready, 0", ar.state, ar.attempt)
	}

	// A stale OnReclaimElapsed (e.g. a timer that fires again) must be a no-op.
	if ar.OnReclaimElapsed() {
		t.Errorf("second OnReclaimElapsed() = true, want false (no-op)")
	}

	// A brand new fault starts again at attempt 1, not attempt 3.
	deadTime, willReclose := ar.OnTrip()
	if !willReclose || deadTime != 5*time.Second || ar.attempt != 1 {
		t.Fatalf("fresh OnTrip() = (%v, %v), attempt=%d, want (5s, true, 1)", deadTime, willReclose, ar.attempt)
	}
}

// TestAutoReclose_ExplicitLockoutReset checks that Lockout only clears via
// ResetLockout, and that ResetLockout is rejected outside Lockout.
func TestAutoReclose_ExplicitLockoutReset(t *testing.T) {
	ar := newAutoReclose(AutoRecloseConfig{
		Enabled:     true,
		MaxAttempts: 1,
		DeadTimes:   []time.Duration{1 * time.Second},
		ReclaimTime: 1 * time.Second,
	})

	if err := ar.ResetLockout(); err == nil {
		t.Errorf("ResetLockout() outside Lockout: err = nil, want error")
	}

	ar.OnTrip()            // attempt 1
	ar.OnDeadTimeElapsed() // Closing
	ar.OnCloseSucceeded()  // Reclaim
	if _, willReclose := ar.OnTrip(); willReclose {
		t.Fatalf("attempt 2 should exceed MaxAttempts=1")
	}
	if ar.state != AutoRecloseLockout {
		t.Fatalf("state = %v, want Lockout", ar.state)
	}

	if err := ar.ResetLockout(); err != nil {
		t.Fatalf("ResetLockout() = %v, want nil", err)
	}
	if ar.state != AutoRecloseReady || ar.attempt != 0 {
		t.Fatalf("state = %v, attempt = %d, want Ready, 0", ar.state, ar.attempt)
	}
}

// TestAutoReclose_Disabled checks that a disabled configuration never
// attempts a reclose.
func TestAutoReclose_Disabled(t *testing.T) {
	ar := newAutoReclose(AutoRecloseConfig{Enabled: false})
	if _, willReclose := ar.OnTrip(); willReclose {
		t.Errorf("OnTrip() with Enabled=false: willReclose = true, want false")
	}
	if ar.state != AutoRecloseReady {
		t.Errorf("state = %v, want Ready (unchanged)", ar.state)
	}
}
