package clicommon_test

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"model-proxy/internal/cli/clicommon"
)

var r6Now = time.Date(2026, 11, 26, 1, 2, 3, 0, time.UTC)

func r6Unix(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

// TestR6ParseTimeArg pins the CLI's single definition of the --from/--to
// forms: `audit`, `stats` and `shadow report` all resolve their time flags
// through it, so a form one command accepts is a form every command accepts.
func TestR6ParseTimeArg(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	cases := []struct {
		in   string
		want time.Time
	}{
		{"now", r6Now},
		{"1h", r6Now.Add(-time.Hour)},
		{"30m", r6Now.Add(-30 * time.Minute)},
		{"7d", r6Now.AddDate(0, 0, -7)},
		{r6Unix(r6Now), r6Now},
		// "0" is a valid (zero) duration: the instant is now. Callers that
		// pass absolute forms through read it as unix seconds instead — see
		// TestR6SetTimeWindow.
		{"0", r6Now},
		{"2026-11-01T00:00:00+08:00", time.Date(2026, 11, 1, 0, 0, 0, 0, cst)},
		{"2026-11-02T03:04:05Z", time.Date(2026, 11, 2, 3, 4, 5, 0, time.UTC)},
	}
	for _, tc := range cases {
		got, err := clicommon.ParseTimeArg(tc.in, r6Now)
		if err != nil {
			t.Errorf("ParseTimeArg(%q): %v", tc.in, err)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("ParseTimeArg(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct{ in, wantErr string }{
		{"", "use now, a duration (1h, 7d), unix seconds, or RFC3339"},
		{"garbage", "use now, a duration (1h, 7d), unix seconds, or RFC3339"},
		// The daemon never accepted a bare date; it is not a form here either.
		{"2026-01-01", "use now, a duration (1h, 7d), unix seconds, or RFC3339"},
		{"-1h", "must not be negative"},
		{"-7d", "must not be negative"},
	} {
		if _, err := clicommon.ParseTimeArg(tc.in, r6Now); err == nil {
			t.Errorf("ParseTimeArg(%q) = nil error, want a rejected form", tc.in)
		} else if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("ParseTimeArg(%q) error = %q, want it to contain %q", tc.in, err, tc.wantErr)
		}
	}
}

// TestR6SetTimeWindow pins what the admin read endpoints receive: they take
// unix seconds or RFC3339 only, so the relative forms are resolved before the
// request and an accepted spelling is handed through untouched.
func TestR6SetTimeWindow(t *testing.T) {
	cases := []struct {
		name, from, to string
		want           url.Values
	}{
		{"both unset leaves the query untouched", "", "", url.Values{}},
		{"relative window", "1h", "now", url.Values{"from": {r6Unix(r6Now.Add(-time.Hour))}}},
		{"day count", "7d", "", url.Values{"from": {r6Unix(r6Now.AddDate(0, 0, -7))}}},
		{"unix seconds pass through verbatim", "1795654923", "1795657999",
			url.Values{"from": {"1795654923"}, "to": {"1795657999"}}},
		{"RFC3339 passes through verbatim", "2026-11-01T00:00:00+08:00", "2026-11-02T00:00:00Z",
			url.Values{"from": {"2026-11-01T00:00:00+08:00"}, "to": {"2026-11-02T00:00:00Z"}}},
		{"--to now stays unset for the daemon's own now", "1795654923", "now",
			url.Values{"from": {"1795654923"}}},
		{"relative from with an absolute to", "2h", "2026-11-02T00:00:00Z",
			url.Values{"from": {r6Unix(r6Now.Add(-2 * time.Hour))}, "to": {"2026-11-02T00:00:00Z"}}},
		// "0" parses as a zero duration too; it must reach the daemon as the
		// literal sentinel (/api/analytics: from=0 is all-time).
		{"0 passes through as the all-time sentinel", "0", "", url.Values{"from": {"0"}}},
		{"--from now resolves to the current instant", "now", "", url.Values{"from": {r6Unix(r6Now)}}},
		{"a zero duration is relative", "0h", "", url.Values{"from": {r6Unix(r6Now)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := url.Values{}
			if err := clicommon.SetTimeWindow(q, tc.from, tc.to, r6Now); err != nil {
				t.Fatalf("SetTimeWindow(%q, %q): %v", tc.from, tc.to, err)
			}
			if got := q.Encode(); got != tc.want.Encode() {
				t.Errorf("query = %s, want %s", got, tc.want.Encode())
			}
		})
	}
	for _, tc := range []struct{ name, from, to, flag string }{
		{"bad from", "garbage", "", "--from"},
		{"bad to", "1h", "garbage", "--to"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := url.Values{}
			err := clicommon.SetTimeWindow(q, tc.from, tc.to, r6Now)
			if err == nil || !strings.Contains(err.Error(), tc.flag) ||
				!strings.Contains(err.Error(), "use now, a duration (1h, 7d), unix seconds, or RFC3339") {
				t.Fatalf("SetTimeWindow(%q, %q) error = %v, want it to name %s and the accepted forms",
					tc.from, tc.to, err, tc.flag)
			}
			if len(q) != 0 {
				t.Errorf("a rejected window set %s; the query must stay untouched", q.Encode())
			}
		})
	}
}
