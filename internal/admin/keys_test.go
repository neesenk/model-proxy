package admin

// keys_test.go — the forward-key management service: list/create/revoke over
// the real key file, usage enrichment from a real request-log index, and the
// auth-off empty shape. The full token appears only in the create result.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"model-proxy/internal/appapi"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/webauth"
)

func keysService(t *testing.T, apiKeysPath string) *Service {
	t.Helper()
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:15722",
		Web:    configdomain.WebConfig{Auth: configdomain.WebAuthConfig{APIKeysFile: apiKeysPath}},
	}
	return New(Ports{Config: func() *configdomain.Config { return cfg }})
}

func TestAPIKeysListCreateRevoke(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api_keys")
	// Seed one labeled + one bare key by hand (hand edits must survive).
	if err := os.WriteFile(path, []byte("# @label seed\nseed-key-0\nbare-key-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := keysService(t, path)

	// List: id/prefix/label/created, no tokens.
	res, err := s.APIKeys()
	if err != nil {
		t.Fatal(err)
	}
	if !res.Enabled || len(res.Keys) != 2 {
		t.Fatalf("list = %+v", res)
	}
	if res.Keys[0].Label != "seed" || res.Keys[0].ID != webauth.KeyID("seed-key-0") {
		t.Errorf("seed entry = %+v", res.Keys[0])
	}
	if res.Keys[1].Label != "" {
		t.Errorf("bare entry = %+v", res.Keys[1])
	}

	// Create: full token returned exactly once, file gains labeled entry, the
	// GATE accepts the new key immediately (same file).
	created, err := s.CreateAPIKey("phone")
	if err != nil {
		t.Fatal(err)
	}
	if created.Label != "phone" || created.Token == "" || !strings.HasPrefix(created.Token, "mp-key-") {
		t.Fatalf("created = %+v", created)
	}
	if created.ID != webauth.KeyID(created.Token) {
		t.Errorf("created id = %q", created.ID)
	}
	entries := webauth.ReadKeyEntries(path)
	if len(entries) != 3 || entries[2].Label != "phone" || entries[2].Token != created.Token || entries[2].Created == "" {
		t.Fatalf("file after create = %+v", entries)
	}
	if entries[0].Label != "seed" || entries[1].Token != "bare-key-1" {
		t.Errorf("hand edits drifted: %+v", entries)
	}
	if !webauth.NewSource(path).Accept(created.Token) {
		t.Error("gate does not accept the newly created key")
	}

	// The listing never carries the token.
	again, _ := s.APIKeys()
	for _, k := range again.Keys {
		if strings.Contains(k.Prefix, created.Token) {
			t.Errorf("listing leaked the full token: %+v", k)
		}
	}

	// Revoke by id: only that line disappears.
	if err := s.RevokeAPIKey(webauth.KeyID("bare-key-1")); err != nil {
		t.Fatal(err)
	}
	entries = webauth.ReadKeyEntries(path)
	if len(entries) != 2 {
		t.Fatalf("file after revoke = %+v", entries)
	}
	if entries[1].Token != created.Token {
		t.Errorf("wrong entry removed: %+v", entries)
	}
	// Unknown id is a 404-shaped error, not a silent success.
	err = s.RevokeAPIKey("deadbeef")
	var httpErr *appapi.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusNotFound {
		t.Errorf("unknown id error = %v, want 404", err)
	}
}

// TestAPIKeysAuthOffShape: no api_keys_file configured (loopback deployment)
// answers Enabled=false — the page's empty state.
func TestAPIKeysAuthOffShape(t *testing.T) {
	s := keysService(t, "")
	res, err := s.APIKeys()
	if err != nil {
		t.Fatal(err)
	}
	if res.Enabled || len(res.Keys) != 0 {
		t.Errorf("auth-off shape = %+v", res)
	}
	_, err = s.CreateAPIKey("x")
	var httpErr *appapi.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusConflict {
		t.Errorf("create without api_keys_file = %v, want 409", err)
	}
}

// TestAPIKeysUsageEnrichment: last-use and the 24h call count ride the
// listing from the REAL request-log index (the same port production wires);
// APIKeyUsage narrows to the key and honors the window.
func TestAPIKeysUsageEnrichment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api_keys")
	if err := os.WriteFile(path, []byte("k-one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	idOne := webauth.KeyID("k-one")
	now := time.Now().UTC()
	recent := now.Add(-time.Hour).Format(time.RFC3339)
	old := now.Add(-72 * time.Hour).Format(time.RFC3339)
	if err := os.WriteFile(filepath.Join(dir, "requests-20260101.log"), []byte(
		`{"ts":"`+recent+`","request_id":"r1","called_model":"m","exposed":"m","provider":"p","api_key":"`+idOne+`","status":200,"response_body":"{\"usage\":{\"input_tokens\":3,\"output_tokens\":4}}"}`+"\n"+
			`{"ts":"`+recent+`","request_id":"r2","called_model":"m","exposed":"m","provider":"p","api_key":"`+idOne+`","status":500,"response_body":"{}"}`+"\n"+
			`{"ts":"`+old+`","request_id":"r3","called_model":"m","exposed":"m","provider":"p","api_key":"`+idOne+`","status":200,"response_body":"{}"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	index, err := requestlog.NewIndexer(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { index.Shutdown() })
	go index.Run()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := index.WaitReconciled(ctx); err != nil {
		t.Fatal(err)
	}

	cfg := &configdomain.Config{
		Listen: "127.0.0.1:15722",
		Web:    configdomain.WebConfig{Auth: configdomain.WebAuthConfig{APIKeysFile: path}},
	}
	s := New(Ports{
		Config:              func() *configdomain.Config { return cfg },
		RequestLogDirectory: func() string { return dir },
		RequestLogIndex:     func() *requestlog.Indexer { return index },
	})

	res, err := s.APIKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Keys) != 1 {
		t.Fatalf("keys = %+v", res.Keys)
	}
	// 2 of the 3 records are inside 24h; last use is the recent timestamp.
	if res.Keys[0].Requests24h != 2 {
		t.Errorf("requests_24h = %d, want 2 (the 72h-old record excluded)", res.Keys[0].Requests24h)
	}
	if res.Keys[0].LastUsedMs == 0 {
		t.Errorf("last_used_ms missing: %+v", res.Keys[0])
	}

	// Usage view over the last 24h: 2 calls, 1 failure, tokens from r1.
	rows, err := s.APIKeyUsage(idOne, now.Add(-24*time.Hour), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Requests != 2 || rows[0].Failures != 1 || rows[0].Input != 3 || rows[0].Output != 4 {
		t.Errorf("usage rows = %+v", rows)
	}

	// Full window sees all three.
	all, err := s.APIKeyUsage(idOne, now.Add(-96*time.Hour), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Requests != 3 {
		t.Errorf("full-window rows = %+v", all)
	}
}
