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

// AuditOpts holds parsed `audit` command flags. Limit defaults to 50; <= 0
// means no cap.
type AuditOpts struct {
	From  string
	To    string
	Kind  string
	Limit int
	JSON  bool
}

// ParseAuditFlags scans `audit` flags: --from/--to (now | duration-ago like
// 1h | unix seconds | RFC3339), --kind (secret|path|drift), --limit N,
// --json. --config is left to configPath. An unparseable --limit value is an
// immediate error (a silent 0 would mean "no cap" — never what the user
// mistyped).
func ParseAuditFlags(args []string) (AuditOpts, error) {
	o := AuditOpts{Limit: 50}
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case a == "--from":
			o.From = value()
		case a == "--to":
			o.To = value()
		case a == "--kind":
			o.Kind = value()
		case a == "--limit":
			n, err := strconv.Atoi(value())
			if err != nil {
				return o, fmt.Errorf("invalid --limit: must be an integer (default 50, 0 = no cap)")
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
			if err != nil {
				return o, fmt.Errorf("invalid --limit: must be an integer (default 50, 0 = no cap)")
			}
			o.Limit = n
		case a == "--json":
			o.JSON = true
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
	var err error
	if filter.From, err = ParseAuditTime(opts.From, now); err != nil {
		return "", fmt.Errorf("invalid --from %q: %v", opts.From, err)
	}
	if filter.To, err = ParseAuditTime(opts.To, now); err != nil {
		return "", fmt.Errorf("invalid --to %q: %v", opts.To, err)
	}
	result, err := observeseclog.Query(dir, filter)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Sprintf("(no security audit records yet — %s does not exist)\n", dir), nil
		}
		return "", fmt.Errorf("read security audit log %s: %w", dir, err)
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
// (1h, 30m), unix seconds, or RFC3339. stats has no reusable helper — its
// --from is parsed server-side by the daemon — so the CLI forms live here.
func ParseAuditTime(v string, now time.Time) (int64, error) {
	if v == "" {
		return 0, nil
	}
	if v == "now" {
		return now.UnixMilli(), nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		return now.Add(-d).UnixMilli(), nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n * 1000, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UnixMilli(), nil
	}
	return 0, fmt.Errorf("use now, a duration (1h), unix seconds, or RFC3339")
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
			r.Kind, r.Agent, r.Exposed, strings.Join(r.Names, ","), r.Action, r.Detail)
	}
	return out
}
