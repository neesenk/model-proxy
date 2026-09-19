package takeover_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/takeover"
)

// The structured reports (RunTakeoverReport / RunRestoreReport) and the
// drift projection (CheckDrift) are the Web admin surface's data source;
// these tests pin their semantics against the same fixtures the CLI path uses.

func TestRunTakeoverReport_AppliedSkippedWarnings(t *testing.T) {
	dir := t.TempDir()
	claudeFile := filepath.Join(dir, "claude.json")
	templatesDir := writeTemplateOverrides(t, map[string]string{"claude": claudeFile})
	cfg := &configdomain.Config{Listen: "127.0.0.1:15721"}
	bakDir := filepath.Join(dir, ".mp")

	// claude's config is absent → batch skips it and reports the skip.
	report, err := takeover.RunTakeoverReport(cfg, "claude", bakDir, takeover.ModelFacts{SourceDefault: -1}, templatesDir, takeover.ModeUnified, takeover.ScopeAll)
	if err == nil || !strings.Contains(err.Error(), "backup") {
		// A single named client is a hard error when its file is missing.
		t.Fatalf("single missing client: err = %v, want a backup error", err)
	}

	os.WriteFile(claudeFile, []byte(`{"env":{"OLD":"1"}}`), 0o644)
	report, err = takeover.RunTakeoverReport(cfg, "claude", bakDir, takeover.ModelFacts{SourceDefault: -1}, templatesDir, takeover.ModeUnified, takeover.ScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Applied) != 1 || report.Applied[0].Name != "claude" {
		t.Fatalf("applied = %+v, want [claude]", report.Applied)
	}
	if len(report.Skipped) != 0 {
		t.Errorf("skipped = %v, want none", report.Skipped)
	}

	// Restore report mirrors it.
	restored, err := takeover.RunRestoreReport(cfg, "claude", bakDir, templatesDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Restored) != 1 || restored.Restored[0] != "claude" || len(restored.Skipped) != 0 {
		t.Fatalf("restored = %+v, want [claude] and no skips", restored)
	}
}

func TestCheckDrift_ThreeStates(t *testing.T) {
	dir := t.TempDir()
	claudeFile := filepath.Join(dir, "claude.json")
	templatesDir := writeTemplateOverrides(t, map[string]string{"claude": claudeFile})
	bakDir := filepath.Join(dir, ".mp")
	cfg := &configdomain.Config{Listen: "127.0.0.1:15721"}

	driftOf := func(name string) takeover.ClientDrift {
		t.Helper()
		drift, err := takeover.CheckDrift(cfg, bakDir, templatesDir)
		if err != nil {
			t.Fatalf("CheckDrift: %v", err)
		}
		for _, d := range drift {
			if d.Client == name {
				return d
			}
		}
		t.Fatalf("client %q missing from drift report", name)
		return takeover.ClientDrift{}
	}

	// No backup marker → not taken over.
	if d := driftOf("claude"); d.Taken {
		t.Errorf("no backup: taken = true, want false (%+v)", d)
	}

	// Takeover → taken, pointer matches.
	os.WriteFile(claudeFile, []byte(`{"env":{"OLD":"1"}}`), 0o644)
	if _, err := takeover.RunTakeoverReport(cfg, "claude", bakDir, takeover.ModelFacts{SourceDefault: -1}, templatesDir, takeover.ModeUnified, takeover.ScopeAll); err != nil {
		t.Fatal(err)
	}
	if d := driftOf("claude"); !d.Taken || !d.OK {
		t.Errorf("post-takeover: %+v, want taken and ok", d)
	}

	// Drift: repoint the client elsewhere.
	os.WriteFile(claudeFile, []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://elsewhere:1"}}`), 0o644)
	if d := driftOf("claude"); !d.Taken || d.OK {
		t.Errorf("drifted: %+v, want taken and !ok", d)
	} else if d.Expected == "" || d.Current == "" {
		t.Errorf("drift detail empty: %+v", d)
	}

	// Restore → back to not-taken-over (marker removed).
	if _, err := takeover.RunRestoreReport(cfg, "claude", bakDir, templatesDir); err != nil {
		t.Fatal(err)
	}
	if d := driftOf("claude"); d.Taken {
		t.Errorf("post-restore: taken = true, want false (%+v)", d)
	}
}
