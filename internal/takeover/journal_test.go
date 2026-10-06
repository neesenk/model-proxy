package takeover_test

// journal_test.go covers the op-journal lifecycle (intent log + interrupted
// detection, the cc-switch live-state.json pattern) and the post-write
// readback verification.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/takeover"
)

func journalTestConfig() *configdomain.Config {
	return &configdomain.Config{
		Listen: "127.0.0.1:15721",
		Providers: map[string]configdomain.Provider{
			"aqp": {OpenAIBaseURL: "http://x", Provider: "aqp", Models: []string{"glm-5.2"}},
		},
		Routes: map[string][]configdomain.RouteTarget{"glm-5.2": {{Provider: "aqp", Model: "glm-5.2"}}},
	}
}

// A fully successful takeover clears the journal; a leftover journal from an
// interrupted run surfaces as a warning and is superseded (cleared on the
// new run's success).
func TestRunTakeover_JournalLifecycle(t *testing.T) {
	dir := t.TempDir()
	claudeFile := filepath.Join(dir, "claude.json")
	templatesDir := writeTemplateOverrides(t, map[string]string{"claude": claudeFile})
	cfg := journalTestConfig()
	bakDir := filepath.Join(dir, ".mp")
	journal := filepath.Join(bakDir, "op-journal.json")

	os.WriteFile(claudeFile, []byte(`{"env":{"OLD":"1"}}`), 0o644)
	report, err := takeover.RunTakeoverReport(cfg, "claude", bakDir, takeover.ModelFacts{SourceDefault: -1}, templatesDir, takeover.ModeUnified, takeover.ScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("journal must be cleared after a successful takeover, stat err = %v", err)
	}
	for _, w := range report.Warnings {
		if strings.Contains(w, "interrupted") {
			t.Fatalf("first run reported a spurious interruption: %q", w)
		}
	}

	// Simulate an interrupted previous op: a leftover journal whose recorded
	// op never completed. The next run must warn (interruption is otherwise
	// invisible on the daemon-side Web path) and converge.
	os.MkdirAll(bakDir, 0o700)
	os.WriteFile(journal, []byte(`{"op":"restore","clients":["claude"],"started_at":"2026-10-05T22:00:00Z"}`), 0o600)
	report, err = takeover.RunTakeoverReport(cfg, "claude", bakDir, takeover.ModelFacts{SourceDefault: -1}, templatesDir, takeover.ModeUnified, takeover.ScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range report.Warnings {
		if strings.Contains(w, "previous restore") && strings.Contains(w, "interrupted") {
			found = true
		}
	}
	if !found {
		t.Fatalf("interrupted previous op not reported: %+v", report.Warnings)
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("journal must be cleared after the converging run, stat err = %v", err)
	}
}

// A leftover journal interrupts the restore path the same way.
func TestRunRestore_JournalInterruptWarning(t *testing.T) {
	dir := t.TempDir()
	claudeFile := filepath.Join(dir, "claude.json")
	templatesDir := writeTemplateOverrides(t, map[string]string{"claude": claudeFile})
	cfg := journalTestConfig()
	bakDir := filepath.Join(dir, ".mp")

	os.WriteFile(claudeFile, []byte(`{"env":{"OLD":"1"}}`), 0o644)
	if err := takeover.RunTakeover(cfg, "claude", bakDir, takeover.ModelFacts{SourceDefault: -1}, templatesDir, takeover.ModeUnified); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(bakDir, "op-journal.json"), []byte(`{"op":"takeover","clients":["claude"],"started_at":"2026-10-05T22:30:00Z"}`), 0o600)
	report, err := takeover.RunRestoreReport(cfg, "claude", bakDir, templatesDir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range report.Warnings {
		if strings.Contains(w, "previous takeover") && strings.Contains(w, "interrupted") {
			found = true
		}
	}
	if !found {
		t.Fatalf("restore did not report the interrupted takeover: %+v", report.Warnings)
	}
	if len(report.Restored) != 1 || report.Restored[0] != "claude" {
		t.Fatalf("restored = %+v, want [claude]", report.Restored)
	}
}

// A template whose drift probe disagrees with what json.set writes fails the
// takeover at the post-write readback — the managed keys did not land where
// the probe looks, which previously only surfaced as later doctor drift.
func TestRunTakeover_ReadbackMismatch(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "agent.json")
	templatesDir := t.TempDir()
	os.WriteFile(filepath.Join(templatesDir, "claude.yaml"), []byte("file: "+target+`
format: json
json:
  set:
    env.ANTHROPIC_BASE_URL: "{{base_url}}"
  drift_path: env.SOMETHING_ELSE
`), 0o600)
	os.WriteFile(target, []byte(`{}`), 0o644)

	err := takeover.RunTakeover(journalTestConfig(), "claude", filepath.Join(dir, ".mp"), takeover.ModelFacts{SourceDefault: -1}, templatesDir, takeover.ModeUnified)
	if err == nil || !strings.Contains(err.Error(), "verify: post-write readback") {
		t.Fatalf("err = %v, want a post-write readback verify failure", err)
	}
}
