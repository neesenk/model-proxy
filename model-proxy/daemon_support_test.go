package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- openLogFile ---

func TestOpenLogFile_CreatesDirAndFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "model-proxy.log")
	f, err := openLogFile(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := os.Stat(p); err != nil {
		t.Errorf("log file not created: %v", err)
	}
}

func TestOpenLogFile_Appends(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.log")
	f1, _ := openLogFile(p)
	f1.Write([]byte("first\n"))
	f1.Close()
	f2, _ := openLogFile(p)
	f2.Write([]byte("second\n"))
	f2.Close()
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "first") || !strings.Contains(string(data), "second") {
		t.Errorf("append lost content: %s", data)
	}
}

// --- pidFilePath (daemon_test.go already covers it; skipped here) ---

// --- writePidFile ---

func TestWritePidFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.pid")
	if err := writePidFile(p, 12345); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "12345") {
		t.Errorf("pid file content=%q want 12345", data)
	}
}
