package qds

import "testing"

func TestQuality_Has(t *testing.T) {
	q := QdsInvalid | QdsNoSource

	if !q.Has(QdsInvalid) {
		t.Errorf("expected Has(QdsIv) to be true")
	}
	if !q.Has(QdsNoSource) {
		t.Errorf("expected Has(QdsNs) to be true")
	}
	if q.Has(QdsSubstituted) {
		t.Errorf("expected Has(QdsSb) to be false")
	}
	if QdsGood.Has(QdsInvalid) {
		t.Errorf("expected QdsGood to have no flags set")
	}
}

func TestQuality_Set(t *testing.T) {
	q := QdsGood
	q.Set(QdsOverflow)

	if !q.Has(QdsOverflow) {
		t.Errorf("expected QdsOv to be set")
	}

	q.Set(QdsWarnLl)
	if !q.Has(QdsOverflow) || !q.Has(QdsWarnLl) {
		t.Errorf("expected both QdsOv and QdsLl to remain set, got %v", q)
	}
}

func TestQuality_Clear(t *testing.T) {
	q := QdsInvalid | QdsNoSource | QdsSubstituted
	q.Clear(QdsNoSource)

	if q.Has(QdsNoSource) {
		t.Errorf("expected QdsNs to be cleared")
	}
	if !q.Has(QdsInvalid) || !q.Has(QdsSubstituted) {
		t.Errorf("expected QdsIv and QdsSb to remain set, got %v", q)
	}
}

func TestQuality_String(t *testing.T) {
	tests := []struct {
		name string
		q    Quality
		want string
	}{
		{"good", QdsGood, "GOOD"},
		{"single flag", QdsInvalid, "IV"},
		{"multiple flags in bit order", QdsWarnUl | QdsInvalid, "IV ULW"},
		{"all flags", QdsInvalid | QdsNoSource | QdsSubstituted | QdsOverflow | QdsWarnLl | QdsWarnUl | QdsAlarmLl | QdsAlarmUl | QdsTest | QdsBlocked | QdsOutOfScan, "IV NS SB OV LIW ULW LIA ULA TST BL OOS"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.q.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestQuality_MarshalJSON(t *testing.T) {
	got, err := QdsInvalid.MarshalJSON()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := `"IV"`; string(got) != want {
		t.Errorf("MarshalJSON() = %s, want %s", got, want)
	}
}

func TestQuality_Uint64(t *testing.T) {
	q := QdsInvalid | QdsNoSource
	if got, want := q.Uint64(), uint64(0x00000003); got != want {
		t.Errorf("Uint64() = %d, want %d", got, want)
	}
}

func TestQuality_AsString(t *testing.T) {
	q := QdsInvalid | QdsNoSource
	if got, want := q.AsString(), "3"; got != want {
		t.Errorf("AsString() = %q, want %q", got, want)
	}
}
