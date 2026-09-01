package breaker

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// AutoRecloseConfig configures the automatic reclosing (ANSI device 79)
// sequence that follows a protection trip.
type AutoRecloseConfig struct {
	// Enabled turns the automatic reclose sequence on or off.
	Enabled bool
	// MaxAttempts is the number of reclose shots.
	MaxAttempts int
	// DeadTimes[i] is the dead time waited before reclose attempt i+1.
	// Must have len(DeadTimes) >= MaxAttempts when Enabled.
	DeadTimes []time.Duration
	// ReclaimTime is how long a successful close must hold before the
	// sequence resets to Ready with the attempt counter cleared.
	ReclaimTime time.Duration
}

// UnmarshalYAML decodes AutoRecloseConfig from YAML, accepting
// human-friendly duration strings (e.g. "5s") for DeadTimes and
// ReclaimTime via time.ParseDuration.
func (c *AutoRecloseConfig) UnmarshalYAML(value *yaml.Node) error {
	var shadow struct {
		Enabled     bool           `yaml:"enabled"`
		MaxAttempts int            `yaml:"max_attempts"`
		DeadTimes   []yamlDuration `yaml:"dead_times"`
		ReclaimTime yamlDuration   `yaml:"reclaim_time"`
	}
	if err := value.Decode(&shadow); err != nil {
		return err
	}
	deadTimes := make([]time.Duration, len(shadow.DeadTimes))
	for i, d := range shadow.DeadTimes {
		deadTimes[i] = time.Duration(d)
	}
	*c = AutoRecloseConfig{
		Enabled:     shadow.Enabled,
		MaxAttempts: shadow.MaxAttempts,
		DeadTimes:   deadTimes,
		ReclaimTime: time.Duration(shadow.ReclaimTime),
	}
	return nil
}

// AutoRecloseState is the state of the autoreclose sequence.
type AutoRecloseState uint8

const (
	// AutoRecloseReady: no trip in progress; the next trip starts attempt 1.
	AutoRecloseReady AutoRecloseState = iota
	// AutoRecloseDeadTime: breaker open, waiting out the dead time before the next reclose attempt.
	AutoRecloseDeadTime
	// AutoRecloseClosing: the reclose Close operation for the current attempt has been issued.
	AutoRecloseClosing
	// AutoRecloseReclaim: breaker closed; waiting out ReclaimTime to confirm the fault cleared.
	AutoRecloseReclaim
	// AutoRecloseLockout: attempts exhausted; requires an explicit operator/SCADA reset.
	AutoRecloseLockout
)

// String returns a short human-readable name for the state.
func (s AutoRecloseState) String() string {
	switch s {
	case AutoRecloseReady:
		return "Ready"
	case AutoRecloseDeadTime:
		return "DeadTime"
	case AutoRecloseClosing:
		return "Closing"
	case AutoRecloseReclaim:
		return "Reclaim"
	case AutoRecloseLockout:
		return "Lockout"
	default:
		return "Unknown"
	}
}

// AutoRecloseStatus reports the sequence's current state.
type AutoRecloseStatus struct {
	// State is the sequence's current state.
	State AutoRecloseState
	// Attempt is the current/last reclose attempt number, 0 before any attempt.
	Attempt int
}

// autoReclose is the autoreclose sequencing state machine described in the
// package's design: Ready -> (trip) -> DeadTime -> Closing -> (fault
// cleared) -> Reclaim -> Ready, or -> (fault persists) -> next DeadTime,
// or, after MaxAttempts, -> Lockout. It is pure and event-driven — callers
// (normally the Simulator's run loop) tell it what happened and it reports
// what to do next; it owns no timers or goroutines of its own, which keeps
// it trivial to unit test.
type autoReclose struct {
	config  AutoRecloseConfig // configuration this sequence was built with
	state   AutoRecloseState  // current sequence state
	attempt int               // current/last reclose attempt number, 0 before any attempt
}

// newAutoReclose builds a new autoreclose sequence in state AutoRecloseReady.
func newAutoReclose(cfg AutoRecloseConfig) *autoReclose {
	return &autoReclose{config: cfg, state: AutoRecloseReady}
}

// Status reports the sequence's current state.
func (ar *autoReclose) Status() AutoRecloseStatus {
	return AutoRecloseStatus{State: ar.state, Attempt: ar.attempt}
}

// OnTrip is called when the protection relay trips the breaker open. It
// reports whether the sequence will attempt a reclose and, if so, the dead
// time to wait before doing so. A trip that arrives while a previous
// attempt is still being reclaimed (AutoRecloseReclaim) or is itself
// mid-sequence (AutoRecloseDeadTime/AutoRecloseClosing) is treated as the
// fault persisting: it advances to the next attempt rather than restarting
// the count. A trip while already in Lockout never reclose.
func (ar *autoReclose) OnTrip() (deadTime time.Duration, willReclose bool) {
	if !ar.config.Enabled {
		return 0, false
	}

	switch ar.state {
	case AutoRecloseLockout:
		return 0, false
	case AutoRecloseReady:
		ar.attempt = 1
	default: // DeadTime, Closing, Reclaim: fault persists
		ar.attempt++
	}

	if ar.attempt > ar.config.MaxAttempts || ar.attempt > len(ar.config.DeadTimes) {
		ar.state = AutoRecloseLockout
		return 0, false
	}

	ar.state = AutoRecloseDeadTime
	return ar.config.DeadTimes[ar.attempt-1], true
}

// OnDeadTimeElapsed transitions DeadTime -> Closing once the dead timer for
// the current attempt has elapsed, reporting whether it did (it is a no-op,
// returning false, if a newer event already moved the sequence on — e.g. a
// stale timer firing after Lockout). The caller is responsible for actually
// issuing the Close operation when this returns true.
func (ar *autoReclose) OnDeadTimeElapsed() bool {
	if ar.state != AutoRecloseDeadTime {
		return false
	}
	ar.state = AutoRecloseClosing
	return true
}

// OnCloseSucceeded is called when the current attempt's Close operation
// completes and the breaker settles Closed. It starts the reclaim timer and
// reports its duration; it no-ops (returning 0) if called out of sequence.
func (ar *autoReclose) OnCloseSucceeded() time.Duration {
	if ar.state != AutoRecloseClosing {
		return 0
	}
	ar.state = AutoRecloseReclaim
	return ar.config.ReclaimTime
}

// OnReclaimElapsed is called when the reclaim timer expires without a
// further trip, returning the sequence to Ready with the attempt counter
// cleared. It reports whether it did so (a no-op, returning false, if a new
// trip already moved the sequence on before the reclaim timer fired).
func (ar *autoReclose) OnReclaimElapsed() bool {
	if ar.state != AutoRecloseReclaim {
		return false
	}
	ar.state = AutoRecloseReady
	ar.attempt = 0
	return true
}

// ResetLockout clears a Lockout state, as required after attempts are
// exhausted — autoreclose does not recover on its own. It is an error to
// call this when the sequence is not in Lockout.
func (ar *autoReclose) ResetLockout() error {
	if ar.state != AutoRecloseLockout {
		return fmt.Errorf("breaker: autoreclose is not in lockout")
	}
	ar.state = AutoRecloseReady
	ar.attempt = 0
	return nil
}
