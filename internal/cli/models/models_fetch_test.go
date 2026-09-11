package models

import (
	"context"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/providerbuild"
	"strings"
	"testing"
)

// --- fetchProviderModels: unknown provider ---

func TestFetchProviderModels_Unknown(t *testing.T) {
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{"a": {OpenAIBaseURL: "http://x", Provider: "static"}}}
	_, err := FetchProviderModels(cfg, "nope")
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
	_, err := providerbuild.ListArkAgentPlanModelIDs(context.Background(), "volcengine")
	if err == nil || !strings.Contains(err.Error(), "AK/SK") {
		t.Errorf("no creds: err=%v want AK/SK error", err)
	}
}
