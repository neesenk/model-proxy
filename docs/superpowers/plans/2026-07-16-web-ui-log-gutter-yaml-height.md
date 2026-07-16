# Web UI Log Gutter and Raw YAML Height Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix wrapped Status log alignment and line selection styling, and make the Config Raw YAML editor fill—but never exceed—the visible viewport.

**Architecture:** Keep the existing generated log markup and CodeMirror integration. Turn each log entry into a two-column grid with a continuous gutter, add delegated click selection, and replace the fixed viewport-offset CSS with a geometry-based sizing helper that is rerun after rendering and on resize.

**Tech Stack:** Vanilla JavaScript, CSS Grid, CodeMirror 5, Go `embed.FS` asset-contract tests, Go test suite.

## Global Constraints

- Do not change backend handlers or `/api/*` response shapes.
- Preserve unrelated working-tree changes in `model-proxy/config.yaml`, `model-proxy/web_assets/app.js`, `model-proxy/web_assets/styles.css`, and the supplied screenshots.
- Raw YAML has a hard 480px minimum; a short viewport scrolls the page instead of collapsing the editor.
- A log row becomes selected by a click; native text selection and copying remain unchanged.
- Update `AGENTS.md` whenever implementation behavior changes.

---

### Task 1: Asset regression-test seam

**Files:**
- Create: `model-proxy/web_assets_contract_test.go`

**Interfaces:**
- Consumes: package-level `webAssets embed.FS` from `model-proxy/web.go`.
- Produces: `mustWebAsset(t *testing.T, name string) string`, used by asset contract tests in this file.

- [ ] **Step 1: Add the test helper and failing log-layout test**

```go
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
        "--log-gutter-width:",
        "grid-template-columns: var(--log-gutter-width) minmax(0, 1fr)",
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
```

- [ ] **Step 2: Run the focused test and verify RED**

Run: `cd model-proxy && go test ./... -run TestWebAssetsLogGutterAndSelectionContract -count=1`

Expected: FAIL because the current stylesheet uses hanging indentation and the JavaScript has no delegated row selection.

### Task 2: Log gutter, wrapping, and selected row

**Files:**
- Modify: `model-proxy/web_assets/styles.css` in the Logs section.
- Modify: `model-proxy/web_assets/app.js` in `renderLogsCard` / `renderLogsInto`.
- Test: `model-proxy/web_assets_contract_test.go`

**Interfaces:**
- Consumes: existing `.log-pre`, `.log-line`, CSS counter, and `renderLogsInto(target, lines)`.
- Produces: `bindLogSelection(pre)` and `.log-line.selected` state.

- [ ] **Step 1: Replace hanging indentation with a two-column gutter**

Use a gutter-width custom property and grid rows:

```css
.log-pre {
  --log-gutter-width: calc(3.5ch + 16px);
  padding: 12px 14px 12px 0;
  background: linear-gradient(to right,
    var(--lognum-bg) 0,
    var(--lognum-bg) var(--log-gutter-width),
    var(--surface-2) var(--log-gutter-width),
    var(--surface-2) 100%);
}
.log-line {
  display: grid;
  grid-template-columns: var(--log-gutter-width) minmax(0, 1fr);
  column-gap: 10px;
}
.log-line:hover { background: color-mix(in srgb, var(--accent) 8%, transparent); }
.log-line::before {
  box-sizing: border-box;
  padding: 0 2px 0 14px;
  border-right: 1px solid var(--border-2);
  background: transparent;
}
.log-line.selected::before {
  color: var(--accent-ink);
  opacity: 1;
  font-weight: 600;
}
```

Remove `.log-line`'s `padding-left` and `text-indent`; these are the root cause of wrapped text aligning under the gutter.

- [ ] **Step 2: Add delegated single-row click selection**

```js
function bindLogSelection(pre) {
  if (!pre) return;
  pre.addEventListener('click', (event) => {
    const line = event.target.closest('.log-line');
    if (!line || !pre.contains(line)) return;
    pre.querySelectorAll('.log-line.selected').forEach((el) => {
      if (el !== line) el.classList.remove('selected');
    });
    line.classList.add('selected');
  });
}
```

Call `bindLogSelection(logsPre)` immediately after `logsPre = target.querySelector('.log-pre')` in `renderLogsInto`.

- [ ] **Step 3: Run the focused test and verify GREEN**

Run: `cd model-proxy && go test ./... -run TestWebAssetsLogGutterAndSelectionContract -count=1`

Expected: PASS.

### Task 3: Raw YAML geometry-based height

**Files:**
- Modify: `model-proxy/web_assets/app.js` in the Raw YAML editor state, initialization, and Config rendering path.
- Modify: `model-proxy/web_assets/styles.css` in the textarea/CodeMirror section.
- Test: `model-proxy/web_assets_contract_test.go`

**Interfaces:**
- Consumes: `yamlEditor`, `#yaml-editor`, `#yaml-card`, `#yaml-msg`, and the existing Config render lifecycle.
- Produces: `visibleYamlEditorHeight(viewportHeight, editorTop, spaceBelow)`, `scheduleYamlEditorResize()`, and one active window-resize listener.

- [ ] **Step 1: Add the failing editor-height contract test**

```go
func TestWebAssetsYAMLVisibleHeightContract(t *testing.T) {
    css := mustWebAsset(t, "styles.css")
    js := mustWebAsset(t, "app.js")
    for _, want := range []string{
        "function visibleYamlEditorHeight(viewportHeight, editorTop, spaceBelow)",
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
```

- [ ] **Step 2: Run the focused test and verify RED**

Run: `cd model-proxy && go test ./... -run TestWebAssetsYAMLVisibleHeightContract -count=1`

Expected: FAIL because sizing still uses `calc(100vh - 230px)` and no geometry helper exists.

- [ ] **Step 3: Implement visible-height calculation and coalesced resize**

Add state and helpers near `yamlEditor`:

```js
let yamlResizeFrame = 0;

const YAML_EDITOR_MIN_HEIGHT = 480;

function visibleYamlEditorHeight(viewportHeight, editorTop, spaceBelow) {
  return Math.max(
    YAML_EDITOR_MIN_HEIGHT,
    Math.floor(viewportHeight - editorTop - spaceBelow),
  );
}

function resizeYamlEditor() {
  yamlResizeFrame = 0;
  const host = document.getElementById('yaml-editor');
  const card = document.getElementById('yaml-card');
  const target = yamlEditor ? yamlEditor.getWrapperElement()
    : document.getElementById('yaml-editor-fallback');
  if (!host || !card || !target || !host.offsetParent) return;

  const hostRect = host.getBoundingClientRect();
  const targetRect = target.getBoundingClientRect();
  const cardRect = card.getBoundingClientRect();
  const main = card.closest('main');
  const cardMargin = parseFloat(getComputedStyle(card).marginBottom) || 0;
  const mainPadding = main ? (parseFloat(getComputedStyle(main).paddingBottom) || 0) : 0;
  const spaceBelow = Math.max(0, cardRect.bottom - targetRect.bottom) + cardMargin + mainPadding;
  const height = visibleYamlEditorHeight(window.innerHeight, hostRect.top, spaceBelow);
  if (yamlEditor) yamlEditor.setSize(null, height);
  else target.style.height = `${height}px`;
}

function scheduleYamlEditorResize() {
  if (yamlResizeFrame) cancelAnimationFrame(yamlResizeFrame);
  yamlResizeFrame = requestAnimationFrame(resizeYamlEditor);
}
```

Register the resize listener once at module initialization. Call `scheduleYamlEditorResize()` after `initYamlEditor()`, after `loadConfigAll()` has rebuilt the Summary/forms and set YAML, and after `setYamlValue()`.

- [ ] **Step 4: Replace the fixed CSS offset with a hard 480px minimum**

```css
textarea.yaml {
  height: 480px;
  min-height: 480px;
}
#tab-config .yaml-cm-host .CodeMirror {
  height: 480px;
  min-height: 480px;
}
```

JavaScript runs before/at the next animation frame and grows the editor to the measured visible space, while the hard minimum prevents a short viewport from collapsing it.

- [ ] **Step 5: Run focused tests and verify GREEN**

Run: `cd model-proxy && go test ./... -run 'TestWebAssets(LogGutterAndSelection|YAMLVisibleHeight)Contract' -count=1`

Expected: PASS.

### Task 4: Contract documentation and verification

**Files:**
- Modify: `AGENTS.md` in the Web UI contract section.
- Test: all Go packages and browser-visible behavior.

**Interfaces:**
- Consumes: completed UI behavior from Tasks 2–3.
- Produces: authoritative repository documentation for future Web UI changes.

- [ ] **Step 1: Document the two UI contracts**

Add a concise paragraph under Web UI describing that Status log entries use a continuous full-height gutter, wrapped text aligns to the message column, click selection changes the number foreground, and Raw YAML is measured from its actual viewport position with a hard 480px minimum.

- [ ] **Step 2: Run formatting and static checks**

Run: `gofmt -w model-proxy/web_assets_contract_test.go`

Run: `git diff --check`

Expected: both commands exit 0.

- [ ] **Step 3: Run the full test suite**

Run: `cd model-proxy && go test ./... -count=1`

Expected: all packages PASS with zero failures.

- [ ] **Step 4: Verify the original UI scenarios in a browser**

Verify at a wide viewport and a short viewport:

1. Open Status → Logs and confirm a wrapped log's continuation aligns with its message text, while the gutter background spans the whole number column and full logical-row height.
2. Click two different log rows and confirm only the most recently clicked row's number uses the selected foreground.
3. Open Config and confirm Raw YAML ends above the viewport bottom on first render.
4. Resize taller and confirm it fills the extra space; resize to a short viewport and confirm the editor stays 480px while the page scrolls vertically.

- [ ] **Step 5: Review the final diff without committing unrelated files**

Run: `git diff -- model-proxy/web_assets_contract_test.go model-proxy/web_assets/app.js model-proxy/web_assets/styles.css AGENTS.md`

Expected: only the scoped regression tests, two UI fixes, and contract documentation are present; pre-existing unrelated edits within `app.js`/`styles.css` remain intact.
