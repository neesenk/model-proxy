package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"time"
)

// statsOpts holds parsed `stats` command flags.
type statsOpts struct {
	From     string
	To       string
	Provider string
	Model    string
	Bucket   string
	JSON     bool
}

// parseStatsFlags scans `stats` flags: --from/--to (unix sec or RFC3339),
// --provider/--model filters, --bucket (display granularity), --json. --config
// is left to configPath.
func parseStatsFlags(args []string) statsOpts {
	o := statsOpts{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--from", a == "--to", a == "--provider", a == "--model", a == "--bucket":
			if i+1 < len(args) {
				switch a {
				case "--from":
					o.From = args[i+1]
				case "--to":
					o.To = args[i+1]
				case "--provider":
					o.Provider = args[i+1]
				case "--model":
					o.Model = args[i+1]
				case "--bucket":
					o.Bucket = args[i+1]
				}
				i++
			}
		case strings.HasPrefix(a, "--from="):
			o.From = strings.TrimPrefix(a, "--from=")
		case strings.HasPrefix(a, "--to="):
			o.To = strings.TrimPrefix(a, "--to=")
		case strings.HasPrefix(a, "--provider="):
			o.Provider = strings.TrimPrefix(a, "--provider=")
		case strings.HasPrefix(a, "--model="):
			o.Model = strings.TrimPrefix(a, "--model=")
		case strings.HasPrefix(a, "--bucket="):
			o.Bucket = strings.TrimPrefix(a, "--bucket=")
		case a == "--json":
			o.JSON = true
		}
	}
	return o
}

// statsResp is the decoded /api/stats shape.
type statsResp struct {
	From    int64         `json:"from"`
	To      int64         `json:"to"`
	Bucket  int64         `json:"bucket"`
	Buckets []statsBucket `json:"buckets"`
}

// cmdStats queries the running daemon's /api/stats endpoint and prints per-
// (provider, model) call statistics from the SQLite store. The daemon
// (`model-proxy serve`) must be running with web.enabled (default true).
func cmdStats(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	out, err := renderStats(cfg.Listen, parseStatsFlags(args))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %s\n", cRed("✗"), err)
		os.Exit(1)
	}
	fmt.Print(out)
}

// renderStats fetches /api/stats from the daemon and returns either the rendered
// terminal table or the raw JSON (opts.JSON). listen is the daemon "host:port".
// Extracted from cmdStats so tests can drive it against an httptest server.
func renderStats(listen string, opts statsOpts) (string, error) {
	base := "http://" + listen
	q := url.Values{}
	if opts.From != "" {
		q.Set("from", opts.From)
	}
	if opts.To != "" {
		q.Set("to", opts.To)
	}
	if opts.Provider != "" {
		q.Set("provider", opts.Provider)
	}
	if opts.Model != "" {
		q.Set("model", opts.Model)
	}
	if opts.Bucket != "" {
		q.Set("bucket", opts.Bucket)
	}
	body, status, err := statusGet(base, "/api/stats?"+q.Encode())
	if err != nil {
		return "", fmt.Errorf("cannot reach daemon at %s: %v\nis `model-proxy serve` running?", listen, err)
	}
	if status == 404 {
		return "", fmt.Errorf("web UI endpoints not available - is web.enabled true on the daemon?")
	}
	if status != 200 {
		return "", fmt.Errorf("daemon returned HTTP %d: %s", status, truncate(string(body), 200))
	}
	if opts.JSON {
		return string(body), nil
	}
	var resp statsResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parse stats response: %w", err)
	}
	return formatStatsTable(resp), nil
}

// formatStatsTable renders the bucket rows as a compact terminal table. The
// "bucket" column header reflects the display granularity (1m / 10m / 1h ...);
// the cell value is the bucket's start time. Empty result -> a short note.
func formatStatsTable(resp statsResp) string {
	bucketLabel := bucketLabel(resp.Bucket)
	if len(resp.Buckets) == 0 {
		from := time.Unix(resp.From, 0).Format("01-02 15:04")
		to := time.Unix(resp.To, 0).Format("01-02 15:04")
		return fmt.Sprintf("(no stats in range %s .. %s, bucket %s)\n", from, to, bucketLabel)
	}
	hdr := fmt.Sprintf("%-16s %-18s %-12s %8s %8s %8s %8s %10s %10s\n",
		"provider", "model", bucketLabel, "reqs", "failover", "429", "fail", "input", "output")
	out := hdr
	for _, b := range resp.Buckets {
		out += fmt.Sprintf("%-16.16s %-18.18s %-12s %8s %8s %8s %8s %10s %10s\n",
			b.Provider, b.Model,
			time.Unix(b.Minute, 0).Format("01-02 15:04"),
			compactNum(b.Requests), compactNum(b.Failovers),
			compactNum(b.RateLimited429), compactNum(b.Failures),
			compactNum(b.Input), compactNum(b.Output))
	}
	return out
}

// bucketLabel renders a bucket width (seconds) as a short column header. 60 ->
// "1m"; multiples of 3600 -> "Nh"; else "<min>m".
func bucketLabel(secs int64) string {
	if secs <= 60 {
		return "1m"
	}
	if secs%3600 == 0 {
		return fmt.Sprintf("%dh", secs/3600)
	}
	return fmt.Sprintf("%dm", secs/60)
}
