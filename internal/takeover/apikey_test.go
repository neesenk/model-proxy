package takeover_test

// apikey_test.go — the S2 boundary of takeover: when the daemon listens
// non-loopback (web.auth.api_keys_file configured), client configs must carry
// the REAL first key instead of the PROXY_MANAGED sentinel (the serving gate
// rejects the public sentinel), and the preview DISPLAY surface must mask it
// back to the sentinel so the secret never reaches the confirmation dialog.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/takeover"
)

func writeForwardKeys(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api_keys.txt")
	if err := os.WriteFile(path, []byte("# rotation queue\nmp-forward-key-1\nmp-forward-key-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTakeoverEmbedsRealForwardKey: the hermes preset ({{token}} in its
// provider block) writes the real first key with api_keys_file configured,
// and keeps the PROXY_MANAGED sentinel without it (loopback deployments).
func TestTakeoverEmbedsRealForwardKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	run := func(apiKeysFile string) string {
		target := filepath.Join(home, ".hermes", "config.yaml")
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(hermesSeed), 0o600); err != nil {
			t.Fatal(err)
		}
		tmpl, err := takeover.TemplateByName("hermes-openai", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		cfg, routes := hermesFixture()
		cfg.Web.Auth.APIKeysFile = apiKeysFile
		if err := tmpl.Rewrite(cfg, nil, routes); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	withKeys := run(writeForwardKeys(t))
	if !strings.Contains(withKeys, "api_key: mp-forward-key-1") {
		t.Errorf("rewrite must embed the real first key:\n%s", withKeys)
	}
	if strings.Contains(withKeys, "PROXY_MANAGED") {
		t.Errorf("authenticated deployment must not leave the public sentinel in the client config:\n%s", withKeys)
	}

	without := run("")
	if !strings.Contains(without, "api_key: PROXY_MANAGED") {
		t.Errorf("loopback deployment keeps the sentinel:\n%s", without)
	}
}

// TestPreviewWritesMasksForwardKey: the preview renders the exact bytes a run
// would write — including the real key — but its DISPLAY content masks every
// configured key back to the sentinel and notes that the write carries the
// real one.
func TestPreviewWritesMasksForwardKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	modelsJSON := filepath.Join(home, ".pi", "agent", "models.json")
	if err := os.MkdirAll(filepath.Dir(modelsJSON), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modelsJSON, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &configdomain.Config{
		Listen: "100.81.1.4:15721",
		Providers: map[string]configdomain.Provider{
			"zhipu": {OpenAIBaseURL: "https://z/v1", Models: []string{"glm-5.3"}},
		},
		Web: configdomain.WebConfig{Auth: configdomain.WebAuthConfig{APIKeysFile: writeForwardKeys(t)}},
	}
	facts := takeover.ModelFactsFor(cfg, "pi", home, "", takeover.ModeUnified, nil)

	writes, err := takeover.PreviewWrites(cfg, "pi", "", takeover.ModeUnified, facts, false, takeover.ScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	var main *takeover.PreviewWrite
	for i := range writes {
		if writes[i].File == modelsJSON {
			main = &writes[i]
		}
	}
	if main == nil {
		t.Fatalf("preview must cover models.json: %+v", writes)
	}
	for _, key := range []string{"mp-forward-key-1", "mp-forward-key-2"} {
		if strings.Contains(main.Content, key) {
			t.Errorf("preview leaks forward key %q:\n%s", key[:len(key)-4]+"…", main.Content)
		}
	}
	if !strings.Contains(main.Content, "PROXY_MANAGED") {
		t.Errorf("preview must show the masked sentinel where the real key is written:\n%s", main.Content)
	}
	noteFound := false
	for _, n := range main.Notes {
		if strings.Contains(n, "masked in preview") {
			noteFound = true
		}
	}
	if !noteFound {
		t.Errorf("preview must note that the write carries the real key, notes = %v", main.Notes)
	}
}
