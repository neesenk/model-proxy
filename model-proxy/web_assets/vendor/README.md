# Vendored CodeMirror 5

CodeMirror 5 (UMD/global build), used by the Config tab's Raw YAML editor. All files
sit flat in this directory and are loaded via plain `<script>`/`<link>` tags in
`../index.html` (before the `app.js` module) so the `CodeMirror` global is defined when
the module runs:

- `codemirror.min.js` + `codemirror.min.css` — core
- `yaml.min.js` — `mode/yaml` (registers the `yaml` mode on the `CodeMirror` global)
- `matchbrackets.min.js`, `closebrackets.min.js` — `addon/edit` editing aids

Source: cdnjs, version **5.65.16** (pinned). No CDN at runtime — the app serves these
from `/ui/vendor/*` via `//go:embed` (it runs on 127.0.0.1 offline).

CM5 is used (not CM6) because CM6 is pure-ESM and needs a bundler; CM5's UMD build loads
with plain script tags, fitting the no-build-step constraint.

To upgrade: re-download the same set from a newer `5.x` tag on cdnjs and replace these
files (keep the flat names so `index.html` references stay valid).

