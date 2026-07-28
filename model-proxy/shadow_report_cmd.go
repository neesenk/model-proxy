package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"

	"model-proxy/internal/observe/requestlog"
)

// shadow_report_cmd.go implements `model-proxy shadow report` — the shadow
// evaluation aggregation report (primary vs shadow comparison by request_id
// pairing). It queries the daemon's /api/shadow-report endpoint and renders a
// table: per (route, primary, shadow), samples, status-match rate, latency diff,
// response-size ratio.

func cmdShadow(args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "%s usage: model-proxy shadow report [--from --to --config]\n", cRed("✗"))
		os.Exit(1)
	}
	switch args[0] {
	case "report":
		cmdShadowReport(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "%s unknown shadow subcommand: %s (try 'report')\n", cRed("✗"), args[0])
		os.Exit(1)
	}
}

func cmdShadowReport(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %s\n", cRed("✗"), err)
		os.Exit(1)
	}
	base := "http://" + cfg.Listen
	q := makeURLQuery(args)
	resp, err := daemonHTTPClient.Get(base + "/api/shadow-report?" + q.Encode())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s cannot reach daemon at %s: %v\nis `model-proxy serve` running?\n", cRed("✗"), cfg.Listen, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "%s daemon returned HTTP %d\n", cRed("✗"), resp.StatusCode)
		os.Exit(1)
	}
	var out struct {
		Enabled bool                           `json:"enabled"`
		Entries []requestlog.ShadowReportEntry `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		fmt.Fprintf(os.Stderr, "%s parse response: %v\n", cRed("✗"), err)
		os.Exit(1)
	}
	if !out.Enabled {
		fmt.Println("(request logging is off — enable request_log to collect shadow data)")
		return
	}
	if len(out.Entries) == 0 {
		fmt.Println("(no paired shadow samples in range)")
		return
	}
	fmt.Printf("%-16s %-12s %-12s %6s %7s %7s %7s %10s %10s\n",
		"ROUTE", "PRIMARY", "SHADOW", "SAMPLES", "MATCH", "P_LAT", "S_LAT", "P_BYTES", "S_BYTES")
	for _, e := range out.Entries {
		fmt.Printf("%-16.16s %-12.12s %-12.12s %6d %6.0f%% %7s %7s %10s %10s\n",
			e.Route, e.PrimaryProvider, e.ShadowProvider, e.Samples,
			e.StatusMatchRate*100,
			fmt.Sprintf("%dms", e.PrimaryLatencyMs),
			fmt.Sprintf("%dms", e.ShadowLatencyMs),
			compactNum(uint64(e.PrimarySizeAvg)), compactNum(uint64(e.ShadowSizeAvg)))
	}
}

// makeURLQuery builds url.Values from --from/--to flags.
func makeURLQuery(args []string) url.Values {
	q := url.Values{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--from":
			if i+1 < len(args) {
				q.Set("from", args[i+1])
				i++
			}
		case "--to":
			if i+1 < len(args) {
				q.Set("to", args[i+1])
				i++
			}
		}
	}
	return q
}
