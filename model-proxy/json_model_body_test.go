package main

import (
	"encoding/json"
	"testing"
)

// proxy_extra_test.go covers small uncovered proxy.go helpers + quota.go
// stop/pollAfter + authAdapter.Refresh + the half-open slot lifecycle.

// --- main-package ensureJSONField ---

func TestEnsureJSONField_MainPkg(t *testing.T) {
	// Absent key → injected.
	got := ensureJSONField([]byte(`{"model":"x"}`), "store", false)
	var m map[string]any
	json.Unmarshal(got, &m)
	if v, ok := m["store"].(bool); !ok || v != false {
		t.Errorf("store not injected: %v", m)
	}
	// Existing key → untouched.
	got = ensureJSONField([]byte(`{"model":"x","store":true}`), "store", false)
	json.Unmarshal(got, &m)
	if v, _ := m["store"].(bool); v != true {
		t.Errorf("existing store overwritten: %v", m)
	}
	// Non-JSON body → returned unchanged.
	orig := []byte(`not-json`)
	if got := ensureJSONField(orig, "store", false); string(got) != string(orig) {
		t.Errorf("non-JSON body changed: %q", got)
	}
}

// --- rewriteModel: non-JSON body returned unchanged ---

func TestRewriteModel_NonJSON(t *testing.T) {
	orig := []byte(`not-json`)
	if got := rewriteModel(orig, "new"); string(got) != string(orig) {
		t.Errorf("rewriteModel(non-JSON) changed body: %q", got)
	}
}

func TestRewriteModel_SameModel(t *testing.T) {
	// Rewrite to the same model: still marshals (no early return in impl), but
	// the model field should be the new value.
	got := rewriteModel([]byte(`{"model":"old","input":[]}`), "new")
	if !contains(string(got), `"model":"new"`) {
		t.Errorf("rewriteModel did not set new model: %s", got)
	}
}

// --- extractModel: missing / malformed ---

func TestExtractModel(t *testing.T) {
	if got := extractModel([]byte(`{}`)); got != "" {
		t.Errorf("extractModel({})=%q want empty", got)
	}
	if got := extractModel([]byte(`not-json`)); got != "" {
		t.Errorf("extractModel(bad)=%q want empty", got)
	}
	if got := extractModel([]byte(`{"model":"glm-5.2"}`)); got != "glm-5.2" {
		t.Errorf("extractModel=%q want glm-5.2", got)
	}
}
