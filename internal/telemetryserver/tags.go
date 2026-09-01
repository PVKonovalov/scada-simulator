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
	// "boolean", "integer" or "float".
	DataType string
}

// TagsFor returns every tag for the breaker named name: every tag Subscribe
// reports (i.e. breakerSnapshot's keys and types, without reading its
// current values) plus the write-only "<name>.control" tag SupervisoryControl
// accepts (see control.go and pkg/breaker/README.md's "SCADA tag naming"
// section) — included here so a caller can provision it too, even though it
// never appears in a Subscribe update. Its DataType is "boolean" (1 bit):
// positionForControlValue only accepts Open/Close, a single bit, unlike
// "<name>.position" itself, which needs "integer" (2 bits) for its four
// double-point states (Intermediate/Open/Closed/Bad).
func TagsFor(name string, sim *breaker.Simulator) []Tag {
	points := breakerSnapshot(name, sim, false) // identity/datatype only: test mode doesn't affect either
	tags := make([]Tag, 0, len(points)+1)
	for _, p := range points {
		tags = append(tags, Tag{Key: p.GetKey(), DataType: dataTypeString(p.GetType())})
	}
	tags = append(tags, Tag{Key: name + controlTagSuffix, DataType: "boolean"})
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
	default:
		return "float"
	}
}
