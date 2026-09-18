package web

import (
	"encoding/json"
	"net/http"
)

// handlers_diag.go — the diagnostics mutations: request replay (one-shot
// force-provider re-answer), per-route-target model probes, and the
// models.dev catalog force-refresh. Semantics live in internal/admin; these
// handlers only decode, classify errors, and encode.

// handleReplay serves POST /api/replay {id, provider} — the daemon twin of
// `model-proxy replay <id> --to <provider>`.
func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string `json:"id"`
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return
	}
	result, err := s.commands.Replay(r.Context(), req.ID, req.Provider)
	if err != nil {
		writePortErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleRouteTest serves POST /api/routes/test {model} — the daemon twin of
// `model-proxy test <model>` (one real upstream probe per route target).
func (s *Server) handleRouteTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return
	}
	result, err := s.commands.TestRoute(r.Context(), req.Model)
	if err != nil {
		writePortErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleModelsCatalogRefresh serves POST /api/models/catalog/refresh — the
// daemon twin of `model-proxy models pull` (force-refresh the models.dev
// metadata cache).
func (s *Server) handleModelsCatalogRefresh(w http.ResponseWriter, r *http.Request) {
	result, err := s.commands.PullModelsCatalog(r.Context())
	if err != nil {
		writePortErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
