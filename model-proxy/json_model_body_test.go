package main

import (
	"testing"
)

// json_model_body_test.go covers the JSON body helpers owned by the
// composition root. Codex-specific field injection belongs to provider tests.

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
