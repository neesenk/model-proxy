package main

import (
	clilogin "model-proxy/internal/cli/login"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- PollSession via pollAt with a mock (covers PollSession's 1-line delegate) ---
// PollSession() calls pollAt(aqpAuthInfo, ...) — the real URL. We can't
// redirect it (no URL-param variant on PollSession). Instead cover pollAt +
// checkSessionAt directly (already 72.7%/83.3%), and exercise the timeout
// path of pollAt with a mock that always fails.

func TestPollAt_TimesOut(t *testing.T) {
	// A mock that returns 401 every time → pollAt loops until the deadline,
	// then returns the last error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"retcode":1,"message":"pending"}`))
	}))
	defer srv.Close()
	c := clilogin.NewAqpClient(filepath.Join(t.TempDir(), "store.json"))
	_, err := c.PollAt(srv.URL, 1*time.Millisecond)
	if err == nil {
		t.Error("pollAt always-401: want error, got nil")
	}
}

// keep imports referenced.
var _ = strings.Contains
