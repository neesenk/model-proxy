package main

import (
	"io/fs"
	"strings"
	"testing"
)

func mustWebAsset(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(webAssets, "web_assets/"+name)
	if err != nil {
		t.Fatalf("read embedded asset %s: %v", name, err)
	}
	return string(b)
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
	for _, want := range []string{
		"const YAML_EDITOR_MIN_HEIGHT = 480",
		"function visibleYamlEditorHeight(viewportHeight, editorTop, spaceBelow)",
		"Math.max(YAML_EDITOR_MIN_HEIGHT,",
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

// TestWebAssetsAnalyticsTabContract pins the Analytics tab's presence in the
// admin UI: the tab button + panel in index.html, the vendored uPlot asset
// references (loaded before app.js, same pattern as CodeMirror), and the
// renderAnalyticsTab wiring in app.js. Mirrors the existing contract tests'
// plain strings.Contains pattern.
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
	// The uPlot script must load before the deferred app.js module so the
	// uPlot global exists when renderAnalyticsTab runs.
	if !strings.Contains(indexHTML, `src="vendor/uPlot.min.js"`) ||
		strings.Index(indexHTML, `src="vendor/uPlot.min.js"`) >
			strings.Index(indexHTML, `type="module" src="app.js"`) {
		t.Error("index.html: uPlot script must come before the deferred app.js module")
	}
	for _, want := range []string{
		"async function renderAnalyticsTab()",
		"function analyticsState()",
		"function analyticsSave(name, val)",
		"function analyticsRenderHints(panel, resp)",
		"function analyticsRenderCharts(panel, resp)",
		"function analyticsRenderTable(panel, resp)",
		"if (name === 'analytics') renderAnalyticsTab();",
		"apiGet('/api/analytics?'",
		"price_coverage",
		"new uPlot(",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	// The vendored uPlot assets themselves must be embedded and non-trivial.
	jsMin := mustWebAsset(t, "vendor/uPlot.min.js")
	cssMin := mustWebAsset(t, "vendor/uPlot.min.css")
	if !strings.Contains(jsMin, "uPlot") {
		t.Error("vendor/uPlot.min.js does not look like uPlot (no 'uPlot' token)")
	}
	if len(jsMin) < 10000 {
		t.Errorf("vendor/uPlot.min.js is suspiciously small (%d bytes)", len(jsMin))
	}
	if !strings.Contains(cssMin, ".uplot") {
		t.Error("vendor/uPlot.min.css does not look like uPlot's stylesheet")
	}
}
