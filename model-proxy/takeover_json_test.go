package main

import (
	"model-proxy/internal/takeover"
	"os"
	"path/filepath"
	"testing"
)

// --- readJSONConfig / writeJSONConfig ---

func TestReadJSONConfig_MissingReturnsEmpty(t *testing.T) {
	v, err := takeover.ReadJSONConfig(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || v == nil || len(v) != 0 {
		t.Errorf("takeover.ReadJSONConfig(missing)=%v err=%v want empty map", v, err)
	}
}

func TestReadJSONConfig_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.json")
	os.WriteFile(p, []byte(`not-json`), 0o644)
	_, err := takeover.ReadJSONConfig(p)
	if err == nil {
		t.Error("takeover.ReadJSONConfig(invalid): want error, got nil")
	}
}

func TestWriteReadJSONConfig_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.json")
	v := map[string]any{"k": "v"}
	if err := takeover.WriteJSONConfig(p, v); err != nil {
		t.Fatal(err)
	}
	got, err := takeover.ReadJSONConfig(p)
	if err != nil || got["k"] != "v" {
		t.Errorf("round-trip: got=%v err=%v", got, err)
	}
}
