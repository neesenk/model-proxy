package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"model-proxy/internal/cli/clitest"
)

// TestDoUnfreeze: the unfreeze line reports what the daemon cleared, under
// the scope the operator asked for — the account's cleared names, the model
// locks alone, or that nothing was frozen — and the provider reaches the
// request body (empty = every provider).
func TestDoUnfreeze(t *testing.T) {
	plainText(t)
	cases := []struct {
		name, provider, want string
		reply                map[string]any
	}{
		{
			name: "cleared accounts and model locks", provider: "zhipu",
			want:  "✓ unfroze zhipu: zhipu, zhipu#a1 (+2 model lock(s))\n",
			reply: map[string]any{"cleared": []string{"zhipu", "zhipu#a1"}, "model_locks_cleared": 2},
		},
		{
			name: "only model locks were frozen", provider: "zhipu",
			want:  "✓ unfroze zhipu: (no provider cooldowns) (+1 model lock(s))\n",
			reply: map[string]any{"cleared": []string{}, "model_locks_cleared": 1},
		},
		{
			name: "nothing frozen", provider: "zhipu",
			want:  "• no frozen state on zhipu\n",
			reply: map[string]any{"cleared": []string{}, "model_locks_cleared": 0},
		},
		{
			name: "no provider clears every account", provider: "",
			want:  "✓ unfroze all providers: shopee, zhipu (+3 model lock(s))\n",
			reply: map[string]any{"cleared": []string{"shopee", "zhipu"}, "model_locks_cleared": 3},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotMethod, gotProvider string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotMethod = r.URL.Path, r.Method
				var body struct {
					Provider string `json:"provider"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				gotProvider = body.Provider
				clitest.WriteJSON(w, http.StatusOK, tc.reply)
			}))
			defer srv.Close()

			out, err := DoUnfreeze(srv.URL, tc.provider)
			if err != nil {
				t.Fatalf("DoUnfreeze(%q): %v", tc.provider, err)
			}
			if out != tc.want {
				t.Errorf("DoUnfreeze(%q) line = %q, want %q", tc.provider, out, tc.want)
			}
			if gotPath != "/api/health/reset" || gotMethod != http.MethodPost || gotProvider != tc.provider {
				t.Errorf("request = %s %s provider=%q, want POST /api/health/reset provider=%q", gotMethod, gotPath, gotProvider, tc.provider)
			}
		})
	}
}
