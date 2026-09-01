package breaker

import (
	"context"

	"scada-simulator/pkg/qds"
)

// PositionReader satisfies SCADA requirement 1: reading breaker position.
type PositionReader interface {
	// Position returns the breaker's current double-point position and its quality.
	Position() (Position, qds.Quality)
}

// AnalogReader satisfies SCADA requirement 2: reading three-phase analog measurements.
type AnalogReader interface {
	// Measurement returns the most recently injected three-phase measurement.
	Measurement() Measurement
}

// ProtectionConfigurator satisfies SCADA requirement 3: reading and setting
// the protection relay's characteristics, including which settings group
// is active.
type ProtectionConfigurator interface {
	// SetProtectionSettings stores settings into group.
	SetProtectionSettings(group SettingsGroup, settings ProtectionSettings) error
	// ProtectionSettings returns the settings currently stored in group.
	ProtectionSettings(group SettingsGroup) (ProtectionSettings, error)
	// ActiveSettingsGroup returns the settings group currently in effect.
	ActiveSettingsGroup() SettingsGroup
	// SelectSettingsGroup makes group the active settings group.
	SelectSettingsGroup(group SettingsGroup) error
}

// AutoRecloseConfigurator satisfies SCADA requirement 4: configuring
// automatic reclosing.
type AutoRecloseConfigurator interface {
	// SetAutoRecloseConfig replaces the autoreclose configuration and
	// resets the sequence to AutoRecloseReady.
	SetAutoRecloseConfig(AutoRecloseConfig) error
	// AutoRecloseConfig returns the current autoreclose configuration.
	AutoRecloseConfig() AutoRecloseConfig
}

// BreakerController satisfies SCADA requirement 5: remote control of the
// breaker using select-before-operate. A caller Selects a Command,
// receiving a SelectionID, then Operates with that ID before it expires;
// Cancel releases a selection early.
type BreakerController interface {
	// Select reserves cmd for a later Operate, returning an id that must be
	// used to Operate or Cancel before it expires.
	Select(ctx context.Context, cmd Command) (SelectionID, error)
	// Operate executes the command previously Selected as id. cmd.Target
	// must match the Selected command's Target.
	Operate(ctx context.Context, id SelectionID, cmd Command) error
	// Cancel releases a selection early without operating it.
	Cancel(ctx context.Context, id SelectionID) error
	// ResetLockout clears an autoreclose Lockout, requiring an explicit
	// operator/SCADA action per ANSI 79 practice. It is an error to call
	// this when the sequence is not in Lockout.
	ResetLockout(ctx context.Context) error
	// Block prevents Select and Operate from executing any control command
	// until Unblock is called — e.g. an operator's maintenance block/tag.
	// It is reflected as qds.QdsBlocked on the quality Position returns, so
	// SCADA sees it on the same measurement it already polls. reason is
	// recorded and surfaced on any control command rejected while blocked.
	Block(ctx context.Context, reason string) error
	// Unblock clears a block set by Block.
	Unblock(ctx context.Context) error
}

// ProtectionStatusReader satisfies SCADA requirement 6: observing
// protection relay and autoreclose status. Protection activation itself is
// not something a SCADA master calls — the relay acts autonomously when
// measurements exceed its configured settings; see EventSubscriber for the
// corresponding PickedUp/Trip/Lockout events.
type ProtectionStatusReader interface {
	// ProtectionStatus returns the protection relay's current status.
	ProtectionStatus() ProtectionStatus
	// AutoRecloseStatus returns the autoreclose sequence's current status.
	AutoRecloseStatus() AutoRecloseStatus
}

// EmulatorFeed satisfies SCADA requirement 7: feeding plant values into the
// simulation. This is the simulation's input side, not something a SCADA
// master calls — a real gateway would never see it.
type EmulatorFeed interface {
	// InjectMeasurement feeds a new three-phase measurement into the
	// simulator, driving protection evaluation.
	InjectMeasurement(Measurement) error
}
