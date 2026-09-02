package breaker

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"scada-simulator/pkg/qds"
)

// Position reports a breaker's contact state as derived from its two
// independent auxiliary contacts (52a "closed" indication, 52b "open"
// indication) rather than as a simple open/closed enum. A real HV breaker
// can be caught mid-travel or wired/mechanised inconsistently, and SCADA
// consumers need to be able to see that rather than have it hidden behind a
// bool.
type Position uint8

const (
	// PositionIntermediate: both auxiliary contacts open/unsettled — the
	// breaker is in transit between Open and Closed.
	PositionIntermediate Position = iota
	// PositionOpen: 52b closed, 52a open — confirmed open.
	PositionOpen
	// PositionClosed: 52a closed, 52b open — confirmed closed.
	PositionClosed
	// PositionBad: both contacts assert "closed" at once — a
	// mechanism or wiring fault. The position cannot be trusted.
	PositionBad
)

// String returns a short human-readable name for the position.
func (p Position) String() string {
	switch p {
	case PositionIntermediate:
		return "Intermediate"
	case PositionOpen:
		return "Open"
	case PositionClosed:
		return "Closed"
	case PositionBad:
		return "Bad"
	default:
		return "Unknown"
	}
}

// ParsePosition parses a position name as produced by Position.String,
// case-insensitively.
func ParsePosition(s string) (Position, error) {
	switch strings.ToLower(s) {
	case "intermediate":
		return PositionIntermediate, nil
	case "open":
		return PositionOpen, nil
	case "closed":
		return PositionClosed, nil
	case "bad":
		return PositionBad, nil
	}
	return 0, fmt.Errorf("breaker: invalid position %q", s)
}

// MarshalYAML renders the position using its String form.
func (p Position) MarshalYAML() (any, error) {
	return p.String(), nil
}

// UnmarshalYAML parses the position from a YAML string via ParsePosition.
func (p *Position) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := ParsePosition(s)
	if err != nil {
		return err
	}
	*p = parsed
	return nil
}

// Mode reports a breaker's IEC 61850 Mod-style operating mode, per Edition
// 1's Mod/Beh table: whether its protection/control function is active, and
// if so, whether it drives real outputs (On/Test) or none (Blocked/Off).
// Numeric values match that table directly; TEST/BLOCKED (4) is deliberately
// not modelled by this simulator, so codes skip from 3 straight to 5.
type Mode uint8

const (
	// ModeOn: function active, real outputs generated, control commands
	// accepted — normal operation.
	ModeOn Mode = 1
	// ModeBlocked: function active — protection still evaluates and still
	// reports a trip decision — but drives no physical output (a trip does
	// not move the breaker), and rejects incoming control commands.
	ModeBlocked Mode = 2
	// ModeTest: function active and behaves exactly like On — the breaker
	// really moves — except every tag it drives reports qds.QdsTest quality
	// instead of Good.
	ModeTest Mode = 3
	// ModeOff: function not active at all — protection evaluation is
	// skipped entirely and reports no updates, and control commands are
	// rejected, same as Blocked.
	ModeOff Mode = 5
)

// String returns a short human-readable name for the mode.
func (m Mode) String() string {
	switch m {
	case ModeOn:
		return "On"
	case ModeBlocked:
		return "Blocked"
	case ModeTest:
		return "Test"
	case ModeOff:
		return "Off"
	default:
		return "Unknown"
	}
}

// PhaseValues holds a three-phase quantity, one value per phase.
type PhaseValues struct {
	A float64 // phase A value
	B float64 // phase B value
	C float64 // phase C value
}

// Max returns the largest of the three phase values. The protection relay
// evaluates overcurrent using the maximum of the three phase currents
// unless a specific stage documents otherwise.
func (p PhaseValues) Max() float64 {
	m := p.A
	if p.B > m {
		m = p.B
	}
	if p.C > m {
		m = p.C
	}
	return m
}

// Measurement is a three-phase analog reading taken from the plant/emulator
// at a point in time.
type Measurement struct {
	Current PhaseValues // amps, RMS, per phase
	Voltage PhaseValues // volts, RMS, per phase, phase-to-neutral

	ActivePower   float64 // W, three-phase total (computed by the emulator feed)
	ReactivePower float64 // VAR, three-phase total
	ApparentPower float64 // VA, three-phase total
	Frequency     float64 // Hz

	Timestamp time.Time
	// Quality reuses the application-wide SCADA point-quality bitflags
	// from pkg/qds (zero value qds.QdsGood means a fully valid reading).
	Quality qds.Quality
}
