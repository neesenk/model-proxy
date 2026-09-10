package admin

import (
	"encoding/json"
	"model-proxy/internal/cli/clitest"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDoFreeze: the CLI do* function renders the daemon's response (and the
// no-match case).
func TestDoFreeze(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health/freeze" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Provider string `json:"provider"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Provider == "missing" {
			clitest.WriteJSON(w, http.StatusOK, map[string]any{"frozen": []string{}})
			return
		}
		clitest.WriteJSON(w, http.StatusOK, map[string]any{"frozen": []string{"zhipu", "zhipu#a1"}})
	}))
	defer srv.Close()

	out, err := DoFreeze(srv.URL, "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "froze zhipu") || !strings.Contains(out, "zhipu#a1") {
		t.Errorf("out = %q, want froze line with scope + matched names", out)
	}
	out, err = DoFreeze(srv.URL, "missing")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no matching provider") {
		t.Errorf("out = %q, want the no-match line", out)
	}
}
