package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"sort"

	cliframework "model-proxy/internal/cli/framework"
	configdomain "model-proxy/internal/config"
	observeseclog "model-proxy/internal/observe/seclog"
)

// audit.go implements `model-proxy audit`: an offline viewer for the security
// audit log written by the guard pipeline and `doctor --live` drift findings.
// It reads the seclog JSONL directory directly — the daemon does not need to
// be running (same offline semantics as `doctor`). The directory derives from
// guard.audit_path (a file path whose basename is the security-*.log prefix
// family), falling back to ~/.model-proxy/security.log.

// auditStatsLimit caps how many filtered records `--stats` aggregates. Stats
// must summarize the whole filtered set, so the table pager --limit does not
// apply; this larger internal cap bounds the scan instead. Records beyond it
// are dropped newest-first (Query top-K semantics), same as a huge --limit.
const auditStatsLimit = 10000

// auditStatsTopN is how many entries the top-names / top-agents lists keep.
const auditStatsTopN = 10

// AuditOpts holds parsed `audit` command flags. Limit defaults to 50; <= 0
// means no cap. Stats switches to the aggregate view (which ignores Limit).
type AuditOpts struct {
	From  string
	To    string
	Kind  string
	Limit int
	JSON  bool
	Stats bool
}

// ParseAuditFlags scans `audit` flags: --from/--to (now | duration-ago like
// 1h or 7d | unix seconds | RFC3339), --kind (secret|path|drift), --limit N,
// --stats, --json. --config is left to configPath (consumed here only to skip
// its value). An unparseable or NEGATIVE --limit value, an unknown flag, and
// a flag missing its value are immediate errors (a silent 0 would mean
// "no cap" — never what the user mistyped, a negative value silently meaning
// "no cap" hides the typo the same way, and a silently ignored flag hides
// typos).
func ParseAuditFlags(args []string) (AuditOpts, error) {
	o := AuditOpts{Limit: 50}
	// value consumes the next arg as this flag's value; missing = error.
	value := func(name string, i *int) (string, error) {
		if *i+1 >= len(args) {
			return "", fmt.Errorf("%s requires a value", name)
		}
		*i++
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		var err error
		switch {
		case a == "--from":
			if o.From, err = value("--from", &i); err != nil {
				return o, err
			}
		case a == "--to":
			if o.To, err = value("--to", &i); err != nil {
				return o, err
			}
		case a == "--kind":
			if o.Kind, err = value("--kind", &i); err != nil {
				return o, err
			}
		case a == "--limit":
			v, verr := value("--limit", &i)
			if verr != nil {
				return o, verr
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return o, fmt.Errorf("invalid --limit: must be a non-negative integer (default 50, 0 = no cap)")
			}
			o.Limit = n
		case strings.HasPrefix(a, "--from="):
			o.From = strings.TrimPrefix(a, "--from=")
		case strings.HasPrefix(a, "--to="):
			o.To = strings.TrimPrefix(a, "--to=")
		case strings.HasPrefix(a, "--kind="):
			o.Kind = strings.TrimPrefix(a, "--kind=")
		case strings.HasPrefix(a, "--limit="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--limit="))
			if err != nil || n < 0 {
				return o, fmt.Errorf("invalid --limit: must be a non-negative integer (default 50, 0 = no cap)")
			}
			o.Limit = n
		case a == "--json":
			o.JSON = true
		case a == "--stats":
			o.Stats = true
		case a == "--config":
			// Resolved by configPath from the full arg list; skip its value.
			if i+1 < len(args) {
				i++
			}
		case strings.HasPrefix(a, "--config="):
			// Resolved by configPath.
		default:
			return o, fmt.Errorf("unknown flag %q", a)
		}
	}
	return o, nil
}

// AuditDir resolves the security audit log directory for a loaded config:
// the directory half of guard.audit_path (default ~/.model-proxy/security.log).
func AuditDir(cfg *configdomain.Config) string {
	return filepath.Dir(cfg.Guard.AuditPathValue(cliframework.HomeDir()))
}

// CmdAudit renders the security audit log for a loaded config. Config loading
// and process exit stay in the application (RunAudit); Run returns a process
// exit code so the command is testable.
func CmdAudit(args []string, cfg *configdomain.Config, stdout, stderr io.Writer) int {
	opts, err := ParseAuditFlags(args)
	if err != nil {
		fmt.Fprintf(stderr, "%s %s\n", "✗", err)
		return 1
	}
	out, err := RenderAudit(AuditDir(cfg), opts, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "%s %s\n", "✗", err)
		return 1
	}
	fmt.Fprint(stdout, out)
	return 0
}

// RenderAudit queries the seclog directory and returns either the rendered
// terminal table or the records as JSON (opts.JSON). now anchors relative
// --from/--to values; callers pass time.Now(), tests pass a fixed clock.
func RenderAudit(dir string, opts AuditOpts, now time.Time) (string, error) {
	switch opts.Kind {
	case "", observeseclog.KindSecret, observeseclog.KindPath, observeseclog.KindDrift:
	default:
		return "", fmt.Errorf("invalid --kind %q: must be secret, path, or drift", opts.Kind)
	}
	filter := observeseclog.Filter{Kind: opts.Kind, Limit: opts.Limit}
	if opts.Stats {
		// Stats aggregates the whole filtered set: --limit (the table pager)
		// must not truncate the counts, so a larger internal cap applies.
		filter.Limit = auditStatsLimit
	}
	var err error
	if filter.From, err = ParseAuditTime(opts.From, now); err != nil {
		return "", fmt.Errorf("invalid --from %q: %v", opts.From, err)
	}
	if filter.To, err = ParseAuditTime(opts.To, now); err != nil {
		return "", fmt.Errorf("invalid --to %q: %v", opts.To, err)
	}
	if filter.From != 0 && filter.To != 0 && filter.From > filter.To {
		return "", fmt.Errorf("--from is after --to (empty window)")
	}
	result, err := observeseclog.Query(dir, filter)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Sprintf("(no security audit records yet — %s does not exist)\n", dir), nil
		}
		return "", fmt.Errorf("read security audit log %s: %w", dir, err)
	}
	if opts.Stats {
		stats := AggregateAuditStats(result.Records, filter.From, filter.To)
		// The internal auditStatsLimit top-K may have dropped older records:
		// surface that so a partial "total" is never mistaken for the whole
		// filtered count.
		stats.Truncated = result.Truncated
		if opts.JSON {
			data, err := json.Marshal(stats)
			if err != nil {
				return "", fmt.Errorf("encode audit stats: %w", err)
			}
			return string(data) + "\n", nil
		}
		out := FormatAuditStats(stats, dir)
		if result.Truncated {
			out += fmt.Sprintf("  (truncated at %d newest records — counts cover only those)\n", auditStatsLimit)
		}
		if result.Skipped > 0 {
			out += fmt.Sprintf("  (%d unreadable %s skipped)\n", result.Skipped, Plural(result.Skipped, "line", "lines"))
		}
		return out, nil
	}
	if opts.JSON {
		records := result.Records
		if records == nil {
			records = []*observeseclog.Record{}
		}
		data, err := json.Marshal(records)
		if err != nil {
			return "", fmt.Errorf("encode audit records: %w", err)
		}
		return string(data) + "\n", nil
	}
	out := FormatAuditTable(result.Records, dir)
	if result.Skipped > 0 {
		out += fmt.Sprintf("  (%d unreadable %s skipped)\n", result.Skipped, Plural(result.Skipped, "line", "lines"))
	}
	return out, nil
}

// ParseAuditTime parses one --from/--to value into unix milliseconds (0 =
// unbounded). Accepted forms: "now", a Go duration meaning "that long ago"
// (1h, 30m), a day count with a "d" suffix (7d), unix seconds, or RFC3339.
// stats has no reusable helper — its --from is parsed server-side by the
// daemon — so the CLI forms live here.
func ParseAuditTime(v string, now time.Time) (int64, error) {
	if v == "" {
		return 0, nil
	}
	if v == "now" {
		return now.UnixMilli(), nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		if d < 0 {
			return 0, fmt.Errorf("duration must not be negative (got %s)", v)
		}
		return now.Add(-d).UnixMilli(), nil
	}
	// Go durations stop at hours; accept an integer day count (7d) too.
	if strings.HasSuffix(v, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(v, "d")); err == nil {
			if n < 0 {
				return 0, fmt.Errorf("duration must not be negative (got %s)", v)
			}
			return now.AddDate(0, 0, -n).UnixMilli(), nil
		}
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n * 1000, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UnixMilli(), nil
	}
	return 0, fmt.Errorf("use now, a duration (1h, 7d), unix seconds, or RFC3339")
}

// FormatAuditTable renders audit records (newest first, as Query returns
// them) as a compact terminal table: local time, kind, agent, route
// (exposed), hit names (comma-joined), action, detail. Empty result -> a
// short note naming the directory that was scanned.
func FormatAuditTable(records []*observeseclog.Record, dir string) string {
	if len(records) == 0 {
		return fmt.Sprintf("(no security audit records in %s)\n", dir)
	}
	out := fmt.Sprintf("%-14s %-7s %-12s %-16s %-20s %-7s %s\n",
		"time", "kind", "agent", "route", "names", "action", "detail")
	for _, r := range records {
		out += fmt.Sprintf("%-14s %-7.7s %-12.12s %-16.16s %-20.20s %-7.7s %s\n",
			time.UnixMilli(r.Ts).Format("01-02 15:04:05"),
			r.Kind, r.Agent, r.Exposed, strings.Join(r.Names, ","), r.Action, sanitizeAuditDetail(r.Detail))
	}
	return out
}

// AuditStatCount is one (name, count) pair in a top-N list.
type AuditStatCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// AuditStats is the aggregate view of a filtered audit record set: total
// record count, hits by kind, top hit names (Record.Names expanded) and
// agents, and counts by action. From/To echo the query window (0 =
// unbounded, omitted from JSON). Truncated marks that the aggregation saw
// only the newest auditStatsLimit records of the filtered set, so Total and
// every count are lower bounds (omitted when false). Lists are ordered by
// count desc, name asc, so both the table and the JSON are deterministic.
type AuditStats struct {
	From      int64            `json:"from,omitempty"`
	To        int64            `json:"to,omitempty"`
	Total     int              `json:"total"`
	Truncated bool             `json:"truncated,omitempty"`
	ByKind    map[string]int   `json:"by_kind"`
	ByAction  map[string]int   `json:"by_action"`
	TopNames  []AuditStatCount `json:"top_names"`
	TopAgents []AuditStatCount `json:"top_agents"`
}

// AggregateAuditStats counts the filtered records into an AuditStats. Records
// with an empty agent or action are left out of those dimensions (their kind
// and names still count).
func AggregateAuditStats(records []*observeseclog.Record, from, to int64) *AuditStats {
	stats := &AuditStats{
		From:      from,
		To:        to,
		Total:     len(records),
		ByKind:    map[string]int{},
		ByAction:  map[string]int{},
		TopNames:  []AuditStatCount{},
		TopAgents: []AuditStatCount{},
	}
	names := map[string]int{}
	agents := map[string]int{}
	for _, r := range records {
		stats.ByKind[r.Kind]++
		if r.Action != "" {
			stats.ByAction[r.Action]++
		}
		for _, name := range r.Names {
			names[name]++
		}
		if r.Agent != "" {
			agents[r.Agent]++
		}
	}
	stats.TopNames = topAuditCounts(names, auditStatsTopN)
	stats.TopAgents = topAuditCounts(agents, auditStatsTopN)
	return stats
}

// topAuditCounts flattens a count map into pairs ordered by count desc, name
// asc (deterministic ties), keeping at most n.
func topAuditCounts(counts map[string]int, n int) []AuditStatCount {
	pairs := make([]AuditStatCount, 0, len(counts))
	for name, count := range counts {
		pairs = append(pairs, AuditStatCount{Name: name, Count: count})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Count != pairs[j].Count {
			return pairs[i].Count > pairs[j].Count
		}
		return pairs[i].Name < pairs[j].Name
	})
	if len(pairs) > n {
		pairs = pairs[:n]
	}
	return pairs
}

// FormatAuditStats renders the aggregate view: a header with the query window
// and total record count, then compact count tables — by kind, top names,
// top agents, by action. Empty result keeps the header and adds the same
// no-records note as the raw table.
func FormatAuditStats(stats *AuditStats, dir string) string {
	window := func(v int64) string {
		if v == 0 {
			return "-"
		}
		return time.UnixMilli(v).Format("2006-01-02 15:04:05")
	}
	out := fmt.Sprintf("security audit stats  range: %s .. %s  total: %d %s\n",
		window(stats.From), window(stats.To), stats.Total, Plural(stats.Total, "record", "records"))
	if stats.Total == 0 {
		return out + fmt.Sprintf("(no security audit records in %s)\n", dir)
	}
	out += formatAuditStatSection("by kind", topAuditCounts(stats.ByKind, len(stats.ByKind)))
	out += formatAuditStatSection(fmt.Sprintf("top names (top %d)", auditStatsTopN), stats.TopNames)
	out += formatAuditStatSection(fmt.Sprintf("top agents (top %d)", auditStatsTopN), stats.TopAgents)
	out += formatAuditStatSection("by action", topAuditCounts(stats.ByAction, len(stats.ByAction)))
	return out
}

// formatAuditStatSection renders one count table: an indented name column
// sized to the longest entry, counts right-aligned. Empty sections are
// omitted entirely.
func formatAuditStatSection(title string, pairs []AuditStatCount) string {
	if len(pairs) == 0 {
		return ""
	}
	width := 0
	for _, p := range pairs {
		if len(p.Name) > width {
			width = len(p.Name)
		}
	}
	out := "\n" + title + "\n"
	for _, p := range pairs {
		out += fmt.Sprintf("  %-*s %6d\n", width, p.Name, p.Count)
	}
	return out
}

// sanitizeAuditDetail replaces control characters (newlines, tabs, ANSI
// escapes, ...) with spaces so a future producer can never break the table
// layout or inject terminal sequences through the detail column.
func sanitizeAuditDetail(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}
