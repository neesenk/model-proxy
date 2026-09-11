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
// Node's built-in test runner — the pure.js helper tests plus the
// docs/frontend field contracts — no framework dependency. Files are
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
		".log-line:hover { background: color-mix(in srgb, var(--accent) 8%, transparent); }",
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

func TestWebAssetsAnalyticsTabContract(t *testing.T) {
	indexHTML := mustWebAsset(t, "index.html")
	js := mustWebAsset(t, "app.js")
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
		"analyticsGranOptions(",
		"function analyticsRenderLegend(host, u, labels, colors, hiddenSet)",
		"function analyticsLegendCollapse(host)",
		"function analyticsTickLabel(v, tickSpanSec)",
		"function analyticsXAxisValues(_u, splits)",
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
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
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
	// The Live panel must reuse the shared summary renderer, not a second copy.
	if !strings.Contains(js, "sessionSummaryHTML(s)") {
		t.Errorf("app.js: Live session panel should reuse sessionSummaryHTML")
	}
}

// TestWebAssetsLiveDetailErrorTerminal protects the 404 fetch-loop fix: the
// Live detail popover must consult shouldFetchDetail (which treats a recorded
// error or a 404 as terminal) instead of unconditionally re-fetching the open
// request, and a 404/notLogged must render as a neutral hint, not a red error.
// The popover has a single ensure/fetch path shared by the All/live table and
// the session panel.
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
