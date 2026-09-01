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
}

func TestWebAssetsEmbeddedAndOffline(t *testing.T) {
	for _, name := range []string{
		"index.html", "app.js", "pure.js", "styles.css", "vendor/codemirror.min.js",
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
