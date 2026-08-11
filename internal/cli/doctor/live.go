package doctor

import (
	"encoding/json"
	"fmt"
	"model-proxy/internal/app"
	"model-proxy/internal/appapi"
	clicommon "model-proxy/internal/cli/clicommon"
	cliframework "model-proxy/internal/cli/framework"
	climodels "model-proxy/internal/cli/models"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/takeover"
	"model-proxy/provider"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// doctor_live.go implements `model-proxy doctor --live`: a read-only, live
// counterpart to the offline doctor. It answers "why is my agent stuck right
// now" by aggregating the daemon's /api/status (health, model locks, quota,
// schedule, warnings), the request log's recent failures, and local takeover
// pointer drift into a conclusion-first report. Pure diagnosis — it changes
// nothing on the daemon or on disk.

// statusModelLock decodes one entry of /api/status model_locks. The field is
// decoded here (not in serve_status.go) because only doctor --live reads it.
type statusModelLock struct {
	Model string `json:"model"`
	Until string `json:"until"` // RFC3339, lockout horizon
}

// doctorRequestsResp decodes the parts of /api/requests doctor --live reads.
type doctorRequestsResp struct {
	Enabled bool `json:"enabled"`
	Records []struct {
		Ts        string `json:"ts"` // RFC3339 UTC
		Exposed   string `json:"exposed"`
		Provider  string `json:"provider"`
		Status    int    `json:"status"`
		LatencyMs int64  `json:"latency_ms"`
	} `json:"records"`
}

// doctorLive reports whether args contain --live. Manual scan, same convention
// as parseStatusFlags (no flag package); --config is handled by configPath.
func DoctorLive(args []string) bool {
	for _, a := range args {
		if a == "--live" {
			return true
		}
	}
	return false
}

// renderDoctorLive fetches the running daemon's status (+ recent failed
// requests) and checks local takeover pointer drift, then renders the
// conclusion-first diagnosis. cfg supplies the listen address, the takeover
// pointer expectations, and the offline route→target expansion used to name
// recovery candidates when a route is fully down (the daemon's schedule only
// lists currently schedulable targets, so a down route arrives with an empty
// ordered list). cfgPath locates the takeover backup markers
// (<configDir>/.model-proxy/). Errors mirror renderStatus exactly.
func RenderDoctorLive(cfg *configdomain.Config, cfgPath string) (string, error) {
	base := "http://" + cfg.Listen
	statusBody, status, err := clicommon.StatusGet(base, "/api/status")
	if err != nil {
		return "", fmt.Errorf("cannot reach daemon at %s: %v\nis `model-proxy serve` running?", cfg.Listen, err)
	}
	if status == 404 {
		return "", fmt.Errorf("web UI endpoints not available — is web.enabled true on the daemon?")
	}
	if status != 200 {
		return "", fmt.Errorf("daemon returned HTTP %d: %s", status, provider.Truncate(string(statusBody), 200))
	}

	// Recent failures are context, not verdict: a fetch failure or a disabled
	// request_log degrades to a dim note, never to a command error.
	reqBody, reqStatus, reqErr := clicommon.StatusGet(base, "/api/requests?errors=1&limit=5")

	var st appapi.StatusResp
	if err := json.Unmarshal(statusBody, &st); err != nil {
		return "", fmt.Errorf("parse status response: %v", err)
	}
	drift := CheckTakeoverDrift(cfg, takeover.BackupDir(cfgPath))

	var b strings.Builder
	fmt.Fprintf(&b, "%s · %s\n", provider.Bold("model-proxy doctor --live"), provider.Dim(base))
	fmt.Fprintf(&b, "%s daemon running (v%s, uptime %s)\n\n", provider.Green("✓"), st.Version, st.Uptime)
	clicommon.AppendSection(&b, RenderDiagnosis(cfg, &st, drift))
	clicommon.AppendSection(&b, clicommon.RenderSchedule(&st))
	if reqErr == nil && reqStatus == 200 {
		clicommon.AppendSection(&b, RenderDoctorFailures(reqBody))
	}
	clicommon.AppendSection(&b, RenderDoctorTakeover(drift))
	return b.String(), nil
}

// diagLine is one conclusion row: severity 0 = ✗ (down), 1 = ⚠ (risk), 2 = ✓
// (healthy route landing). hint is an optional indented follow-up (the "what
// to do about it").
type diagLine struct {
	sev  int
	text string
	hint string
}

// renderDiagnosis applies the doctor --live verdict rules, most severe first:
// route fully down → pin active → first-choice quota nearly exhausted →
// daemon warnings → takeover drift; healthy routes close the section with
// their current landing. All data comes from /api/status + the local drift
// check; nothing is probed live.
func RenderDiagnosis(cfg *configdomain.Config, st *appapi.StatusResp, drift []ClientDrift) string {
	now := time.Now()
	implicit, _ := app.SynthesizeImplicitRoutes(cfg, accountStore())

	routes := make([]string, 0, len(st.Schedule.Models))
	for r := range st.Schedule.Models {
		routes = append(routes, r)
	}
	sort.Strings(routes)

	var errs, warns, oks []diagLine
	for _, r := range routes {
		ri := st.Schedule.Models[r]
		availN := 0
		for _, t := range ri.Ordered {
			if t.Available {
				availN++
			}
		}
		if ri.Pin != "" {
			text := fmt.Sprintf("route %q pinned to %s — no failover while pinned (if %s goes down, the route goes down)",
				r, ri.Pin, ri.Pin)
			if ri.PinExpires != "" {
				text += " (" + ri.PinExpires + ")"
			}
			warns = append(warns, diagLine{1, text, "unpin with: model-proxy unpin " + r})
		}
		if availN == 0 {
			targets := LiveTargets(cfg, implicit, r)
			n := len(targets)
			if n == 0 {
				// Route unknown to this config (CLI and daemon configs differ):
				// still report the outage, just without recovery detail.
				n = len(ri.Ordered)
			}
			text := fmt.Sprintf("route %q: %d %s all unavailable", r, n, cliframework.Plural(n, "target", "targets"))
			hint := ""
			if rec, ok := EarliestRecovery(st, targets, now); ok {
				text += fmt.Sprintf(" — earliest recovery %s (%s, %s)",
					rec.until.Local().Format("15:04"), rec.provider, rec.kind)
				hint = "wait for recovery, or: model-proxy unfreeze " + rec.provider
			}
			errs = append(errs, diagLine{0, text, hint})
			continue
		}
		if q, ok := st.Quota[ri.First]; ok && q.RemainingPct >= 0 && q.RemainingPct <= 0.05 {
			warns = append(warns, diagLine{1, fmt.Sprintf("route %q: %s quota nearly exhausted (%d%% remaining)",
				r, ri.First, int(q.RemainingPct*100+0.5)), ""})
		}
		line := fmt.Sprintf("route %q → %s", r, ri.First)
		if q, ok := st.Quota[ri.First]; ok && q.RemainingPct >= 0 {
			line += fmt.Sprintf(" (%d%% remaining)", int(q.RemainingPct*100+0.5))
		}
		oks = append(oks, diagLine{2, line, ""})
	}
	for _, w := range st.Warnings {
		warns = append(warns, diagLine{1, w, ""})
	}
	for _, d := range drift {
		if d.Taken && !d.OK {
			warns = append(warns, diagLine{1, fmt.Sprintf("takeover drift: %s (%s points to %s, want %s)",
				d.Client, d.File, d.Current, d.Expected),
				"re-run: model-proxy takeover " + d.Client + " (or: model-proxy restore " + d.Client + ")"})
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", provider.Bold("Diagnosis"))
	if len(errs) == 0 && len(warns) == 0 {
		fmt.Fprintf(&b, "  %s\n", provider.Green("✓ no problems found"))
	}
	emit := func(l diagLine) {
		mark := provider.Green("✓")
		switch l.sev {
		case 0:
			mark = provider.Red("✗")
		case 1:
			mark = provider.Yellow("⚠")
		}
		fmt.Fprintf(&b, "  %s %s\n", mark, l.text)
		if l.hint != "" {
			fmt.Fprintf(&b, "    %s\n", provider.Dim("→ "+l.hint))
		}
	}
	for _, l := range errs {
		emit(l)
	}
	for _, l := range warns {
		emit(l)
	}
	for _, l := range oks {
		emit(l)
	}
	return b.String()
}

// liveTargets expands a route's targets to the (provider, model) pairs the
// daemon schedules over: pool parents become their virtual account ids,
// matching the keys /api/status uses in health and model_locks. Returns nil
// when the route is unknown to this config.
func LiveTargets(cfg *configdomain.Config, implicit map[string]configdomain.RouteTarget, route string) []configdomain.RouteTarget {
	targets, ok := cfg.Routes[route]
	if !ok {
		if t, found := implicit[route]; found {
			targets = []configdomain.RouteTarget{t}
		} else {
			return nil
		}
	}
	var out []configdomain.RouteTarget
	for _, t := range targets {
		if vids, pooled := climodels.PoolVirtuals(cfg, t.Provider); pooled {
			for _, vid := range vids {
				out = append(out, configdomain.RouteTarget{Provider: vid, Model: t.Model, Priority: t.Priority})
			}
			continue
		}
		out = append(out, t)
	}
	return out
}

// recovery is the soonest moment a down route's target becomes usable again.
type recovery struct {
	provider string
	kind     string // display label: quota/daily/rate-limit cooldown, circuit breaker, model lock
	until    time.Time
}

// earliestRecovery scans a down route's targets for their cooldown horizons —
// 429 rate-limit, circuit breaker, and (provider, model) lockout — and returns
// the soonest. Model locks are matched by the target's model, not just the
// provider, so a lock on the provider's OTHER models doesn't mislead.
func EarliestRecovery(st *appapi.StatusResp, targets []configdomain.RouteTarget, now time.Time) (recovery, bool) {
	var best recovery
	found := false
	consider := func(provider, kind, untilStr string) {
		if untilStr == "" {
			return
		}
		t, err := time.Parse(time.RFC3339, untilStr)
		if err != nil || !now.Before(t) {
			return
		}
		if !found || t.Before(best.until) {
			best = recovery{provider, kind, t}
			found = true
		}
	}
	for _, tgt := range targets {
		h := st.Health[tgt.Provider]
		kind := "rate-limit cooldown"
		switch h.RateLimitKind {
		case "quota":
			kind = "quota cooldown"
		case "daily":
			kind = "daily cooldown"
		}
		consider(tgt.Provider, kind, h.RateLimitedUntil)
		consider(tgt.Provider, "circuit breaker", h.CircuitUntil)
		for _, l := range st.ModelLocks[tgt.Provider] {
			if tgt.Model == "" || l.Model == tgt.Model {
				consider(tgt.Provider, "model lock", l.Until)
			}
		}
	}
	return best, found
}

// renderDoctorFailures renders the last few failed requests as context for the
// verdict. request_log defaults to off, so the disabled case is a dim hint,
// not an error.
func RenderDoctorFailures(body []byte) string {
	var rr doctorRequestsResp
	if err := json.Unmarshal(body, &rr); err != nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", provider.Bold("Recent failures"))
	switch {
	case !rr.Enabled:
		fmt.Fprintf(&b, "  %s\n", provider.Dim("request_log disabled — set request_log.enabled in config to record requests"))
	case len(rr.Records) == 0:
		fmt.Fprintf(&b, "  %s\n", provider.Dim("none recorded"))
	default:
		for _, r := range rr.Records {
			ts := r.Ts
			if t, err := time.Parse(time.RFC3339, r.Ts); err == nil {
				ts = t.Local().Format("15:04:05")
			}
			statusColor := provider.Yellow
			if r.Status >= 500 {
				statusColor = provider.Red
			}
			route := r.Exposed
			if route == "" {
				route = provider.Dim("(no route)")
			}
			fmt.Fprintf(&b, "  %s  %s → %s  %s  %dms\n",
				provider.Dim(ts), route, r.Provider, statusColor(strconv.Itoa(r.Status)), r.LatencyMs)
		}
	}
	return b.String()
}

// renderDoctorTakeover renders the one-line takeover state: per client either
// not taken over, ✓ (pointer intact), or ✗ drift (details in Diagnosis).
func RenderDoctorTakeover(drift []ClientDrift) string {
	parts := make([]string, 0, len(drift))
	for _, d := range drift {
		switch {
		case !d.Taken:
			parts = append(parts, d.Client+" "+provider.Dim("not taken over"))
		case d.OK:
			parts = append(parts, d.Client+" "+provider.Green("✓"))
		default:
			parts = append(parts, d.Client+" "+provider.Red("✗ drift"))
		}
	}
	return provider.Bold("Takeover") + "\n  " + strings.Join(parts, "  ·  ") + "\n"
}

// ClientDrift is the takeover state of one agent client: taken (a backup
// marker exists) or not; when taken, ok reports whether the client's config
// still points at this proxy. current/expected feed the drift detail line.
type ClientDrift struct {
	Client   string
	File     string
	Taken    bool
	OK       bool
	Current  string
	Expected string
}

// checkTakeoverDrift compares every taken-over client's proxy pointer against
// the value takeover would write today. Drift happens when a client upgrade
// rewrites its config or the proxy's listen address changes — the agent then
// silently talks to a dead endpoint, which looks exactly like "agent stuck".
// Local files only, read-only.
func CheckTakeoverDrift(cfg *configdomain.Config, bakDir string) []ClientDrift {
	pid := takeover.ProviderID(cfg)
	out := []ClientDrift{}
	for _, c := range takeover.ListClients(cfg, "") {
		d := ClientDrift{Client: c.Name, File: c.File}
		if _, err := os.Stat(filepath.Join(bakDir, c.Name+".bak")); err != nil {
			out = append(out, d) // no backup marker → not taken over
			continue
		}
		d.Taken = true
		d.Current, d.Expected = TakeoverPointer(c.Name, c.File, pid, cfg.Takeover.ProxyURL)
		d.OK = d.Current == d.Expected
		out = append(out, d)
	}
	return out
}

// takeoverPointer reads one client's current proxy pointer and computes the
// expected one, mirroring exactly what the client's rewrite in clients.go
// writes (including opencode's /v1 suffix and pi's trimmed base). A missing
// file, unreadable JSON, or absent key yields a descriptive placeholder as
// current, which can never equal the expected URL — i.e. drift.
func TakeoverPointer(client, file, pid, proxyURL string) (current, expected string) {
	if client == "codex" {
		return CodexPointer(file, pid, proxyURL)
	}
	var path []string
	switch client {
	case "claude":
		path, expected = []string{"env", "ANTHROPIC_BASE_URL"}, proxyURL
	case "opencode":
		path, expected = []string{"provider", pid, "options", "baseURL"}, strings.TrimRight(proxyURL, "/")+"/v1"
	case "pi":
		path, expected = []string{"providers", pid, "baseUrl"}, strings.TrimRight(proxyURL, "/")
	default:
		return "(unknown client)", proxyURL
	}
	if _, err := os.Stat(file); err != nil {
		return "(file missing)", expected
	}
	v, err := takeover.ReadJSONConfig(file)
	if err != nil {
		return "(unreadable: " + err.Error() + ")", expected
	}
	s, ok := JSONNestedString(v, path...)
	if !ok {
		return "(missing)", expected
	}
	return s, expected
}

// jsonNestedString walks v along path and returns the terminal string.
func JSONNestedString(v map[string]any, path ...string) (string, bool) {
	cur := v
	for i, k := range path {
		if i == len(path)-1 {
			s, ok := cur[k].(string)
			return s, ok
		}
		next, ok := cur[k].(map[string]any)
		if !ok {
			return "", false
		}
		cur = next
	}
	return "", false
}

// codexPointer is the TOML variant of takeoverPointer: drift when the top-level
// model_provider no longer selects our section, or the section's base_url no
// longer equals the proxy URL. Text scan only (the repo has no TOML decoder);
// it matches the shape rewriteCodex writes, which is all takeover needs.
func CodexPointer(file, pid, proxyURL string) (current, expected string) {
	expected = proxyURL
	data, err := os.ReadFile(file)
	if err != nil {
		return "(file missing)", expected
	}
	modelProvider := ""
	baseURL := ""
	seenSection := false
	inSection := false
	for _, line := range strings.Split(string(data), "\n") {
		l := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(l, "["):
			seenSection = true
			inSection = l == `[model_providers."`+pid+`"]`
		case !seenSection && strings.HasPrefix(l, "model_provider"):
			if i := strings.Index(l, "="); i >= 0 {
				modelProvider = strings.Trim(strings.TrimSpace(l[i+1:]), `"`)
			}
		case inSection && strings.HasPrefix(l, "base_url"):
			if i := strings.Index(l, "="); i >= 0 {
				baseURL = strings.Trim(strings.TrimSpace(l[i+1:]), `"`)
			}
		}
	}
	if modelProvider != pid {
		if modelProvider == "" {
			return "model_provider (missing)", expected
		}
		return "model_provider = " + strconv.Quote(modelProvider), expected
	}
	if baseURL == "" {
		return "(missing)", expected
	}
	return baseURL, expected
}
