package clitest

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"model-proxy/internal/accounts"
)

func TestGrabStdout(t *testing.T) {
	out := GrabStdout(t, func() { os.Stdout.WriteString("captured") })
	if out != "captured" {
		t.Errorf("GrabStdout = %q, want captured", out)
	}
}

func TestWriteTempConfig(t *testing.T) {
	path := WriteTempConfig(t, "listen: x\n")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "listen: x\n" {
		t.Errorf("config body = %q", data)
	}
}

func TestPoolFixtures(t *testing.T) {
	dir := t.TempDir()
	SetPoolHome(t, dir)
	if os.Getenv("HOME") != dir {
		t.Fatalf("HOME = %q, want %q", os.Getenv("HOME"), dir)
	}
	WritePoolFile(t, "zhipu", "zhipu", "K1", "K2")
	pool, err := accounts.NewStore(accounts.HomeDir()).Load("zhipu", "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 2 {
		t.Errorf("pool accounts = %d, want 2", len(pool.Accounts))
	}
}

func TestWriteZhipuPoolConfig(t *testing.T) {
	path := WriteZhipuPoolConfig(t, "https://usage.invalid/u")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "usage_url: https://usage.invalid/u") {
		t.Errorf("zhipu pool config missing usage_url:\n%s", data)
	}
}

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, 200, map[string]any{"ok": true})
	if ct := rec.Header().Get("content-type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	var body map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || !body["ok"] {
		t.Errorf("body = %q: %v", rec.Body.String(), err)
	}
}

func TestMinimalConfigParses(t *testing.T) {
	if !strings.Contains(MinimalConfig, "provider_id: aqp") {
		t.Error("MinimalConfig should describe the aqp fixture provider")
	}
}
