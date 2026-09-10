package status

import (
	"encoding/json"
	"model-proxy/internal/cli/clitest"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newMockCacheDaemon serves /api/status with the given cache object — the
// minimal slice of the status payload CmdCache parses.
func newMockCacheDaemon(t *testing.T, cache string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"uptime":"1m","version":"test","listen":"127.0.0.1:1","cache":` + cache + `}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func cacheCmdConfig(t *testing.T, listen string) string {
	t.Helper()
	return clitest.WriteTempConfig(t, "listen: "+listen+"\nproviders:\n  aqp:\n    openai_base_url: https://x\n    provider_id: aqp\n    models: [glm]\n")
}

func TestCmdCacheRendersCountersAndHitRate(t *testing.T) {
	mock := newMockCacheDaemon(t, `{"enabled":true,"hits":5,"misses":13,"entries":9,"models":[{"model":"glm-5.2","hits":3,"misses":8,"entries":6},{"model":"kimi-k3","hits":2,"misses":5,"entries":3}]}`)
	out := clitest.GrabStdout(t, func() {
		RunCache([]string{"--config", cacheCmdConfig(t, strings.TrimPrefix(mock.URL, "http://"))})
	})
	for _, want := range []string{"Exact response cache", "entries (live)", "hits", "misses", "hit rate", "MODEL", "ENTRIES", "HIT RATE"} {
		if !strings.Contains(out, want) {
			t.Errorf("cache output missing %q:\n%s", want, out)
		}
	}
	// 5 hits over 18 lookups → 27.8%; counters rendered verbatim.
	for _, want := range []string{"9", "5", "13", "27.8%"} {
		if !strings.Contains(out, want) {
			t.Errorf("cache output missing value %q:\n%s", want, out)
		}
	}
	// Per-model rows: sorted by model name (glm before kimi), each with its
	// own hit rate (3/11 = 27.3%, 2/7 = 28.6%).
	for _, want := range []string{"glm-5.2", "27.3%", "kimi-k3", "28.6%"} {
		if !strings.Contains(out, want) {
			t.Errorf("cache output missing per-model row value %q:\n%s", want, out)
		}
	}
	if i, j := strings.Index(out, "glm-5.2"), strings.Index(out, "kimi-k3"); !(i >= 0 && j > i) {
		t.Errorf("per-model rows must be sorted by model name:\n%s", out)
	}
}

func TestCmdCacheZeroLookupsShowsDashRate(t *testing.T) {
	mock := newMockCacheDaemon(t, `{"enabled":true,"hits":0,"misses":0,"entries":2}`)
	out := clitest.GrabStdout(t, func() {
		RunCache([]string{"--config", cacheCmdConfig(t, strings.TrimPrefix(mock.URL, "http://"))})
	})
	if !strings.Contains(out, "—") {
		t.Errorf("fresh cache (no lookups) must render an em-dash hit rate, not 0%%:\n%s", out)
	}
}

func TestCmdCacheDisabledPrintsHint(t *testing.T) {
	mock := newMockCacheDaemon(t, `{"enabled":false,"hits":0,"misses":0,"entries":0}`)
	out := clitest.GrabStdout(t, func() {
		RunCache([]string{"--config", cacheCmdConfig(t, strings.TrimPrefix(mock.URL, "http://"))})
	})
	if !strings.Contains(out, "disabled") || !strings.Contains(out, "cache.enabled") {
		t.Errorf("disabled cache must print the enable hint:\n%s", out)
	}
}

func TestCmdCacheJSONDumpsRawObject(t *testing.T) {
	mock := newMockCacheDaemon(t, `{"enabled":true,"hits":5,"misses":13,"entries":9,"models":[{"model":"glm-5.2","hits":5,"misses":13,"entries":9}]}`)
	out := clitest.GrabStdout(t, func() {
		RunCache([]string{"--json", "--config", cacheCmdConfig(t, strings.TrimPrefix(mock.URL, "http://"))})
	})
	var parsed CacheStats
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("--json output is not the cache object: %v\n%s", err, out)
	}
	if !parsed.Enabled || parsed.Hits != 5 || parsed.Misses != 13 || parsed.Entries != 9 {
		t.Errorf("parsed cache = %+v, want enabled with 5/13/9", parsed)
	}
	if len(parsed.Models) != 1 || parsed.Models[0].Model != "glm-5.2" || parsed.Models[0].Hits != 5 {
		t.Errorf("parsed models = %+v, want glm-5.2 with 5 hits", parsed.Models)
	}
}

func TestCacheHitRate(t *testing.T) {
	if got := CacheHitRate(5, 13); got != "27.8%" {
		t.Errorf("CacheHitRate(5,13) = %q, want 27.8%%", got)
	}
	if got := CacheHitRate(0, 7); got != "0.0%" {
		t.Errorf("CacheHitRate(0,7) = %q, want 0.0%%", got)
	}
	if got := CacheHitRate(0, 0); got != "—" {
		t.Errorf("CacheHitRate(0,0) = %q, want — (no lookups yet)", got)
	}
}

// TestCLI_CacheNoDaemon: unreachable daemon exits non-zero with the pointing
// error (subprocess — the command path calls os.Exit(1)).
func TestCLI_CacheNoDaemon(t *testing.T) {
	cfg := clitest.WriteTempConfig(t, "listen: 127.0.0.1:1\nproviders:\n  aqp:\n    openai_base_url: https://x\n    provider_id: aqp\n    models:\n      - m\n")
	_, stderr, code := clitest.RunCLI(t, "cache", cfg)
	if code == 0 {
		t.Error("cache no daemon: exit=0 want non-zero")
	}
	if !strings.Contains(stderr, "cannot reach") {
		t.Errorf("cache no-daemon stderr missing 'cannot reach':\n%s", stderr)
	}
}
