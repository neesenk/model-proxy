package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression: OAuth/SSO credential stores are rewritten with ROTATED tokens —
// a crash during a direct write truncated the file while the old refresh token
// was already invalidated, locking the account. All writes must land via
// rename: complete old or complete new content, 0600, no temp leftovers.
func TestAtomicCredentialWrites(t *testing.T) {
	dir := t.TempDir()

	aqpPath := filepath.Join(dir, "google_oauth_auth.json")
	first := &AqpAccountData{Email: "u@example.com"}
	if err := SaveAqpAccount(aqpPath, first); err != nil {
		t.Fatal(err)
	}
	rotated := &AqpAccountData{Email: "u@example.com", SSOSessionCookie: "SSO_C=new-cookie"}
	if err := SaveAqpAccount(aqpPath, rotated); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(aqpPath)
	if err != nil {
		t.Fatal(err)
	}
	var got AqpAccountData
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("aqp file not valid JSON: %v", err)
	}
	if got.SSOSessionCookie != "SSO_C=new-cookie" {
		t.Fatalf("aqp rotation lost: %+v", got)
	}
	if info, _ := os.Stat(aqpPath); info.Mode().Perm() != 0o600 {
		t.Errorf("aqp file perm = %v, want 0600", info.Mode().Perm())
	}

	codexPath := filepath.Join(dir, "codex_auth.json")
	auth := &CodexAuthFile{AuthMode: "chatgpt"}
	auth.Tokens.AccountID = "acct"
	if err := WriteCodexAuthFile(codexPath, auth); err != nil {
		t.Fatal(err)
	}
	auth.Tokens.RefreshToken = "rotated-refresh"
	if err := WriteCodexAuthFile(codexPath, auth); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	var codex CodexAuthFile
	if err := json.Unmarshal(data, &codex); err != nil {
		t.Fatalf("codex file not valid JSON: %v", err)
	}
	if codex.Tokens.RefreshToken != "rotated-refresh" || codex.Tokens.AccountID != "acct" {
		t.Fatalf("codex rotation lost: %+v", codex.Tokens)
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("temp leftover: %s", e.Name())
		}
	}
}

// Regression: the parse error used to hardcode "google_oauth_auth.json" even
// when the file being parsed was <name>_oauth_auth.json. The error must name
// the actual store path.
func TestLoadAqpAccountParseErrorNamesRealPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zhipu-work_oauth_auth.json")
	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadAqpAccount(path)
	if err == nil {
		t.Fatal("LoadAqpAccount(invalid JSON): want error, got nil")
	}
	if !strings.Contains(err.Error(), "zhipu-work_oauth_auth.json") {
		t.Errorf("parse error %q missing real path %q", err.Error(), path)
	}
	if strings.Contains(err.Error(), "google_oauth_auth.json") {
		t.Errorf("parse error %q names the legacy default, not the parsed file", err.Error())
	}
}
