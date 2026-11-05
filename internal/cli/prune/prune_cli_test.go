package prune

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
)

// seedLogs creates a request-log layout: one old rotated file, one fresh
// rotated file, the active (today's) file and index.db in dir. It returns
// the paths that must survive a correct prune of the seeded old file.
func seedLogs(t *testing.T, dir, prefix string, now time.Time) (old string, keep []string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old = filepath.Join(dir, prefix+now.AddDate(0, 0, -10).Format("20060102")+".log")
	write := func(path string, age time.Duration) {
		mtime := now.Add(-age)
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	write(old, 9*24*time.Hour) // older than 7d — must go
	freshRotated := filepath.Join(dir, prefix+now.AddDate(0, 0, -3).Format("20060102")+".log")
	write(freshRotated, 3*24*time.Hour) // inside window — keep
	active := filepath.Join(dir, prefix+now.Format("20060102")+".log")
	write(active, 2*24*time.Hour) // active file — keep
	index := filepath.Join(dir, "index.db")
	if err := os.WriteFile(index, []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(index, now.Add(-9*24*time.Hour), now.Add(-9*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	return old, []string{freshRotated, active, index}
}

func testCfg(reqDir, mcpDir string) *configdomain.Config {
	cfg := &configdomain.Config{}
	cfg.RequestLog.Dir = reqDir
	cfg.RequestLog.MCPDir = mcpDir
	return cfg
}

// TestCmdPruneDeletesOnlyOldRotatedFiles pins the one-shot semantics: the
// 9-day-old rotated files in BOTH streams are deleted; fresh rotated files,
// the active files and index.db survive.
func TestCmdPruneDeletesOnlyOldRotatedFiles(t *testing.T) {
	now := time.Now()
	reqDir := t.TempDir()
	mcpDir := t.TempDir()
	oldReq, keepReq := seedLogs(t, reqDir, "requests-", now)
	oldMcp, keepMcp := seedLogs(t, mcpDir, "mcp-", now)

	var out, errb bytes.Buffer
	code := CmdPrune([]string{"7"}, testCfg(reqDir, mcpDir), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb.String())
	}
	for _, gone := range []string{oldReq, oldMcp} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s still exists after prune (stat err %v)", gone, err)
		}
	}
	for _, keep := range append(keepReq, keepMcp...) {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("prune deleted %s which must be kept: %v", keep, err)
		}
	}
	summary := out.String()
	if !strings.Contains(summary, "pruned 2 files") {
		t.Errorf("summary output = %q, want it to report 2 pruned files", summary)
	}
}

// TestCmdPruneDryRunDeletesNothing pins --dry-run: candidates are listed,
// no file is removed.
func TestCmdPruneDryRunDeletesNothing(t *testing.T) {
	now := time.Now()
	reqDir := t.TempDir()
	mcpDir := t.TempDir() // left empty of mcp files; dir exists but has none old
	old, _ := seedLogs(t, reqDir, "requests-", now)

	var out, errb bytes.Buffer
	code := CmdPrune([]string{"7", "--dry-run"}, testCfg(reqDir, mcpDir), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb.String())
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("dry-run deleted %s: %v", old, err)
	}
	if !strings.Contains(out.String(), filepath.Base(old)) {
		t.Errorf("dry-run output = %q, want it to list %s", out.String(), filepath.Base(old))
	}
	if !strings.Contains(out.String(), "would prune 1 file") {
		t.Errorf("dry-run output = %q, want would-prune summary", out.String())
	}
}

// TestCmdPruneMissingMCPDirIsFine pins that an mcp split stream that never
// existed is simply skipped, while a missing requests- dir is an error.
func TestCmdPruneMissingMCPDirIsFine(t *testing.T) {
	now := time.Now()
	reqDir := t.TempDir()
	seedLogs(t, reqDir, "requests-", now)

	var out, errb bytes.Buffer
	if code := CmdPrune([]string{"7"}, testCfg(reqDir, filepath.Join(t.TempDir(), "no-mcp")), &out, &errb); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb.String())
	}
	var out2, errb2 bytes.Buffer
	if code := CmdPrune([]string{"7"}, testCfg(filepath.Join(t.TempDir(), "no-req"), t.TempDir()), &out2, &errb2); code != 1 {
		t.Fatalf("missing requests dir exit = %d, want 1 (stderr %s)", code, errb2.String())
	}
	if !strings.Contains(errb2.String(), "request-log directory not found") {
		t.Errorf("stderr = %q, want missing-dir message", errb2.String())
	}
}

// TestCmdPruneUsageErrors pins argument validation: missing/non-positive/
// non-integer days and unknown flags exit 1 with usage on stderr.
func TestCmdPruneUsageErrors(t *testing.T) {
	cases := [][]string{nil, {"0"}, {"-3"}, {"abc"}, {"7", "9"}, {"7", "--bogus"}, {"7", "--config"}}
	for _, args := range cases {
		var out, errb bytes.Buffer
		if code := CmdPrune(args, testCfg(t.TempDir(), t.TempDir()), &out, &errb); code != 1 {
			t.Errorf("CmdPrune(%q) exit = %d, want 1", args, code)
		}
		if !strings.Contains(errb.String(), "usage: model-proxy prune") {
			t.Errorf("CmdPrune(%q) stderr = %q, want usage line", args, errb.String())
		}
	}
}

func TestCmdPruneAcceptsConfigPathForms(t *testing.T) {
	cases := [][]string{
		{"7", "--config", "custom.yaml"},
		{"--config", "custom.yaml", "7"},
		{"7", "--config=custom.yaml"},
	}
	for _, args := range cases {
		args = append(args, "--dry-run")
		var out, errb bytes.Buffer
		if code := CmdPrune(args, testCfg(t.TempDir(), t.TempDir()), &out, &errb); code != 0 {
			t.Errorf("CmdPrune(%q) exit = %d, stderr = %q, want success", args, code, errb.String())
		}
	}
}
