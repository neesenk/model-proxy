package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDoUnfreeze: the CLI do* function renders the daemon's response (and the
// nothing-frozen case).
func TestDoUnfreeze(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health/reset" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Provider string `json:"provider"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Provider == "empty" {
			writeJSON(w, http.StatusOK, map[string]any{"cleared": []string{}, "model_locks_cleared": 0})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"cleared": []string{"zhipu"}, "model_locks_cleared": 2})
	}))
	defer srv.Close()

	out, err := DoUnfreeze(srv.URL, "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "zhipu") || !strings.Contains(out, "2 model lock") {
		t.Errorf("out = %q, want unfroze line with provider + lock count", out)
	}
	out, err = DoUnfreeze(srv.URL, "empty")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no frozen state") {
		t.Errorf("out = %q, want the nothing-frozen line", out)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
