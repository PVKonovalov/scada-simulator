package breaker

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Default characteristics applied by New when the corresponding Config
// field is left zero.
const (
	// DefaultMechanicalOperateTime is roughly 3 power-system cycles at 50Hz.
	DefaultMechanicalOperateTime = 60 * time.Millisecond
	// DefaultSelectionTimeout bounds how long a select-before-operate
	// selection may go un-Operated/un-Cancelled before it expires.
	DefaultSelectionTimeout = 10 * time.Second
	// DefaultNominalFrequencyHz is the power-system frequency assumed when
	// Config.NominalFrequencyHz is not set.
	DefaultNominalFrequencyHz = 50
	// DefaultName is used to prefix log messages when Config.Name is empty.
	DefaultName = "breaker"
)

// Logger is the minimal logging interface the Simulator needs for internal
// structured logging of trips, reclose attempts and settings changes. It is
// described here rather than depending on any concrete logging package, so
// pkg/breaker stays standalone; attach a real logger with Config.WithConfig.
// scada-simulator/pkg/llog.LevelLog satisfies this interface as-is, since
// its Warnf/Errorf/Infof methods already match this shape.
type Logger interface {
	// Warnf logs a formatted warning — used for trips and rejected control commands.
	Warnf(format string, args ...any)
	// Errorf logs a formatted error — used when the autoreclose sequence enters Lockout.
	Errorf(format string, args ...any)
	// Infof logs formatted information — used for autoreclose attempts and settings changes.
	Infof(format string, args ...any)
}

// noopLogger is the Logger used when Config.Logger is left unset: it
// discards everything.
type noopLogger struct{}

// Warnf discards its arguments.
func (noopLogger) Warnf(string, ...any) {}

// Errorf discards its arguments.
func (noopLogger) Errorf(string, ...any) {}

// Infof discards its arguments.
func (noopLogger) Infof(string, ...any) {}

// Tracef discards its arguments.
func (noopLogger) Tracef(string, ...any) {}

// yamlDuration decodes a YAML scalar as a Go duration string (e.g. "50ms",
// "2s") via time.ParseDuration, for embedding in the shadow structs used by
// this package's custom UnmarshalYAML methods.
type yamlDuration time.Duration

// UnmarshalYAML parses a duration string such as "500ms" or "2s".
func (d *yamlDuration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("breaker: invalid duration %q: %w", s, err)
	}
	*d = yamlDuration(parsed)
	return nil
}

// SettingsGroupConfig pre-populates one protection settings group at
// construction time; see Config.SettingsGroups. Groups not listed keep
// their zero value, which leaves both protection stages disabled
// (PickupCurrent and InstantaneousPickup default to 0).
type SettingsGroupConfig struct {
	// Group identifies which of the relay's NumSettingsGroups this configures.
	Group SettingsGroup `yaml:"group"`
	// Settings are the protection settings stored in Group.
	Settings ProtectionSettings `yaml:"settings"`
}

// Config configures a Simulator's initial state and fixed operating
// characteristics — the "specific characteristics of the breaker" a
// deployment wants to model. It is designed to be loaded directly from
// YAML (see config/scada-simulator.yaml's breaker: section, loaded via
// pkg/configuration into internal/configuration.Configuration.Breaker) as
// well as built directly in Go.
type Config struct {
	// Name identifies this breaker (e.g. a bay or feeder name). It has no
	// effect on simulation behaviour — it is only used to prefix this
	// Simulator's internal log messages, so logs from multiple breakers are
	// distinguishable. Defaults to DefaultName if empty.
	Name string
	// InitialPosition is the breaker's position when the simulator starts.
	InitialPosition Position
	// ActiveSettingsGroup is the protection settings group active on start.
	ActiveSettingsGroup SettingsGroup
	// SettingsGroups pre-populates one or more protection settings groups.
	SettingsGroups []SettingsGroupConfig
	// AutoReclose configures the ANSI-79 automatic reclose sequence.
	AutoReclose AutoRecloseConfig
	// MechanicalOperateTime is how long an Open or Close operation takes to
	// settle, during which Position() reports PositionIntermediate.
	// Defaults to DefaultMechanicalOperateTime if zero.
	MechanicalOperateTime time.Duration
	// SelectionTimeout bounds how long a Select()ed command may go
	// un-Operated/un-Cancelled before it expires automatically. Defaults to
	// DefaultSelectionTimeout if zero.
	SelectionTimeout time.Duration
	// NominalFrequencyHz is the power-system frequency used to size the
	// instantaneous stage's operate delay. Defaults to
	// DefaultNominalFrequencyHz if zero.
	NominalFrequencyHz float64
	// Logger receives internal structured logging of trips, reclose
	// attempts and settings changes. Defaults to a no-op logger if nil; set
	// it with WithConfig.
	Logger Logger
}

// WithConfig returns a copy of c with its Logger set to logger. Use it to
// attach a real logging package's logger — e.g. the application's
// scada-simulator/pkg/llog.Logger — without pkg/breaker importing that
// package directly:
//
//	cfg := breaker.Config{...}.WithConfig(llog.Logger)
func (c Config) WithConfig(logger Logger) Config {
	c.Logger = logger
	return c
}

// UnmarshalYAML decodes Config from YAML, accepting human-friendly
// duration strings (e.g. "60ms") for MechanicalOperateTime and
// SelectionTimeout via time.ParseDuration. Logger is not settable from
// YAML.
func (c *Config) UnmarshalYAML(value *yaml.Node) error {
	var shadow struct {
		Name                  string                `yaml:"name"`
		InitialPosition       Position              `yaml:"initial_position"`
		ActiveSettingsGroup   SettingsGroup         `yaml:"active_settings_group"`
		SettingsGroups        []SettingsGroupConfig `yaml:"settings_groups"`
		AutoReclose           AutoRecloseConfig     `yaml:"auto_reclose"`
		MechanicalOperateTime yamlDuration          `yaml:"mechanical_operate_time"`
		SelectionTimeout      yamlDuration          `yaml:"selection_timeout"`
		NominalFrequencyHz    float64               `yaml:"nominal_frequency_hz"`
	}
	if err := value.Decode(&shadow); err != nil {
		return err
	}
	*c = Config{
		Name:                  shadow.Name,
		InitialPosition:       shadow.InitialPosition,
		ActiveSettingsGroup:   shadow.ActiveSettingsGroup,
		SettingsGroups:        shadow.SettingsGroups,
		AutoReclose:           shadow.AutoReclose,
		MechanicalOperateTime: time.Duration(shadow.MechanicalOperateTime),
		SelectionTimeout:      time.Duration(shadow.SelectionTimeout),
		NominalFrequencyHz:    shadow.NominalFrequencyHz,
	}
	return nil
}

// withDefaults returns a copy of c with every zero-valued tunable
// characteristic replaced by its default.
func (c Config) withDefaults() Config {
	if c.Name == "" {
		c.Name = DefaultName
	}
	if c.MechanicalOperateTime <= 0 {
		c.MechanicalOperateTime = DefaultMechanicalOperateTime
	}
	if c.SelectionTimeout <= 0 {
		c.SelectionTimeout = DefaultSelectionTimeout
	}
	if c.NominalFrequencyHz <= 0 {
		c.NominalFrequencyHz = DefaultNominalFrequencyHz
	}
	if c.Logger == nil {
		c.Logger = noopLogger{}
	}
	return c
}
