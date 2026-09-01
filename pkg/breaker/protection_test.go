package breaker

import (
	"math"
	"testing"
	"time"
)

// referenceOperateTime independently re-implements the IEC 60255 formula
// (rather than calling OperateTime) so the test can catch a wrong
// operator/exponent in the production implementation, not merely echo it.
func referenceOperateTime(k, alpha, tms, current, pickup float64) time.Duration {
	ratio := current / pickup
	seconds := tms * (k / (math.Pow(ratio, alpha) - 1))
	return time.Duration(seconds * float64(time.Second))
}

// TestOperateTime_IECCurves checks OperateTime against an independently
// computed reference value for each IEC 60255 inverse-time curve at a few
// current multiples of pickup (PSM).
func TestOperateTime_IECCurves(t *testing.T) {
	tests := []struct {
		name    string
		curve   CurveType
		k       float64
		alpha   float64
		tms     float64
		pickup  float64
		current float64
	}{
		{"StandardInverse PSM=2", CurveIECStandardInverse, 0.14, 0.02, 1, 10, 20},
		{"StandardInverse PSM=10", CurveIECStandardInverse, 0.14, 0.02, 0.5, 5, 50},
		{"VeryInverse PSM=2", CurveIECVeryInverse, 13.5, 1, 1, 10, 20},
		{"VeryInverse PSM=5", CurveIECVeryInverse, 13.5, 1, 0.3, 4, 20},
		{"ExtremelyInverse PSM=3", CurveIECExtremelyInverse, 80, 2, 1, 10, 30},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := ProtectionSettings{
				PickupCurrent:  tt.pickup,
				Curve:          tt.curve,
				TimeMultiplier: tt.tms,
			}
			want := referenceOperateTime(tt.k, tt.alpha, tt.tms, tt.current, tt.pickup)
			got, ok := OperateTime(settings, tt.current)
			if !ok {
				t.Fatalf("OperateTime() ok = false, want true")
			}
			if diff := got - want; diff > time.Microsecond || diff < -time.Microsecond {
				t.Errorf("OperateTime() = %v, want %v (diff %v)", got, want, diff)
			}
		})
	}
}

// TestOperateTime_DefiniteTime checks the definite-time curve returns the
// configured fixed delay, ignoring the inverse-time formula entirely.
func TestOperateTime_DefiniteTime(t *testing.T) {
	settings := ProtectionSettings{
		PickupCurrent: 10,
		Curve:         CurveDefiniteTime,
		DefiniteTime:  500 * time.Millisecond,
	}

	got, ok := OperateTime(settings, 100)
	if !ok {
		t.Fatalf("OperateTime() ok = false, want true")
	}
	if got != 500*time.Millisecond {
		t.Errorf("OperateTime() = %v, want 500ms", got)
	}
}

// TestOperateTime_BelowOrAtPickup checks that no trip timer runs at or
// below pickup, and that a disabled stage (PickupCurrent 0) never picks up.
func TestOperateTime_BelowOrAtPickup(t *testing.T) {
	settings := ProtectionSettings{
		PickupCurrent:  10,
		Curve:          CurveIECStandardInverse,
		TimeMultiplier: 1,
	}

	tests := []struct {
		name    string
		current float64
	}{
		{"below pickup", 5},
		{"at pickup", 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := OperateTime(settings, tt.current); ok {
				t.Errorf("OperateTime() ok = true, want false")
			}
		})
	}

	disabled := ProtectionSettings{PickupCurrent: 0, Curve: CurveIECStandardInverse, TimeMultiplier: 1}
	if _, ok := OperateTime(disabled, 1000); ok {
		t.Errorf("OperateTime() with PickupCurrent=0 ok = true, want false")
	}
}

// TestInstantaneousOperateTime checks the stage-50 delay is proportional to
// the configured number of power-system cycles, and that pickup gating and
// the disabled (0) case behave like stage 51.
func TestInstantaneousOperateTime(t *testing.T) {
	settings := ProtectionSettings{InstantaneousPickup: 1000}

	got, ok := InstantaneousOperateTime(settings, 2000, 50)
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	want := time.Duration(instantaneousCycles * float64(time.Second) / 50)
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}

	if _, ok := InstantaneousOperateTime(settings, 500, 50); ok {
		t.Errorf("below pickup: ok = true, want false")
	}
	if _, ok := InstantaneousOperateTime(settings, 1000, 50); ok {
		t.Errorf("at pickup: ok = true, want false")
	}

	disabled := ProtectionSettings{InstantaneousPickup: 0}
	if _, ok := InstantaneousOperateTime(disabled, 100000, 50); ok {
		t.Errorf("disabled stage: ok = true, want false")
	}

	// nominalFrequencyHz <= 0 falls back to 50Hz.
	gotDefault, _ := InstantaneousOperateTime(settings, 2000, 0)
	if gotDefault != want {
		t.Errorf("nominalFrequencyHz=0 got %v, want %v (50Hz default)", gotDefault, want)
	}
}

// TestParseCurveType checks round-tripping through String/ParseCurveType.
func TestParseCurveType(t *testing.T) {
	for _, c := range []CurveType{CurveDefiniteTime, CurveIECStandardInverse, CurveIECVeryInverse, CurveIECExtremelyInverse} {
		parsed, err := ParseCurveType(c.String())
		if err != nil {
			t.Fatalf("ParseCurveType(%q): %v", c.String(), err)
		}
		if parsed != c {
			t.Errorf("ParseCurveType(%q) = %v, want %v", c.String(), parsed, c)
		}
	}
	if _, err := ParseCurveType("not-a-curve"); err == nil {
		t.Errorf("ParseCurveType(invalid) err = nil, want error")
	}
}
