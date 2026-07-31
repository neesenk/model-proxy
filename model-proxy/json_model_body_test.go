package main

import (
	"model-proxy/internal/protocol"
	"testing"
)

// json_model_body_test.go covers client-model extraction owned by the
// composition root. Target-model rewriting belongs to internal/targetexec.

func TestExtractModel(t *testing.T) {
	if got := protocol.ExtractModel([]byte(`{}`)); got != "" {
		t.Errorf("protocol.ExtractModel({})=%q want empty", got)
	}
	if got := protocol.ExtractModel([]byte(`not-json`)); got != "" {
		t.Errorf("protocol.ExtractModel(bad)=%q want empty", got)
	}
	if got := protocol.ExtractModel([]byte(`{"model":"glm-5.2"}`)); got != "glm-5.2" {
		t.Errorf("extractModel=%q want glm-5.2", got)
	}
}
