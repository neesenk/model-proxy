package web

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func mustWebAsset(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(defaultAssets(), "assets/"+name)
	if err != nil {
		t.Fatalf("read embedded asset %s: %v", name, err)
	}
	return string(b)
}

// requireNode resolves the node binary. Without node the JS gates skip —
// UNLESS MP_REQUIRE_NODE=1 (CI), where a missing node is a hard failure: an
// explicitly enabled gate must never degrade into a silent skip.
func requireNode(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("MP_REQUIRE_NODE") == "1" {
			t.Fatal("MP_REQUIRE_NODE=1 but node is not installed; the JS syntax/unit gates must not be skipped in CI")
		}
		t.Skip("node is not installed; run the required node --check / node --test validation separately")
	}
	return node
}

func TestWebAssetsJavaScriptSyntax(t *testing.T) {
	node := requireNode(t)
	for _, assetPath := range []string{
		filepath.Join("assets", "app.js"),
		filepath.Join("assets", "pure.js"),
	} {
		cmd := exec.Command(node, "--check", assetPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("node --check internal/web/%s: %v\n%s", assetPath, err, out)
		}
	}
}

// TestWebAssetsPureJSUnitTests runs every jstests/*.test.mjs file with
// Node's built-in test runner — the pure.js helper tests, the web-api.md field
// contracts, and the browser-driven UI e2e (uie2e/uivisual self-gate: fast
// skip without MP_UI_E2E=1 — the heavy browser automation is an on-demand
// gate, not part of the every-change matrix; docs/engineering/testing.md
// 「UI 浏览器 e2e」) — no npm framework dependency. Files are
// enumerated here (not via `node --test <dir>`, whose directory-argument
// support varies across node versions). Same node gate as the syntax check.
func TestWebAssetsPureJSUnitTests(t *testing.T) {
	node := requireNode(t)
	files, err := filepath.Glob(filepath.Join("jstests", "*.test.mjs"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob jstests/*.test.mjs: %v (found %d)", err, len(files))
	}
	args := append([]string{"--test"}, files...)
	cmd := exec.Command(node, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node --test %s: %v\n%s", strings.Join(files, " "), err, out)
	}
}

func TestWebAssetsLogGutterAndSelectionContract(t *testing.T) {
	css := mustWebAsset(t, "styles.css")
	js := mustWebAsset(t, "app.js")
	for _, want := range []string{
		"--log-gutter-width: calc(3.5ch + 16px);",
		"grid-template-columns: var(--log-gutter-width) minmax(0, 1fr)",
		"column-gap: 10px;",
		"padding: 0 2px 0 14px;",
		".log-line:hover { background: color-mix(in srgb, var(--accent) 6%, transparent); }",
		".log-line.selected::before",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("styles.css missing %q", want)
		}
	}
	for _, want := range []string{
		"function bindLogSelection(pre)",
		"closest('.log-line')",
		"classList.add('selected')",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
}

func TestWebAssetsYAMLVisibleHeightContract(t *testing.T) {
	css := mustWebAsset(t, "styles.css")
	js := mustWebAsset(t, "app.js")
	// The height algorithm itself lives in pure.js (behavior-tested by
	// jstests/pure.test.mjs); app.js keeps the measurement and call sites.
	pure := mustWebAsset(t, "pure.js")
	for _, want := range []string{
		"const YAML_EDITOR_MIN_HEIGHT = 480",
		"function visibleYamlEditorHeight(viewportHeight, editorTop, spaceBelow)",
		"Math.max(YAML_EDITOR_MIN_HEIGHT,",
	} {
		if !strings.Contains(pure, want) {
			t.Errorf("pure.js missing %q", want)
		}
	}
	for _, want := range []string{
		"from './pure.js'",
		"visibleYamlEditorHeight(",
		"getBoundingClientRect()",
		"yamlEditor.setSize(null, height)",
		"window.addEventListener('resize', scheduleYamlEditorResize)",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	if strings.Contains(css, "height: calc(100vh - 230px)") {
		t.Error("styles.css still uses the fixed Raw YAML viewport offset")
	}
	if got := strings.Count(css, "min-height: 480px"); got < 2 {
		t.Errorf("styles.css has %d Raw YAML 480px min-height rules, want at least 2", got)
	}
}

// TestWebAssetsHeatLevelCascade pins the heatmap level-rule cascade: the
// level rules must tie the scoped base (.an-heat .hm) on specificity and
// follow it in source order, while staying UNSCOPED so the legend swatches
// (.an-heat-scale lives outside .an-heat) keep their backgrounds. Either
// regression renders every square as an empty base cell.
func TestWebAssetsHeatLevelCascade(t *testing.T) {
	css := mustWebAsset(t, "styles.css")
	base := strings.Index(css, ".an-heat .hm {")
	if base < 0 {
		t.Fatal("styles.css missing the scoped .an-heat .hm base rule")
	}
	for _, lvl := range []string{".hm.hm-l1", ".hm.hm-l2", ".hm.hm-l3", ".hm.hm-l4"} {
		at := strings.Index(css, lvl+" {")
		if at < 0 {
			t.Errorf("styles.css missing level rule %s (keep it unscoped: the legend swatches are outside .an-heat)", lvl)
			continue
		}
		if at < base {
			t.Errorf("level rule %s precedes the .an-heat .hm base rule — source order must favor the levels", lvl)
		}
		if strings.Contains(css, ".an-heat "+lvl+" {") {
			t.Errorf("re-scoped level rule %s detected — use the unscoped form so the legend swatches keep their background", lvl)
		}
	}
}

func TestWebAssetsAnalyticsTabContract(t *testing.T) {
	indexHTML := mustWebAsset(t, "index.html")
	js := mustWebAsset(t, "app.js")
	pure := mustWebAsset(t, "pure.js")
	for _, want := range []string{
		`data-tab="analytics"`,
		`id="tab-analytics"`,
		`href="vendor/uPlot.min.css"`,
		`src="vendor/uPlot.min.js"`,
	} {
		if !strings.Contains(indexHTML, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	if !strings.Contains(indexHTML, `src="vendor/uPlot.min.js"`) ||
		strings.Index(indexHTML, `src="vendor/uPlot.min.js"`) > strings.Index(indexHTML, `type="module" src="app.js"`) {
		t.Error("index.html: uPlot script must come before the deferred app.js module")
	}
	for _, want := range []string{
		"async function renderAnalyticsTab(background = false)",
		"function analyticsState()",
		"function analyticsSave(name, val)",
		"function analyticsRenderKpis(host, resp)",
		"function analyticsRenderCharts(panel, resp, metricId, gran)",
		"function analyticsRenderTable(panel, resp, metricId)",
		"function fmtDelta(delta, warn)",
		"analyticsChartSeries(",
		"analyticsChartColors()",
		"ANALYTICS_METRICS",
		"pctDelta(",
		"analyticsGranularity(",
		"analyticsTableRows(",
		"uplotAxisStyle()",
		"function analyticsTooltip(metricId, gran)",
		"function analyticsWindowGrid(from, to, gran)",
		"paths: analyticsBarPaths()",
		"analyticsValueText(",
		"function analyticsPickerHTML()",
		"function analyticsPickerRender(panel)",
		"analyticsRangeBounds(",
		// All-time anchor: from=0 is clamped server-side to the oldest bucket
		// and re-learned from the echoed window for granularity gating.
		"let anAllTimeSince = 0",
		"analyticsGranOptions(",
		"function analyticsRenderLegend(host, u, labels, colors, hiddenSet)",
		"function analyticsLegendCollapse(host)",
		"analyticsTickLabel(",
		"function analyticsXAxisValues(u, splits)",
		"function analyticsMaybeAutoRefresh()",
		"analyticsSave('gran', 'auto')",
		"function analyticsXRange(xs)",
		"function analyticsZoomControls(panel)",
		"id=\"an-zoom-reset\"",
		"id=\"an-table\"",
		"id=\"an-kpis\"",
		"id=\"an-metric\"",
		"granularity: gran, by: state.by",
		"if (name === 'analytics') renderAnalyticsTab();",
		"apiGet('/api/analytics?'",
		"price_coverage",
		"resp.compare",
		"new uPlot(",
		// Agent filter: toolbar input + datalist, forwarded as agent=.
		`id="an-agent"`,
		`id="an-agent-list"`,
		"q.set('agent', state.agent)",
		// Trailing-year token-usage heatmap card (contribution-graph style).
		`id="an-heat"`,
		"function analyticsRenderHeatmap(panel, resp)",
		"analyticsYearGrid(",
		"analyticsYearMonthSpans(",
		"analyticsHeatLevel(",
		"resp.heatmap",
		"'Token Activity'",
		"function showHeatTip(cellEl)",
		// Leaderboard: sortable headers + row drilldown into Requests.
		"function analyticsSortState()",
		"analyticsSortRows(",
		"data-sort=",
		"data-drill=",
		"requestsFilterQuery({ provider: r.provider, model: r.model, agent: r.agent })",
		// No-match filter hint (one-click clear).
		`id="an-filter-hint"`,
		"function analyticsFilterHint(panel, resp, state)",
		// Failover/429 attempt metrics + agent-dimension gating.
		"analyticsMetricOptions(state.by, state.agent)",
		"analyticsMetricAllowed(state.metric, state.by, state.agent) ? state.metric : 'tokens'",
		"failover attempts",
		"rate-limited (429)",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	for _, want := range []string{
		"export function analyticsYearGrid(",
		"export function analyticsTickLabel(v, tickSpanSec, tickPx)",
		"export function analyticsYearMonthSpans(",
		"export function analyticsHeatTip(",
		"export function analyticsHeatCellSize(",
		"export function analyticsHeatLevel(",
		"export const HEAT_DAYS",
		"export function analyticsRowSortKey(",
		"export function analyticsSortRows(",
		"export function analyticsTableSortValue(",
		"export function analyticsMetricOptions(",
		"export function analyticsMetricAllowed(",
	} {
		if !strings.Contains(pure, want) {
			t.Errorf("pure.js missing %q", want)
		}
	}
}

// TestWebAssetsDashboardContract pins the Status→Dashboard wiring: the
// section reuses the Analytics rendering (KPI chips + metric-switchable
// chart + leaderboard) pinned to the fixed 1h/minute/model window, with the
// former Model Health view merged into the leaderboard.
func TestWebAssetsDashboardContract(t *testing.T) {
	js := mustWebAsset(t, "app.js")
	pure := mustWebAsset(t, "pure.js")
	for _, want := range []string{
		"{ key: 'dashboard', label: 'Dashboard' },",
		"case 'dashboard':",
		"function renderDashboardSection(main)",
		"async function refreshDashboardData()",
		"function renderDashboardContent()",
		"function dashRenderChart(host, legendHost)",
		"function dashRenderTable(host)",
		"function destroyDashChart()",
		"modelHealthFromSeries(",
		// The legend wiring is easy to typo silently (a wrong name throws
		// inside dashRenderChart's catch and the chart still draws): pin the
		// exact call.
		"analyticsRenderLegend(legendHost, u, data.labels, colors, dashLegendHidden)",
		"apiGet('/api/analytics?from=' + (to - DASH_WINDOW_SEC)",
		"granularity=minute&by=model", // fixed window: 1h · minute · by model
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	// The standalone Model Health section is gone (merged into Dashboard).
	for _, gone := range []string{
		"{ key: 'health', label: 'Model Health' }",
		"case 'health':",
		"renderModelHealthCard",
		"apiGet('/api/stats?",
	} {
		if strings.Contains(js, gone) {
			t.Errorf("app.js still references removed dashboard wiring %q", gone)
		}
	}
	for _, want := range []string{
		"export function modelHealthFromSeries(series)",
		"export function modelHealthGrade(score)",
		"MODEL_HEALTH_DIMS",
	} {
		if !strings.Contains(pure, want) {
			t.Errorf("pure.js missing %q", want)
		}
	}
	for _, gone := range []string{
		"export function modelHealth(buckets)",
		"export function dashboardChartSeries",
		"export function dashboardLatest",
		"export function dashBucketValue",
		"DASH_WINDOW_MIN",
	} {
		if strings.Contains(pure, gone) {
			t.Errorf("pure.js still exports removed helper %q", gone)
		}
	}
}

func TestWebAssetsEmbeddedAndOffline(t *testing.T) {
	for _, name := range []string{
		"index.html", "app.js", "pure.js", "styles.css", "icon.svg", "vendor/codemirror.min.js",
		"vendor/codemirror.min.css", "vendor/uPlot.min.js", "vendor/uPlot.min.css",
		"vendor/yaml.min.js", "vendor/closebrackets.min.js", "vendor/matchbrackets.min.js",
		"vendor/README.md",
	} {
		if got := mustWebAsset(t, name); len(got) == 0 {
			t.Errorf("embedded asset %s is empty", name)
		}
	}
	for _, name := range []string{"index.html", "app.js", "pure.js", "styles.css"} {
		if strings.Contains(mustWebAsset(t, name), "https://") || strings.Contains(mustWebAsset(t, name), "http://") {
			t.Errorf("%s contains a runtime CDN URL", name)
		}
	}
}

// TestWebAssetsLiveSessionContract protects the Live session-analysis wiring:
// the session selector/panel exist, the pure summary helper is used, and the
// persisted session query is wired to the documented endpoint.
func TestWebAssetsLiveSessionContract(t *testing.T) {
	js := mustWebAsset(t, "app.js")
	for _, want := range []string{
		`id="live-session"`,
		`id="live-session-panel"`,
		"function onLiveSessionChange(",
		"function renderLiveSessionPanel(",
		"liveSessionSummary(",
		"apiGet('/api/requests?session='",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
}

// TestWebAssetsBodyViewerContract protects the request/response body viewer
// regressions: the body box must be tall with a visible scrollbar, the
// long-line collapse threshold must leave ordinary prose readable, and chunk
// loading must keep appending when a chunk renders collapsed (near-zero
// height) content instead of stalling with the scrollbar already at the end.
func TestWebAssetsBodyViewerContract(t *testing.T) {
	css := mustWebAsset(t, "styles.css")
	for _, want := range []string{
		"max-height: min(70vh, 760px)",
		"scrollbar-width: thin",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("styles.css missing %q", want)
		}
	}
	js := mustWebAsset(t, "app.js")
	for _, want := range []string{
		"const BODY_LONG_LINE = 4000;",
		"for (let guard = 0; guard < 64; guard += 1)",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
}

// TestWebAssetsRequestsSessionContract protects the Requests-tab session
// drill-down: a session selector + summary strip, the session query param, and
// the shared summary renderer (also used by the Live session panel).
func TestWebAssetsRequestsSessionContract(t *testing.T) {
	js := mustWebAsset(t, "app.js")
	// Segment routing contract: the Requests tab's view identity rides the
	// hash SEGMENT (#requests/model_all | model_live | mcp_all | mcp_live);
	// the query carries only filter/session/drill keys. Legacy shapes
	// (bare #requests, #requests/live, ?stream=mcp) rewrite in place at boot
	// and on every hashchange, and the four cross-tab drill builders go
	// through requestsLink. Behavior-covered by the UI e2e routing test.
	for _, want := range []string{
		"function requestsViewKey(stream, view) {",
		"function requestsViewFromKey(key) {",
		"function normalizeRequestsHash(p) {",
		"function requestsLink(queryStr) {",
		"const { tab, sub, query } = normalizeRequestsHash(parseHash());",
		"({ tab: bootTab, sub: bootSub, query: bootQuery } = normalizeRequestsHash(parseHash()));",
		// Four-page architecture: page configs + the router are the single
		// writer of which page is mounted (behavior-covered by the UI e2e
		// routing test).
		"const REQUESTS_PAGES = {",
		"function navigateRequestsPage(key, opts) {",
		"function activeRequestsPage() {",
		"activeRequestsPageKey = key;",
		"return '#requests/model_all' + (queryStr ? '?' + queryStr : '');",
		// The query layer must NOT know the stream key (segment-owned).
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	pureJs := mustWebAsset(t, "pure.js")
	if strings.Contains(pureJs, "q.set('stream'") {
		t.Errorf("pure.js requestsFilterQuery must not emit a stream query key (the hash segment owns the stream)")
	}
	// .card carries overflow: hidden (radius clipping), which makes it the
	// scroll container for sticky descendants — a card that never scrolls
	// kills page-level stickiness. The two session-view host cards must opt
	// out via card-open, and the opt-out rule must exist in styles.css.
	css := mustWebAsset(t, "styles.css")
	if !strings.Contains(css, ".card.card-open { overflow: visible; }") {
		t.Errorf("styles.css missing card-open overflow opt-out")
	}
	// Table headers must pin BELOW the session view, not hide under it: the
	// pinned view's measured height rides --sess-h into the th top calc.
	if !strings.Contains(css, "var(--sess-h, 0px)") {
		t.Errorf("styles.css missing --sess-h in sticky th top")
	}
	// Bar hover: metadata summary comes from pure sessionBarSummary; the
	// response excerpt (responseExcerpt over /api/requests/<id>, cached and
	// shared with detail rows) fills the shared .tl-tip tooltip.
	pure := mustWebAsset(t, "pure.js")
	for _, want := range []string{
		"export function sessionBarSummary(",
		"export function responseExcerpt(",
		"export function requestExcerpt(",
		"[tool_use: ",
		"export function chatViewHTML(",
		"export function readableValue(",
		"export function parseChatRequest(",
		"export function requestRowHTML(",
		"export function requestTableHeadHTML(",
		"export function sessionHealthSummary(",
		"export function chatTurnsSliceHTML(",
		// Detail meta strip + guard trail card: the labeled-group and
		// verdict-led rendering (behavioral coverage in jstests).
		"export function requestMetaHTML(",
		"export function guardMarksDetailHTML(",
		"export const CHAT_RECENT = 4;",
	} {
		if !strings.Contains(pure, want) {
			t.Errorf("pure.js missing %q", want)
		}
	}
	// #requests/<stream>_live?session=… must survive a refresh: the router
	// hands the query to renderLiveCard, which stashes the pin in the page's
	// bootPin and consumes it right after the mount that resets the selection
	// (a direct apply raced that mount). The pin lives in the page's state
	// slice (livePageState), not a module global.
	for _, want := range []string{
		"S.bootPin = (query && query.session) || '';",
		"if (S.bootPin) {",
		"const resumeSession = S.bootPin || S.session;",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing live-session boot pin marker %q", want)
		}
	}
	for _, want := range []string{
		"function showTlTip(",
		"hideTlTip();",
		// Record details lead with the readable chat transcript; the raw
		// bodies stay below in collapsed <details> that render lazily on
		// first expand (registry + capture-phase toggle listener).
		"raw request body (",
		// All three request tables (Requests tab, Live ring, Live session
		// view) render through the one shared row renderer; bytes columns
		// are gone in favor of the token cell with cache read. The Requests
		// tab rows are pooled per request id and mounted window-by-window
		// (virtual scrolling) — still the same renderer, just not one big
		// string per table.
		"holder.innerHTML = requestRowHTML(persistedSummaryRow(rec), {",
		// Health chips row + relative-time context on the detail meta strip.
		"sessionSummaryHTML(s, o, sessionHealthSummary(rows))",
		"function requestRelTimeOpts(id)",
		// TTFT: rows map ttft_ms; the detail meta strip (pure requestMetaHTML)
		// renders it inside the RESULT group.
		"ttftMs: rec.ttft_ms != null ? rec.ttft_ms : null,",
		"return requestRowHTML(r, {",
		"raw response body (",
		"const rawBodyRegistry = new Map();",
		"renders on first expand",
		"dropRawBodies(tbl);",
		// Detail structure: labeled meta strip leads each record, the guard
		// trail card rides the first record via opts.guard (server-joined
		// annotations — the frontend never re-derives them).
		"${requestMetaHTML(r, rel)}${guardCard}",
		"detailRecordsHTML(cached, { ...relOpts, guard: requestsGuardCache.get(id) })",
		"detailRecordsHTML(recs, { ...requestRelTimeOpts(id), guard })",
		// The chat history expands to a FLAT chunked transcript (parse once,
		// 25-turn slices appended by the shared scroll loader) — measured
		// 35KB/340 nodes for the first chunk of an 862-turn session.
		"chatViewHTML(r.request_body, r.response_body, ct, { histKey: histId })",
		"d.classList.contains('cv-history')",
		// Regression: the popover rebuild guard once referenced r.* inside
		// fillLiveDetailPop (parameter is row) and threw on every open —
		// caught by real-browser E2E, pinned so it cannot return silently.
		"requestsDetailCache.get(row.requestId)",
		"chatTurnsSliceHTML(entry.msgs, r.from, r.to)",
		"const id = 'cv-hist-' + (++bodyChunkSeq)",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing detail chat-view marker %q", want)
		}
	}
	for _, want := range []string{
		".cv-msg",
		// Sticky coverage must span the card gutters (negative margins) and
		// sit flush under the topbar — no transparent seams.
		"margin-left: -16px;",
		"#live-session-panel .table th",
		".sess-health",
		".tok-cache",
		".sess-meta-k",
		".tl-legend",
		".cv-tool-name",
		// History turns must stay natively virtualized: without this the
		// scroll cost grows with loaded depth (user-felt jank on long
		// sessions) even though chunking bounds the initial DOM.
		"content-visibility: auto",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("styles.css missing chat-view marker %q", want)
		}
	}
	// Two unlabeled excerpt paragraphs (input muted, response normal) and the
	// load-bearing pointer-events:none that keeps the tooltip transparent to
	// the cursor.
	for _, want := range []string{
		".tl-tip-in",
		".tl-tip-out",
		"pointer-events: none;",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("styles.css missing tooltip marker %q", want)
		}
	}
	for _, want := range []string{
		`id="req-session"`,
		`id="req-session-summary"`,
		"function renderRequestsSessionSummary(",
		"function sessionSummaryHTML(",
		"q.set('session', requestsFilter.session)",
		"renderRequestsSessionSummary(combos)",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	// The Live panel and the Requests session summary must share ONE session
	// view — sessionViewHTML (summary chips + trace timeline) — instead of
	// each assembling its own copy; only the detail opener differs
	// (popover vs inline row expand), wired through wireSessionTimeline. Both
	// call sites key the timeline zoom by their session id (drag-select state
	// survives the Live panel's SSE re-renders), and both hosts sit in the
	// sticky .sess-sticky container so the view stays visible while the
	// session's table scrolls.
	for _, want := range []string{
		"function sessionViewHTML(",
		"function wireSessionTimeline(",
		"sessionViewHTML(rows, S.agg, { session: S.session, ...activeRequestsPage().sessionView })",
		"sessionViewHTML(rows, agg, { live: false, session: requestsFilter.session, ...page.sessionView })",
		// The session view (and the Live session panel's table head) must
		// speak the stream's domain: the MCP variant carries the 7-column
		// Server/Account geometry and drops the LLM token/cost chips — the
		// LLM head over 7-cell MCP rows misaligns and reads as the Model view
		// (behavior-covered by the UI e2e MCP-domain session-panel test).
		"requestTableHeadHTML(activeRequestsPage().table)",
		`<div class="sess-sticky">${sessionViewHTML(rows, S.agg`,
		"class=\"sess-sticky\" style=\"margin-bottom:12px\" hidden",
		// Timeline bar → inline detail + locate: instant rect-based jump (a
		// smooth/element scroll dies on the first mid-flight replaceChildren),
		// and only when EXPANDING an off-screen row — collapse or an
		// already-visible row must not move the page under the cursor.
		"window.scrollTo({ top: window.scrollY + rect.top",
		"if (!tr.isConnected || !opening) return",
		// Both host cards must drop the .card overflow clipping (card-open):
		// an overflow ancestor becomes the scroll container for sticky
		// descendants and a never-scrolling card kills the pin entirely.
		`<div class="card card-open"><div class="card-body">`,
		`<div id="live-table"><span class="msg hint">connecting…</span></div>`,
		// Zoom drag must not capture the pointer on pointerdown: capture
		// retargets the release + derived click to the SVG root and kills
		// every plain bar click. Capture belongs to the drag branch only.
		"do NOT capture here",
		"svg.setPointerCapture(start.id)",
		"function syncSessThOffset(",
		"syncSessThOffset(host);",
		// Clearing the session filter hides the pinned view through the SAME
		// re-sync: skipping it leaves --sess-h at the pinned view's stale
		// height and the sticky th floats that many px below the topbar, a
		// transparent gap where scrolled rows bleed through.
		"if (!requestsFilter.session) { host.hidden = true; host.innerHTML = ''; syncSessThOffset(host); return; }",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing shared session-view marker %q", want)
		}
	}
}

// TestWebAssetsRequestsVirtualScrollContract protects the Requests tab's
// virtualized table: a small default page (50) with scroll-driven older
// pages via to= keyset pagination, and a DOM that only carries the viewport
// window of rows (spacers keep the scrollbar sized). These are wiring facts
// (DOM behavior node cannot observe), pinned at their exact call sites.
func TestWebAssetsRequestsVirtualScrollContract(t *testing.T) {
	js := mustWebAsset(t, "app.js")
	pure := mustWebAsset(t, "pure.js")
	css := mustWebAsset(t, "styles.css")
	// Default page sizes: browse 50 (scroll loads older), session drill-down
	// keeps its deeper 500 window; paging stops at the backend's 1000 cap.
	for _, want := range []string{
		"const REQ_BROWSE_LIMIT = 50;",
		"const REQ_SESSION_LIMIT = 500;",
		"const REQ_MAX_LOADED = 1000;",
		"requestsFilter.session ? REQ_SESSION_LIMIT : REQ_BROWSE_LIMIT",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing virtual-scroll limit marker %q", want)
		}
	}
	// Keyset pagination: the older page reuses the exact filter params and
	// sets to=oldest second; the boundary-second duplicates dedupe through
	// mergeRecordsPages (pure.js, behavior-tested).
	for _, want := range []string{
		"function reqFilterParams()",
		"const to = oldestTsSec(v.recs);",
		"q.set('to', String(to));",
		"q.set('limit', String(v.pageSize));",
		"const merged = mergeRecordsPages(v.recs, page);",
		"v.more = page.length >= v.pageSize && v.recs.length < REQ_MAX_LOADED;",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing keyset-pagination marker %q", want)
		}
	}
	// Window rendering: scroll listener must be capture-phase + rAF
	// coalesced (scroll does not bubble; sync DOM work in the handler janks
	// the frame), reconcile must skip unchanged windows, and spacer rows
	// must keep the scroll geometry (class pinned in styles.css too).
	for _, want := range []string{
		"const win = virtualWindow(v.offsets,",
		"if (key === v.key) return;",
		"v.tbody.replaceChildren(frag);",
		"frag.appendChild(reqSpacer(gap));",
		"}, true);",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing window-render marker %q", want)
		}
	}
	// Pinned open details + timeline reveals must survive window moves, and
	// pooled rows' chunk/raw-body state must be freed when the table is
	// rebuilt (pooled nodes may be detached from the container).
	for _, want := range []string{
		"tr.classList.contains('req-open')",
		"v.revealId = id;",
		"function reqTearDown(v)",
		"dropRawBodies(node);",
		"function reqRowForId(id)",
		"function reqDetailChanged()",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing pinned-detail marker %q", want)
		}
	}
	// Pure windowing helpers stay in pure.js (behavior coverage lives in
	// jstests/pure.test.mjs) and the spacer row styling must neutralize the
	// generic tbody hover/border rules.
	for _, want := range []string{
		"export function cumulativeOffsets(",
		"export function virtualWindow(",
		"export function mergeRecordsPages(",
		"export function oldestTsSec(",
	} {
		if !strings.Contains(pure, want) {
			t.Errorf("pure.js missing %q", want)
		}
	}
	for _, want := range []string{
		"tr.req-spacer td { padding: 0; border-bottom: 0; }",
		"tr.req-spacer, .table tbody tr.req-spacer:hover { background: transparent; }",
		// The request tables ride a fixed colgroup geometry (pure.js
		// requestTableHeadHTML): auto layout re-derives column widths from
		// whichever rows the window has mounted, so the columns jittered
		// while scrolling. table-layout: fixed pins every window to the
		// same split.
		"#req-table .table, #live-table .table, #live-session-panel .table { table-layout: fixed; }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("styles.css missing spacer marker %q", want)
		}
	}
	// The colgroup is the single geometry source shared by all three request
	// tables; dropping it (or the fixed layout above) reopens scroll jitter.
	if !strings.Contains(pure, "<colgroup><col ") {
		t.Errorf("pure.js requestTableHeadHTML missing fixed column geometry (colgroup)")
	}
}

// TestWebAssetsLiveDetailErrorTerminal protects the 404 fetch-loop fix: the
// Live detail popover must consult shouldFetchDetail (which treats a recorded
// error or a 404 as terminal) instead of unconditionally re-fetching the open
// request, and a 404/notLogged must render as a neutral hint, not a red error.
// The popover has a single ensure/fetch path shared by the All/live table and
// the session panel.
// TestWebAssetsTabSwitchStabilityContract pins the tab-switch stability
// wiring: entering a tab restores that tab's last scroll position (panels
// share ONE document scroll — display toggling lets the browser clamp
// scrollY to the incoming panel's height), and re-entering Status→Live
// keeps the card's height stable: the live ring survives the remount
// (in-flight rows dropped — their end events were missed while
// disconnected), the retained ring paints in the mount task, the session
// selection resumes, and the empty-ring placeholder reserves height in CSS.
// The ring is also deduped by request id at that remount: every SSE
// (re)connect replays the hub's recent-event ring, so a replayed start
// must never add a second row for a request the retained ring already
// holds (the Live sub-view round trip used to duplicate every retained
// row this way — behavior-covered by the UI e2e replay test).
func TestWebAssetsTabSwitchStabilityContract(t *testing.T) {
	js := mustWebAsset(t, "app.js")
	for _, want := range []string{
		"let tabScrollMemory = {};",
		"tabScrollMemory[prevTab] = window.scrollY;",
		"window.scrollTo(0, tabScrollMemory[name]);",
		"showTabPanel(name);",
		"liveRows = liveRows.filter(dedupeRetainedLiveRows);",
		"function dedupeRetainedLiveRows(r, i, rows) {",
		"return rows.findIndex((o) => o.requestId === r.requestId) === i;",
		"const kept = liveByReq[e.request_id];",
		"const resumeSession = S.bootPin || S.session;",
		"onLiveSessionChange(resumeSession);",
		"} else if (liveRows.length) {",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	css := mustWebAsset(t, "styles.css")
	for _, want := range []string{
		"#live-table { min-height: 180px; }",
		// Entry fade: opacity-only (never transform/height — the panels hold
		// sticky theads and the sticky session view) and guarded by
		// prefers-reduced-motion.
		"animation: panel-in 140ms ease-out;",
		"@media (prefers-reduced-motion: no-preference)",
		"@keyframes panel-in { from { opacity: 0; } }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("styles.css missing %q", want)
		}
	}
}

func TestWebAssetsLiveDetailErrorTerminal(t *testing.T) {
	js := mustWebAsset(t, "app.js")
	for _, want := range []string{
		"shouldFetchDetail,",
		"detailFetchState,",
		"if (state.notLogged)",
		"not logged — the request did not commit",
		"function openLiveDetailPop(",
		"function updateLiveDetailPop(",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	if got := strings.Count(js, "if (!shouldFetchDetail("); got != 1 {
		t.Errorf("app.js should gate the live detail ensure path with shouldFetchDetail, got %d", got)
	}
	if got := strings.Count(js, "detailFetchState(e.status, e.message)"); got != 1 {
		t.Errorf("app.js should normalize live detail fetch failures with detailFetchState, got %d", got)
	}
}

// TestWebAssetsMCPTabContract pins the MCP tab's five wiring points — the
// regression chain that produced an empty page: panel map entry, both tab
// application call sites, the parseHash whitelist, and the boot chain, plus
// the index.html button/section. Any one missing = silent empty tab.
func TestWebAssetsMCPTabContract(t *testing.T) {
	indexHTML := mustWebAsset(t, "index.html")
	js := mustWebAsset(t, "app.js")
	for _, want := range []string{
		`data-tab="mcp"`,
		`id="tab-mcp"`,
	} {
		if !strings.Contains(indexHTML, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	for _, want := range []string{
		`mcp: document.getElementById('tab-mcp'),`, // panels map entry
		`if (name === 'mcp') renderMCPTab();`,      // both activate call sites
		`tab === 'mcp' || tab === 'security'`,      // parseHash whitelist
		`} else if (bootTab === 'mcp') {`,          // boot chain branch
		`async function renderMCPTab()`,            // renderer exists
		`apiGet('/api/mcp')`,                       // read surface
		`apiPost('/api/mcp/test', { name })`,       // probe surface
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	// Both activateTab and activateTabSilent must call the renderer (two
	// application points, user click + hash navigation).
	if strings.Count(js, `if (name === 'mcp') renderMCPTab();`) != 2 {
		t.Errorf("renderMCPTab call sites = %d, want exactly 2 (activateTab + activateTabSilent)",
			strings.Count(js, `if (name === 'mcp') renderMCPTab();`))
	}
}

// TestWebAssetsTakeoverTabContract pins the Takeover tab's wiring points —
// same regression chain as the MCP tab: panel map entry, both tab application
// call sites, the parseHash whitelist, the boot chain, the index.html
// button/section/modal, and the API surfaces it consumes.
func TestWebAssetsTakeoverTabContract(t *testing.T) {
	indexHTML := mustWebAsset(t, "index.html")
	js := mustWebAsset(t, "app.js")
	for _, want := range []string{
		`data-tab="takeover"`,
		`id="tab-takeover"`,
		`id="tk-modal"`,
		`id="tk-run-modal"`,
	} {
		if !strings.Contains(indexHTML, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	for _, want := range []string{
		`takeover: document.getElementById('tab-takeover'),`,                           // panels map entry
		`if (name === 'takeover') renderTakeoverTab();`,                                // both activate call sites
		`tab === 'takeover'`,                                                           // parseHash whitelist
		`} else if (bootTab === 'takeover') {`,                                         // boot chain branch
		`async function renderTakeoverTab()`,                                           // renderer exists
		`apiGet('/api/takeover')`,                                                      // read surface
		`apiPost('/api/takeover', req)`,                                                // run surface (confirm dialog, req = {client, mode, scope, subsets})
		`apiPost('/api/takeover/preview', Object.assign({ managed_only: true }, req))`, // dry-run preview (same req)
		`apiPost('/api/takeover/preview', body || { client: name, mode: 'unified', managed_only: true });`, // editor preview (disk template, or template_body draft)
		`apiPost('/api/takeover/restore', { client })`,                                                     // restore surface
		`/api/takeover/templates/`,                                                                         // template editor surface
		`retainTab(panel, '.tk-host', loadTakeover)`,                                                       // re-entry guard marker
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	if strings.Count(js, `if (name === 'takeover') renderTakeoverTab();`) != 2 {
		t.Errorf("renderTakeoverTab call sites = %d, want exactly 2 (activateTab + activateTabSilent)",
			strings.Count(js, `if (name === 'takeover') renderTakeoverTab();`))
	}
}

// TestWebAssetsEvalTabContract pins the Eval tab's wiring (same regression
// chain as MCP/Takeover) plus the diagnostics actions landing in existing
// surfaces: request replay strip, per-route test button, catalog refresh.
func TestWebAssetsEvalTabContract(t *testing.T) {
	indexHTML := mustWebAsset(t, "index.html")
	js := mustWebAsset(t, "app.js")
	for _, want := range []string{
		`data-tab="eval"`,
		`id="tab-eval"`,
	} {
		if !strings.Contains(indexHTML, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	for _, want := range []string{
		`eval: document.getElementById('tab-eval'),`, // panels map entry
		`if (name === 'eval') renderEvalTab();`,      // both activate call sites
		`tab === 'eval'`,                             // parseHash whitelist
		`} else if (bootTab === 'eval') {`,           // boot chain branch
		`async function renderEvalTab()`,             // renderer exists
		`apiGet('/api/shadow-report')`,               // shadow surface
		`apiGet('/api/fusion')`,                      // fusion surface
		`retainTab(panel, '.eval-host', loadEval)`,   // re-entry guard marker
		// Replay (request detail) / route test (schedule) / catalog refresh
		// (models) wiring.
		`apiPost('/api/replay', { id, provider })`,
		`apiPost('/api/routes/test', { model: route })`,
		`apiPost('/api/models/catalog/refresh')`,
		`data-test-route`,
		`data-replay-id`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	if strings.Count(js, `if (name === 'eval') renderEvalTab();`) != 2 {
		t.Errorf("renderEvalTab call sites = %d, want exactly 2 (activateTab + activateTabSilent)",
			strings.Count(js, `if (name === 'eval') renderEvalTab();`))
	}
	// Shadow records are never replayable: the strip guard must stay.
	if !strings.Contains(js, `if (id.startsWith('shadow-')) return '';`) {
		t.Error("replay strip lost the shadow-record guard")
	}
}
