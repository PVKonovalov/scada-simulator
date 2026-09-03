package telemetryserver

import (
	"scada-simulator/pkg/breaker"
	"scada-simulator/pkg/telemetry"
)

// Tag describes one tag's identity and datatype, independent of any current
// value — for provisioning a point ahead of any data actually arriving
// (see internal/restserver's GET /api/external/telemetry/tags).
type Tag struct {
	// Key is the tag name, e.g. "feeder-1.power.reactive".
	Key string
	// DataType is the SCADA REST API's TagInfo.data_type spelling:
	// "boolean", "integer", "float" or "protection_event".
	DataType string
}

// TagsFor returns every tag for the breaker named name: every tag Subscribe
// reports (i.e. breakerSnapshot's keys and types, without reading its
// current values) plus the write-only "<name>.control" tag SupervisoryControl
// accepts (see control.go and pkg/breaker/README.md's "SCADA tag naming"
// section) and the three one-shot pulse tags dataPointsForEvent pushes on
// EventTrip/EventLockout/EventAutoRecloseSucceeded — included here so a
// caller can provision them too, even though none of the four ever appears
// in breakerSnapshot (the control tag is write-only; the pulse tags carry no
// persistent value). controlTagSuffix is "boolean" (1 bit); the three pulse
// tags are "protection_event" (see pulsePoint/DataPointType_PROTECTION_EVENT).
func TagsFor(name string, sim *breaker.Simulator) []Tag {
	points := breakerSnapshot(name, sim, false) // identity/datatype only: test mode doesn't affect either
	tags := make([]Tag, 0, len(points)+4)
	for _, p := range points {
		tags = append(tags, Tag{Key: p.GetKey(), DataType: dataTypeString(p.GetType())})
	}
	tags = append(tags,
		Tag{Key: name + controlTagSuffix, DataType: "boolean"},
		Tag{Key: name + tripProtectionTagSuffix, DataType: dataTypeString(telemetry.DataPointType_PROTECTION_EVENT)},
		Tag{Key: name + autoRecloseFailedTagSuffix, DataType: dataTypeString(telemetry.DataPointType_PROTECTION_EVENT)},
		Tag{Key: name + autoRecloseSucceededTagSuffix, DataType: dataTypeString(telemetry.DataPointType_PROTECTION_EVENT)},
	)
	return tags
}

// dataTypeString converts a telemetry.DataPointType to the SCADA REST
// API's TagInfo.data_type spelling.
func dataTypeString(t telemetry.DataPointType) string {
	switch t {
	case telemetry.DataPointType_BOOLEAN:
		return "boolean"
	case telemetry.DataPointType_INTEGER:
		return "integer"
	case telemetry.DataPointType_PROTECTION_EVENT:
		return "protection_event"
	default:
		return "float"
	}
}
