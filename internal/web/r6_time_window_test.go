package web

// r6_time_window_test.go — every admin read endpoint that takes a time window
// through the shared from/to parser is fail-closed (R6 B17 follow-up): an
// unparseable bound answers 400 with one error shape and never reaches the
// read port, instead of silently keeping the default window. /api/mcp/analytics
// and /api/requests already were; these pin the other five, plus /api/tokens'
// error body (it had only a status assertion).

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"model-proxy/internal/appapi"
	observestats "model-proxy/internal/observe/stats"
)

const (
	r6BadFrom = "from must be unix seconds or RFC3339"
	r6BadTo   = "to must be unix seconds or RFC3339"
)

// r6WindowProbe records what each time-window endpoint handed to the read
// port, and how many times the port was called.
type r6WindowProbe struct {
	calls     int
	stats     appapi.StatsQuery
	agents    appapi.AgentStatsQuery
	analytics []appapi.AnalyticsQuery
	security  appapi.SecurityQuery
}

// r6WindowServer builds the named endpoint's server over a recording read
// port and returns the stub too, so a test can adjust its other fields.
func r6WindowServer(t *testing.T, name string, probe *r6WindowProbe) (*Server, *readAPIStub) {
	t.Helper()
	reads := &readAPIStub{}
	switch name {
	case "stats":
		reads.stats = func(q appapi.StatsQuery) ([]observestats.Bucket, error) {
			probe.calls++
			probe.stats = q
			return nil, nil
		}
	case "agents":
		reads.agents = func(q appapi.AgentStatsQuery) ([]observestats.AgentBucket, error) {
			probe.calls++
			probe.agents = q
			return nil, nil
		}
	case "analytics":
		reads.analytics = func(q appapi.AnalyticsQuery) ([]observestats.AnalyticsBucket, error) {
			probe.calls++
			probe.analytics = append(probe.analytics, q)
			return nil, nil
		}
	case "security":
		reads.security = func(q appapi.SecurityQuery) (appapi.SecurityResult, error) {
			probe.calls++
			probe.security = q
			return appapi.SecurityResult{}, nil
		}
	default:
		t.Fatalf("unknown time-window endpoint %q", name)
	}
	return newReadServer(t, reads), reads
}

var r6WindowNames = []string{"stats", "agents", "analytics", "security"}

// TestR6TimeWindowEndpointsRejectUnparseableBounds: each endpoint answers 400
// {"error": …} — the /api/tokens shape — for a bound that is neither unix
// seconds nor RFC3339, and the read port is never called.
func TestR6TimeWindowEndpointsRejectUnparseableBounds(t *testing.T) {
	for _, name := range r6WindowNames {
		for _, bad := range []struct{ name, query, wantErr string }{
			{"from", "from=garbage", r6BadFrom},
			{"to", "to=tomorrow", r6BadTo},
			{"from reported before to", "from=garbage&to=tomorrow", r6BadFrom},
		} {
			t.Run(name+"/"+bad.name, func(t *testing.T) {
				probe := &r6WindowProbe{}
				server, _ := r6WindowServer(t, name, probe)
				recorder := serveRead(t, server, http.MethodGet, "/api/"+name+"?"+bad.query)
				if probe.calls != 0 {
					t.Errorf("unparseable bound still reached the read port %d time(s)", probe.calls)
				}
				requireCommandResponse(t, recorder, http.StatusBadRequest, map[string]any{"error": bad.wantErr})
			})
		}
	}
}

// TestR6TokensRejectUnparseableBounds pins the exact 400 body of /api/tokens —
// the message every time-bound endpoint shares. The five endpoints above and
// /api/mcp/analytics (handlers_read_test.go) are already pinned by exact
// body; this closes the family so one shared message cannot drift unnoticed.
func TestR6TokensRejectUnparseableBounds(t *testing.T) {
	server := newReadServer(t, &readAPIStub{})
	for _, tc := range []struct{ name, query, wantErr string }{
		{"from", "from=soon", r6BadFrom},
		{"to", "to=tomorrow", r6BadTo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := serveRead(t, server, http.MethodGet, "/api/tokens?"+tc.query)
			requireCommandResponse(t, recorder, http.StatusBadRequest, map[string]any{"error": tc.wantErr})
		})
	}
}

// TestR6TimeWindowEndpointsKeepValidBounds: unix seconds and RFC3339 bounds
// still reach each read port as the exact window (sibling filters untouched),
// and each endpoint's default window or sentinel keeps working.
func TestR6TimeWindowEndpointsKeepValidBounds(t *testing.T) {
	from := time.Date(2026, 7, 29, 11, 59, 0, 0, time.UTC)
	to := time.Date(2026, 7, 29, 12, 2, 0, 0, time.UTC)
	unix := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }
	forms := []struct{ name, from, to string }{
		{"unix seconds", unix(from), unix(to)},
		{"RFC3339", from.Format(time.RFC3339), to.Format(time.RFC3339)},
	}

	t.Run("stats", func(t *testing.T) {
		for _, form := range forms {
			probe := &r6WindowProbe{}
			server, _ := r6WindowServer(t, "stats", probe)
			recorder := serveRead(t, server, http.MethodGet, "/api/stats?from="+form.from+"&to="+form.to+"&provider=p&model=m&bucket=61s")
			want := appapi.StatsQuery{From: from.Unix(), To: to.Unix(), Provider: "p", Model: "m", BucketSecs: 120}
			var got struct {
				From   int64 `json:"from"`
				To     int64 `json:"to"`
				Bucket int64 `json:"bucket"`
			}
			decodeReadJSON(t, recorder, &got)
			if recorder.Code != http.StatusOK || probe.calls != 1 || probe.stats != want {
				t.Errorf("%s: stats = (%d, query %+v), want 200 with %+v", form.name, recorder.Code, probe.stats, want)
			}
			if got.From != from.Unix() || got.To != to.Unix() || got.Bucket != 120 {
				t.Errorf("%s: stats echo = %+v, want the requested window and bucket", form.name, got)
			}
		}
		// No bound: the default window is still the last hour.
		before := time.Now().Add(-time.Hour).Unix()
		probe := &r6WindowProbe{}
		server, _ := r6WindowServer(t, "stats", probe)
		if recorder := serveRead(t, server, http.MethodGet, "/api/stats"); recorder.Code != http.StatusOK {
			t.Fatalf("default stats window = %d, want 200", recorder.Code)
		}
		after := time.Now().Unix()
		if probe.calls != 1 || probe.stats.From < before || probe.stats.To > after+3600 || probe.stats.To-probe.stats.From != 3600 {
			t.Errorf("default stats window = %+v, want the hour ending now", probe.stats)
		}
	})

	t.Run("agents", func(t *testing.T) {
		for _, form := range forms {
			probe := &r6WindowProbe{}
			server, _ := r6WindowServer(t, "agents", probe)
			recorder := serveRead(t, server, http.MethodGet, "/api/agents?from="+form.from+"&to="+form.to+"&agent=a&provider=p&model=m&bucket=2m")
			want := appapi.AgentStatsQuery{From: from.Unix(), To: to.Unix(), Agent: "a", Provider: "p", Model: "m", BucketSecs: 120}
			if recorder.Code != http.StatusOK || probe.calls != 1 || probe.agents != want {
				t.Errorf("%s: agents = (%d, query %+v), want 200 with %+v", form.name, recorder.Code, probe.agents, want)
			}
		}
	})

	t.Run("analytics", func(t *testing.T) {
		for _, form := range forms {
			probe := &r6WindowProbe{}
			server, _ := r6WindowServer(t, "analytics", probe)
			recorder := serveRead(t, server, http.MethodGet, "/api/analytics?from="+form.from+"&to="+form.to+"&provider=p&model=m&granularity=week")
			want := appapi.AnalyticsQuery{From: from.Unix(), To: to.Unix(), Provider: "p", Model: "m", Granularity: "week", By: "model"}
			var got struct {
				Granularity string `json:"granularity"`
				From        int64  `json:"from"`
				To          int64  `json:"to"`
			}
			decodeReadJSON(t, recorder, &got)
			if recorder.Code != http.StatusOK || len(probe.analytics) == 0 || probe.analytics[0] != want {
				t.Errorf("%s: analytics = (%d, queries %+v), want the main query %+v", form.name, recorder.Code, probe.analytics, want)
			}
			if got.Granularity != "week" || got.From != from.Unix() || got.To != to.Unix() {
				t.Errorf("%s: analytics echo = %+v, want the requested window", form.name, got)
			}
		}
		// from=0 stays the all-time sentinel: clamped to the oldest bucket.
		probe := &r6WindowProbe{}
		server, reads := r6WindowServer(t, "analytics", probe)
		reads.statsSince = 1700000000
		recorder := serveRead(t, server, http.MethodGet, "/api/analytics?from=0&granularity=week")
		if recorder.Code != http.StatusOK || len(probe.analytics) == 0 || probe.analytics[0].From != 1700000000 {
			t.Errorf("all-time analytics = (%d, first query %+v), want from clamped to 1700000000", recorder.Code, probe.analytics)
		}
	})

	t.Run("security", func(t *testing.T) {
		// Mixed forms in one request, converted to the audit log's
		// millisecond domain (to inclusive to the end of the named second).
		probe := &r6WindowProbe{}
		server, _ := r6WindowServer(t, "security", probe)
		recorder := serveRead(t, server, http.MethodGet, "/api/security?kind=secret&from="+unix(from)+"&to="+to.Format(time.RFC3339)+"&limit=5")
		want := appapi.SecurityQuery{Kind: "secret", From: from.Unix() * 1000, To: to.Unix()*1000 + 999, Limit: 5}
		if recorder.Code != http.StatusOK || probe.calls != 1 || probe.security != want {
			t.Errorf("security = (%d, query %+v), want 200 with %+v", recorder.Code, probe.security, want)
		}
		// The Security page's own form: a lone lower bound, default limit.
		probe = &r6WindowProbe{}
		server, _ = r6WindowServer(t, "security", probe)
		recorder = serveRead(t, server, http.MethodGet, "/api/security?from="+unix(from))
		want = appapi.SecurityQuery{From: from.Unix() * 1000, Limit: 100}
		if recorder.Code != http.StatusOK || probe.calls != 1 || probe.security != want {
			t.Errorf("security lower bound only = (%d, query %+v), want 200 with %+v", recorder.Code, probe.security, want)
		}
	})
}
