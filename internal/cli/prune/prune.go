// Package prune owns the `prune` command: a ONE-SHOT request-log cleanup that
// keeps only the last N days of log files. It deletes rotated requests-/mcp-
// files older than the window in both request-log directories (mtime
// semantics, same as the daemon's retention sweep); the active (today's)
// files and index.db are never touched, request_log.retention is neither read
// nor changed, and the daemon's automatic sweep is unaffected. The trailing
// index stays consistent on its own: the daemon's reconcile (or the next
// startup's rebuild) drops the rows of vanished files.
package prune

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	cliframework "model-proxy/internal/cli/framework"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/display"
	"model-proxy/internal/observe/logfile"
	"model-proxy/internal/observe/requestlog"
)

// stream is one request-log directory to prune.
type stream struct {
	label  string
	dir    string
	prefix string
}

// RunPrune is the process-level entry for `prune`: load config, run the
// one-shot cleanup, exit with its code.
func RunPrune(args []string) {
	cfg := cliframework.LoadCmdConfig(args)
	os.Exit(CmdPrune(args, cfg, os.Stdout, os.Stderr))
}

// CmdPrune implements `model-proxy prune <days> [--dry-run] [--config PATH]`.
// Extracted from RunPrune for tests. Exit 0 on success (nothing to prune is
// success), 1 on usage or filesystem errors.
func CmdPrune(args []string, cfg *configdomain.Config, stdout, stderr io.Writer) int {
	days, dryRun, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "%s %v\n", display.Red("✗"), err)
		fmt.Fprintln(stderr, "usage: model-proxy prune <days> [--dry-run] [--config PATH]")
		fmt.Fprintln(stderr, "  prune 7    delete request-log files older than 7 days (one-shot;")
		fmt.Fprintln(stderr, "              request_log.retention and the automatic sweep are untouched)")
		fmt.Fprintln(stderr, "  --dry-run  list what would be deleted, delete nothing")
		return 1
	}
	now := time.Now()
	cutoff := now.AddDate(0, 0, -days)
	streams := []stream{
		{"request log", cfg.RequestLog.ResolvedDir(), "requests-"},
		{"mcp log", cfg.RequestLog.ResolvedMCPDir(), requestlog.MCPFilePrefix},
	}
	totalFiles, totalBytes := 0, int64(0)
	for _, s := range streams {
		if _, err := os.Stat(s.dir); os.IsNotExist(err) {
			if s.prefix == "requests-" {
				fmt.Fprintf(stderr, "%s request-log directory not found: %s (is request_log enabled?)\n", display.Red("✗"), s.dir)
				return 1
			}
			continue // mcp split stream never created — nothing to do
		}
		if dryRun {
			targets, err := logfile.Prunable(s.dir, s.prefix, cutoff, now)
			if err != nil {
				fmt.Fprintf(stderr, "%s scan %s: %v\n", display.Red("✗"), s.dir, err)
				return 1
			}
			reportDryRun(stdout, s, targets, cutoff)
			totalFiles += len(targets)
			for _, t := range targets {
				totalBytes += t.Size
			}
			continue
		}
		removed, err := logfile.Prune(s.dir, s.prefix, cutoff, now)
		reportRemoved(stdout, removed)
		totalFiles += len(removed)
		for _, t := range removed {
			totalBytes += t.Size
		}
		if err != nil {
			fmt.Fprintf(stderr, "%s prune %s: %v\n", display.Red("✗"), s.dir, err)
			return 1
		}
	}
	verb := "pruned"
	if dryRun {
		verb = "would prune"
	}
	fmt.Fprintf(stdout, "%s %s %d %s (%s)\n", display.Green("✓"), verb, totalFiles,
		cliframework.Plural(totalFiles, "file", "files"), formatBytes(totalBytes))
	return 0
}

// parseArgs extracts the positive day count and flags. `--config` and its
// value are consumed by the config loader, not here; unknown flags and a
// missing/non-positive day count are errors.
func parseArgs(args []string) (days int, dryRun bool, err error) {
	for _, a := range args {
		switch {
		case a == "--config" || a == "-config" || strings.HasPrefix(a, "--config="):
			// consumed by cliframework.ConfigPath
		case a == "--dry-run":
			dryRun = true
		case strings.HasPrefix(a, "-"):
			return 0, false, fmt.Errorf("unknown flag %q", a)
		default:
			if days != 0 {
				return 0, false, fmt.Errorf("unexpected argument %q (days is already given)", a)
			}
			n, perr := strconv.Atoi(a)
			if perr != nil || n < 1 {
				return 0, false, fmt.Errorf("days must be a positive integer, got %q", a)
			}
			days = n
		}
	}
	if days == 0 {
		return 0, false, fmt.Errorf("days is required")
	}
	return days, dryRun, nil
}

func reportDryRun(w io.Writer, s stream, targets []logfile.PruneTarget, cutoff time.Time) {
	fmt.Fprintf(w, "%s (%s) — files older than %s:\n", s.label, s.dir, cutoff.Format("2006-01-02"))
	for _, t := range targets {
		fmt.Fprintf(w, "  %s  %s\n", t.Name, formatBytes(t.Size))
	}
	if len(targets) == 0 {
		fmt.Fprintln(w, "  (nothing older than the window)")
	}
}

func reportRemoved(w io.Writer, removed []logfile.PruneTarget) {
	for _, t := range removed {
		fmt.Fprintf(w, "%s deleted %s  %s\n", display.Green("✓"), t.Name, formatBytes(t.Size))
	}
}

// formatBytes renders a byte count in binary units for humans.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, u := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, u)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/unit)
}
