package restserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"scada-simulator/pkg/breaker"
	"scada-simulator/pkg/llog"
)

// TestServer_Tags_ListsEveryBreakersTags checks that every configured
// breaker's tags are reported, correctly typed, grouped under one
// substation entry echoing back the requested id, including the
// write-only control tag.
func TestServer_Tags_ListsEveryBreakersTags(t *testing.T) {
	ctx := t.Context()

	simA := breaker.New(ctx, breaker.Config{Name: "feeder-1"})
	defer simA.Close()
	simB := breaker.New(ctx, breaker.Config{Name: "feeder-2"})
	defer simB.Close()

	handler := NewServer("admin", "secret", map[string]*breaker.Simulator{
		"feeder-1": simA,
		"feeder-2": simB,
	}, llog.Logger).Handler()

	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + tagsPath + "?id=some-substation-uuid")
	if err != nil {
		t.Fatalf("GET %s: %v", tagsPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body tagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(body.Substations) != 1 {
		t.Fatalf("len(Substations) = %d, want 1", len(body.Substations))
	}
	sub := body.Substations[0]
	if sub.SubstationId != "some-substation-uuid" {
		t.Errorf("SubstationId = %q, want the requested id echoed back", sub.SubstationId)
	}

	byTag := make(map[string]tagInfo, len(sub.Tags))
	for _, tag := range sub.Tags {
		if _, dup := byTag[tag.Tag]; dup {
			t.Fatalf("duplicate tag %q", tag.Tag)
		}
		byTag[tag.Tag] = tag
	}

	wantTypes := map[string]string{
		"feeder-1.position":            "integer",
		"feeder-1.blocked":             "boolean",
		"feeder-1.current.a":           "float",
		"feeder-1.power.reactive":      "float",
		"feeder-1.frequency":           "float",
		"feeder-1.protection.state":    "boolean",
		"feeder-1.autoreclose.attempt": "boolean",
		"feeder-1.control":             "boolean",
		"feeder-2.position":            "integer",
		"feeder-2.control":             "boolean",
	}
	for tag, wantType := range wantTypes {
		got, ok := byTag[tag]
		if !ok {
			t.Errorf("missing tag %q", tag)
			continue
		}
		if got.DataType != wantType {
			t.Errorf("%s data_type = %q, want %q", tag, got.DataType, wantType)
		}
		if got.Protection != 0 {
			t.Errorf("%s protection = %d, want 0", tag, got.Protection)
		}
	}

	const wantPerBreaker = 17 // position, blocked, 3xcurrent, 3xvoltage, 3xpower, frequency, protection.state, protection.group, autoreclose.state, autoreclose.attempt, control
	if len(sub.Tags) != wantPerBreaker*2 {
		t.Errorf("len(Tags) = %d, want %d (2 breakers x %d tags)", len(sub.Tags), wantPerBreaker*2, wantPerBreaker)
	}
}

// TestServer_Tags_MissingId checks that a request without the required id
// query parameter is rejected with 422.
func TestServer_Tags_MissingId(t *testing.T) {
	ctx := t.Context()
	sim := breaker.New(ctx, breaker.Config{Name: "feeder-1"})
	defer sim.Close()

	handler := NewServer("admin", "secret", map[string]*breaker.Simulator{"feeder-1": sim}, llog.Logger).Handler()
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + tagsPath)
	if err != nil {
		t.Fatalf("GET %s: %v", tagsPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", resp.StatusCode)
	}
}

// TestServer_Tags_WrongMethod checks that POST (not part of the contract)
// is rejected rather than silently handled.
func TestServer_Tags_WrongMethod(t *testing.T) {
	handler := NewServer("admin", "secret", nil, llog.Logger).Handler()
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Post(server.URL+tagsPath, "application/json", nil)
	if err != nil {
		t.Fatalf("POST %s: %v", tagsPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

// TestServer_Tags_NoBreakers checks that an empty breaker set still
// produces a well-formed (empty-tags) response rather than an error.
func TestServer_Tags_NoBreakers(t *testing.T) {
	handler := NewServer("admin", "secret", map[string]*breaker.Simulator{}, llog.Logger).Handler()
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + tagsPath + "?id=x")
	if err != nil {
		t.Fatalf("GET %s: %v", tagsPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body tagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Substations) != 1 || len(body.Substations[0].Tags) != 0 {
		t.Errorf("body = %+v, want one substation with zero tags", body)
	}
}
