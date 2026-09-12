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
