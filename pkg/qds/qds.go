package qds

import "fmt"

type Quality uint32

const VSNumberOfBits = 13

const (
	QdsGood         Quality = 0x00000000  // Good measurement
	QdsInvalid      Quality = 0x00000001  // Invalid measurement
	QdsNoSource     Quality = 0x00000002  // No source
	QdsSubstituted  Quality = 0x00000004  // Substituted
	QdsOverflow     Quality = 0x00000008  // Overflow
	QdsWarnLl       Quality = 0x00000010  // Lower warning limit
	QdsWarnUl       Quality = 0x00000020  // Upper warning limit
	QdsAlarmLl      Quality = 0x00000040  // Lower alarm limit
	QdsAlarmUl      Quality = 0x00000080  // Upper alarm limit
	QdsTest         Quality = 0x00000100  // Test
	QdsBlocked      Quality = 0x000000200 // Operator blocked
	QdsOutOfScan    Quality = 0x00000400  // Out of scan
	QdsManual       Quality = 0x000000800 // Manual entered data
	QdsQuestionable Quality = 0x000001000 //
)

// GetPriority returns whichever single flag set in g ranks highest in QdsPriority
// (index 0 = highest), or QdsGood if none of QdsPriority's flags are set — for picking
// one representative quality (e.g. a border/badge color) when more than one is set at once.
func (q Quality) GetPriority() Quality {
	QdsPriority := [10]Quality{
		QdsNoSource,
		QdsInvalid,
		QdsAlarmUl,
		QdsAlarmLl,
		QdsWarnLl,
		QdsWarnUl,
		QdsBlocked,
		QdsOutOfScan,
		QdsManual,
		QdsTest,
	}
	for _, flag := range QdsPriority {
		if q.Has(flag) {
			return flag
		}
	}
	return QdsGood
}

// Has checks if a given flag is set.
func (q Quality) Has(flag Quality) bool {
	return q&flag != 0
}

// Set sets a flag.
func (q *Quality) Set(flag Quality) {
	*q = *q | flag
}

// Clear clears a flag.
func (q *Quality) Clear(flag Quality) {
	*q = *q & ^(flag)
}

func (q Quality) String() string {
	qualityCode := []string{"", "IV", "NS", "SB", "OV", "LIW", "ULW", "LIA", "ULA", "TST", "BL", "OOS", "MED", "??"}
	qualityString := "GOOD"

	needToAddSpace := false
	if q != 0 {
		qualityString = ""
		for i := range VSNumberOfBits {
			if q&(0x0001<<i) == 0x0001<<i {
				if needToAddSpace {
					qualityString += " "
				}
				qualityString += qualityCode[i+1]
				needToAddSpace = true
			}
		}
	}
	return qualityString
}

// MarshalJSON converts the Quality to a JSON string in ISO 8601 format UTC
func (q Quality) MarshalJSON() ([]byte, error) {
	return []byte("\"" + q.String() + "\""), nil
}

// Uint64 converts the Quality to uint64
func (q Quality) Uint64() uint64 {
	return uint64(q)
}

// AsString converts the Quality value to string
func (q Quality) AsString() string {
	return fmt.Sprintf("%d", uint64(q))
}
