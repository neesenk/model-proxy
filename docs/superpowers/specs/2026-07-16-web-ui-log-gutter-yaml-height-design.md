# Web UI Log Gutter and Raw YAML Height Design

## Goal

Fix two Web UI layout defects without changing backend APIs or the existing Status/Config navigation:

1. Status Logs must render a full-height line-number gutter, align wrapped text with the first line's message text, and visually identify the line selected by a click.
2. Config Raw YAML must use the visible viewport efficiently on first render without extending below it.

## Logs design

Each `.log-line` is a two-column CSS grid. The `::before` pseudo-element remains the generated line number, but becomes the first grid cell rather than an inline box. Its background and right border stretch across the full height of the logical log entry, including wrapped continuation lines, with 2px between the number and border. A separate 10px column gap keeps the normal log background between the gray gutter and message. The anonymous text content occupies the second grid cell, so every continuation line begins at the same horizontal position as the first line's message.

Hovering a `.log-line` adds a light accent-tinted background across the full logical row, including wrapped continuation lines. Clicking it makes it the only selected log row. Event delegation on the current `.log-pre` avoids one handler per line and remains compatible with the existing five-second re-render. The selected row receives a class that changes the gutter number color; native text selection and copying remain unchanged. Selection is intentionally scoped to the currently rendered log DOM and may reset when new log data replaces the rows.

## Raw YAML design

The editor height is computed from geometry instead of a fixed `calc(100vh - constant)` offset. After CodeMirror is mounted and populated, and whenever the viewport changes size, JavaScript measures the editor host's actual top edge and the vertical space required beneath the editor (card body padding, action row, and page bottom spacing). It sets the CodeMirror height to the remaining visible space.

The hard minimum is 480px. When at least 480px is available, the editor fills all remaining visible space. When less than 480px is available, usability wins: the editor remains 480px tall and the page may scroll vertically. The plain-textarea fallback follows the same rule.

Sizing runs after the first visible render, after YAML content is assigned, and on `resize`. A pending animation-frame callback is coalesced so repeated layout events do not cause unnecessary measurements. Listeners are removed or made harmless when the Config tab/editor is replaced.

## Testing

Frontend contract tests will verify:

- log rows use a dedicated two-column gutter layout;
- wrapped content does not use hanging indentation;
- clicking a row applies single-selection state and the selected gutter has a distinct foreground color;
- editor sizing uses the host's measured viewport position, grows beyond 480px when space permits, and never shrinks below 480px;
- both CodeMirror and textarea fallback receive the computed height.

Browser verification will cover the supplied wide Status screenshot, a deliberately wrapped log entry, the initial Config render, and a short viewport. Existing Go tests and asset-serving tests must remain green.

## Scope

Only `web_assets/app.js`, `web_assets/styles.css`, frontend tests or asset-contract tests, and the corresponding Web UI contract section in `AGENTS.md` are in scope. Existing unrelated working-tree changes, backend handlers, API response shapes, and configuration data are preserved.
