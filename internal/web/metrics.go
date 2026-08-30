package web

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"model-proxy/internal/appapi"
)

// handleMetrics serves GET /metrics — a Prometheus text-exposition (format
// version 0.0.4) of the per-provider counter snapshot. Series are derived from
// the same detached Dashboard snapshot as /api/status (no new lock surface;
// the read view owns Proxy lock discipline). Virtual counter keys (guard,
// attempts, fusion, routing) surface as ordinary providers, matching their
// /api/stats semantics. GuardBrowserOrigin applies: scrapers send no browser
// identity headers and pass untouched.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	auth := s.captureAdminAuth()
	if !s.guardAdminAuth(w, r, auth) {
		return
	}
	if !guardBrowserOrigin(w, r, auth.enabled, s.browserListen) {
		return
	}
	if r.Method != http.MethodGet {
		writeJSONErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	v := s.reads.Dashboard(time.Now())
	body := renderPrometheus(v)
	w.Header().Set("content-type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(body))
}

// renderPrometheus renders the exposition body. Provider label values are
// config keys / virtual ids; escapeLabel keeps the output well-formed even if
// a name ever contains reserved characters.
func renderPrometheus(v appapi.Dashboard) string {
	var b strings.Builder

	help := func(name, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	}
	helpSum := func(name, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	}

	names := make([]string, 0, len(v.Counters))
	for name := range v.Counters {
		names = append(names, name)
	}
	sort.Strings(names)

	type series struct {
		metric string
		value  func(m appapi.Metrics) uint64
	}
	counters := []series{
		{"model_proxy_requests_total", func(m appapi.Metrics) uint64 { return m.Requests }},
		{"model_proxy_failures_total", func(m appapi.Metrics) uint64 { return m.Failures }},
		{"model_proxy_failovers_total", func(m appapi.Metrics) uint64 { return m.Failovers }},
		{"model_proxy_rate_limited_429_total", func(m appapi.Metrics) uint64 { return m.RateLimited429 }},
	}
	for _, se := range counters {
		help(se.metric, "Cumulative counter from the proxy's in-memory metrics store, aggregated by provider (virtual keys: guard, attempts, fusion, routing).")
		for _, name := range names {
			m := v.Counters[name]
			if m.Requests == 0 && m.Failures == 0 && m.Failovers == 0 && m.RateLimited429 == 0 {
				continue // no traffic on this provider — skip zero series
			}
			fmt.Fprintf(&b, "%s{provider=%q} %d\n", se.metric, name, se.value(m))
		}
	}
	sums := []series{
		{"model_proxy_latency_milliseconds_sum", func(m appapi.Metrics) uint64 { return m.LatencySum }},
		{"model_proxy_ttft_milliseconds_sum", func(m appapi.Metrics) uint64 { return m.TTFTSum }},
	}
	for _, se := range sums {
		helpSum(se.metric, "Cumulative millisecond sum over committed responses; average = sum / requests_total.")
		for _, name := range names {
			m := v.Counters[name]
			if m.Requests == 0 {
				continue
			}
			fmt.Fprintf(&b, "%s{provider=%q} %d\n", se.metric, name, se.value(m))
		}
	}
	return b.String()
}
