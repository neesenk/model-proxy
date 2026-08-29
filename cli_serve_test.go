package main

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"model-proxy/internal/observe/logx"
)

// TestServeLogLevelFiltering (A4 integration): runProxyProcess applies
// cfg.LogLevel via logx.SetLevel at startup; after SetLevel("warn") info
// lines must be suppressed and warn lines must still reach the log output.
func TestServeLogLevelFiltering(t *testing.T) {
	savedLevel := logx.CurrentLevel()
	savedWriter := log.Writer()
	savedFlags := log.Flags()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(savedWriter)
		log.SetFlags(savedFlags)
		logx.SetLevel(savedLevel.String())
	})

	logx.SetLevel("warn")
	logx.Infof("info-token")
	logx.Warnf("warn-token")

	out := buf.String()
	if strings.Contains(out, "info-token") {
		t.Errorf("warn level must suppress info lines, got %q", out)
	}
	if !strings.Contains(out, "warn-token") {
		t.Errorf("warn level must pass warn lines, got %q", out)
	}
}
