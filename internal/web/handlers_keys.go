package web

// handlers_keys.go — the forward-key management endpoints: list (usage-
// enriched), create (full token returned exactly once), revoke, and one key's
// per-model usage. Semantics live in internal/admin; these handlers decode,
// classify, and encode.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// handleAPIKeys serves GET /api/keys — the Keys page's list.
func (s *Server) handleAPIKeys(w http.ResponseWriter, r *http.Request) {
	result, err := s.commands.APIKeys()
	if err != nil {
		writePortErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleAPIKeyCreate serves POST /api/keys {label}.
func (s *Server) handleAPIKeyCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return
	}
	result, err := s.commands.CreateAPIKey(strings.TrimSpace(req.Label))
	if err != nil {
		writePortErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleAPIKeyRevoke serves DELETE /api/keys/<id>.
func (s *Server) handleAPIKeyRevoke(w http.ResponseWriter, id string) {
	if id == "" {
		writeJSONErr(w, http.StatusBadRequest, "key id is required")
		return
	}
	if err := s.commands.RevokeAPIKey(id); err != nil {
		writePortErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// handleAPIKeyUsage serves GET /api/keys/<id>/usage?days=N (default 7).
func (s *Server) handleAPIKeyUsage(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" {
		writeJSONErr(w, http.StatusBadRequest, "key id is required")
		return
	}
	days := 7
	if d := r.URL.Query().Get("days"); d != "" {
		v, err := strconv.Atoi(d)
		if err != nil || v <= 0 || v > 90 {
			writeJSONErr(w, http.StatusBadRequest, "days must be an integer in [1,90]")
			return
		}
		days = v
	}
	to := time.Now()
	from := to.AddDate(0, 0, -days)
	rows, err := s.commands.APIKeyUsage(id, from, to)
	if err != nil {
		writePortErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "days": days, "models": rows})
}
