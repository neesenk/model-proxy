package guard

import (
	"regexp"
	"strings"
	"testing"
)

// Synthetic fixture material only — no real credentials anywhere in tests
// (AGENTS.md credential red line).
const locateAnthropicKey = "sk-ant-api03-X9fQ2vB7nM4kL8pR1tW6yU3iO0aS5dF7gH9jK2lZ4"

func TestLocateSecretSpans(t *testing.T) {
	s, err := NewScanner(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"key":"` + locateAnthropicKey + `"}`)
	got := s.Locate(body, []string{"anthropic_api_key"})
	if len(got) != 1 {
		t.Fatalf("Locate = %v, want 1 match", got)
	}
	m := got[0]
	if m.Name != "anthropic_api_key" || m.Strength != "" {
		t.Errorf("match = %+v, want name anthropic_api_key with empty strength", m)
	}
	if string(body[m.Start:m.End]) != locateAnthropicKey {
		t.Errorf("span [%d:%d] = %q, want the fixture key", m.Start, m.End, body[m.Start:m.End])
	}
}

func TestLocateNameFilterAndOrder(t *testing.T) {
	s, err := NewScanner(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Path occurrence precedes the secret occurrence; output must be
	// offset-sorted, and unrequested names must not appear.
	body := []byte("see ~/.ssh first " + locateAnthropicKey)
	got := s.Locate(body, []string{"anthropic_api_key", "ssh", "github_token"})
	if len(got) != 2 {
		t.Fatalf("Locate = %v, want 2 matches (ssh + anthropic_api_key)", got)
	}
	if got[0].Name != "ssh" || got[1].Name != "anthropic_api_key" {
		t.Errorf("order = %v, want [ssh anthropic_api_key] by offset", got)
	}
	if got[0].Start != strings.Index(string(body), "~/.ssh") {
		t.Errorf("ssh span start = %d, want the ~/.ssh occurrence", got[0].Start)
	}
	if got := s.Locate(body, []string{"github_token"}); len(got) != 0 {
		t.Errorf("Locate(github_token) = %v, want none", got)
	}
}

func TestLocateKnownSecret(t *testing.T) {
	known := "known-secret-fixture-7d6c5b4a3"
	s, err := NewScanner(nil, []string{known}, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("prefix " + known + " suffix")
	got := s.Locate(body, []string{"known_secret"})
	if len(got) != 1 || got[0].Name != "known_secret" {
		t.Fatalf("Locate = %v, want 1 known_secret match", got)
	}
	if string(body[got[0].Start:got[0].End]) != known {
		t.Errorf("span = %q, want the known fixture value", body[got[0].Start:got[0].End])
	}
}

func TestLocatePathStrength(t *testing.T) {
	s, err := NewScanner(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, body, cat, wantStrength string
	}{
		{
			"tool_use input is strong",
			`{"messages":[{"role":"assistant","content":[{"type":"tool_use","input":{"file_path":"~/.ssh/id_rsa"}}]}]}`,
			"ssh", StrengthStrong,
		},
		{
			"plain prose is weak",
			`{"messages":[{"role":"user","content":"how do I back up ~/.ssh keys?"}]}`,
			"ssh", StrengthWeak,
		},
		{
			"non-JSON body downgrades to weak",
			`cat ~/.ssh/id_rsa {"broken":`,
			"ssh", StrengthWeak,
		},
		{
			"dotenv boundary respected in tool position",
			`{"messages":[{"role":"assistant","content":[{"type":"tool_use","input":{"file_path":".env"}}]}]}`,
			"dotenv", StrengthStrong,
		},
	}
	for _, c := range cases {
		got := s.Locate([]byte(c.body), []string{c.cat})
		if len(got) == 0 {
			t.Errorf("%s: no matches, want 1+", c.name)
			continue
		}
		for _, m := range got {
			if m.Strength != c.wantStrength {
				t.Errorf("%s: strength = %q, want %q", c.name, m.Strength, c.wantStrength)
			}
		}
	}
}

func TestLocatePathBoundary(t *testing.T) {
	s, err := NewScanner(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Locate([]byte("edit foo.envx then .env"), []string{"dotenv"}); len(got) != 1 {
		t.Fatalf("Locate(dotenv) = %v, want exactly the standalone .env hit", got)
	} else if got[0].Strength != StrengthWeak {
		t.Errorf("strength = %q, want weak (non-JSON body)", got[0].Strength)
	}
}

func TestLocateNilAndEmpty(t *testing.T) {
	var nilScanner *Scanner
	if got := nilScanner.Locate([]byte("x"), []string{"ssh"}); got != nil {
		t.Errorf("nil scanner Locate = %v, want nil", got)
	}
	s, err := NewScanner(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Locate([]byte(locateAnthropicKey), nil); got != nil {
		t.Errorf("empty names Locate = %v, want nil", got)
	}
}

func TestRuleInfo(t *testing.T) {
	custom := []CustomPattern{{
		Name:    "myvendor_key",
		RE:      regexp.MustCompile(`\bmv-[A-Za-z0-9]{32,}`),
		Literal: []byte("mv-"),
	}}
	s, err := NewScanner(custom, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	regex, source, ok := s.RuleInfo("anthropic_api_key")
	if !ok || !strings.Contains(regex, "sk-ant-") || source == "" {
		t.Errorf("RuleInfo(anthropic_api_key) = %q, %q, %v", regex, source, ok)
	}
	regex, source, ok = s.RuleInfo("myvendor_key")
	if !ok || regex != `\bmv-[A-Za-z0-9]{32,}` || source != "config" {
		t.Errorf("RuleInfo(myvendor_key) = %q, %q, %v", regex, source, ok)
	}
	for _, name := range []string{"ssh", "dotenv", "known_secret", "known_secret_fragmented", "no_such_rule"} {
		if _, _, ok := s.RuleInfo(name); ok {
			t.Errorf("RuleInfo(%q) ok = true, want false", name)
		}
	}
	var nilScanner *Scanner
	if _, _, ok := nilScanner.RuleInfo("anthropic_api_key"); ok {
		t.Error("nil scanner RuleInfo ok = true, want false")
	}
}

func TestMaskSnippetSecretHit(t *testing.T) {
	s, err := NewScanner(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"key":"` + locateAnthropicKey + `"}`)
	m := s.Locate(body, []string{"anthropic_api_key"})[0]
	pre, hit, post := s.MaskSnippet(body, m.Start, m.End, true, 80)
	if pre == "" || post == "" {
		t.Fatalf("pre=%q post=%q, want context on both sides", pre, post)
	}
	if strings.Contains(pre+hit+post, locateAnthropicKey) {
		t.Errorf("snippet leaks the secret: %q", pre+hit+post)
	}
	if !strings.HasPrefix(hit, "sk-a") || !strings.HasSuffix(hit, "Z4") || !strings.Contains(hit, "…") {
		t.Errorf("masked hit = %q, want head…tail form", hit)
	}
}

func TestMaskSnippetPathHitMasksAdjacentSecret(t *testing.T) {
	s, err := NewScanner(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A weak path mention in prose next to a full secret: the path stays
	// verbatim (it is the hit), the secret in the context must be masked.
	body := []byte("key " + locateAnthropicKey + " then edit ~/.ssh please")
	m := s.Locate(body, []string{"ssh"})[0]
	pre, hit, post := s.MaskSnippet(body, m.Start, m.End, false, 80)
	if strings.Contains(pre+hit+post, locateAnthropicKey) {
		t.Errorf("path snippet leaks the adjacent secret: %q", pre+hit+post)
	}
	if !strings.Contains(pre, "sk-a…") {
		t.Errorf("adjacent secret should be masked head…tail: %q", pre)
	}
	if hit != "~/.ssh" {
		t.Errorf("hit = %q, want verbatim ~/.ssh", hit)
	}
}

func TestMaskSnippetOffsetsWithMaskBeforeHit(t *testing.T) {
	s, err := NewScanner(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Masked secret BEFORE an unmasked path hit: replacements change the
	// byte count ahead of the hit, so its offsets must be recomputed.
	body := []byte("secret " + locateAnthropicKey + " path ~/.ssh end")
	m := s.Locate(body, []string{"ssh"})[0]
	pre, hit, _ := s.MaskSnippet(body, m.Start, m.End, false, 80)
	if hit != "~/.ssh" {
		t.Errorf("hit after a masked prefix = %q (pre %q)", hit, pre)
	}
	if strings.Contains(pre, locateAnthropicKey) {
		t.Errorf("pre leaks the secret: %q", pre)
	}
}

func TestMaskSnippetShortSecret(t *testing.T) {
	s, err := NewScanner(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := maskSecretBytes([]byte("short")); got != "***" {
		t.Errorf("maskSecretBytes(short) = %q, want ***", got)
	}
	_ = s
}
