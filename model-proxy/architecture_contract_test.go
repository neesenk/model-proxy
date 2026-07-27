package main

import (
	"os"
	"strings"
	"testing"
)

func TestArchitectureBoundaries(t *testing.T) {
	assertSourceExcludes(t, "web.go", []string{
		"w.p.mu", "w.p.healthMu", "w.p.cfg", "w.p.providers",
		"w.p.health", "w.p.modelLocks", "w.p.snapshotConfig",
	})
	assertSourceExcludes(t, "fusion.go", []string{
		"providerConfig(", "resolvedBackendProto(", "convertRequestFor(",
		"catalogSnapshot(",
	})
	assertSourceExcludes(t, "daemon.go", []string{
		"p.initStats(", "p.initRequestLog(", "p.statsFlushLoop(",
		"p.reqLog.loop(", "p.reqLog.shutdown(", "p.flusher.flush(",
	})
}

func assertSourceExcludes(t *testing.T, path string, forbidden []string) {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range forbidden {
		if strings.Contains(string(source), token) {
			t.Errorf("%s bypasses architecture boundary with %q", path, token)
		}
	}
}
