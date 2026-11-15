package config

import (
	"strings"
	"testing"
)

// TestDiskGuardDefaults: an absent disk_guard block keeps the built-in
// defaults — guard on, 50GiB free floor, 95% usage ceiling.
func TestDiskGuardDefaults(t *testing.T) {
	cfg, err := parseConfigBytes("config.yaml", []byte("listen: 127.0.0.1:15721\n"))
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.RequestLog.DiskGuard
	if !g.Enabled || g.MinFreeBytes != 50<<30 || g.MaxUsedPercent != 95 {
		t.Fatalf("disk guard defaults = %+v, want enabled/50GiB/95%%", g)
	}
	if !g.GuardArmed() {
		t.Fatalf("GuardArmed() = false, want armed with both conditions")
	}
}

// TestDiskGuardExplicitOverrides: every knob is overridable; a 0 condition
// disarms only that condition; enabled: false disarms the whole guard.
func TestDiskGuardExplicitOverrides(t *testing.T) {
	cfg, err := parseConfigBytes("config.yaml", []byte(`
listen: 127.0.0.1:15721
request_log:
  disk_guard:
    min_free_bytes: 0
    max_used_percent: 90
`))
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.RequestLog.DiskGuard
	if !g.Enabled || g.MinFreeBytes != 0 || g.MaxUsedPercent != 90 {
		t.Fatalf("overrides = %+v, want enabled/min_free 0/90%%", g)
	}
	if !g.GuardArmed() {
		t.Fatalf("GuardArmed() = false, want armed via the percent condition alone")
	}

	cfg, err = parseConfigBytes("config.yaml", []byte(`
listen: 127.0.0.1:15721
request_log:
  disk_guard:
    enabled: false
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RequestLog.DiskGuard.GuardArmed() {
		t.Fatalf("GuardArmed() = true, want disabled by enabled: false")
	}
}

// TestLogRotateDefaults: absent keys rotate serve.log at 128MiB keeping 4
// archives; an explicit 0 max size disables rotation.
func TestLogRotateDefaults(t *testing.T) {
	cfg, err := parseConfigBytes("config.yaml", []byte("listen: 127.0.0.1:15721\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogRotateMaxSize != 128<<20 || cfg.LogRotateKeep != 4 {
		t.Fatalf("rotate defaults = %d/%d, want 128MiB/4", cfg.LogRotateMaxSize, cfg.LogRotateKeep)
	}

	cfg, err = parseConfigBytes("config.yaml", []byte(`
listen: 127.0.0.1:15721
log_rotate_max_size: 0
log_rotate_keep: 2
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogRotateMaxSize != 0 || cfg.LogRotateKeep != 2 {
		t.Fatalf("rotate overrides = %d/%d, want off/2", cfg.LogRotateMaxSize, cfg.LogRotateKeep)
	}
}

// TestLogRotateNegativeRejected: negative values are configuration errors
// naming the key, not silently-treated-as-off zeros.
func TestLogRotateNegativeRejected(t *testing.T) {
	for _, doc := range []string{
		"listen: 127.0.0.1:15721\nlog_rotate_max_size: -1\n",
		"listen: 127.0.0.1:15721\nlog_rotate_keep: -2\n",
	} {
		_, err := parseConfigBytes("config.yaml", []byte(doc))
		if err == nil || !strings.Contains(err.Error(), "log_rotate_") {
			t.Fatalf("parse(%q) err = %v, want a log_rotate_ key error", doc, err)
		}
	}
}
