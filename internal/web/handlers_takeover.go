package web

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// handlers_takeover.go — the /api/takeover subtree: client takeover surface
// (read), takeover/restore execution, and takeover template management. All
// semantics live in internal/admin + internal/takeover; these handlers only
// decode, classify errors, and encode.

// handleTakeover serves GET /api/takeover: templates with per-client
// install/takeover/drift state.
func (s *Server) handleTakeover(w http.ResponseWriter, r *http.Request) {
	surface, err := s.reads.TakeoverSurface(r.URL.Query().Get("mode"))
	if err != nil {
		writePortErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, surface)
}

// handleTakeoverRun serves POST /api/takeover {client?, mode?} — the daemon
// twin of `model-proxy takeover [client] [--mode]`. client ""/"all" is the
// batch form; mode "" defaults to unified (no interactive prompt on the Web).
func (s *Server) handleTakeoverRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Client string `json:"client"`
		Mode   string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeJSONErr(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return
	}
	result, err := s.commands.RunTakeover(req.Client, req.Mode)
	if err != nil {
		writePortErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleTakeoverRestore serves POST /api/takeover/restore {client?} — the
// daemon twin of `model-proxy restore [client]`.
func (s *Server) handleTakeoverRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Client string `json:"client"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeJSONErr(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return
	}
	result, err := s.commands.RestoreTakeover(req.Client)
	if err != nil {
		writePortErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// takeoverTemplateName extracts and sanity-checks the <name> path segment of
// /api/takeover/templates/<name> (no deeper nesting, no traversal).
func takeoverTemplateName(path string) string {
	name := strings.TrimPrefix(path, "/api/takeover/templates/")
	if name == "" || strings.Contains(name, "/") {
		return ""
	}
	return name
}

// handleTakeoverTemplateGet serves GET /api/takeover/templates/<name>: the
// raw template YAML (user override when present, else the embedded preset).
func (s *Server) handleTakeoverTemplateGet(w http.ResponseWriter, r *http.Request) {
	name := takeoverTemplateName(r.URL.Path)
	if name == "" {
		writeJSONErr(w, http.StatusBadRequest, "invalid template name")
		return
	}
	doc, err := s.reads.TakeoverTemplate(name)
	if err != nil {
		writePortErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// handleTakeoverTemplatePut serves PUT /api/takeover/templates/<name>
// {yaml}: validate-then-write a user template (same-named presets become
// overridden; new names add a client).
func (s *Server) handleTakeoverTemplatePut(w http.ResponseWriter, r *http.Request) {
	name := takeoverTemplateName(r.URL.Path)
	if name == "" {
		writeJSONErr(w, http.StatusBadRequest, "invalid template name")
		return
	}
	var req struct {
		YAML string `json:"yaml"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.YAML) == "" {
		writeJSONErr(w, http.StatusBadRequest, "yaml is required")
		return
	}
	if err := s.commands.SaveTakeoverTemplate(name, []byte(req.YAML)); err != nil {
		writePortErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved", "name": name})
}

// handleTakeoverTemplateDelete serves DELETE /api/takeover/templates/<name>:
// remove a user template (deleting the override that shadows a preset
// restores the preset; a bare preset name is rejected by the service).
func (s *Server) handleTakeoverTemplateDelete(w http.ResponseWriter, r *http.Request) {
	name := takeoverTemplateName(r.URL.Path)
	if name == "" {
		writeJSONErr(w, http.StatusBadRequest, "invalid template name")
		return
	}
	if err := s.commands.DeleteTakeoverTemplate(name); err != nil {
		writePortErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "name": name})
}
