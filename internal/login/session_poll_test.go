package login

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPollAt_TimesOut drives the production wrapper chain (PollSessionContext
// → PollAtContext with AqpClient.Base) against a gateway that answers
// auth/info with 401 every time. The poll deadline expires between retries, so
// the terminal verdict must be the session-check failure wrapping the last 401
// — not nil and not an unrelated error.

func TestPollAt_TimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != AqpAuthInfoPath {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"retcode":1,"message":"pending"}`))
	}))
	t.Cleanup(srv.Close)

	c := NewAqpClient(filepath.Join(t.TempDir(), "store.json"))
	c.Base = srv.URL
	_, err := c.PollSessionContext(context.Background(), 100*time.Millisecond)
	if err == nil {
		t.Fatal("PollSessionContext against an always-401 gateway: want terminal error, got nil")
	}
	if !strings.Contains(err.Error(), "aqp sso session check failed") {
		t.Fatalf("error = %v, want the session-check failure verdict", err)
	}
	if !strings.Contains(err.Error(), "status=401") {
		t.Fatalf("error = %v, want the last 401 failure wrapped in the verdict", err)
	}
}
