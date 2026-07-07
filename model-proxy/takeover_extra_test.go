package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// takeover_extra_test.go covers the backup/restore/listClients/sha256hex
// helpers in takeover.go and the small util.go helpers — all pure file/string
// ops, fully testable with temp dirs.

// --- backup: copies file + writes meta, idempotent ---

func TestBackup_CreatesCopyAndMeta(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "settings.json")
	os.WriteFile(src, []byte(`{"x":1}`), 0o644)
	bakDir := filepath.Join(dir, ".mp")

	if err := backup(src, bakDir, "claude"); err != nil {
		t.Fatal(err)
	}
	bak := filepath.Join(bakDir, "claude.bak")
	data, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("backup file not created: %v", err)
	}
	if string(data) != `{"x":1}` {
		t.Errorf("backup content=%q want {\"x\":1}", data)
	}
	meta, err := os.ReadFile(bak + ".meta")
	if err != nil {
		t.Fatalf("meta file not created: %v", err)
	}
	if !contains(string(meta), "sha256") || !contains(string(meta), "path") {
		t.Errorf("meta missing fields: %s", meta)
	}
}

func TestBackup_Idempotent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "s.json")
	os.WriteFile(src, []byte("original"), 0o644)
	bakDir := filepath.Join(dir, ".mp")
	if err := backup(src, bakDir, "c"); err != nil {
		t.Fatal(err)
	}
	// Change source; second backup must NOT overwrite the existing .bak.
	os.WriteFile(src, []byte("changed"), 0o644)
	if err := backup(src, bakDir, "c"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(bakDir, "c.bak"))
	if string(data) != "original" {
		t.Errorf("idempotent backup overwrote: got %q want original", data)
	}
}

func TestBackup_MissingSource(t *testing.T) {
	dir := t.TempDir()
	err := backup(filepath.Join(dir, "nope"), filepath.Join(dir, ".mp"), "c")
	if err == nil {
		t.Error("backup of missing file: want error, got nil")
	}
}

// --- restore: copies .bak back ---

func TestRestore_WritesBack(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "s.json")
	os.WriteFile(src, []byte("new"), 0o644)
	bakDir := filepath.Join(dir, ".mp")
	os.MkdirAll(bakDir, 0o700)
	os.WriteFile(filepath.Join(bakDir, "c.bak"), []byte("original"), 0o600)

	if err := restore(src, bakDir, "c"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(src)
	if string(data) != "original" {
		t.Errorf("restore content=%q want original", data)
	}
}

func TestRestore_NoBackup(t *testing.T) {
	dir := t.TempDir()
	err := restore(filepath.Join(dir, "out"), filepath.Join(dir, ".mp"), "c")
	if err == nil {
		t.Error("restore with no backup: want error, got nil")
	}
}

// --- sha256hex ---

func TestSha256hex(t *testing.T) {
	// Known: sha256("hello") = 2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824
	got := sha256hex([]byte("hello"))
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got != want {
		t.Errorf("sha256hex(hello)=%q want %q", got, want)
	}
}

// --- backupDir ---

func TestBackupDir(t *testing.T) {
	got := backupDir("/home/user/.config/foo/config.yaml")
	want := "/home/user/.config/foo/.model-proxy"
	if got != want {
		t.Errorf("backupDir=%q want %q", got, want)
	}
}

// --- listClients ---

func TestListClients_All(t *testing.T) {
	cfg := &Config{Takeover: Takeover{Claude: "a", Opencode: "b", Codex: "c", Pi: "d"}}
	all := listClients(cfg, "")
	if len(all) != 4 {
		t.Errorf("listClients('') len=%d want 4", len(all))
	}
	all = listClients(cfg, "all")
	if len(all) != 4 {
		t.Errorf("listClients('all') len=%d want 4", len(all))
	}
}

func TestListClients_OneByName(t *testing.T) {
	cfg := &Config{Takeover: Takeover{Claude: "a", Opencode: "b", Codex: "c", Pi: "d"}}
	one := listClients(cfg, "codex")
	if len(one) != 1 || one[0].name != "codex" {
		t.Errorf("listClients('codex')=%+v want [codex]", one)
	}
}

func TestListClients_UnknownReturnsNil(t *testing.T) {
	cfg := &Config{Takeover: Takeover{Claude: "a"}}
	if got := listClients(cfg, "nope"); got != nil {
		t.Errorf("listClients('nope')=%+v want nil", got)
	}
}

// --- runTakeover / runRestore end-to-end on a single client ---

func TestRunTakeover_AndRestore_Claude(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Providers: map[string]Provider{
			"compass": {OpenAIBaseURL: "http://x", Provider: "compass", Models: map[string]ProviderModel{
				"glm-5.2": {Context: 1000, Output: 2000, Modalities: ProviderModalities{Input: []string{"text"}, Output: []string{"text"}}},
			}},
		},
		Routes:   map[string][]RouteTarget{"glm-5.2": {{Provider: "compass", Model: "glm-5.2"}}},
		Takeover: Takeover{ProxyURL: "http://127.0.0.1:15721", Claude: filepath.Join(dir, "claude.json")},
	}
	bakDir := filepath.Join(dir, ".mp")

	// Seed an existing claude config, then takeover rewrites it (after backing up).
	os.WriteFile(cfg.Takeover.Claude, []byte(`{"env":{"OLD":"1"}}`), 0o644)
	if err := runTakeover(cfg, "claude", bakDir); err != nil {
		t.Fatal(err)
	}
	rewritten, _ := os.ReadFile(cfg.Takeover.Claude)
	if !contains(string(rewritten), "ANTHROPIC_BASE_URL") {
		t.Errorf("takeover did not rewrite claude: %s", rewritten)
	}
	// Backup preserved the original.
	bak, _ := os.ReadFile(filepath.Join(bakDir, "claude.bak"))
	if !contains(string(bak), "OLD") {
		t.Errorf("backup did not preserve original: %s", bak)
	}
	// Restore brings the original back.
	if err := runRestore(cfg, "claude", bakDir); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(cfg.Takeover.Claude)
	if !contains(string(restored), "OLD") || contains(string(restored), "ANTHROPIC_BASE_URL") {
		t.Errorf("restore did not revert: %s", restored)
	}
}

func TestRunTakeover_UnknownClientNoOps(t *testing.T) {
	cfg := &Config{Takeover: Takeover{Claude: "a"}}
	// "nope" → listClients returns nil → loop body never runs → nil error.
	if err := runTakeover(cfg, "nope", t.TempDir()); err != nil {
		t.Errorf("runTakeover unknown client: want nil, got %v", err)
	}
}

// --- readJSONConfig / writeJSONConfig ---

func TestReadJSONConfig_MissingReturnsEmpty(t *testing.T) {
	v, err := readJSONConfig(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || v == nil || len(v) != 0 {
		t.Errorf("readJSONConfig(missing)=%v err=%v want empty map", v, err)
	}
}

func TestReadJSONConfig_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.json")
	os.WriteFile(p, []byte(`not-json`), 0o644)
	_, err := readJSONConfig(p)
	if err == nil {
		t.Error("readJSONConfig(invalid): want error, got nil")
	}
}

func TestWriteReadJSONConfig_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.json")
	v := map[string]any{"k": "v"}
	if err := writeJSONConfig(p, v); err != nil {
		t.Fatal(err)
	}
	got, err := readJSONConfig(p)
	if err != nil || got["k"] != "v" {
		t.Errorf("round-trip: got=%v err=%v", got, err)
	}
}

// --- util.go ---

func TestEnvOrEmpty(t *testing.T) {
	if got := envOrEmpty("MP_TEST_UNSET_VAR"); got != "" {
		t.Errorf("envOrEmpty(unset)=%q want empty", got)
	}
	t.Setenv("MP_TEST_SET", "hello")
	if got := envOrEmpty("MP_TEST_SET"); got != "hello" {
		t.Errorf("envOrEmpty(set)=%q want hello", got)
	}
}

func TestRuntimeOS(t *testing.T) {
	if got := runtimeOS(); got != runtime.GOOS {
		t.Errorf("runtimeOS()=%q want %q", got, runtime.GOOS)
	}
}

func TestReadFile_Missing(t *testing.T) {
	if _, err := readFile(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("readFile(missing): want error, got nil")
	}
}

func TestWriteFile_ReadFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := writeFile(p, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readFile(p)
	if err != nil || string(got) != "hi" {
		t.Errorf("round-trip: got=%q err=%v", got, err)
	}
}

func TestMask_ShortAndEmpty(t *testing.T) {
	if got := mask(""); got != "(empty)" {
		t.Errorf("mask(empty)=%q want (empty)", got)
	}
	if got := mask("short"); got != "****" {
		t.Errorf("mask(short)=%q want ****", got)
	}
	if got := mask("ab"); got != "****" {
		t.Errorf("mask(2-char)=%q want ****", got)
	}
}

func TestMask_Long(t *testing.T) {
	if got := mask("abcdefghijklmnop"); got != "ab…op" {
		t.Errorf("mask(long)=%q want ab…op", got)
	}
}

func TestAuthFilePath(t *testing.T) {
	got := authFilePath("zhipu-work", "apikey")
	if !contains(got, "zhipu-work_apikey.json") || !contains(got, ".model-proxy") {
		t.Errorf("authFilePath=%q want zhipu-work_apikey.json under .model-proxy", got)
	}
}

// --- color.go: cYellow/cMagenta via the color-disabled path ---

func TestColorHelpers_NoColorPassthrough(t *testing.T) {
	// colorEnabled reflects os.Stdout at init. In tests stdout is not a tty
	// (and NO_COLOR may be set), so color helpers return the input verbatim
	// (no ANSI escape codes). Assert the EXACT string — not Contains (which
	// would pass even with ANSI codes wrapping the input).
	for _, s := range []string{"x", "hello", "test-123"} {
		if got := cYellow(s); got != s {
			t.Errorf("cYellow(%q)=%q, want exact %q (no ANSI in test env)", s, got, s)
		}
		if got := cMagenta(s); got != s {
			t.Errorf("cMagenta(%q)=%q, want exact %q", s, got, s)
		}
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
