package presets

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	domainpresets "model-proxy/internal/presets"
)

// runCmdAdd drives CmdAdd with explicit streams and returns (exit, stdout, stderr).
func runCmdAdd(args []string) (int, string, string) {
	var outBuf, errBuf strings.Builder
	code := CmdAdd(args, os.Stdin, &outBuf, &errBuf)
	return code, outBuf.String(), errBuf.String()
}

// TestCmdAdd_EndToEnd_LocalValidationServer covers the full happy path with
// zero real network: the preset block is pre-seeded pointing at an httptest
// server that plays the usage_url validation endpoint, so RunProviderLogin's
// key validation hits localhost only. Asserts exit code 0, the credential
// pool landing in the isolated HOME, and a success line on stdout.
func TestCmdAdd_EndToEnd_LocalValidationServer(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	home := t.TempDir()
	t.Setenv("HOME", home)
	// CmdAdd runs in-process and finishes with MaybeReloadDaemon, whose
	// default pid file is <TMPDIR>/model-proxy.pid — pin TMPDIR so a
	// developer's live serve daemon never gets a real SIGHUP from this test.
	t.Setenv("TMPDIR", t.TempDir())

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := "listen: 127.0.0.1:15799\nlog_level: error\n\nproviders:\n" +
		"  zhipu:\n" +
		"    provider_id: zhipu\n" +
		"    openai_base_url: " + srv.URL + "\n" +
		"    usage_url: " + srv.URL + "/models\n" +
		"    models: [glm-local]\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADD_TEST_KEY", "sk-add-test-key")

	code, stdout, stderr := runCmdAdd([]string{
		"--config", cfgPath,
		"zhipu",
		"--api-key-env", "ADD_TEST_KEY",
	})
	if code != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	// Login's own success lines print via the process-global stdout (existing
	// login flow contract), so assert only on the add-owned next-steps block.
	if !strings.Contains(stdout, "model-proxy test") {
		t.Fatalf("stdout missing next-steps: %q", stdout)
	}
	if _, err := os.ReadFile(filepath.Join(home, ".model-proxy", "zhipu_apikeys.json")); err != nil {
		t.Fatalf("credential pool not written to isolated HOME: %v", err)
	}
	if hits.Load() == 0 {
		t.Fatal("login validation never contacted the local validation endpoint")
	}
}

// TestCmdAdd_AmbiguityRefusesWithoutTTY pins the fail-closed gate: overlapping
// models across configured providers without explicit routes refuse BEFORE any
// login side effects when stdin is not a terminal.
func TestCmdAdd_AmbiguityRefusesWithoutTTY(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	home := t.TempDir()
	t.Setenv("HOME", home)
	// CmdAdd runs in-process and finishes with MaybeReloadDaemon, whose
	// default pid file is <TMPDIR>/model-proxy.pid — pin TMPDIR so a
	// developer's live serve daemon never gets a real SIGHUP from this test.
	t.Setenv("TMPDIR", t.TempDir())

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := "listen: 127.0.0.1:15799\nlog_level: error\n\nproviders:\n" +
		"  zhipu:\n" +
		"    provider_id: zhipu\n" +
		"    openai_base_url: " + srv.URL + "\n" +
		"    models: [shared-model]\n" +
		"  deepseek:\n" +
		"    provider_id: deepseek\n" +
		"    openai_base_url: " + srv.URL + "\n" +
		"    models: [shared-model]\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADD_TEST_KEY", "sk-x")

	code, _, stderr := runCmdAdd([]string{
		"--config", cfgPath,
		"deepseek",
		"--api-key-env", "ADD_TEST_KEY",
	})
	if code != 1 {
		t.Fatalf("ambiguous add must exit 1 without TTY; got %d", code)
	}
	if !strings.Contains(stderr, "--yes") {
		t.Fatalf("refusal must point at --yes; stderr=%q", stderr)
	}
	if hits.Load() != 0 {
		t.Fatal("ambiguity refusal must happen before login contacts any endpoint")
	}
	if _, err := os.Stat(filepath.Join(home, ".model-proxy")); !os.IsNotExist(err) {
		t.Fatal("no credentials may be written when the ambiguity gate refuses")
	}
}

// TestCmdAdd_AmbiguityRefusalLeavesConfigUntouched pins the gate-before-write
// ordering: the ambiguity gate runs against the WOULD-BE-MERGED config, so a
// refusal (the non-TTY fail-closed path here) must leave config.yaml
// byte-identical — otherwise the next reload activates a credential-less
// provider block whose models join implicit routing. The added preset's
// template model is deliberately placed on an existing provider to trigger
// the gate; the preset block itself is NOT yet configured.
func TestCmdAdd_AmbiguityRefusalLeavesConfigUntouched(t *testing.T) {
	tplProv, ok := domainpresets.Lookup("deepseek")
	if !ok || len(tplProv.Models) == 0 {
		t.Fatal("deepseek preset missing from the built-in template")
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	// Keep MaybeReloadDaemon's pid file away from any live daemon (not
	// reached on the refusal path, but pin it like the other add tests).
	t.Setenv("TMPDIR", t.TempDir())

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := "listen: 127.0.0.1:15799\nlog_level: error\n\nproviders:\n" +
		"  zhipu:\n" +
		"    provider_id: zhipu\n" +
		"    openai_base_url: http://127.0.0.1:9\n" +
		"    models: [" + tplProv.Models[0] + "]\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADD_TEST_KEY", "sk-x")

	code, _, stderr := runCmdAdd([]string{
		"--config", cfgPath,
		"deepseek",
		"--api-key-env", "ADD_TEST_KEY",
	})
	if code != 1 {
		t.Fatalf("ambiguous add must exit 1 without TTY; got %d", code)
	}
	if !strings.Contains(stderr, "--yes") {
		t.Fatalf("refusal must point at --yes; stderr=%q", stderr)
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != content {
		t.Fatalf("refused add must leave config.yaml byte-identical;\nbefore:\n%s\nafter:\n%s", content, after)
	}
	if _, err := os.Stat(filepath.Join(home, ".model-proxy")); !os.IsNotExist(err) {
		t.Fatal("no credentials may be written when the ambiguity gate refuses")
	}
}

// TestCmdAdd_MissingConfigHintsInit checks the onboarding error path.
func TestCmdAdd_MissingConfigHintsInit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()

	code, _, stderr := runCmdAdd([]string{"--config", filepath.Join(dir, "absent.yaml"), "zhipu"})
	if code != 1 {
		t.Fatalf("missing config must exit 1; got %d", code)
	}
	if !strings.Contains(stderr, "config init") {
		t.Fatalf("error must hint `config init`; stderr=%q", stderr)
	}
}

// TestCmdAdd_UnknownPresetListsCatalog checks the typo path.
func TestCmdAdd_UnknownPresetListsCatalog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := writeMinimalConfig(t)

	code, _, stderr := runCmdAdd([]string{"--config", path, "not-a-preset"})
	if code != 1 {
		t.Fatalf("unknown preset must exit 1; got %d", code)
	}
	for _, want := range []string{"zhipu", "deepseek"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("catalog listing missing %q: stderr=%q", want, stderr)
		}
	}
}

// TestCmdPresets_ListsAndSuggestsAdd checks the catalog command output shape.
func TestCmdPresets_ListsAndSuggestsAdd(t *testing.T) {
	catalog, err := domainpresets.List()
	if err != nil {
		t.Fatal(err)
	}
	presetName := catalog[0].Name
	var out strings.Builder
	code := CmdPresets(nil, os.Stdin, &out, os.Stderr)
	if code != 0 {
		t.Fatalf("presets list exit = %d", code)
	}
	if !strings.Contains(out.String(), presetName) || !strings.Contains(out.String(), "model-proxy add") {
		t.Fatalf("presets output missing entries/add hint: %q", out.String())
	}
}

// TestCmdAdd_ModelVisibilityWarning pins the S3 cross-check: after login, a
// configured model absent from the account's /models list surfaces as a
// warning (stale preset), while visible models stay silent.
func TestCmdAdd_ModelVisibilityWarning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if r.URL.Path == "/models" {
			// The account can see glm-live only — glm-gone is configured but stale.
			_, _ = w.Write([]byte(`{"data":[{"id":"glm-live"}]}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := "listen: 127.0.0.1:15799\nlog_level: error\n\nproviders:\n" +
		"  zhipu:\n" +
		"    provider_id: zhipu\n" +
		"    openai_base_url: " + srv.URL + "\n" +
		"    models: [glm-live, glm-gone]\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADD_TEST_KEY", "sk-vis-key")

	code, stdout, stderr := runCmdAdd([]string{"--config", cfgPath, "zhipu", "--api-key-env", "ADD_TEST_KEY"})
	if code != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "glm-gone") || !strings.Contains(stdout, "not visible") {
		t.Fatalf("stale model must be named in the warning; stdout=%q", stdout)
	}
	if strings.Contains(stdout, "glm-live") && strings.Contains(stdout, "not visible") && !strings.Contains(stdout, "glm-gone") {
		t.Fatalf("visible model must not be flagged; stdout=%q", stdout)
	}
}
