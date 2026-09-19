package admin

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/internal/appapi"
	configdomain "model-proxy/internal/config"
)

// takeoverTestRig builds a Service rooted at temp dirs: HOME is isolated via
// t.Setenv (the same seam internal/takeover's own tests use), so template
// `~` expansion, the user templates dir, the catalog cache and the backup dir
// all land inside t.TempDir — the tests never touch the real HOME's client
// configs.
type takeoverTestRig struct {
	service      *Service
	home         string
	templatesDir string
	configFile   string
	clientFile   string
	templateYAML string
}

func newTakeoverTestRig(t *testing.T) *takeoverTestRig {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := t.TempDir()
	rig := &takeoverTestRig{
		home:         home,
		templatesDir: filepath.Join(home, ".model-proxy", "takeover-templates"),
		configFile:   filepath.Join(cfgDir, "config.yaml"),
		clientFile:   filepath.Join(home, "client", "settings.json"),
	}
	if err := os.WriteFile(rig.configFile, []byte("listen: 127.0.0.1:15721\nproviders: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rig.templateYAML = "description: test client\nfile: ~/client/settings.json\nformat: json\njson:\n  set:\n    env.TEST_BASE_URL: \"{{base_url}}\"\n  drift_path: env.TEST_BASE_URL\n"
	if err := os.MkdirAll(rig.templatesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig.templatesDir, "testclient.yaml"), []byte(rig.templateYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &configdomain.Config{Listen: "127.0.0.1:15721", Providers: map[string]configdomain.Provider{}}
	rig.service = New(Ports{
		Config:     func() *configdomain.Config { return cfg },
		ConfigFile: func() string { return rig.configFile },
		HomeDir:    func() string { return rig.home },
	})
	return rig
}

func (rig *takeoverTestRig) clientRow(t *testing.T, name string) appapi.TakeoverClient {
	t.Helper()
	surface, err := rig.service.TakeoverSurface("")
	if err != nil {
		t.Fatalf("TakeoverSurface: %v", err)
	}
	for _, c := range surface.Clients {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("client %q missing from surface (have %d clients)", name, len(surface.Clients))
	return appapi.TakeoverClient{}
}

func TestTakeoverSurfaceNilConfig(t *testing.T) {
	service := New(Ports{Config: func() *configdomain.Config { return nil }})
	_, err := service.TakeoverSurface("")
	if httpErrorStatus(t, err) != http.StatusServiceUnavailable {
		t.Fatalf("nil config err = %v, want 503", err)
	}
}

func TestTakeoverSurfaceRunAndRestoreRoundTrip(t *testing.T) {
	rig := newTakeoverTestRig(t)

	// Initially: the client config does not exist → not installed, not taken over.
	row := rig.clientRow(t, "testclient")
	if row.Installed || row.TakenOver {
		t.Fatalf("initial row = %+v, want not installed / not taken over", row)
	}
	if row.Source != "user" || row.Format != "json" {
		t.Errorf("row source/format = %q/%q, want user/json", row.Source, row.Format)
	}
	// Presets ride along: claude is an embedded preset.
	preset := rig.clientRow(t, "claude")
	if preset.Source != "preset" {
		t.Errorf("claude source = %q, want preset", preset.Source)
	}

	// Install the client config, then take it over through the service.
	if err := os.MkdirAll(filepath.Dir(rig.clientFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rig.clientFile, []byte(`{"other": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := rig.service.RunTakeover(appapi.TakeoverRunRequest{Client: "testclient"})
	if err != nil {
		t.Fatalf("RunTakeover: %v", err)
	}
	if result.Status != "ok" || len(result.Applied) != 1 || result.Applied[0].Name != "testclient" {
		t.Fatalf("result = %+v, want one applied testclient", result)
	}
	if len(result.Skipped) != 0 {
		t.Errorf("skipped = %v, want none (client file exists)", result.Skipped)
	}
	data, err := os.ReadFile(rig.clientFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "http://127.0.0.1:15721") {
		t.Errorf("client file not pointed at the proxy:\n%s", data)
	}
	if !strings.Contains(string(data), `"other": true`) {
		t.Errorf("client file lost unrelated content:\n%s", data)
	}

	row = rig.clientRow(t, "testclient")
	if !row.Installed || !row.TakenOver || !row.DriftOK {
		t.Errorf("post-takeover row = %+v, want installed+taken+drift-free", row)
	}

	restored, err := rig.service.RestoreTakeover("testclient")
	if err != nil {
		t.Fatalf("RestoreTakeover: %v", err)
	}
	if len(restored.Restored) != 1 || restored.Restored[0] != "testclient" {
		t.Fatalf("restored = %+v, want testclient", restored)
	}
	if got, _ := os.ReadFile(rig.clientFile); string(got) != `{"other": true}` {
		t.Errorf("restored client file = %q, want the original", got)
	}
	row = rig.clientRow(t, "testclient")
	if row.TakenOver {
		t.Errorf("post-restore row still taken over: %+v", row)
	}
}

func TestTakeoverSurfaceModePreview(t *testing.T) {
	rig := newTakeoverTestRig(t)

	// Unified (default): the pi family has variants, exactly one is marked;
	// single-variant families (claude, testclient) are NEVER marked — the
	// marker only signals a choice.
	piMarked := map[string]bool{}
	for _, name := range []string{"pi", "pi-openai", "pi-responses"} {
		piMarked[name] = rig.clientRow(t, name).AutoSelected
	}
	if !piMarked["pi"] || piMarked["pi-openai"] || piMarked["pi-responses"] {
		t.Errorf("unified pi-family marks = %v, want only pi (no route signal → default variant)", piMarked)
	}
	for _, name := range []string{"claude", "codex", "testclient"} {
		if rig.clientRow(t, name).AutoSelected {
			t.Errorf("%s auto_selected = true, want false (single-variant family)", name)
		}
	}

	// A protocol mode pins the family variant that speaks it.
	surface, err := rig.service.TakeoverSurface("responses")
	if err != nil {
		t.Fatalf("TakeoverSurface(responses): %v", err)
	}
	for _, c := range surface.Clients {
		if c.Family == "pi" {
			want := c.Name == "pi-responses"
			if c.AutoSelected != want {
				t.Errorf("responses mode: %s auto_selected = %v, want %v", c.Name, c.AutoSelected, want)
			}
		}
	}

	// Unknown mode → 400 (same classification as the run path).
	_, err = rig.service.TakeoverSurface("bogus")
	var httpErr *appapi.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != 400 {
		t.Errorf("bad mode err = %v, want 400 HTTPError", err)
	}
}

func TestPreviewTakeover(t *testing.T) {
	rig := newTakeoverTestRig(t)

	// Existing client file: preview merges over it and reports exists.
	os.MkdirAll(filepath.Dir(rig.clientFile), 0o755)
	if err := os.WriteFile(rig.clientFile, []byte(`{"keep":"me"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	preview, err := rig.service.PreviewTakeover(appapi.TakeoverRunRequest{Client: "testclient"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Client != "testclient" || preview.Mode != "unified" || len(preview.Writes) != 1 {
		t.Fatalf("preview = %+v, want one unified write for testclient", preview)
	}
	w := preview.Writes[0]
	if !w.Exists || w.File != rig.clientFile || len(w.Templates) != 1 || w.Templates[0] != "testclient" {
		t.Fatalf("write = %+v", w)
	}
	if !strings.Contains(w.Content, "TEST_BASE_URL") || !strings.Contains(w.Content, "http://127.0.0.1:15721") || !strings.Contains(w.Content, "keep") {
		t.Errorf("preview content wrong:\n%s", w.Content)
	}
	// The real file must be untouched.
	after, _ := os.ReadFile(rig.clientFile)
	if string(after) != `{"keep":"me"}` {
		t.Errorf("preview mutated the real file: %s", after)
	}

	// Error classes: unknown client and bad mode are 400s, not 500s.
	_, err = rig.service.PreviewTakeover(appapi.TakeoverRunRequest{Client: "nope"}, false)
	var httpErr *appapi.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != 400 {
		t.Errorf("unknown client err = %v, want 400", err)
	}
	_, err = rig.service.PreviewTakeover(appapi.TakeoverRunRequest{Client: "testclient", Mode: "bogus"}, false)
	if !errors.As(err, &httpErr) || httpErr.Status != 400 {
		t.Errorf("bad mode err = %v, want 400", err)
	}
}

func TestTakeoverBatchSkipsUninstalled(t *testing.T) {
	rig := newTakeoverTestRig(t)
	// Batch mode with an isolated HOME: no client config exists anywhere, so
	// every template (presets included) is skipped, nothing is applied, and
	// no file is created under the fake HOME.
	result, err := rig.service.RunTakeover(appapi.TakeoverRunRequest{Client: "all", Mode: "unified"})
	if err != nil {
		t.Fatalf("RunTakeover(all): %v", err)
	}
	if len(result.Applied) != 0 {
		t.Errorf("applied = %v, want none (isolated HOME has no client configs)", result.Applied)
	}
	found := false
	for _, skipped := range result.Skipped {
		if skipped == "testclient" {
			found = true
		}
	}
	if !found {
		t.Errorf("skipped = %v, want testclient among them", result.Skipped)
	}
	if entries, err := filepath.Glob(filepath.Join(rig.home, ".claude*")); err != nil || len(entries) != 0 {
		t.Errorf("unexpected files under fake HOME: %v", entries)
	}
}

func TestRunTakeoverRejectsUnknownClientAndMode(t *testing.T) {
	rig := newTakeoverTestRig(t)
	if _, err := rig.service.RunTakeover(appapi.TakeoverRunRequest{Client: "nope"}); err == nil ||
		!strings.Contains(err.Error(), "unknown takeover client") {
		t.Errorf("unknown client err = %v, want the available-templates error", err)
	}
	if _, err := rig.service.RestoreTakeover("nope"); err == nil ||
		!strings.Contains(err.Error(), "unknown takeover client") {
		t.Errorf("restore unknown client err = %v, want the available-templates error", err)
	}
	_, err := rig.service.RunTakeover(appapi.TakeoverRunRequest{Client: "testclient", Mode: "bogus"})
	var httpErr *appapi.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != 400 {
		t.Errorf("bad mode err = %v, want 400 HTTPError", err)
	}
}

func TestTakeoverTemplateGetSaveDelete(t *testing.T) {
	rig := newTakeoverTestRig(t)

	// Preset read: verbatim embedded YAML.
	doc, err := rig.service.TakeoverTemplate("claude")
	if err != nil {
		t.Fatalf("TakeoverTemplate(claude): %v", err)
	}
	if doc.Source != "preset" || !strings.Contains(doc.YAML, "ANTHROPIC_BASE_URL") {
		t.Errorf("preset doc = %+v", doc)
	}
	// User override read.
	doc, err = rig.service.TakeoverTemplate("testclient")
	if err != nil {
		t.Fatalf("TakeoverTemplate(testclient): %v", err)
	}
	if doc.Source != "user" || doc.Path == "" || doc.YAML != rig.templateYAML {
		t.Errorf("user doc = %+v", doc)
	}
	// Unknown → 404.
	_, err = rig.service.TakeoverTemplate("nope")
	var httpErr *appapi.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != 404 {
		t.Errorf("unknown template err = %v, want 404", err)
	}
	// Traversal-ish names are rejected before touching the filesystem.
	for _, bad := range []string{"", "../x", "a/b", ".hidden", ".."} {
		if _, err := rig.service.TakeoverTemplate(bad); err == nil {
			t.Errorf("TakeoverTemplate(%q) succeeded, want rejection", bad)
		}
		if err := rig.service.SaveTakeoverTemplate(bad, []byte("format: json")); err == nil {
			t.Errorf("SaveTakeoverTemplate(%q) succeeded, want rejection", bad)
		}
		if err := rig.service.DeleteTakeoverTemplate(bad); err == nil {
			t.Errorf("DeleteTakeoverTemplate(%q) succeeded, want rejection", bad)
		}
	}

	// Save: invalid YAML and family violations are rejected fail-closed
	// (nothing persisted).
	if err := rig.service.SaveTakeoverTemplate("broken", []byte("format: [unclosed")); err == nil {
		t.Error("invalid YAML saved, want rejection")
	}
	familyBreaker := "file: ~/x.json\nformat: json\nclient: pi\njson:\n  set:\n    env.X: \"{{base_url}}\"\n" // pi family variant without protocol
	if err := rig.service.SaveTakeoverTemplate("pi-broken", []byte(familyBreaker)); err == nil ||
		!strings.Contains(err.Error(), "protocol") {
		t.Errorf("family violation err = %v, want a protocol complaint", err)
	}
	if _, statErr := os.Stat(filepath.Join(rig.templatesDir, "pi-broken.yaml")); !os.IsNotExist(statErr) {
		t.Error("rejected candidate still landed in the templates dir")
	}

	// Valid new template persists and becomes visible.
	valid := "description: mine\nfile: /tmp/mine.json\nformat: json\njson:\n  set:\n    env.X: \"{{base_url}}\"\n"
	if err := rig.service.SaveTakeoverTemplate("mine", []byte(valid)); err != nil {
		t.Fatalf("SaveTakeoverTemplate(mine): %v", err)
	}
	if got := rig.clientRow(t, "mine"); got.Source != "user" {
		t.Errorf("saved template source = %q, want user", got.Source)
	}

	// Deleting a bare preset (no override) is rejected; deleting a real user
	// template works.
	if err := rig.service.DeleteTakeoverTemplate("claude"); err == nil {
		t.Error("preset delete succeeded, want rejection")
	}
	if err := rig.service.DeleteTakeoverTemplate("mine"); err != nil {
		t.Fatalf("DeleteTakeoverTemplate(mine): %v", err)
	}
	if _, err := rig.service.TakeoverTemplate("mine"); err == nil {
		t.Error("deleted template still readable")
	}

	// Overriding a preset then deleting the override restores the preset.
	override := "description: custom claude\nfile: " + rig.clientFile + "\nformat: json\njson:\n  set:\n    env.ANTHROPIC_BASE_URL: \"{{base_url}}\"\n"
	if err := rig.service.SaveTakeoverTemplate("claude", []byte(override)); err != nil {
		t.Fatalf("SaveTakeoverTemplate(claude override): %v", err)
	}
	if got := rig.clientRow(t, "claude"); got.Source != "user" {
		t.Errorf("override source = %q, want user", got.Source)
	}
	if err := rig.service.DeleteTakeoverTemplate("claude"); err != nil {
		t.Fatalf("delete override: %v", err)
	}
	if got := rig.clientRow(t, "claude"); got.Source != "preset" {
		t.Errorf("post-delete source = %q, want preset restored", got.Source)
	}
}
