package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// cli_extra3_test.go covers Task 9 observability grouping: `doctor` (offline)
// and `schedule` (live daemon) text output renders the credential pool — the
// parent name, account count, and per-account virtual ids — instead of listing
// opaque zhipu#<id> entries.

// --- doctor: pool rendered as parent + account count + virtual ids ---

// TestCmdDoctor_PoolGrouping runs cmdDoctor in-process on a config with a
// 3-account zhipu pool and asserts:
//   - the parent name "zhipu" appears (not just opaque virtual ids)
//   - the account count "(3 accounts)" is shown
//   - each virtual id (zhipu#<id>) is listed
//   - the session-sticky round-robin note is present (offline: no live quota)
func TestCmdDoctor_PoolGrouping(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "K1", "K2", "K3")
	cfgPath := writeTempConfig(t, `listen: 127.0.0.1:1
providers:
  zhipu:
    openai_base_url: https://zhipu.invalid/api/paas/v4
    provider_id: zhipu
    models:
      - glm-5.2
routes:
  glm-5.2:
    - {provider: zhipu, model: glm-5.2, priority: 1}
`)

	out := grabStdout(t, func() { cmdDoctor([]string{"--config", cfgPath}) })

	if !strings.Contains(out, "zhipu") {
		t.Errorf("doctor output missing parent name zhipu:\n%s", out)
	}
	if !strings.Contains(out, "3 accounts") {
		t.Errorf("doctor output missing '3 accounts':\n%s", out)
	}
	// Each virtual id from the pool appears (parent is shown expanded).
	pool, _ := loadPool("zhipu", "zhipu")
	for _, a := range pool.Accounts {
		vid := "zhipu#" + a.ID
		if !strings.Contains(out, vid) {
			t.Errorf("doctor output missing virtual id %q:\n%s", vid, out)
		}
	}
	// Session-sticky round-robin note (offline path).
	if !strings.Contains(out, "round-robin") {
		t.Errorf("doctor output missing 'round-robin' note:\n%s", out)
	}
}

// TestCmdDoctor_SingleProviderNoPool verifies the non-pooled path is unchanged:
// no "(N accounts)" annotation, no virtual ids.
func TestCmdDoctor_SingleProviderNoPool(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(home+"/.model-proxy", 0o700)
	// No pool file → single-account path; provider name appears plainly.
	cfgPath := writeTempConfig(t, minimalConfig)
	out := grabStdout(t, func() { cmdDoctor([]string{"--config", cfgPath}) })
	if !strings.Contains(out, "aqp") {
		t.Errorf("doctor output missing provider aqp:\n%s", out)
	}
	if strings.Contains(out, "accounts") {
		t.Errorf("non-pooled doctor output should not mention accounts:\n%s", out)
	}
	if strings.Contains(out, "#") {
		t.Errorf("non-pooled doctor output should not contain virtual ids:\n%s", out)
	}
}

// TestCmdDoctor_ConversionReport: a route target declaring a backend protocol is
// surfaced in the doctor report with its protocol + the fixed lossy-items list.
func TestCmdDoctor_ConversionReport(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(home+"/.model-proxy", 0o700)
	cfgPath := writeTempConfig(t, `listen: 127.0.0.1:1
providers:
  oai:
    openai_base_url: https://x.invalid
    anthropic_base_url: https://x.invalid
    provider_id: static
routes:
  claude-x:
    - {provider: oai, model: gpt-x, priority: 1, protocol: openai}
`)
	out := grabStdout(t, func() { cmdDoctor([]string{"--config", cfgPath}) })
	if !strings.Contains(out, "converts when client protocol differs") {
		t.Errorf("doctor missing conversion note:\n%s", out)
	}
	if !strings.Contains(out, "protocol openai") {
		t.Errorf("doctor missing the declared protocol:\n%s", out)
	}
	// Current residual-loss and newly-preserved items.
	for _, want := range []string{"anthropic↔chat thinking", "unsupported server tools", "input_audio", "documents", "tool-result media"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor missing lossy item %q:\n%s", want, out)
		}
	}
}

// TestCmdDoctor_ShadowSection: a configured shadow block is surfaced in the
// doctor report — one line per route with the candidate provider/model, the
// backend protocol, and the global sample_rate / max_concurrent.
func TestCmdDoctor_ShadowSection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(home+"/.model-proxy", 0o700)
	cfgPath := writeTempConfig(t, `listen: 127.0.0.1:1
providers:
  zhipu:
    openai_base_url: https://x.invalid
    provider_id: zhipu
  deepseek:
    openai_base_url: https://x.invalid
    provider_id: deepseek
routes:
  glm:
    - {provider: zhipu, model: glm-5.2, priority: 1}
shadow:
  glm: {provider: deepseek, model: deepseek-v4-pro, protocol: openai}
shadow_sample_rate: 0.5
shadow_max_concurrent: 8
`)
	out := grabStdout(t, func() { cmdDoctor([]string{"--config", cfgPath}) })
	for _, want := range []string{"Shadow", "glm", "deepseek/deepseek-v4-pro", "protocol=openai", "sample_rate=0.5", "max_concurrent=8"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor shadow section missing %q:\n%s", want, out)
		}
	}
}

// TestCmdDoctor_ShadowDefaults: without explicit shadow_sample_rate /
// shadow_max_concurrent the section shows the runtime defaults (1 / 4), and an
// entry without protocol renders as same-as-primary.
func TestCmdDoctor_ShadowDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(home+"/.model-proxy", 0o700)
	cfgPath := writeTempConfig(t, `listen: 127.0.0.1:1
providers:
  zhipu:
    openai_base_url: https://x.invalid
    provider_id: zhipu
routes:
  glm:
    - {provider: zhipu, model: glm-5.2, priority: 1}
shadow:
  glm: {provider: zhipu, model: glm-4.5}
`)
	out := grabStdout(t, func() { cmdDoctor([]string{"--config", cfgPath}) })
	for _, want := range []string{"Shadow", "zhipu/glm-4.5", "protocol=same-as-primary", "sample_rate=1", "max_concurrent=4"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor shadow defaults missing %q:\n%s", want, out)
		}
	}
}

// TestCmdDoctor_NoShadowSection: with no shadow configured the report has no
// Shadow section at all.
func TestCmdDoctor_NoShadowSection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(home+"/.model-proxy", 0o700)
	cfgPath := writeTempConfig(t, minimalConfig)
	out := grabStdout(t, func() { cmdDoctor([]string{"--config", cfgPath}) })
	if strings.Contains(out, "Shadow") {
		t.Errorf("doctor without shadow config should not show a Shadow section:\n%s", out)
	}
}

// --- schedule: live daemon payload rendered with pool grouping ---

// TestCmdSchedule_PoolGrouping stands up a mock /debug/schedule daemon whose
// payload describes a 2-account zhipu pool (virtuals carrying pool_parent), and
// asserts cmdSchedule's text output groups them under a "pool:" header.
func TestCmdSchedule_PoolGrouping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/debug/schedule" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		// Payload mirrors what scheduleStatus would emit for a 2-account pool.
		w.Write([]byte(`{"models":{"glm-5.2":{"first":"zhipu#acc-a","ordered":[{"provider":"zhipu#acc-a","pool_parent":"zhipu","priority":1,"tier":"plan","surplus":0.5,"available":true,"peak":false},{"provider":"zhipu#acc-b","pool_parent":"zhipu","priority":1,"tier":"plan","surplus":0.4,"available":true,"peak":false}],"pools":[{"parent":"zhipu","accounts":2,"available":2}]}}}`))
	}))
	defer srv.Close()
	listen := strings.TrimPrefix(srv.URL, "http://")
	cfgPath := writeTempConfig(t, "listen: "+listen+"\nproviders:\n  zhipu:\n    openai_base_url: https://x\n    provider_id: zhipu\n    models:\n      - glm-5.2\nroutes:\n  glm-5.2:\n    - {provider: zhipu, model: glm-5.2}\n")

	out := grabStdout(t, func() { cmdSchedule([]string{"--config", cfgPath}) })
	if !strings.Contains(out, "glm-5.2") {
		t.Errorf("schedule output missing model:\n%s", out)
	}
	// Pool header: "pool: zhipu (2 accounts)" or similar — at minimum the
	// parent name + account count must appear together so a human can see it's
	// a pool.
	if !strings.Contains(out, "pool:") {
		t.Errorf("schedule output missing 'pool:' header:\n%s", out)
	}
	if !strings.Contains(out, "zhipu") {
		t.Errorf("schedule output missing parent name zhipu:\n%s", out)
	}
	if !strings.Contains(out, "2 accounts") {
		t.Errorf("schedule output missing '2 accounts':\n%s", out)
	}
	// Virtual ids are still listed (each carries its own surplus).
	if !strings.Contains(out, "zhipu#acc-a") || !strings.Contains(out, "zhipu#acc-b") {
		t.Errorf("schedule output missing virtual ids:\n%s", out)
	}
}

// TestCmdSchedule_NonPooledUnchanged verifies a non-pooled schedule payload
// (no pool_parent, no pools) renders without a pool header.
func TestCmdSchedule_NonPooledUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"models":{"m":{"first":"aqp","ordered":[{"provider":"aqp","priority":1,"tier":"plan","surplus":0,"available":true,"peak":false}]}}}`))
	}))
	defer srv.Close()
	listen := strings.TrimPrefix(srv.URL, "http://")
	cfgPath := writeTempConfig(t, "listen: "+listen+"\nproviders:\n  aqp:\n    openai_base_url: https://x\n    provider_id: aqp\n    models:\n      - m\nroutes:\n  m:\n    - {provider: aqp, model: m}\n")
	out := grabStdout(t, func() { cmdSchedule([]string{"--config", cfgPath}) })
	if !strings.Contains(out, "aqp") {
		t.Errorf("schedule output missing provider aqp:\n%s", out)
	}
	if strings.Contains(out, "pool:") {
		t.Errorf("non-pooled schedule should not emit pool header:\n%s", out)
	}
}
