package diag

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	cliframework "model-proxy/internal/cli/framework"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/display"
	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/pricing"

	_ "modernc.org/sqlite"
)

// CmdRouting dispatches `model-proxy routing <subcommand>`.
func CmdRouting(args []string, cfg *configdomain.Config) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "%s usage: model-proxy routing report [--route NAME] [--since DUR|TIME] [--retry-window DUR] [--json] [--config PATH]\n", display.Red("✗"))
		os.Exit(1)
	}
	switch args[0] {
	case "report":
		CmdRoutingReport(args[1:], cfg)
	default:
		fmt.Fprintf(os.Stderr, "%s unknown routing subcommand: %s (try 'report')\n", display.Red("✗"), args[0])
		os.Exit(1)
	}
}

// CmdRoutingReport is the offline L1 reconciliation report.
func CmdRoutingReport(args []string, cfg *configdomain.Config) {
	opts := parseRoutingReportArgs(args)
	dir := cfg.RequestLog.ResolvedDir()
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			fmt.Println("(no request log directory — enable request_log to collect routing data)")
			return
		}
		fmt.Fprintf(os.Stderr, "%s cannot stat request log dir %s: %v\n", display.Red("✗"), dir, err)
		os.Exit(1)
	}

	indexPath := filepath.Join(dir, "index.db")
	if _, err := os.Stat(indexPath); err != nil {
		if os.IsNotExist(err) {
			fmt.Println("(no request log index yet — routing data will appear after the indexer runs)")
			return
		}
		fmt.Fprintf(os.Stderr, "%s cannot stat request log index: %v\n", display.Red("✗"), err)
		os.Exit(1)
	}

	db, err := openRoutingIndexDBReadOnly(indexPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s cannot open request log index: %v\n", display.Red("✗"), err)
		os.Exit(1)
	}
	defer db.Close()

	rows, err := queryRoutingRows(dir, db, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s query request log index: %v\n", display.Red("✗"), err)
		os.Exit(1)
	}

	report := buildRoutingReport(rows, cfg, opts)
	if opts.JSON {
		renderRoutingReportJSON(report)
		return
	}
	renderRoutingReportText(report)
}

type routingReportOptions struct {
	Route       string
	Since       time.Time
	RetryWindow time.Duration
	JSON        bool
	ConfigPath  string
}

func parseRoutingReportArgs(args []string) routingReportOptions {
	opts := routingReportOptions{
		Since:       time.Now().UTC().Add(-7 * 24 * time.Hour),
		RetryWindow: 10 * time.Minute,
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--route":
			if i+1 < len(args) {
				opts.Route = args[i+1]
				i++
			}
		case "--since":
			if i+1 < len(args) {
				opts.Since = parseRoutingReportTime(args[i+1])
				i++
			}
		case "--retry-window":
			if i+1 < len(args) {
				d, err := time.ParseDuration(args[i+1])
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s invalid --retry-window %q (use a Go duration like 10m)\n", display.Red("✗"), args[i+1])
					os.Exit(1)
				}
				opts.RetryWindow = d
				i++
			}
		case "--json":
			opts.JSON = true
		case "--config":
			if i+1 < len(args) {
				opts.ConfigPath = args[i+1]
				i++
			}
		}
	}
	return opts
}

func parseRoutingReportTime(s string) time.Time {
	if s == "" {
		return time.Now().UTC().Add(-7 * 24 * time.Hour)
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().UTC().Add(-d)
	}
	fmt.Fprintf(os.Stderr, "%s invalid --since %q (use RFC3339 or a Go duration like 7d, 24h)\n", display.Red("✗"), s)
	os.Exit(1)
	return time.Time{}
}

func openRoutingIndexDBReadOnly(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

type routingRow struct {
	RequestID     string
	Ts            time.Time
	Status        int
	ResponseSize  int64
	SessionID     string
	TurnKey       string
	Routing       *configdomain.RoutingDecision
	Exposed       string
	Provider      string
	UpstreamModel string
	Input         uint64
	Output        uint64
	CacheRead     uint64
	CacheCreation uint64
	LatencyMs     int64
	Diagnostics   []requestlog.ConversionDiagnostic
}

func queryRoutingRows(dir string, db *sql.DB, opts routingReportOptions) ([]routingRow, error) {
	query := `SELECT request_id, ts_ms, status, response_size, session_id, turn_key, routing,
		exposed, provider, upstream_model, input, output, cache_read, cache_creation, latency_ms,
		file, "offset", length
		FROM records WHERE ts_ms > 0 AND ts_ms >= ?`
	args := []any{opts.Since.UnixMilli()}
	if opts.Route != "" {
		query += ` AND exposed = ?`
		args = append(args, opts.Route)
	}
	query += ` ORDER BY ts_ms ASC`

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []routingRow
	for rows.Next() {
		var r routingRow
		var tsMs int64
		var routingJSON sql.NullString
		var loc fileLocation
		if err := rows.Scan(
			&r.RequestID, &tsMs, &r.Status, &r.ResponseSize,
			&r.SessionID, &r.TurnKey, &routingJSON,
			&r.Exposed, &r.Provider, &r.UpstreamModel,
			&r.Input, &r.Output, &r.CacheRead, &r.CacheCreation, &r.LatencyMs,
			&loc.file, &loc.offset, &loc.length,
		); err != nil {
			return nil, err
		}
		r.Ts = time.UnixMilli(tsMs).UTC()
		if routingJSON.Valid && routingJSON.String != "" {
			var rd configdomain.RoutingDecision
			if err := json.Unmarshal([]byte(routingJSON.String), &rd); err == nil {
				r.Routing = &rd
			}
		}
		r.Diagnostics = readRecordDiagnostics(dir, loc)
		out = append(out, r)
	}
	return out, rows.Err()
}

type fileLocation struct {
	file   string
	offset int64
	length int64
}

func readRecordDiagnostics(dir string, loc fileLocation) []requestlog.ConversionDiagnostic {
	if loc.length <= 0 {
		return nil
	}
	path := filepath.Join(dir, loc.file)
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, loc.length)
	if _, err := f.ReadAt(buf, loc.offset); err != nil {
		return nil
	}
	var rec requestlog.Record
	if err := json.Unmarshal(buf, &rec); err != nil {
		return nil
	}
	return rec.Diagnostics
}

type routingReport struct {
	Opts             routingReportOptions
	WindowStart      time.Time
	WindowEnd        time.Time
	TotalRequests    int
	BusinessRequests int
	WeakLabelSummary weakLabelSummary
	SelectorMatrix   selectorMatrix
	EvalVerdicts     evalVerdictReport
	Cost             costReport
	Overhead         overheadReport
}

type weakLabelSummary struct {
	ByRoute []routeWeakLabel
}

type routeWeakLabel struct {
	Route        string
	Grade        string
	Samples      int
	OkBasic      int
	NoRetry      int
	NoEscalation int
	WeakOK       int
}

type selectorMatrix struct {
	Enforced []selectorMatrixRow
	Shadow   []selectorMatrixRow
}

type selectorMatrixRow struct {
	Difficulty string
	Predicted  string
	Actual     string
	Samples    int
	WeakOK     int
}

type evalVerdictReport struct {
	Rows []evalVerdictRow
}

type evalVerdictRow struct {
	Route        string
	PrimaryGrade string `json:"primary_grade"`
	ShadowGrade  string `json:"shadow_grade"`
	Verdict      string
	Samples      int
}

type costReport struct {
	ActualUSD   float64
	BaselineUSD float64
	UnknownUSD  float64
	SavingsPct  float64
	ByRoute     []routeCost
}

type routeCost struct {
	Route       string
	Samples     int
	ActualUSD   float64
	BaselineUSD float64
	UnknownUSD  float64
}

type overheadReport struct {
	DecisionRequests int
	TotalLatencyMs   int64
	TotalTokens      uint64
	BusinessTokens   uint64
	PctTokens        float64
	PctRequests      float64
}

func buildRoutingReport(rows []routingRow, cfg *configdomain.Config, opts routingReportOptions) routingReport {
	report := routingReport{Opts: opts}
	if len(rows) == 0 {
		return report
	}
	report.WindowStart = rows[0].Ts
	report.WindowEnd = rows[len(rows)-1].Ts
	report.TotalRequests = len(rows)

	catalog := loadPricingCatalog(cfg)

	// Partition rows.
	var business []routingRow
	var decisions []routingRow
	for _, r := range rows {
		if isDecisionRecord(r.RequestID) {
			decisions = append(decisions, r)
			continue
		}
		if isExcludedRecord(r.RequestID) {
			continue
		}
		business = append(business, r)
	}
	report.BusinessRequests = len(business)

	// Weak label pass needs global ordering for retry/escalation checks.
	weakRows := evaluateWeakLabels(business, opts.RetryWindow)
	report.WeakLabelSummary = summarizeWeakLabels(weakRows)
	report.SelectorMatrix = buildSelectorMatrix(weakRows)
	report.EvalVerdicts = buildEvalVerdictReport(rows)
	report.Cost = buildCostReport(weakRows, cfg, catalog)
	report.Overhead = buildOverheadReport(decisions, business)

	return report
}

func isDecisionRecord(requestID string) bool {
	return strings.HasPrefix(requestID, "route-select-") || strings.HasPrefix(requestID, "fusion-select-")
}

func isExcludedRecord(requestID string) bool {
	return strings.HasPrefix(requestID, "shadow-")
}

// weakRow extends routingRow with label flags.
type weakRow struct {
	routingRow
	OkBasic      bool
	NoRetry      bool
	NoEscalation bool
	WeakOK       bool
}

func evaluateWeakLabels(rows []routingRow, retryWindow time.Duration) []weakRow {
	// Index by session+turn_key and by session for efficient checks.
	type sessionTurn struct {
		session string
		turnKey string
	}
	bySessionTurn := map[sessionTurn][]routingRow{}
	bySession := map[string][]routingRow{}
	for _, r := range rows {
		st := sessionTurn{session: r.SessionID, turnKey: r.TurnKey}
		bySessionTurn[st] = append(bySessionTurn[st], r)
		bySession[r.SessionID] = append(bySession[r.SessionID], r)
	}

	out := make([]weakRow, 0, len(rows))
	for _, r := range rows {
		w := weakRow{routingRow: r}
		w.OkBasic = r.Status == 200 && r.ResponseSize > 0 && !hasErrorDiagnostic(r)
		w.NoRetry = !hasRetryInWindow(r, bySessionTurn[sessionTurn{session: r.SessionID, turnKey: r.TurnKey}], retryWindow)
		w.NoEscalation = !hasLaterLatch(r, bySession[r.SessionID])
		w.WeakOK = w.OkBasic && w.NoRetry && w.NoEscalation
		out = append(out, w)
	}
	return out
}

func hasErrorDiagnostic(r routingRow) bool {
	if r.Routing != nil && r.Routing.Selector != nil && r.Routing.Selector.Err != "" {
		return true
	}
	for _, d := range r.Diagnostics {
		if isErrorDiagnosticCode(d.Code) {
			return true
		}
	}
	return false
}

func isErrorDiagnosticCode(code string) bool {
	lower := strings.ToLower(code)
	return strings.Contains(lower, "error") || strings.Contains(lower, "failed") || strings.Contains(lower, "fatal")
}

func hasRetryInWindow(r routingRow, same []routingRow, window time.Duration) bool {
	for _, other := range same {
		if other.RequestID == r.RequestID {
			continue
		}
		if other.Ts.After(r.Ts) && other.Ts.Sub(r.Ts) <= window {
			return true
		}
	}
	return false
}

func hasLaterLatch(r routingRow, same []routingRow) bool {
	for _, other := range same {
		if other.RequestID == r.RequestID {
			continue
		}
		if other.Ts.After(r.Ts) && other.Routing != nil && other.Routing.Latch != "" {
			return true
		}
	}
	return false
}

func summarizeWeakLabels(rows []weakRow) weakLabelSummary {
	type key struct {
		route string
		grade string
	}
	aggs := map[key]*routeWeakLabel{}
	for _, r := range rows {
		k := key{route: r.Exposed, grade: effectiveGrade(r.routingRow)}
		agg, ok := aggs[k]
		if !ok {
			agg = &routeWeakLabel{Route: k.route, Grade: k.grade}
			aggs[k] = agg
		}
		agg.Samples++
		if r.OkBasic {
			agg.OkBasic++
		}
		if r.NoRetry {
			agg.NoRetry++
		}
		if r.NoEscalation {
			agg.NoEscalation++
		}
		if r.WeakOK {
			agg.WeakOK++
		}
	}
	var list []routeWeakLabel
	for _, agg := range aggs {
		list = append(list, *agg)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Route != list[j].Route {
			return list[i].Route < list[j].Route
		}
		return list[i].Grade < list[j].Grade
	})
	return weakLabelSummary{ByRoute: list}
}

func effectiveGrade(r routingRow) string {
	if r.Routing != nil && r.Routing.Grade != "" {
		return r.Routing.Grade
	}
	if r.Routing != nil && r.Routing.Target != "" {
		return r.Routing.Target
	}
	return r.UpstreamModel
}

func buildSelectorMatrix(rows []weakRow) selectorMatrix {
	type cell struct {
		difficulty string
		predicted  string
		actual     string
		enforced   bool
	}
	aggs := map[cell]*selectorMatrixRow{}
	for _, r := range rows {
		if r.Routing == nil || r.Routing.Selector == nil {
			continue
		}
		s := r.Routing.Selector
		diff := difficultyBucket(s.Difficulty)
		predicted := s.Choice
		actual := effectiveGrade(r.routingRow)
		if predicted == "" {
			predicted = "(none)"
		}
		if actual == "" {
			actual = "(none)"
		}
		k := cell{difficulty: diff, predicted: predicted, actual: actual, enforced: s.Enforced}
		agg, ok := aggs[k]
		if !ok {
			agg = &selectorMatrixRow{Difficulty: diff, Predicted: predicted, Actual: actual}
			aggs[k] = agg
		}
		agg.Samples++
		if r.WeakOK {
			agg.WeakOK++
		}
	}

	var enforced, shadow []selectorMatrixRow
	for k, agg := range aggs {
		if k.enforced {
			enforced = append(enforced, *agg)
		} else {
			shadow = append(shadow, *agg)
		}
	}
	sortMatrixRows(enforced)
	sortMatrixRows(shadow)
	return selectorMatrix{Enforced: enforced, Shadow: shadow}
}

func difficultyBucket(d float64) string {
	switch {
	case d < 0.3:
		return "low"
	case d < 0.7:
		return "mid"
	default:
		return "high"
	}
}

func sortMatrixRows(rows []selectorMatrixRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Difficulty != rows[j].Difficulty {
			return rows[i].Difficulty < rows[j].Difficulty
		}
		if rows[i].Predicted != rows[j].Predicted {
			return rows[i].Predicted < rows[j].Predicted
		}
		return rows[i].Actual < rows[j].Actual
	})
}

func buildEvalVerdictReport(rows []routingRow) evalVerdictReport {
	type key struct {
		route        string
		primaryGrade string
		shadowGrade  string
		verdict      string
	}
	aggs := map[key]*evalVerdictRow{}
	for _, r := range rows {
		if !strings.HasPrefix(r.RequestID, "shadow-") {
			continue
		}
		verdict := ""
		shadowGrade := ""
		for _, d := range r.Diagnostics {
			switch d.Code {
			case "eval_verdict":
				verdict = d.Detail
			case "eval_shadow_grade":
				shadowGrade = d.Detail
			}
		}
		if verdict == "" {
			continue
		}
		primaryGrade := ""
		if r.Routing != nil {
			primaryGrade = r.Routing.Grade
		}
		k := key{route: r.Exposed, primaryGrade: primaryGrade, shadowGrade: shadowGrade, verdict: verdict}
		agg, ok := aggs[k]
		if !ok {
			agg = &evalVerdictRow{Route: k.route, PrimaryGrade: k.primaryGrade, ShadowGrade: k.shadowGrade, Verdict: k.verdict}
			aggs[k] = agg
		}
		agg.Samples++
	}
	list := make([]evalVerdictRow, 0, len(aggs))
	for _, agg := range aggs {
		list = append(list, *agg)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Route != list[j].Route {
			return list[i].Route < list[j].Route
		}
		if list[i].PrimaryGrade != list[j].PrimaryGrade {
			return list[i].PrimaryGrade < list[j].PrimaryGrade
		}
		if list[i].ShadowGrade != list[j].ShadowGrade {
			return list[i].ShadowGrade < list[j].ShadowGrade
		}
		return list[i].Verdict < list[j].Verdict
	})
	return evalVerdictReport{Rows: list}
}

func buildCostReport(rows []weakRow, cfg *configdomain.Config, catalog *pricing.Catalog) costReport {
	// ResolveAliased needs overrides and aliases from config.
	overrides := map[string]pricing.Override{}
	for model, override := range cfg.Prices {
		overrides[model] = pricing.Override{
			Input:      override.Input,
			Output:     override.Output,
			CacheRead:  override.CacheRead,
			CacheWrite: override.CacheWrite,
		}
	}
	aliases := map[string]string{}
	for name, p := range cfg.Providers {
		for upstream, exposed := range p.Alias {
			aliases[pricing.AliasKey(name, upstream)] = exposed
		}
	}

	// Per-route most-expensive baseline model entry.
	routeMaxEntry := map[string]pricing.Entry{}
	for route, policy := range cfg.RoutePolicies {
		if !policy.HasGrades() {
			continue
		}
		var maxEntry pricing.Entry
		maxCost := -1.0
		for _, targets := range policy.Grades {
			for _, t := range targets {
				entry, _ := pricing.ResolveAliased(overrides, catalog, aliases, t.Provider, t.Model)
				cost := entry.Prompt + entry.Completion + entry.CacheRead + entry.CacheWrite
				if cost > maxCost {
					maxCost = cost
					maxEntry = entry
				}
			}
		}
		routeMaxEntry[route] = maxEntry
	}
	// Fallback: derive from records if config grades absent.
	for _, r := range rows {
		entry, _ := pricing.ResolveAliased(overrides, catalog, aliases, r.Provider, r.UpstreamModel)
		cost := entry.Prompt + entry.Completion + entry.CacheRead + entry.CacheWrite
		max := routeMaxEntry[r.Exposed]
		if cost > max.Prompt+max.Completion+max.CacheRead+max.CacheWrite {
			routeMaxEntry[r.Exposed] = entry
		}
	}

	report := costReport{}
	type key struct{ route string }
	aggs := map[key]*routeCost{}
	for _, r := range rows {
		k := key{route: r.Exposed}
		agg, ok := aggs[k]
		if !ok {
			agg = &routeCost{Route: k.route}
			aggs[k] = agg
		}
		agg.Samples++

		entry, priced := pricing.ResolveAliased(overrides, catalog, aliases, r.Provider, r.UpstreamModel)
		actual := 0.0
		if priced && (r.Input+r.Output+r.CacheRead+r.CacheCreation) > 0 {
			actual = pricing.ComputeCost(r.Input, r.Output, r.CacheRead, r.CacheCreation, entry)
			report.ActualUSD += actual
			agg.ActualUSD += actual
		} else {
			// Best-effort: price at max known rate for the route.
			if max, ok := routeMaxEntry[r.Exposed]; ok {
				unknown := pricing.ComputeCost(r.Input, r.Output, r.CacheRead, r.CacheCreation, max)
				report.UnknownUSD += unknown
				agg.UnknownUSD += unknown
			}
		}

		if max, ok := routeMaxEntry[r.Exposed]; ok {
			baseline := pricing.ComputeCost(r.Input, r.Output, r.CacheRead, r.CacheCreation, max)
			report.BaselineUSD += baseline
			agg.BaselineUSD += baseline
		}
	}

	for _, agg := range aggs {
		report.ByRoute = append(report.ByRoute, *agg)
	}
	sort.Slice(report.ByRoute, func(i, j int) bool { return report.ByRoute[i].Route < report.ByRoute[j].Route })

	if report.BaselineUSD > 0 {
		report.SavingsPct = (report.BaselineUSD - report.ActualUSD - report.UnknownUSD) / report.BaselineUSD * 100
	}
	return report
}

func buildOverheadReport(decisions, business []routingRow) overheadReport {
	var totalTokens uint64
	var businessTokens uint64
	for _, r := range decisions {
		totalTokens += r.Input + r.Output
	}
	for _, r := range business {
		businessTokens += r.Input + r.Output
	}
	var lat int64
	for _, r := range decisions {
		lat += r.LatencyMs
	}
	report := overheadReport{
		DecisionRequests: len(decisions),
		TotalLatencyMs:   lat,
		TotalTokens:      totalTokens,
		BusinessTokens:   businessTokens,
	}
	allTokens := totalTokens + businessTokens
	if allTokens > 0 {
		report.PctTokens = float64(totalTokens) / float64(allTokens) * 100
	}
	if len(business)+len(decisions) > 0 {
		report.PctRequests = float64(len(decisions)) / float64(len(business)+len(decisions)) * 100
	}
	return report
}

func loadPricingCatalog(cfg *configdomain.Config) *pricing.Catalog {
	cacheFile := pricing.CachePath(cliframework.HomeDir())
	catalog, _ := pricing.EnsureFresh(pricing.RefreshOptions{
		CacheFile: cacheFile,
		Endpoint:  cfg.Pricing.ResolvedSourceURL(),
		Fetch:     pricing.FetchHTTP,
		TTL:       cfg.Pricing.TTLDuration(),
	})
	if catalog == nil {
		return pricing.Empty()
	}
	return catalog
}

func renderRoutingReportText(report routingReport) {
	if report.BusinessRequests == 0 && len(report.EvalVerdicts.Rows) == 0 {
		fmt.Println("(no routing business requests in range)")
		return
	}

	fmt.Printf("Routing report · %s .. %s\n",
		report.WindowStart.Format(time.RFC3339),
		report.WindowEnd.Format(time.RFC3339))

	// Weak labels.
	fmt.Println()
	fmt.Println("Weak labels by route/grade")
	fmt.Printf("%-16s %-12s %8s %8s %8s %8s %8s\n", "ROUTE", "GRADE", "SAMPLES", "OK", "NO_RETRY", "NO_ESC", "WEAK_OK")
	for _, rw := range report.WeakLabelSummary.ByRoute {
		fmt.Printf("%-16.16s %-12.12s %8d %8d %8d %8d %8d\n",
			rw.Route, rw.Grade, rw.Samples, rw.OkBasic, rw.NoRetry, rw.NoEscalation, rw.WeakOK)
	}

	// Selector matrix.
	renderMatrixTable("Selector enforce matrix", report.SelectorMatrix.Enforced)
	renderMatrixTable("Selector shadow matrix", report.SelectorMatrix.Shadow)

	// L2 eval verdicts.
	renderEvalVerdictTable(report.EvalVerdicts)

	// Cost.
	fmt.Println()
	fmt.Println("Cost baseline")
	fmt.Printf("%-16s %8s %10s %10s %10s\n", "ROUTE", "SAMPLES", "ACTUAL", "BASELINE", "UNKNOWN")
	for _, rc := range report.Cost.ByRoute {
		fmt.Printf("%-16.16s %8d %10s %10s %10s\n",
			rc.Route, rc.Samples, fmt.Sprintf("$%.2f", rc.ActualUSD),
			fmt.Sprintf("$%.2f", rc.BaselineUSD), fmt.Sprintf("$%.2f", rc.UnknownUSD))
	}
	fmt.Printf("%-16s %8s %10s %10s %10s\n", "TOTAL", "", fmt.Sprintf("$%.2f", report.Cost.ActualUSD),
		fmt.Sprintf("$%.2f", report.Cost.BaselineUSD), fmt.Sprintf("$%.2f", report.Cost.UnknownUSD))
	fmt.Printf("savings vs baseline: %.1f%%\n", report.Cost.SavingsPct)

	// Overhead.
	fmt.Println()
	fmt.Println("Decision overhead")
	fmt.Printf("decision requests: %d\n", report.Overhead.DecisionRequests)
	if report.Overhead.DecisionRequests > 0 {
		avg := report.Overhead.TotalLatencyMs / int64(report.Overhead.DecisionRequests)
		fmt.Printf("total latency: %d ms (avg %d ms)\n", report.Overhead.TotalLatencyMs, avg)
	}
	fmt.Printf("decision tokens: %d (%.2f%% of all tokens)\n", report.Overhead.TotalTokens, report.Overhead.PctTokens)
	fmt.Printf("decision requests: %.2f%% of all requests\n", report.Overhead.PctRequests)

	// Disclaimer.
	fmt.Println()
	fmt.Println("Note: weak labels are biased — users may not retry even when dissatisfied.")
	fmt.Println("Low-confidence / low weak-ok regions should be calibrated with L2 strong labels.")
}

func renderMatrixTable(title string, rows []selectorMatrixRow) {
	fmt.Println()
	fmt.Println(title)
	if len(rows) == 0 {
		fmt.Println("  (no selector samples)")
		return
	}
	fmt.Printf("%-10s %-16s %-16s %-16s %8s %8s\n", "DIFF", "PREDICTED", "ACTUAL", "GRADE", "SAMPLES", "WEAK_OK")
	for _, rw := range rows {
		fmt.Printf("%-10s %-16.16s %-16.16s %-16.16s %8d %8d\n",
			rw.Difficulty, rw.Predicted, rw.Actual, "", rw.Samples, rw.WeakOK)
	}
}

func renderEvalVerdictTable(report evalVerdictReport) {
	fmt.Println()
	fmt.Println("L2 eval verdicts")
	if len(report.Rows) == 0 {
		fmt.Println("  (no eval shadow samples)")
		return
	}
	fmt.Printf("%-16s %-14s %-14s %-16s %8s\n", "ROUTE", "PRIMARY", "SHADOW", "VERDICT", "SAMPLES")
	for _, rw := range report.Rows {
		fmt.Printf("%-16.16s %-14.14s %-14.14s %-16.16s %8d\n",
			rw.Route, rw.PrimaryGrade, rw.ShadowGrade, rw.Verdict, rw.Samples)
	}
}

func renderRoutingReportJSON(report routingReport) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(report)
}
