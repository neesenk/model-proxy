package main

import (
	"testing"
)

// json_model_body_test.go covers client-model extraction owned by the
// composition root. Target-model rewriting belongs to internal/targetexec.

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
