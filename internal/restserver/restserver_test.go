package restserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"scada-simulator/pkg/llog"
)

// postToken issues a POST /api/token request against handler's server with
// the given form values, returning the response and its decoded JSON body.
func postToken(t *testing.T, handler http.Handler, form url.Values) (*http.Response, map[string]any) {
	t.Helper()

	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Post(server.URL+tokenPath, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("POST %s: %v", tokenPath, err)
	}
	defer resp.Body.Close()

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp, body
}

// TestServer_Token_ValidCredentials checks that correct username/password
// gets a 200 with a TokenResponse-shaped body.
func TestServer_Token_ValidCredentials(t *testing.T) {
	handler := NewServer("admin", "secret", nil, llog.Logger).Handler()

	form := url.Values{}
	form.Set("grant_type", "")
	form.Set("username", "admin")
	form.Set("password", "secret")
	form.Set("scope", "")
	form.Set("client_id", "")
	form.Set("client_secret", "")

	resp, body := postToken(t, handler, form)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	token, _ := body["access_token"].(string)
	if token == "" {
		t.Errorf("access_token missing or empty: %+v", body)
	}
	if tokenType, _ := body["token_type"].(string); tokenType != "bearer" {
		t.Errorf("token_type = %q, want %q", tokenType, "bearer")
	}
	if expiresIn, _ := body["expires_in"].(float64); expiresIn != tokenTTL {
		t.Errorf("expires_in = %v, want %v", expiresIn, tokenTTL)
	}
}

// TestServer_Token_ValidCredentials_UniqueTokens checks that two logins
// don't hand back the same token.
func TestServer_Token_ValidCredentials_UniqueTokens(t *testing.T) {
	handler := NewServer("admin", "secret", nil, llog.Logger).Handler()
	form := url.Values{"username": {"admin"}, "password": {"secret"}}

	_, first := postToken(t, handler, form)
	_, second := postToken(t, handler, form)

	if first["access_token"] == second["access_token"] {
		t.Errorf("two logins returned the same access_token: %v", first["access_token"])
	}
}

// TestServer_Token_WrongPassword checks that an incorrect password is
// rejected with 401 and a FastAPI-style {"detail": ...} body.
func TestServer_Token_WrongPassword(t *testing.T) {
	handler := NewServer("admin", "secret", nil, llog.Logger).Handler()
	form := url.Values{"username": {"admin"}, "password": {"wrong"}}

	resp, body := postToken(t, handler, form)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if _, ok := body["access_token"]; ok {
		t.Errorf("access_token present in a rejected response: %+v", body)
	}
	if _, ok := body["detail"]; !ok {
		t.Errorf("detail missing from error response: %+v", body)
	}
}

// TestServer_Token_MissingFields checks that an incomplete request is
// rejected with 422 (matching FastAPI's validation-error status for a
// required field), not silently treated as a wrong password.
func TestServer_Token_MissingFields(t *testing.T) {
	handler := NewServer("admin", "secret", nil, llog.Logger).Handler()
	form := url.Values{"username": {"admin"}} // no password

	resp, _ := postToken(t, handler, form)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
}

// TestServer_Token_WrongMethod checks that GET /api/token (not part of the
// contract) is rejected rather than silently handled.
func TestServer_Token_WrongMethod(t *testing.T) {
	handler := NewServer("admin", "secret", nil, llog.Logger).Handler()
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + tokenPath)
	if err != nil {
		t.Fatalf("GET %s: %v", tokenPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}
