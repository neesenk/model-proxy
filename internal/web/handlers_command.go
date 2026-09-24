package web

import (
	"context"
	"encoding/json"
	"io"
	"model-proxy/internal/appapi"
	"net/http"
	"strings"
	"time"
)

func (s *Server) handleTokensReset(w http.ResponseWriter, _ *http.Request) {
	if err := s.commands.ResetStats(); err != nil {
		writePortErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
}

// handleModelsRefresh serves POST /api/models/refresh: the daemon twin of
// `model-proxy models refresh <provider>` (fetch live list → 3-protocol probe
// → write the callable subset to config → hot-reload → replace cached
// verdicts). The result carries the kept list, the config diff, and any
// unvalidated-write warning for the UI to surface.
func (s *Server) handleModelsRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return
	}
	if req.Provider == "" {
		writeJSONErr(w, http.StatusBadRequest, "provider is required")
		return
	}
	result, err := s.commands.RefreshModels(r.Context(), req.Provider)
	if err != nil {
		writePortErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleModelsDisable serves POST /api/models/disable: the Status→Models
// card's per-model Disable/Enable toggle. disabled=true hides the model from
// GET /v1/models and drops it from scheduling (fail-closed validation — an
// unknown provider/model is a 400, never a silently-dead override); the
// override is memory-only (survives reloads, cleared on restart).
func (s *Server) handleModelsDisable(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Disabled *bool  `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return
	}
	if req.Provider == "" || req.Model == "" {
		writeJSONErr(w, http.StatusBadRequest, "provider and model are required")
		return
	}
	if req.Disabled == nil {
		writeJSONErr(w, http.StatusBadRequest, "disabled (boolean) is required")
		return
	}
	if err := s.commands.SetModelDisabled(req.Provider, req.Model, *req.Disabled); err != nil {
		writePortErr(w, http.StatusBadRequest, err)
		return
	}
	status := "enabled"
	if *req.Disabled {
		status = "disabled"
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": req.Provider, "model": req.Model, "disabled": *req.Disabled, "status": status})
}

func (s *Server) handleQuotaRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider,omitempty"`
	}
	// A malformed body must not silently degrade into a full-network refresh;
	// an empty body (io.EOF) is the documented "refresh all" form.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeJSONErr(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return
	}
	if req.Provider != "" {
		if !s.commands.RefreshQuota(req.Provider) {
			writeJSONErr(w, http.StatusNotFound, "unknown provider: "+req.Provider)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "refreshed", "provider": req.Provider})
		return
	}
	s.commands.RefreshQuota("")
	writeJSON(w, http.StatusOK, map[string]string{"status": "refreshed"})
}

func (s *Server) handleHealthReset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeJSONErr(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return
	}
	cleared, locks, err := s.commands.ResetHealth(req.Provider)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "state cleared in memory but persist failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cleared": cleared, "model_locks_cleared": locks})
}

func (s *Server) handleHealthFreeze(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeJSONErr(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return
	}
	// Unlike /api/health/reset (empty = all), freeze always requires an
	// explicit target — a freeze-all footgun has no unfreeze-all urgency
	// justification.
	if req.Provider == "" {
		writeJSONErr(w, http.StatusBadRequest, "provider is required")
		return
	}
	frozen, err := s.commands.FreezeHealth(req.Provider)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "state frozen in memory but persist failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"frozen": frozen})
}

func (s *Server) handlePinSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Route      string `json:"route"`
		Provider   string `json:"provider"`
		TTLSeconds int64  `json:"ttl_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "parse pin body: "+err.Error())
		return
	}
	if req.Route == "" || req.Provider == "" {
		writeJSONErr(w, http.StatusBadRequest, "route and provider are required")
		return
	}
	ttl := time.Duration(0)
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	pin, ok := s.commands.SetPin(req.Route, req.Provider, ttl)
	if !ok {
		writeJSONErr(w, http.StatusBadRequest, "cannot pin \""+req.Route+"\" to \""+req.Provider+"\": no such route, or the route has no target for that provider")
		return
	}
	expires := ""
	if !pin.ExpiresAt.IsZero() {
		expires = pin.ExpiresAt.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, map[string]any{"route": req.Route, "provider": req.Provider, "expires_at": expires, "status": "pinned"})
}

func (s *Server) handlePinClear(w http.ResponseWriter, r *http.Request) {
	route := r.URL.Query().Get("route")
	if route == "" {
		writeJSONErr(w, http.StatusBadRequest, "route query param is required")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"route": route, "removed": s.commands.ClearPin(route)})
}

func (s *Server) handleConfigPut(w http.ResponseWriter, r *http.Request) {
	var req struct {
		YAML string `json:"yaml"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if err := s.commands.SaveConfig([]byte(req.YAML)); err != nil {
		writePortErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reloaded"})
}

// handleConfigValidate lints a candidate config without persisting it, so the
// Raw YAML editor can show errors as you type. Issues carry a best-effort
// 1-based line (0 = not locatable); ok=true means the YAML would pass
// SaveConfig's validation gate.
func (s *Server) handleConfigValidate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		YAML string `json:"yaml"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	issues := s.commands.ValidateConfig([]byte(req.YAML))
	if issues == nil {
		issues = []appapi.ValidationIssue{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": len(issues) == 0, "errors": issues})
}

// configEditKinds are the structured edit kinds POST /api/config/edit accepts.
// provider/route are name-scoped; the rest mutate a fixed scalar block.
var configEditKinds = map[string]bool{
	"general": true, "scheduling": true, "request_log": true,
	"stats": true, "cache": true, "guard": true, "provider": true, "route": true,
}

func (s *Server) handleConfigEdit(w http.ResponseWriter, r *http.Request) {
	var req appapi.EditRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !configEditKinds[req.Kind] {
		writeJSONErr(w, http.StatusBadRequest, "unknown edit kind: "+req.Kind)
		return
	}
	if err := s.commands.EditConfig(req); err != nil {
		writePortErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reloaded"})
}

func (s *Server) handleAccountAdd(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	if name == "" || strings.Contains(name, "/") {
		writeJSONErr(w, http.StatusBadRequest, "expected /api/accounts/<provider>")
		return
	}
	var in appapi.AccountInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONErr(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.commands.AddAccount(r.Context(), name, in)
	if err != nil {
		writePortErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": result.ID, "status": "added", "warning": result.Warning})
}

func (s *Server) handleAccountTest(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/accounts/"), "/test")
	parts := strings.SplitN(strings.Trim(rest, "/"), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeJSONErr(w, http.StatusBadRequest, "expected /api/accounts/<provider>/<id>/test")
		return
	}
	v, err := s.commands.ProbeAccount(r.Context(), parts[0], parts[1])
	if err != nil {
		writePortErr(w, http.StatusNotFound, err)
		return
	}
	out := map[string]any{"http_status": v.HTTPStatus, "latency_ms": v.Latency.Milliseconds(), "provider": v.Provider, "account_id": v.AccountID, "model": v.Model}
	if v.OK {
		out["status"] = "ok"
	} else {
		out["status"] = "failed"
		out["reason"] = v.Reason
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAccountRemove(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeJSONErr(w, http.StatusBadRequest, "expected /api/accounts/<provider>/<id>")
		return
	}
	result, err := s.commands.RemoveAccount(parts[0], parts[1])
	if err != nil {
		writePortErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed", "warning": result.Warning})
}

func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/login/"), "/start")
	if name == "" || strings.Contains(name, "/") {
		writeJSONErr(w, http.StatusBadRequest, "expected /api/login/<provider>/start")
		return
	}
	start, err := s.commands.BeginLogin(r.Context(), name)
	if err != nil {
		writePortErr(w, http.StatusBadGateway, err)
		return
	}
	if start.Job == nil {
		writeJSONErr(w, http.StatusInternalServerError, "login flow returned no job")
		return
	}
	detail := start.LoginURL
	if detail == "" {
		detail = start.VerifyURL + "  code: " + start.UserCode
	}
	id := s.sessions.Create(start.Provider, detail)
	if !s.tasks.Run(func(ctx context.Context) { s.sessions.Update(id, start.Job.Run(ctx)) }) {
		err := "server is shutting down"
		s.sessions.Update(id, appapi.LoginUpdate{State: "error", Detail: err})
		writeJSONErr(w, http.StatusServiceUnavailable, err)
		return
	}
	out := map[string]string{"session_id": id}
	if start.LoginURL != "" {
		out["login_url"] = start.LoginURL
	} else {
		out["verify_url"] = start.VerifyURL
		out["user_code"] = start.UserCode
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/login/"), "/poll")
	v, ok := s.sessions.Snapshot(id)
	if !ok {
		writeJSONErr(w, http.StatusNotFound, "unknown or expired session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": v.State, "detail": v.Detail, "result": v.Result, "warning": v.Warning})
}

// handlePresetsList serves GET /api/presets: the shared preset catalog for
// the Add-Provider wizard.
func (s *Server) handlePresetsList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"presets": s.reads.Presets()})
}

// handlePresetAdd serves POST /api/presets/<name>: merge the template block,
// hot-reload, and return the ambiguity warnings for the UI to surface. A
// failed reload rides in a separate reload_warning field — warnings stays
// model-name-only. The credential step happens through the existing
// account/login endpoints.
func (s *Server) handlePresetAdd(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/presets/")
	if name == "" || strings.Contains(name, "/") {
		writeJSONErr(w, http.StatusBadRequest, "expected /api/presets/<name>")
		return
	}
	warnings, reloadWarning, err := s.commands.AddPreset(name)
	if err != nil {
		writePortErr(w, http.StatusBadRequest, err)
		return
	}
	resp := map[string]any{"status": "added", "warnings": warnings}
	if reloadWarning != "" {
		resp["reload_warning"] = reloadWarning
	}
	writeJSON(w, http.StatusOK, resp)
}
