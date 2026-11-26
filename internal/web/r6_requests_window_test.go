package web

// r6_requests_window_test.go — GET /api/requests from/to selectors are
// fail-closed (R6 B17): an unparseable bound is a 400 with the /api/tokens
// error shape, never a silently unbounded scan over the whole request log.

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"model-proxy/internal/observe/requestlog"
)

// r6RecordingQueries is the directory-scan query port with the list filter
// recorded, so a test can see both what window reached the scan and how many
// scans ran.
type r6RecordingQueries struct {
	dirRequestLogQueries
	calls []requestlog.Filter
}

func (q *r6RecordingQueries) SummariesWithFacets(f requestlog.Filter) ([]requestlog.Summary, requestlog.Facets, error) {
	q.calls = append(q.calls, f)
	return q.dirRequestLogQueries.SummariesWithFacets(f)
}

// TestR6RequestsListRejectsUnparseableWindow: a from/to that is neither unix
// seconds nor RFC3339 answers 400 and never reaches the query port; valid
// bounds keep narrowing the window exactly as before.
func TestR6RequestsListRejectsUnparseableWindow(t *testing.T) {
	tmp := t.TempDir()
	writeReadLog(t, tmp,
		requestlog.Record{Ts: "2026-07-29T12:00:00Z", RequestID: "old", CalledModel: "m", Provider: "p", Status: 200},
		requestlog.Record{Ts: "2026-07-29T12:01:00Z", RequestID: "wanted", CalledModel: "m", Provider: "p", Status: 200},
	)
	queries := &r6RecordingQueries{dirRequestLogQueries: dirRequestLogQueries{tmp}}
	reads := &readAPIStub{queries: queries}
	s := newReadServer(t, reads)

	for _, tc := range []struct{ name, query, wantErr string }{
		{"from", "from=garbage", "from must be unix seconds or RFC3339"},
		{"to", "to=tomorrow", "to must be unix seconds or RFC3339"},
		{"from is reported before to", "from=garbage&to=tomorrow", "from must be unix seconds or RFC3339"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			queries.calls = nil
			recorder := serveRead(t, s, http.MethodGet, "/api/requests?"+tc.query)
			if len(queries.calls) != 0 {
				t.Errorf("unparseable window still ran %d list scan(s) with an unbounded From: %v",
					len(queries.calls), queries.calls[0].From.IsZero())
			}
			requireCommandResponse(t, recorder, http.StatusBadRequest, map[string]any{"error": tc.wantErr})
		})
	}

	// Valid bounds narrow the scan window to exactly the requested seconds:
	// unix seconds (what the minute picker sends) and RFC3339 alike, with the
	// other filters riding along unchanged.
	from := time.Date(2026, 7, 29, 12, 0, 30, 0, time.UTC)
	to := time.Date(2026, 7, 29, 12, 2, 0, 0, time.UTC)
	unix := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }
	for _, tc := range []struct {
		query    string
		provider string
		limit    int
	}{
		{"from=" + unix(from) + "&to=" + unix(to) + "&provider=p&limit=7", "p", 7},
		{"from=" + from.Format(time.RFC3339) + "&to=" + to.Format(time.RFC3339), "", 100},
	} {
		queries.calls = nil
		recorder := serveRead(t, s, http.MethodGet, "/api/requests?"+tc.query)
		if len(queries.calls) != 1 {
			t.Fatalf("%s: list scans = %d, want 1", tc.query, len(queries.calls))
		}
		if got := queries.calls[0]; !got.From.Equal(from) || !got.To.Equal(to) || got.Provider != tc.provider || got.Limit != tc.limit || !got.UsageOnly {
			t.Errorf("%s: scan filter = %+v, want window [%s, %s]", tc.query, got, from, to)
		}
		var got struct {
			Enabled bool                 `json:"enabled"`
			Records []requestlog.Summary `json:"records"`
		}
		decodeReadJSON(t, recorder, &got)
		if recorder.Code != http.StatusOK || !got.Enabled || len(got.Records) != 1 || got.Records[0].RequestID != "wanted" {
			t.Errorf("%s: list = (%d, %d records), want 200 with only %q", tc.query, recorder.Code, len(got.Records), "wanted")
		}
	}
	// Keyset pagination sends only to=<oldest loaded second>; a lone bound
	// stays valid.
	queries.calls = nil
	recorder := serveRead(t, s, http.MethodGet, "/api/requests?to="+unix(from))
	if len(queries.calls) != 1 || !queries.calls[0].From.IsZero() || !queries.calls[0].To.Equal(from) || recorder.Code != http.StatusOK {
		t.Errorf("to-only window = (%d, filter %+v), want 200 with From unbounded", recorder.Code, queries.calls)
	}

	// No query port (request logging off) keeps its disabled shape: there is
	// no scan to bound, so the selector is not validated.
	reads.queries = nil
	recorder = serveRead(t, s, http.MethodGet, "/api/requests?from=garbage")
	var got struct {
		Enabled bool `json:"enabled"`
	}
	decodeReadJSON(t, recorder, &got)
	if recorder.Code != http.StatusOK || got.Enabled {
		t.Errorf("logging off + unparseable from = (%d, enabled %v), want 200 disabled shape", recorder.Code, got.Enabled)
	}
}
