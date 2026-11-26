package stats

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestR6RenderTimeWindow pins what the three `stats` read paths send for
// --from/--to. The admin endpoints take unix seconds or RFC3339 and reject
// anything else, so the relative forms the README documents are resolved
// before the request (handed over raw they used to buy the daemon's default
// window). `--to now` is left to the daemon.
func TestR6RenderTimeWindow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	pinned := time.Date(2026, 11, 26, 1, 2, 3, 0, time.UTC)
	previous := nowFunc
	nowFunc = func() time.Time { return pinned }
	t.Cleanup(func() { nowFunc = previous })

	var mu sync.Mutex
	var queries []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query())
		mu.Unlock()
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	listen := srv.Listener.Addr().String()

	renders := []struct {
		name     string
		call     func(string, StatsOpts) (string, error)
		noWindow url.Values
	}{
		{"stats", RenderStats, url.Values{}},
		{"analytics", RenderAnalytics, url.Values{"granularity": {"day"}}},
		{"agents", RenderAgents, url.Values{}},
	}
	hourAgo := strconv.FormatInt(pinned.Add(-time.Hour).Unix(), 10)
	for _, r := range renders {
		t.Run(r.name, func(t *testing.T) {
			sent := func(opts StatsOpts) url.Values {
				mu.Lock()
				queries = nil
				mu.Unlock()
				_, _ = r.call(listen, opts) // the stub answers {} — only the request matters here
				mu.Lock()
				defer mu.Unlock()
				if len(queries) != 1 {
					t.Fatalf("daemon received %d requests, want exactly 1", len(queries))
				}
				return queries[0]
			}
			if got := sent(StatsOpts{From: "1h", To: "now"}); got.Get("from") != hourAgo || got.Has("to") {
				t.Errorf("--from 1h --to now sent from=%q to=%q, want from=%s and no `to`", got.Get("from"), got.Get("to"), hourAgo)
			}
			if got := sent(StatsOpts{}); got.Encode() != r.noWindow.Encode() {
				t.Errorf("no window sent %s, want %s", got.Encode(), r.noWindow.Encode())
			}
			if got := sent(StatsOpts{From: "1795654923", To: "2026-11-26T00:00:00Z"}); got.Get("from") != "1795654923" || got.Get("to") != "2026-11-26T00:00:00Z" {
				t.Errorf("absolute forms sent from=%q to=%q, want them verbatim", got.Get("from"), got.Get("to"))
			}
			mu.Lock()
			queries = nil
			mu.Unlock()
			_, err := r.call(listen, StatsOpts{From: "garbage"})
			if err == nil || !strings.Contains(err.Error(), `--from "garbage"`) {
				t.Fatalf("a rejected --from returned %v, want an error naming the flag and the value", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(queries) != 0 {
				t.Errorf("a rejected window still reached the daemon (%d requests)", len(queries))
			}
		})
	}
}
