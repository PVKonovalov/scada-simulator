// Package restserver implements the small slice of the RDSS SCADA/DMS REST
// API (api/scada/openapi.json) this simulator needs to serve as the SCADA
// side of that connection: POST /api/token (the OAuth2-password-grant
// login every REST and gRPC client is expected to call first) and GET
// /api/external/telemetry/tags (every tag-id, for provisioning a point
// ahead of any data arriving via internal/telemetryserver's Subscribe). It
// is not a general implementation of that API — see each handler's doc
// comment for what's deliberately out of scope.
package restserver

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"scada-simulator/pkg/breaker"
	"scada-simulator/pkg/llog"
)

// tokenPath is POST /api/token.
const tokenPath = "/api/token"

// tokenTTL is the value reported as TokenResponse.ExpiresIn. This simulator
// does not actually expire or otherwise validate tokens anywhere (neither
// internal/telemetryserver's gRPC service nor this REST server checks a
// bearer token on any other call) — ExpiresIn exists only because real
// OAuth2-password-grant clients (e.g. rdss-dms-rtdb/pkg/rest_client)
// expect the field to be present.
const tokenTTL = 3600 // seconds

// TokenResponse is POST /api/token's success response body, matching the
// OAuth2-password-grant convention the real RDSS SCADA/DMS REST API uses
// (api/scada/openapi.json's Body_login_for_access_token_token_post) and
// the shape rdss-dms-rtdb/pkg/rest_client.TokenResponse expects.
type TokenResponse struct {
	// AccessToken is the bearer token a client would send as
	// "authorization: Bearer <AccessToken>" on further calls.
	AccessToken string `json:"access_token"`
	// TokenType is always "bearer".
	TokenType string `json:"token_type"`
	// ExpiresIn is how many seconds AccessToken is valid for; see tokenTTL's comment.
	ExpiresIn int `json:"expires_in"`
}

// errorResponse is POST /api/token's failure response body, matching
// FastAPI's HTTPException convention ({"detail": "..."}) used throughout
// api/scada/openapi.json.
type errorResponse struct {
	// Detail is a human-readable description of what went wrong.
	Detail string `json:"detail"`
}

// Server serves the REST endpoints this simulator implements.
type Server struct {
	username string                        // required POST /api/token username
	password string                        // required POST /api/token password
	breakers map[string]*breaker.Simulator // configured breakers, keyed by Config.Name; source for GET /api/external/telemetry/tags
	logger   *llog.LevelLog                // logger for request activity
}

// NewServer builds a Server that accepts POST /api/token requests
// presenting username/password, and serves GET
// /api/external/telemetry/tags over breakers (keyed by Config.Name, same
// as internal/telemetryserver.NewTelemetryServer).
func NewServer(username, password string, breakers map[string]*breaker.Simulator, logger *llog.LevelLog) *Server {
	return &Server{username: username, password: password, breakers: breakers, logger: logger}
}

// Handler returns the http.Handler serving this Server's endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+tokenPath, s.handleToken)
	mux.HandleFunc("GET "+tagsPath, s.handleTags)
	return mux
}

// handleToken implements POST /api/token: an OAuth2-password-grant login,
// form-urlencoded per api/scada/openapi.json (username/password required;
// grant_type/scope/client_id/client_secret accepted but not checked against
// anything, matching the DMS-side client, rdss-dms-rtdb/pkg/rest_client,
// which sends them empty). A username/password matching Server's
// credentials gets a freshly generated opaque bearer token back.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid form body")
		return
	}

	username := r.PostFormValue("username")
	password := r.PostFormValue("password")
	if username == "" || password == "" {
		writeError(w, http.StatusUnprocessableEntity, "username and password are required")
		return
	}
	if username != s.username || password != s.password {
		s.logger.Warnf("rest: /api/token rejected: incorrect username or password (username=%q)", username)
		writeError(w, http.StatusUnauthorized, "incorrect username or password")
		return
	}

	token, err := newToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}
	s.logger.Infof("rest: /api/token issued (username=%q)", username)
	writeJSON(w, http.StatusOK, TokenResponse{
		AccessToken: token,
		TokenType:   "bearer",
		ExpiresIn:   tokenTTL,
	})
}

// newToken generates a random, opaque bearer token.
func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// writeJSON writes v as a JSON response body with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a FastAPI-style {"detail": ...} error response.
func writeError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, errorResponse{Detail: detail})
}
