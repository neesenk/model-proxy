package status

// CmdCache — `cache`: fetch the exact-response cache counters from the
// running daemon's /api/status (the same detached Dashboard snapshot the Web
// UI Status Cache section renders) and show entries, hits, misses and the
// derived hit rate. A disabled cache prints a hint instead of zero rows.

import (
	"encoding/json"
	"fmt"
	"model-proxy/internal/appapi"
	clicommon "model-proxy/internal/cli/clicommon"
	cliframework "model-proxy/internal/cli/framework"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/display"
	"os"
	"strings"
)

// CacheStats aliases the shared /api/status `cache` DTO (internal/appapi).
type CacheStats = appapi.CacheStats

// RunCache is the registered entrypoint: `model-proxy cache`.
func RunCache(args []string) { CmdCache(args, cliframework.LoadCmdConfig(args)) }

// CmdCache fetches and renders the cache stats. cfg.Listen selects the daemon;
// any --json flag in args dumps the raw cache object instead of the table.
func CmdCache(args []string, cfg *configdomain.Config) {
	jsonOut := false
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
		}
	}

	body, code, err := clicommon.StatusGet("http://"+cfg.Listen, "/api/status")
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s cannot reach daemon at %s: %v\nis `model-proxy serve` running?\n",
			display.Red("✗"), cfg.Listen, err)
		os.Exit(1)
	}
	if code == 404 {
		fmt.Fprintf(os.Stderr, "%s web UI endpoints not available — is web.enabled true on the daemon?\n", display.Red("✗"))
		os.Exit(1)
	}
	if code != 200 {
		fmt.Fprintf(os.Stderr, "%s daemon returned HTTP %d: %s\n", display.Red("✗"), code, display.Truncate(string(body), 200))
		os.Exit(1)
	}
	var st StatusResp
	if err := json.Unmarshal(body, &st); err != nil {
		fmt.Fprintf(os.Stderr, "%s parse status response: %v\n", display.Red("✗"), err)
		os.Exit(1)
	}
	cache := st.Cache
	if cache == nil {
		cache = &CacheStats{}
	}
	if !cache.Enabled {
		fmt.Println("(exact response cache is disabled — set cache.enabled: true in config)")
		return
	}
	if jsonOut {
		enc, _ := json.MarshalIndent(cache, "", "  ")
		fmt.Println(string(enc))
		return
	}
	fmt.Print(RenderCacheStats(cache))
}

// RenderCacheStats renders the global counters block followed by the per-model
// breakdown (one row per called model, sorted by name): entries/hits/misses
// plus the hit rate over all recorded lookups (— before the first one).
func RenderCacheStats(c *CacheStats) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", display.Bold("Exact response cache"))
	fmt.Fprintf(&b, "  %s %d\n", display.Pad("entries (live)", 15), c.Entries)
	fmt.Fprintf(&b, "  %s %d\n", display.Pad("hits", 15), c.Hits)
	fmt.Fprintf(&b, "  %s %d\n", display.Pad("misses", 15), c.Misses)
	fmt.Fprintf(&b, "  %s %s\n", display.Pad("hit rate", 15), CacheHitRate(c.Hits, c.Misses))
	if len(c.Models) == 0 {
		return b.String()
	}
	fmt.Fprintf(&b, "\n  %s  %8s  %8s  %8s  %9s\n",
		display.Pad("MODEL", 20), "ENTRIES", "HITS", "MISSES", "HIT RATE")
	for _, m := range c.Models {
		fmt.Fprintf(&b, "  %s  %8d  %8d  %8d  %9s\n",
			display.Pad(m.Model, 20), m.Entries, m.Hits, m.Misses, CacheHitRate(m.Hits, m.Misses))
	}
	return b.String()
}

// CacheHitRate returns hits/(hits+misses) as a percentage with one decimal,
// or "—" when no lookup has been recorded yet.
func CacheHitRate(hits, misses uint64) string {
	total := hits + misses
	if total == 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(hits)/float64(total))
}
