package main

import (
	"model-proxy/internal/app"
	climodels "model-proxy/internal/cli/models"
	"strings"
	"testing"
)

// --- fetchProviderModels: unknown provider ---

func TestFetchProviderModels_Unknown(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: testProviderID}}}
	_, err := climodels.FetchProviderModels(cfg, "nope")
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("unknown provider: err=%v", err)
	}
}

// fetchProviderModels' happy path needs a provider whose FetchModels doesn't
// require a cred file. buildProviders wires real auth (reads cred files), so we
// can't easily inject a mock — covered indirectly by provider-side
// TestZhipuFetchModels_FromEndpoint instead.

// --- listArkAgentPlanModelIDs: no AK/SK → error ---

func TestListArkAgentPlanModelIDs_NoCreds(t *testing.T) {
	// loadVolcengineCreds reads ~/.model-proxy/<provName>_apikey.json. With HOME
	// in a temp dir, the file is absent → error.
	t.Setenv("HOME", t.TempDir())
	_, err := app.ListArkAgentPlanModelIDs("volcengine")
	if err == nil || !strings.Contains(err.Error(), "AK/SK") {
		t.Errorf("no creds: err=%v want AK/SK error", err)
	}
}
