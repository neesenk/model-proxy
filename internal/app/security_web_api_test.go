package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"model-proxy/internal/appapi"
	"model-proxy/internal/observe/seclog"
)

// writeSecLogFile seeds one audit-log file (plus any raw extra lines) in dir.
func writeSecLogFile(t *testing.T, dir string, extraLines []string, records ...seclog.Record) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(dir, "security-20260101-000000.log"))
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	for _, line := range extraLines {
		if _, err := file.WriteString(line + "\n"); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func serveSecurity(t *testing.T, w *WebServer, path string) (int, appapi.SecurityResult) {
	t.Helper()
	mux := http.NewServeMux()
	w.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	var result appapi.SecurityResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("parse /api/security response %q: %v", rec.Body.String(), err)
	}
	return rec.Code, result
}

// TestAPISecurityProjection: /api/security projects seclog records into DTOs
// (newest first, unreadable lines counted in skipped) and passes the kind
// filter through to the audit query.
func TestAPISecurityProjection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeSecLogFile(t, filepath.Join(home, ".model-proxy"), []string{"not-json"},
		seclog.Record{Ts: 1700000000000, Kind: "secret", RequestID: "r1", Agent: "codex", Protocol: "anthropic", Exposed: "gpt-x", Names: []string{"aws-access-key"}, Action: "blocked"},
		seclog.Record{Ts: 1700000001000, Kind: "path", Agent: "pi", Exposed: "gpt-x", Names: []string{"home-outside-root"}, Action: "warn", Detail: "outside allowed roots"},
		seclog.Record{Ts: 1700000002000, Kind: "drift", Agent: "codex", Exposed: "gpt-x", Names: []string{"credential-shape"}, Action: "warn"},
	)
	w, _ := newTestWeb(t)

	code, all := serveSecurity(t, w, "/api/security")
	if code != http.StatusOK || !all.Enabled || all.Skipped != 1 || len(all.Records) != 3 {
		t.Fatalf("all = (%d, %#v)", code, all)
	}
	// Newest first, and every DTO field projects from the record.
	first := all.Records[0]
	if first.Kind != "drift" || first.Ts != 1700000002000 {
		t.Fatalf("newest-first order broken: %#v", all.Records)
	}
	secret := all.Records[2]
	wantSecret := appapi.SecurityRecord{Ts: 1700000000000, Kind: "secret", RequestID: "r1", Agent: "codex", Protocol: "anthropic", Exposed: "gpt-x", Names: []string{"aws-access-key"}, Action: "blocked"}
	if !reflect.DeepEqual(secret, wantSecret) {
		t.Fatalf("secret DTO = %#v, want %#v", secret, wantSecret)
	}
	if all.Records[1].Detail != "outside allowed roots" {
		t.Fatalf("path DTO detail = %#v", all.Records[1])
	}

	code, filtered := serveSecurity(t, w, "/api/security?kind=secret")
	if code != http.StatusOK || len(filtered.Records) != 1 || filtered.Records[0].Kind != "secret" {
		t.Fatalf("kind filter = (%d, %#v)", code, filtered)
	}

	code, limited := serveSecurity(t, w, "/api/security?limit=2")
	if code != http.StatusOK || len(limited.Records) != 2 || limited.Records[0].Kind != "drift" || limited.Records[1].Kind != "path" {
		t.Fatalf("limit = (%d, %#v)", code, limited)
	}
}

// TestAPISecurityDisabledOrMissing: guard.audit off — or the audit directory
// simply absent — yields the request-log-style disabled response instead of
// an error.
func TestAPISecurityDisabledOrMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg, _ := LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
guard: {audit: false}
`))
	w := NewWebServer(newTestProxy(t, cfg), "test-config.yaml")
	code, off := serveSecurity(t, w, "/api/security")
	if code != http.StatusOK || off.Enabled || off.Records == nil || len(off.Records) != 0 || off.Skipped != 0 {
		t.Fatalf("audit off = (%d, %#v)", code, off)
	}

	// Audit on (default) but the directory was never created.
	w2, _ := newTestWeb(t)
	code, missing := serveSecurity(t, w2, "/api/security")
	if code != http.StatusOK || missing.Enabled || missing.Records == nil || len(missing.Records) != 0 {
		t.Fatalf("missing dir = (%d, %#v)", code, missing)
	}
}
