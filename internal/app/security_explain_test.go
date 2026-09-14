package app

import (
	"errors"
	"strings"
	"testing"

	"model-proxy/internal/appapi"
	"model-proxy/internal/guard"
)

// Synthetic fixture material only — no real credentials anywhere in tests
// (AGENTS.md credential red line).
const explainFixtureKey = "sk-ant-api03-X9fQ2vB7nM4kL8pR1tW6yU3iO0aS5dF7gH9jK2lZ4"

func explainTestProxy(t *testing.T, scanner *guard.Scanner) *Proxy {
	t.Helper()
	return &Proxy{generationState: generationState{guardScanner: scanner}}
}

func TestLocateGuardHitsSecretMasked(t *testing.T) {
	scanner, err := guard.NewScanner(nil, guard.Known(), nil)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"messages":[{"role":"user","content":"use ` + explainFixtureKey + ` please"}]}`)
	matches, err := explainTestProxy(t, scanner).locateGuardHits(body, "secret", []string{"anthropic_api_key"})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || !matches[0].Located {
		t.Fatalf("matches = %+v, want 1 located", matches)
	}
	m := matches[0]
	if strings.Contains(m.Pre+m.Hit+m.Post, explainFixtureKey) {
		t.Errorf("snippet leaks the secret: %q", m.Pre+m.Hit+m.Post)
	}
	if !strings.HasPrefix(m.Hit, "sk-a") || !strings.HasSuffix(m.Hit, "Z4") || !strings.Contains(m.Hit, "…") {
		t.Errorf("masked hit = %q, want head…tail form", m.Hit)
	}
	if m.Regex == "" || m.Source == "" || m.Explanation == "" {
		t.Errorf("match = %+v, want regex/source/explanation populated", m)
	}
}

func TestLocateGuardHitsPathVerbatim(t *testing.T) {
	scanner, err := guard.NewScanner(nil, guard.Known(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The path hit sits near a secret: the snippet must mask the secret even
	// though the requested kind is path (context bytes are sensitive too).
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","input":{"file_path":"~/.aws/credentials","note":"key ` + explainFixtureKey + `"}}]}]}`)
	matches, err := explainTestProxy(t, scanner).locateGuardHits(body, "path", []string{"aws_creds"})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || !matches[0].Located {
		t.Fatalf("matches = %+v, want 1 located", matches)
	}
	m := matches[0]
	if m.Strength != guard.StrengthStrong {
		t.Errorf("strength = %q, want strong (tool_use input)", m.Strength)
	}
	if m.Hit != "~/.aws/credentials" {
		t.Errorf("path hit = %q, want the verbatim literal", m.Hit)
	}
	if strings.Contains(m.Pre+m.Post, explainFixtureKey) {
		t.Errorf("path snippet leaks the adjacent secret: %q", m.Pre+m.Post)
	}
	if !strings.Contains(m.Pre+m.Post, "sk-a…") {
		t.Errorf("path snippet should mask the adjacent secret head/tail: %q", m.Pre+m.Post)
	}
	if m.Explanation == "" {
		t.Error("path category explanation is empty")
	}
}

func TestLocateGuardHitsUnlocatedNames(t *testing.T) {
	scanner, err := guard.NewScanner(nil, guard.Known(), nil)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"messages":[]}`)
	matches, err := explainTestProxy(t, scanner).locateGuardHits(body, "secret",
		[]string{"anthropic_api_key", "known_secret_fragmented"})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("matches = %+v, want one Located=false entry per requested name", matches)
	}
	for _, m := range matches {
		if m.Located {
			t.Errorf("%s located on a clean body?", m.Name)
		}
		if m.Explanation == "" {
			t.Errorf("%s: explanation empty", m.Name)
		}
	}
	if !strings.Contains(matches[1].Explanation, "cross-request") {
		t.Errorf("fragmented explanation = %q, want cross-request note", matches[1].Explanation)
	}
}

func TestLocateGuardHitsPerNameCap(t *testing.T) {
	scanner, err := guard.NewScanner(nil, guard.Known(), nil)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(strings.Repeat("~/.ssh ", explainMaxPerName+3))
	matches, err := explainTestProxy(t, scanner).locateGuardHits(body, "path", []string{"ssh"})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != explainMaxPerName {
		t.Errorf("matches = %d, want capped at %d", len(matches), explainMaxPerName)
	}
}

func TestLocateGuardHitsNoScanner(t *testing.T) {
	_, err := explainTestProxy(t, nil).locateGuardHits([]byte("x"), "secret", []string{"ssh"})
	if !errors.Is(err, appapi.ErrGuardScannerUnavailable) {
		t.Errorf("err = %v, want ErrGuardScannerUnavailable", err)
	}
}
