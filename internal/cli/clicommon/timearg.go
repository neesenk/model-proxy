package clicommon

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// timearg.go is the CLI's single definition of the --from/--to flag forms.
// `audit` resolves them through ParseTimeArg (it reads the log files itself),
// and the commands that hand a window to an admin read endpoint — `stats`
// and `shadow report` — through SetTimeWindow. Before this existed the
// relative forms were `audit`-only: the other commands forwarded them raw and
// the daemon, which parses unix seconds or RFC3339 only, fell back to its
// default window.

// ParseTimeArg resolves one non-empty --from/--to value into the instant it
// names. Accepted forms: "now", a Go duration meaning "that long ago" (1h,
// 30m), an integer day count with a "d" suffix (7d), unix seconds, or RFC3339.
// The relative forms resolve against now. An empty value is an error — the
// caller decides what an unset flag means.
func ParseTimeArg(v string, now time.Time) (time.Time, error) {
	if v == "now" {
		return now, nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		if d < 0 {
			return time.Time{}, fmt.Errorf("duration must not be negative (got %s)", v)
		}
		return now.Add(-d), nil
	}
	// Go durations stop at hours; accept an integer day count (7d) too.
	if strings.HasSuffix(v, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(v, "d")); err == nil {
			if n < 0 {
				return time.Time{}, fmt.Errorf("duration must not be negative (got %s)", v)
			}
			return now.AddDate(0, 0, -n), nil
		}
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(n, 0), nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("use now, a duration (1h, 7d), unix seconds, or RFC3339")
}

// SetTimeWindow writes --from/--to onto q in the forms the admin read
// endpoints accept (unix seconds or RFC3339 — they reject other spellings). A
// value already in an accepted form passes through verbatim; a relative form
// becomes unix seconds. "--to now" stays unset so the daemon's own "now"
// applies when it answers. A rejected value names its flag in the error and
// leaves q untouched.
func SetTimeWindow(q url.Values, from, to string, now time.Time) error {
	resolvedFrom, err := resolveTimeArg(from, now)
	if err != nil {
		return fmt.Errorf("--from %q: %w", from, err)
	}
	resolvedTo := ""
	if to != "now" {
		if resolvedTo, err = resolveTimeArg(to, now); err != nil {
			return fmt.Errorf("--to %q: %w", to, err)
		}
	}
	if resolvedFrom != "" {
		q.Set("from", resolvedFrom)
	}
	if resolvedTo != "" {
		q.Set("to", resolvedTo)
	}
	return nil
}

// resolveTimeArg returns the query value for one flag: "" when unset, the
// original text for an absolute form, unix seconds for a relative one. A bare
// number is read as unix seconds even though "0" also parses as a zero
// duration — /api/analytics treats from=0 as its all-time sentinel.
func resolveTimeArg(v string, now time.Time) (string, error) {
	if v == "" {
		return "", nil
	}
	t, err := ParseTimeArg(v, now)
	if err != nil {
		return "", err
	}
	if _, err := strconv.ParseInt(v, 10, 64); err == nil {
		return v, nil
	}
	if _, err := time.Parse(time.RFC3339, v); err == nil {
		return v, nil
	}
	return strconv.FormatInt(t.Unix(), 10), nil
}
