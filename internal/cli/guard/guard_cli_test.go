package guard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	configdomain "model-proxy/internal/config"
)

func testConfig(listen string) *configdomain.Config {
	cfg := &configdomain.Config{}
	cfg.Listen = listen
	return cfg
}

func TestCmdGuard_BlocksListAndUnblock(t *testing.T) {
	var mu sync.Mutex
	blocks := []map[string]any{{
		"session_id": "s-1", "kind": "secret", "rule": "jwt",
		"reason": "live token material", "request_id": "r9", "ts": 1789000000000,
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("content-type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/security/blocks":
			json.NewEncoder(w).Encode(map[string]any{"blocks": blocks})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/security/blocks/s-1":
			blocks = nil
			json.NewEncoder(w).Encode(map[string]string{"status": "unblocked"})
		default:
			http.Error(w, "no route", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	listen := strings.TrimPrefix(srv.URL, "http://")

	var out, errb bytes.Buffer
	if code := CmdGuard([]string{"blocks"}, testConfig(listen), &out, &errb); code != 0 {
		t.Fatalf("blocks exit = %d, stderr = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "s-1") || !strings.Contains(out.String(), "jwt") || !strings.Contains(out.String(), "guard unblock s-1") {
		t.Errorf("blocks output = %q", out.String())
	}

	out.Reset()
	errb.Reset()
	if code := CmdGuard([]string{"unblock", "s-1"}, testConfig(listen), &out, &errb); code != 0 {
		t.Fatalf("unblock exit = %d, stderr = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "unblocked session s-1") {
		t.Errorf("unblock output = %q", out.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if blocks != nil {
		t.Error("daemon-side table not cleared")
	}
}

func TestCmdGuard_BlocksEmptyAndJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"blocks": []map[string]any{}})
	}))
	defer srv.Close()
	listen := strings.TrimPrefix(srv.URL, "http://")

	var out, errb bytes.Buffer
	if code := CmdGuard([]string{"blocks"}, testConfig(listen), &out, &errb); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out.String(), "no adjudicated-blocked sessions") {
		t.Errorf("empty output = %q", out.String())
	}
	out.Reset()
	if code := CmdGuard([]string{"blocks", "--json"}, testConfig(listen), &out, &errb); code != 0 {
		t.Fatalf("json exit = %d", code)
	}
	if strings.TrimSpace(out.String()) != "[]" {
		t.Errorf("json output = %q, want []", out.String())
	}
}

func TestCmdGuard_UsageErrors(t *testing.T) {
	cfg := testConfig("127.0.0.1:1")
	var out, errb bytes.Buffer
	if code := CmdGuard([]string{}, cfg, &out, &errb); code != 1 || !strings.Contains(errb.String(), "usage") {
		t.Errorf("no-args exit = %d stderr = %q", code, errb.String())
	}
	errb.Reset()
	if code := CmdGuard([]string{"frobnicate"}, cfg, &out, &errb); code != 1 || !strings.Contains(errb.String(), "unknown guard subcommand") {
		t.Errorf("bad sub exit = %d stderr = %q", code, errb.String())
	}
	errb.Reset()
	if code := CmdGuard([]string{"unblock"}, cfg, &out, &errb); code != 1 || !strings.Contains(errb.String(), "usage") {
		t.Errorf("unblock-no-id exit = %d stderr = %q", code, errb.String())
	}
}

func TestCmdGuard_AllowedListJsonAndEmpty(t *testing.T) {
	allowed := []map[string]any{{
		"hash": "h-abc123", "kind": "secret", "rule": "jwt",
		"reason": "verdict medium — operator released", "source": "session-unblock", "ts": 1789000000000,
	}}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path != "/api/security/allowed" {
			http.Error(w, "no route", http.StatusNotFound)
			return
		}
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"allowed": allowed})
	}))
	defer srv.Close()
	base := strings.TrimPrefix(srv.URL, "http://")

	var out, errb bytes.Buffer
	if code := CmdGuard([]string{"allowed"}, testConfig(base), &out, &errb); code != 0 {
		t.Fatalf("allowed exit = %d, stderr = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "h-abc123") || !strings.Contains(out.String(), "jwt") ||
		!strings.Contains(out.String(), "guard disallow h-abc123") {
		t.Errorf("allowed output = %q", out.String())
	}

	// --json: the raw array, one entry per line of indented JSON.
	out.Reset()
	errb.Reset()
	if code := CmdGuard([]string{"allowed", "--json"}, testConfig(base), &out, &errb); code != 0 {
		t.Fatalf("allowed --json exit = %d, stderr = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), `"hash": "h-abc123"`) {
		t.Errorf("allowed --json output = %q", out.String())
	}

	// Empty table renders the dim empty note.
	mu.Lock()
	allowed = nil
	mu.Unlock()
	out.Reset()
	errb.Reset()
	if code := CmdGuard([]string{"allowed"}, testConfig(base), &out, &errb); code != 0 {
		t.Fatalf("allowed-empty exit = %d, stderr = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "no operator content overrides") {
		t.Errorf("allowed-empty output = %q", out.String())
	}
}

func TestCmdGuard_DisallowSuccessAndNotFound(t *testing.T) {
	revoked := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/api/security/allowed/h-abc123" {
			revoked = r.URL.Path
			json.NewEncoder(w).Encode(map[string]string{"status": "revoked"})
			return
		}
		http.Error(w, "unknown hash", http.StatusNotFound)
	}))
	defer srv.Close()
	base := strings.TrimPrefix(srv.URL, "http://")

	var out, errb bytes.Buffer
	if code := CmdGuard([]string{"disallow", "h-abc123"}, testConfig(base), &out, &errb); code != 0 {
		t.Fatalf("disallow exit = %d, stderr = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "revoked content override h-abc123") {
		t.Errorf("disallow output = %q", out.String())
	}
	if revoked == "" {
		t.Error("disallow never reached the DELETE endpoint")
	}

	// Unknown hash: 404 surfaces on stderr, exit 1.
	out.Reset()
	errb.Reset()
	if code := CmdGuard([]string{"disallow", "h-missing"}, testConfig(base), &out, &errb); code != 1 ||
		!strings.Contains(errb.String(), "unknown hash") {
		t.Errorf("disallow-404 exit = %d stderr = %q", code, errb.String())
	}
}
