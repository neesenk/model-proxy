package config

import (
	"path/filepath"
	"testing"
)

func TestUpstreamTimeoutZeroRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "listen: 127.0.0.1:0\nscheduling:\n  upstream_timeout: \"0\"\n"
	if _, err := LoadConfigFromBytes(path, []byte(yaml)); err == nil {
		t.Fatal("upstream_timeout \"0\" accepted, want validation error")
	}
}
