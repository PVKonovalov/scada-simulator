package breaker

import (
	"context"
	"time"

	"scada-simulator/pkg/qds"
)

// EventKind identifies the kind of event published on an EventSubscriber
// channel. The concrete type of Event.Detail depends on Kind — see the
// per-constant documentation.
type EventKind uint8

const (
	// EventPositionChanged fires whenever Position() changes, and also as a
	// confirmation when a successful Operate finds the breaker already at
	// the requested position (a no-op Position() change) — so a SCADA
	// client that just issued a command always sees a positive response on
	// the position tag, not silence, even when nothing physically moved.
	// Detail is PositionChangedDetail.
	EventPositionChanged EventKind = iota
	// EventMeasurementChanged fires whenever a new measurement is injected
	// via EmulatorFeed.InjectMeasurement. Detail is Measurement.
	EventMeasurementChanged
	// EventProtectionPickedUp fires when a protection stage picks up
	// (current exceeds its pickup but has not yet operated). Detail is
	// PickedUpDetail.
	EventProtectionPickedUp
	// EventTrip fires when the protection relay trips the breaker open.
	// Detail is TripDetail.
	EventTrip
	// EventAutoRecloseAttempt fires when the autoreclose sequence starts a
	// reclose attempt. Detail is AutoRecloseAttemptDetail.
	EventAutoRecloseAttempt
	// EventLockout fires when the autoreclose sequence enters Lockout.
	// Detail is LockoutDetail.
	EventLockout
	// EventAutoRecloseSucceeded fires when the autoreclose sequence's
	// reclaim timer elapses without a further trip, returning it to Ready
	// with the breaker back in service. Detail is AutoRecloseSucceededDetail.
	EventAutoRecloseSucceeded
	// EventSettingsChanged fires when protection settings are edited, or
	// the active settings group changes. Detail is SettingsChangedDetail.
	EventSettingsChanged
	// EventControlRejected fires when a control operation is rejected
	// (interlock, expired/unknown selection, invalid command). Detail is
	// ControlRejectedDetail.
	EventControlRejected
	// EventModeChanged fires when SetMode changes the breaker's Mode. Detail
	// is ModeChangedDetail.
	EventModeChanged
)

// String returns a short human-readable name for the event kind.
func (k EventKind) String() string {
	switch k {
	case EventPositionChanged:
		return "PositionChanged"
	case EventMeasurementChanged:
		return "MeasurementChanged"
	case EventProtectionPickedUp:
		return "ProtectionPickedUp"
	case EventTrip:
		return "Trip"
	case EventAutoRecloseAttempt:
		return "AutoRecloseAttempt"
	case EventLockout:
		return "Lockout"
	case EventAutoRecloseSucceeded:
		return "AutoRecloseSucceeded"
	case EventSettingsChanged:
		return "SettingsChanged"
	case EventControlRejected:
		return "ControlRejected"
	case EventModeChanged:
		return "ModeChanged"
	default:
		return "Unknown"
	}
}

// Event is a single notification published to EventSubscriber subscribers.
type Event struct {
	// Kind identifies what happened; it determines Detail's concrete type.
	Kind EventKind
	// Timestamp is when the simulator emitted the event.
	Timestamp time.Time
	// Detail carries kind-specific data; see EventKind's documentation.
	Detail any
}

// PositionChangedDetail is the Event.Detail for EventPositionChanged.
type PositionChangedDetail struct {
	// Position is the breaker's new position.
	Position Position
	// Quality is the point quality attached to Position.
	Quality qds.Quality
}

// PickedUpDetail is the Event.Detail for EventProtectionPickedUp.
type PickedUpDetail struct {
	// Cause identifies which protection stage picked up.
	Cause ProtectionCause
	// Current is the phase current that caused the pickup, in amps.
	Current float64
}

// TripDetail is the Event.Detail for EventTrip.
type TripDetail struct {
	// Cause identifies which protection stage issued the trip.
	Cause ProtectionCause
	// Current is the phase current that caused the trip, in amps.
	Current float64
	// OperateTime is the stage's computed operate time for Current.
	OperateTime time.Duration
}

// AutoRecloseAttemptDetail is the Event.Detail for EventAutoRecloseAttempt.
type AutoRecloseAttemptDetail struct {
	// Attempt is the reclose attempt number, starting at 1.
	Attempt int
	// DeadTime is how long the sequence waits before closing for this attempt.
	DeadTime time.Duration
}

// LockoutDetail is the Event.Detail for EventLockout.
type LockoutDetail struct {
	// Attempts is the number of reclose attempts made before lockout.
	Attempts int
}

// AutoRecloseSucceededDetail is the Event.Detail for EventAutoRecloseSucceeded.
type AutoRecloseSucceededDetail struct {
	// Attempts is the number of reclose attempts made before the sequence
	// succeeded.
	Attempts int
}

// SettingsChangedDetail is the Event.Detail for EventSettingsChanged.
type SettingsChangedDetail struct {
	// Group is the settings group affected.
	Group SettingsGroup
	// SettingsUpdated is true when this event reports an edit to Group's settings.
	SettingsUpdated bool
	// GroupSelected is true when this event reports ActiveSettingsGroup switching to Group.
	GroupSelected bool
}

// ControlRejectedDetail is the Event.Detail for EventControlRejected.
type ControlRejectedDetail struct {
	// Command is the control command that was rejected.
	Command Command
	// Reason is a human-readable explanation of the rejection.
	Reason string
}

// ModeChangedDetail is the Event.Detail for EventModeChanged.
type ModeChangedDetail struct {
	// Mode is the breaker's new mode.
	Mode Mode
	// Reason is why the mode was changed, as passed to SetMode.
	Reason string
}

// EventSubscriber lets a consumer receive push-based notifications
// alongside the polling getters on the other SCADA interfaces.
type EventSubscriber interface {
	// Subscribe returns a channel that receives Events of the given kinds
	// (every kind, if none are given) until ctx is cancelled or the
	// simulator is closed, at which point the channel is closed. The
	// channel is buffered; a subscriber that falls behind has its oldest
	// queued event dropped in favor of the newest one — a slow consumer
	// must never block the simulation.
	Subscribe(ctx context.Context, kinds ...EventKind) (<-chan Event, error)
}
