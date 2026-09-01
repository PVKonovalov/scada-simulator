package breaker

import (
	"fmt"
	"math"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// CurveType selects the time-current characteristic used by the
// time-overcurrent (ANSI device 51) protection stage.
type CurveType uint8

const (
	// CurveDefiniteTime trips a fixed ProtectionSettings.DefiniteTime after
	// pickup, independent of how far above pickup the current is.
	CurveDefiniteTime CurveType = iota
	// CurveIECStandardInverse is the IEC 60255 Standard Inverse curve (k=0.14, alpha=0.02).
	CurveIECStandardInverse
	// CurveIECVeryInverse is the IEC 60255 Very Inverse curve (k=13.5, alpha=1).
	CurveIECVeryInverse
	// CurveIECExtremelyInverse is the IEC 60255 Extremely Inverse curve (k=80, alpha=2).
	CurveIECExtremelyInverse
)

// String returns a short human-readable name for the curve.
func (c CurveType) String() string {
	switch c {
	case CurveDefiniteTime:
		return "DefiniteTime"
	case CurveIECStandardInverse:
		return "IECStandardInverse"
	case CurveIECVeryInverse:
		return "IECVeryInverse"
	case CurveIECExtremelyInverse:
		return "IECExtremelyInverse"
	default:
		return "Unknown"
	}
}

// ParseCurveType parses a curve name as produced by CurveType.String, plus
// the snake_case spellings used in YAML config files (e.g.
// "iec_standard_inverse"), case-insensitively.
func ParseCurveType(s string) (CurveType, error) {
	switch strings.ToLower(strings.ReplaceAll(s, "_", "")) {
	case "definitetime":
		return CurveDefiniteTime, nil
	case "iecstandardinverse":
		return CurveIECStandardInverse, nil
	case "iecveryinverse":
		return CurveIECVeryInverse, nil
	case "iecextremelyinverse":
		return CurveIECExtremelyInverse, nil
	}
	return 0, fmt.Errorf("breaker: invalid curve type %q", s)
}

// MarshalYAML renders the curve using its String form.
func (c CurveType) MarshalYAML() (any, error) {
	return c.String(), nil
}

// UnmarshalYAML parses the curve from a YAML string via ParseCurveType.
func (c *CurveType) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := ParseCurveType(s)
	if err != nil {
		return err
	}
	*c = parsed
	return nil
}

// SettingsGroup selects one of a relay's stored protection settings
// profiles. Only one group is active at a time; see
// ProtectionConfigurator.
type SettingsGroup uint8

// The relay holds NumSettingsGroups independent settings groups, a common
// arrangement on real numerical relays (e.g. a "normal" and a "storm" or
// "maintenance" profile).
const NumSettingsGroups = 4

const (
	// SettingsGroup1 is the first (and default) protection settings group.
	SettingsGroup1 SettingsGroup = iota
	// SettingsGroup2 is the second protection settings group.
	SettingsGroup2
	// SettingsGroup3 is the third protection settings group.
	SettingsGroup3
	// SettingsGroup4 is the fourth protection settings group.
	SettingsGroup4
)

// MarshalYAML renders the group using the 1-based numbering ("1".."4")
// operators normally use, rather than the 0-based Go iota.
func (g SettingsGroup) MarshalYAML() (any, error) {
	return int(g) + 1, nil
}

// UnmarshalYAML parses a 1-based group number ("1".."4") from YAML.
func (g *SettingsGroup) UnmarshalYAML(value *yaml.Node) error {
	var n int
	if err := value.Decode(&n); err != nil {
		return err
	}
	if n < 1 || n > NumSettingsGroups {
		return fmt.Errorf("breaker: invalid settings group %d (must be 1-%d)", n, NumSettingsGroups)
	}
	*g = SettingsGroup(n - 1)
	return nil
}

// ProtectionSettings configures one protection settings group: the
// time-overcurrent (51) and instantaneous overcurrent (50) stages.
type ProtectionSettings struct {
	// PickupCurrent (Is) is the stage-51 pickup, in amps. Zero disables the
	// time-overcurrent stage.
	PickupCurrent float64
	// Curve selects the stage-51 time-current characteristic.
	Curve CurveType
	// TimeMultiplier (TMS) scales the inverse-time curves per IEC 60255.
	// Ignored when Curve is CurveDefiniteTime.
	TimeMultiplier float64
	// DefiniteTime is the fixed operate time used when Curve is CurveDefiniteTime.
	DefiniteTime time.Duration
	// InstantaneousPickup (stage 50), in amps. Zero disables the instantaneous stage.
	InstantaneousPickup float64
	// ResetTime is how long a picked-up-but-not-yet-tripped stage-51 timer
	// takes to fully reset once current drops back below PickupCurrent.
	ResetTime time.Duration
}

// UnmarshalYAML decodes ProtectionSettings from YAML, accepting
// human-friendly duration strings (e.g. "500ms") for DefiniteTime and
// ResetTime via time.ParseDuration.
func (s *ProtectionSettings) UnmarshalYAML(value *yaml.Node) error {
	var shadow struct {
		PickupCurrent       float64      `yaml:"pickup_current"`
		Curve               CurveType    `yaml:"curve"`
		TimeMultiplier      float64      `yaml:"time_multiplier"`
		DefiniteTime        yamlDuration `yaml:"definite_time"`
		InstantaneousPickup float64      `yaml:"instantaneous_pickup"`
		ResetTime           yamlDuration `yaml:"reset_time"`
	}
	if err := value.Decode(&shadow); err != nil {
		return err
	}
	*s = ProtectionSettings{
		PickupCurrent:       shadow.PickupCurrent,
		Curve:               shadow.Curve,
		TimeMultiplier:      shadow.TimeMultiplier,
		DefiniteTime:        time.Duration(shadow.DefiniteTime),
		InstantaneousPickup: shadow.InstantaneousPickup,
		ResetTime:           time.Duration(shadow.ResetTime),
	}
	return nil
}

// curveConstants holds the IEC 60255 k and alpha constants for one inverse-time curve.
type curveConstants struct {
	k     float64 // IEC 60255 k constant
	alpha float64 // IEC 60255 alpha (exponent) constant
}

// iecCurveConstants holds the IEC 60255 k/alpha pair for each inverse-time CurveType.
var iecCurveConstants = map[CurveType]curveConstants{
	CurveIECStandardInverse:  {k: 0.14, alpha: 0.02},
	CurveIECVeryInverse:      {k: 13.5, alpha: 1},
	CurveIECExtremelyInverse: {k: 80, alpha: 2},
}

// OperateTime computes the stage-51 (time-overcurrent) operate time for
// settings at the given measured current, per IEC 60255:
//
//	t(I) = TMS * (k / ((I/Is)^alpha - 1))
//
// It reports ok=false when current is at or below pickup — below pickup, no
// trip timer runs. This is a pure function, independent of the simulator's
// goroutine/timing machinery, so it can be unit tested directly.
func OperateTime(settings ProtectionSettings, current float64) (time.Duration, bool) {
	if settings.PickupCurrent <= 0 || current <= settings.PickupCurrent {
		return 0, false
	}

	if settings.Curve == CurveDefiniteTime {
		return settings.DefiniteTime, true
	}

	cc, ok := iecCurveConstants[settings.Curve]
	if !ok {
		return 0, false
	}

	ratio := current / settings.PickupCurrent
	denom := math.Pow(ratio, cc.alpha) - 1
	if denom <= 0 {
		return 0, false
	}

	seconds := settings.TimeMultiplier * (cc.k / denom)
	return time.Duration(seconds * float64(time.Second)), true
}

// instantaneousCycles is the fixed operate delay for the stage-50
// instantaneous stage, expressed in power-system cycles rather than as a
// truly-zero delay — real instantaneous elements still take a cycle or two
// to detect and commit to a trip.
const instantaneousCycles = 1.5

// InstantaneousOperateTime is the short, effectively-fixed delay for the
// stage-50 instantaneous stage, modelled as instantaneousCycles power-system
// cycles at nominalFrequencyHz (defaulting to 50 Hz if not positive). It
// reports ok=false when InstantaneousPickup is disabled (0) or current does
// not exceed it. Stage 50 trips independently of the inverse-time stage.
func InstantaneousOperateTime(settings ProtectionSettings, current float64, nominalFrequencyHz float64) (time.Duration, bool) {
	if settings.InstantaneousPickup <= 0 || current <= settings.InstantaneousPickup {
		return 0, false
	}
	if nominalFrequencyHz <= 0 {
		nominalFrequencyHz = 50
	}
	cycle := time.Duration(float64(time.Second) / nominalFrequencyHz)
	return time.Duration(float64(cycle) * instantaneousCycles), true
}

// ProtectionState is the protection relay's overall state.
type ProtectionState uint8

const (
	// ProtectionNormal: current is below every pickup.
	ProtectionNormal ProtectionState = iota
	// ProtectionPickedUp: current exceeds a stage's pickup but that
	// stage's operate timer has not yet elapsed.
	ProtectionPickedUp
	// ProtectionTripped: a stage has operated and issued a trip; the
	// breaker is open (or opening) because of it.
	ProtectionTripped
	// ProtectionLockout: the autoreclose sequence exhausted its attempts.
	// The breaker stays open until an operator/SCADA ResetLockout.
	ProtectionLockout
)

// String returns a short human-readable name for the state.
func (s ProtectionState) String() string {
	switch s {
	case ProtectionNormal:
		return "Normal"
	case ProtectionPickedUp:
		return "PickedUp"
	case ProtectionTripped:
		return "Tripped"
	case ProtectionLockout:
		return "Lockout"
	default:
		return "Unknown"
	}
}

// ProtectionStatus reports the protection relay's current state.
type ProtectionStatus struct {
	// State is the relay's overall protection state.
	State ProtectionState
	// ActiveGroup is the settings group currently in effect.
	ActiveGroup SettingsGroup
}

// ProtectionCause identifies which protection stage caused a pickup or trip.
type ProtectionCause uint8

const (
	// CauseInstantaneous is the stage-50 instantaneous overcurrent stage.
	CauseInstantaneous ProtectionCause = iota
	// CauseTimeOvercurrent is the stage-51 inverse/definite time overcurrent stage.
	CauseTimeOvercurrent
)

// String returns a short human-readable name for the cause.
func (c ProtectionCause) String() string {
	switch c {
	case CauseInstantaneous:
		return "Instantaneous"
	case CauseTimeOvercurrent:
		return "TimeOvercurrent"
	default:
		return "Unknown"
	}
}

// validSettingsGroup reports an error if g is outside the configured
// NumSettingsGroups groups.
func validSettingsGroup(g SettingsGroup) error {
	if int(g) >= NumSettingsGroups {
		return fmt.Errorf("breaker: invalid settings group %d", g)
	}
	return nil
}
