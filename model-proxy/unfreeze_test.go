package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestResetHealth: the proxy method clears circuit/rate-limit state + model
// locks for the named provider (pooled parent = all its virtual accounts), or
// everything when empty — and never touches param blocklists, sticky, or pins.
func TestResetHealth(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{
		"a": {OpenAIBaseURL: "http://x", Provider: "static"},
		"b": {OpenAIBaseURL: "http://y", Provider: "static"},
	}}
	p := NewProxy(cfg)
	p.mu.Lock()
	p.parentOf = map[string]string{"a#v1": "a", "a#v2": "a"}
	p.mu.Unlock()
	now := time.Now()
	p.recordRateLimit("a#v1", now.Add(time.Hour), rlQuota)
	p.recordRateLimit("a#v2", now.Add(time.Hour), rlTransient)
	p.recordRateLimit("b", now.Add(time.Hour), rlDaily)
	p.recordModelFailure("a#v1", "m1", Scheduling{ModelLockout: "1h"})
	p.recordModelFailure("b", "m2", Scheduling{ModelLockout: "1h"})
	p.learnParamBlock("a#v1", "m1", "max_tokens")
	p.healthMu.Lock()
	p.sticky["route1"] = routeSticky{provider: "a#v1", since: now}
	p.pins["route2"] = pinEntry{provider: "a#v1"}
	p.healthMu.Unlock()

	cleared, locks := p.resetHealth("a")
	if len(cleared) != 2 || cleared[0] != "a#v1" || cleared[1] != "a#v2" {
		t.Errorf("cleared = %v, want [a#v1 a#v2] (pooled parent matches virtuals)", cleared)
	}
	if locks != 1 {
		t.Errorf("locks = %d, want 1 (only (a#v1,m1))", locks)
	}
	p.healthMu.Lock()
	_, bFrozen := p.health["b"]
	_, bLock := p.modelLocks[modelLockKey{provider: "b", model: "m2"}]
	blocked := p.paramBlock[modelLockKey{provider: "a#v1", model: "m1"}]["max_tokens"]
	_, stickyOK := p.sticky["route1"]
	_, pinOK := p.pins["route2"]
	p.healthMu.Unlock()
	if !bFrozen || !bLock {
		t.Error("provider b state must survive resetHealth(\"a\")")
	}
	if !blocked {
		t.Error("param blocklist must NOT be cleared by resetHealth")
	}
	if !stickyOK || !pinOK {
		t.Error("sticky/pins must NOT be cleared by resetHealth")
	}

	cleared, locks = p.resetHealth("")
	if len(cleared) != 1 || cleared[0] != "b" || locks != 1 {
		t.Errorf("reset all: cleared = %v locks = %d, want [b], 1", cleared, locks)
	}
}

// TestHealthResetAPI: POST /api/health/reset — empty body resets all,
// {"provider":name} resets one; response carries cleared names + lock count.
func TestHealthResetAPI(t *testing.T) {
	w, p := newTestWeb(t)
	mux := http.NewServeMux()
	w.register(mux)
	p.recordRateLimit("zhipu", time.Now().Add(time.Hour), rlQuota)
	p.recordModelFailure("zhipu", "glm-x", Scheduling{ModelLockout: "1h"})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/health/reset", strings.NewReader(`{"provider":"zhipu"}`)))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Cleared           []string `json:"cleared"`
		ModelLocksCleared int      `json:"model_locks_cleared"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Cleared) != 1 || out.Cleared[0] != "zhipu" || out.ModelLocksCleared != 1 {
		t.Errorf("response = %+v, want cleared [zhipu] + 1 lock", out)
	}
	if _, frozen := p.health["zhipu"]; frozen {
		t.Error("zhipu still frozen after API reset")
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/health/reset", nil))
	if rec.Code != 200 {
		t.Fatalf("empty-body reset: status = %d, want 200", rec.Code)
	}
}

// TestHealthResetAPI_MalformedJSON: a malformed body must NOT be treated as
// "empty = reset all" — it returns 400 and leaves the state untouched.
func TestHealthResetAPI_MalformedJSON(t *testing.T) {
	w, p := newTestWeb(t)
	mux := http.NewServeMux()
	w.register(mux)
	p.recordRateLimit("zhipu", time.Now().Add(time.Hour), rlQuota)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/health/reset", strings.NewReader(`{"provider":`)))
	if rec.Code != 400 {
		t.Fatalf("malformed body: status = %d, want 400", rec.Code)
	}
	if _, frozen := p.health["zhipu"]; !frozen {
		t.Error("malformed body must leave frozen state untouched")
	}
}

// TestDoUnfreeze: the CLI do* function renders the daemon's response (and the
// nothing-frozen case).
func TestDoUnfreeze(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health/reset" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Provider string `json:"provider"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Provider == "empty" {
			writeJSON(w, http.StatusOK, map[string]any{"cleared": []string{}, "model_locks_cleared": 0})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"cleared": []string{"zhipu"}, "model_locks_cleared": 2})
	}))
	defer srv.Close()

	out, err := doUnfreeze(srv.URL, "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "zhipu") || !strings.Contains(out, "2 model lock") {
		t.Errorf("out = %q, want unfroze line with provider + lock count", out)
	}
	out, err = doUnfreeze(srv.URL, "empty")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no frozen state") {
		t.Errorf("out = %q, want the nothing-frozen line", out)
	}
}

// TestHealthResetAPI_PersistsClearedState (P1-3b): after a successful reset,
// the on-disk state file no longer carries the frozen entry — a restart right
// after must NOT resurrect it.
func TestHealthResetAPI_PersistsClearedState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	w, p := newTestWeb(t)
	mux := http.NewServeMux()
	w.register(mux)
	p.recordRateLimit("zhipu", time.Now().Add(time.Hour), rlQuota)
	if err := p.quota.persist(); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(os.Getenv("HOME"), ".model-proxy", "quota_state.json")
	if data, _ := os.ReadFile(statePath); !strings.Contains(string(data), "zhipu") {
		t.Fatalf("precondition: frozen entry should be on disk: %s", data)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/health/reset", strings.NewReader(`{"provider":"zhipu"}`)))
	if rec.Code != 200 {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	// Check the HEALTH section specifically — the providers section may
	// legitimately hold a (BillingUnknown) quota snapshot for the same name.
	var wrap struct {
		Health map[string]json.RawMessage `json:"health"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		t.Fatal(err)
	}
	if _, frozen := wrap.Health["zhipu"]; frozen {
		t.Errorf("frozen entry still on disk after reset: %s", data)
	}
}
