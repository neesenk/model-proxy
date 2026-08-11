package serve_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	serve "model-proxy/internal/cli/serve"
)

func TestReadLivePid(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "mp.log")
	pidPath := serve.PidFilePath(logFile)

	// No pid file.
	if got := serve.ReadLivePid(logFile); got != 0 {
		t.Errorf("missing pid file: got %d, want 0", got)
	}

	// Invalid content.
	if err := os.WriteFile(pidPath, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := serve.ReadLivePid(logFile); got != 0 {
		t.Errorf("invalid pid: got %d, want 0", got)
	}

	// Dead process: stale file must be removed.
	if err := os.WriteFile(pidPath, []byte("999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := serve.ReadLivePid(logFile); got != 0 {
		t.Errorf("dead process: got %d, want 0", got)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("stale pid file must be removed")
	}

	// Live process (self).
	if err := os.WriteFile(pidPath, []byte(fmt.Sprint(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := serve.ReadLivePid(logFile); got != os.Getpid() {
		t.Errorf("live pid = %d, want %d", got, os.Getpid())
	}
}
