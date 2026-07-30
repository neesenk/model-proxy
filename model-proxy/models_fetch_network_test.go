package main

import (
	climodels "model-proxy/internal/cli/models"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestListArkAgentPlanModelIDs_DeadProxy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "volcengine_apikey.json"),
		[]byte(`{"api_key":"ark","access_key":"AK","secret_key":"SK"}`), 0o600)
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := dead.Listener.Addr().String()
	dead.Close()
	t.Setenv("HTTPS_PROXY", "http://"+addr)
	t.Setenv("HTTP_PROXY", "http://"+addr)
	_, err := climodels.ListArkAgentPlanModelIDs("volcengine")
	if err == nil {
		t.Error("listArkAgentPlanModelIDs (dead proxy): want error, got nil")
	}
}
