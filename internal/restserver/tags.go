package restserver

import (
	"net/http"

	"scada-simulator/internal/telemetryserver"
)

// tagsPath is GET /api/external/telemetry/tags.
const tagsPath = "/api/external/telemetry/tags"

// substationName is a fixed placeholder used as every response's
// substation_name: this simulator has no client/substation hierarchy of
// its own, the same simplification internal/telemetryserver's Subscribe
// makes for SubstationRequest.Id (see its doc comment) — every tag from
// every configured breaker is reported under one substation entry.
const substationName = "scada-simulator"

// tagInfo mirrors api/scada/openapi.json's TagInfo schema.
type tagInfo struct {
	// Tag is the point tag (telemetryserver.Tag.Key).
	Tag string `json:"tag"`
	// DataType is "boolean", "integer" or "float" (telemetryserver.Tag.DataType).
	DataType string `json:"data_type"`
	// Protection flags a point as a protection event regardless of
	// DataType. None of this simulator's current tags represent a
	// discrete protection-event point (see pkg/breaker/README.md's "SCADA
	// tag naming" section — protection.state is a continuous status
	// value, not an event pulse), so this is always 0.
	Protection int `json:"protection"`
}

// substationTags is one entry in tagsResponse.Substations.
type substationTags struct {
	SubstationId   string    `json:"substation_id"`
	SubstationName string    `json:"substation_name"`
	Tags           []tagInfo `json:"tags"`
}

// tagsResponse is GET /api/external/telemetry/tags's response body.
type tagsResponse struct {
	Substations []substationTags `json:"substations"`
}

// handleTags implements GET /api/external/telemetry/tags: every tag-id
// (with its datatype) across every configured breaker. id is required by
// the real API's contract but otherwise unused here — see substationName's
// comment; it is only echoed back as SubstationId.
func (s *Server) handleTags(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeError(w, http.StatusUnprocessableEntity, "id is required")
		return
	}

	var tags []tagInfo
	for name, sim := range s.breakers {
		for _, t := range telemetryserver.TagsFor(name, sim) {
			tags = append(tags, tagInfo{Tag: t.Key, DataType: t.DataType})
		}
	}

	writeJSON(w, http.StatusOK, tagsResponse{
		Substations: []substationTags{
			{SubstationId: id, SubstationName: substationName, Tags: tags},
		},
	})
}
