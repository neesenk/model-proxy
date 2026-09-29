package takeover_test

import (
	"strings"
	"testing"

	"model-proxy/internal/takeover"
)

// tomlkeys_test.go pins the re-takeover safety contract: the TOML/env text
// editors must INSPECT the file semantically instead of blind-appending. A
// client that rewrites its own config (kimi-cli normalizing
// `[providers."model-proxy"]` to `[providers.model-proxy]`, a hand-added
// `export` prefix, spacing changes) must never produce duplicate keys or
// duplicate tables — TOML rejects both with parse errors, bricking the
// client config (the kimi incident).

func countLines(t *testing.T, text, needle string) int {
	t.Helper()
	return strings.Count(text, needle)
}

// TestReplaceOrAppendTOMLSection_ClientRewrittenHeader: every semantically
// equal spelling of the target header matches — the section is replaced in
// place (and normalized back to the template's canonical header), never
// appended as a second table.
func TestReplaceOrAppendTOMLSection_ClientRewrittenHeader(t *testing.T) {
	variants := []string{
		`[providers.model-proxy]`,     // kimi-cli dropped the quotes
		`[ providers."model-proxy" ]`, // whitespace inside brackets
		`[providers. 'model-proxy']`,  // single-quoted part + spacing
		`[providers."model-proxy"]`,   // canonical (control)
	}
	for _, header := range variants {
		in := "keep = 1\n" + header + "\ntype = \"openai\"\nbase_url = \"http://old\"\n"
		section := "\n[providers.\"model-proxy\"]\ntype = \"openai\"\nbase_url = \"http://new\"\n"
		out := takeover.ReplaceOrAppendTOMLSection(in, `providers."model-proxy"`, section)
		if got := countLines(t, out, "[providers."); got != 1 {
			t.Errorf("header %q: %d provider tables after rewrite, want 1:\n%s", header, got, out)
		}
		if !strings.Contains(out, `base_url = "http://new"`) || strings.Contains(out, "http://old") {
			t.Errorf("header %q: body not replaced:\n%s", header, out)
		}
		if !strings.Contains(out, `[providers."model-proxy"]`) {
			t.Errorf("header %q: canonical header not restored:\n%s", header, out)
		}
		if !strings.Contains(out, "keep = 1") {
			t.Errorf("header %q: unrelated content dropped:\n%s", header, out)
		}
	}
}

// TestReplaceOrAppendTOMLSection_CollapsesDuplicates: a file already carrying
// duplicate tables (e.g. from an earlier buggy append) self-heals — one
// canonical section remains.
func TestReplaceOrAppendTOMLSection_CollapsesDuplicates(t *testing.T) {
	in := `[providers."model-proxy"]
type = "openai"
base_url = "http://old"

[models."glm-5.3-flash"]
provider = "model-proxy"

[providers.model-proxy]
type = "openai"
base_url = "http://older"
`
	section := "\n[providers.\"model-proxy\"]\ntype = \"openai\"\nbase_url = \"http://new\"\n"
	out := takeover.ReplaceOrAppendTOMLSection(in, `providers."model-proxy"`, section)
	if got := countLines(t, out, "[providers."); got != 1 {
		t.Errorf("%d provider tables after rewrite, want 1:\n%s", got, out)
	}
	if !strings.Contains(out, `base_url = "http://new"`) {
		t.Errorf("body not replaced:\n%s", out)
	}
	if !strings.Contains(out, `[models."glm-5.3-flash"]`) {
		t.Errorf("unrelated model table dropped:\n%s", out)
	}
}

// TestReplaceOrAppendTOMLSection_HeaderTrailingComment: TOML allows a
// trailing comment on a header line (`[providers.model-proxy] # my note`,
// e.g. left behind by hand editing). The line must still be recognized as the
// managed table — replacing in place — never appended as a second table,
// which TOML rejects with a parse error. The comment belongs to the replaced
// section and goes with it (the canonical template header is restored).
func TestReplaceOrAppendTOMLSection_HeaderTrailingComment(t *testing.T) {
	in := "keep = 1\n[providers.model-proxy] # 手写注释\ntype = \"openai\"\nbase_url = \"http://old\"\n"
	section := "\n[providers.\"model-proxy\"]\ntype = \"openai\"\nbase_url = \"http://new\"\n"
	out := takeover.ReplaceOrAppendTOMLSection(in, `providers."model-proxy"`, section)
	if got := countLines(t, out, "[providers."); got != 1 {
		t.Errorf("%d provider tables after rewrite, want 1:\n%s", got, out)
	}
	if !strings.Contains(out, `base_url = "http://new"`) || strings.Contains(out, "http://old") {
		t.Errorf("body not replaced:\n%s", out)
	}
	if !strings.Contains(out, `[providers."model-proxy"]`) {
		t.Errorf("canonical header not restored:\n%s", out)
	}
	if !strings.Contains(out, "keep = 1") {
		t.Errorf("unrelated content dropped:\n%s", out)
	}
}

// TestReplaceOrAppendTOMLSection_CommentHashInsideQuotes: a `#` inside a
// quoted key part is NOT a comment — `[providers."a#b"]` is one table named
// a#b and must still match its quoted target.
func TestReplaceOrAppendTOMLSection_CommentHashInsideQuotes(t *testing.T) {
	in := "[providers.\"a#b\"]\nbase_url = \"http://old\"\n"
	section := "\n[providers.\"a#b\"]\nbase_url = \"http://new\"\n"
	out := takeover.ReplaceOrAppendTOMLSection(in, `providers."a#b"`, section)
	if got := countLines(t, out, "[providers."); got != 1 {
		t.Errorf("%d provider tables after rewrite, want 1:\n%s", got, out)
	}
	if !strings.Contains(out, `base_url = "http://new"`) {
		t.Errorf("body not replaced:\n%s", out)
	}
}

// TestReplaceOrAppendTOMLSection_ArrayTableUntouched: [[array]] headers never
// match a plain-section target.
func TestReplaceOrAppendTOMLSection_ArrayTableUntouched(t *testing.T) {
	in := "[[history]]\nentry = 1\n"
	section := "\n[history]\nx = 1\n"
	out := takeover.ReplaceOrAppendTOMLSection(in, "history", section)
	if !strings.Contains(out, "[[history]]") || !strings.Contains(out, "[history]") {
		t.Errorf("array table matched or dropped:\n%s", out)
	}
}

// TestSetTOMLTopKey_QuotedAndDuplicateVariants: quoted key spellings match,
// and duplicate managed key lines collapse to one canonical line.
func TestSetTOMLTopKey_QuotedAndDuplicateVariants(t *testing.T) {
	in := "keep = \"x\"\nmodel_provider=\"model-proxy\"\n'model_provider' = \"other\"\n\n[models.\"m\"]\nprovider = \"model-proxy\"\n"
	out := takeover.SetTOMLTopKey(in, "model_provider", `"model-proxy"`)
	if got := countLines(t, out, "model_provider"); got != 1 {
		t.Errorf("%d model_provider lines, want 1:\n%s", got, out)
	}
	if !strings.Contains(out, `model_provider = "model-proxy"`) {
		t.Errorf("canonical line missing:\n%s", out)
	}
	if !strings.Contains(out, `keep = "x"`) || !strings.Contains(out, `[models."m"]`) {
		t.Errorf("unrelated content dropped:\n%s", out)
	}
}
