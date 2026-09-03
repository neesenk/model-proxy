package doctor_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clidoctor "model-proxy/internal/cli/doctor"
	configdomain "model-proxy/internal/config"
	observeseclog "model-proxy/internal/observe/seclog"
)

// writeDriftScene builds the two-client takeover scene used by the drift
// audit tests: claude taken over with an intact pointer (✓), opencode taken
// over with a stale pointer that carries a path AND a query string (drift).
// Client files land at the preset template locations under the isolated home;
// the .bak markers live in <home>/.model-proxy — the same dir the audit log
// resolves to when cfgPath is <home>/config.yaml.
func writeDriftScene(t *testing.T, home string, cfg *configdomain.Config, proxyURL string) {
	t.Helper()
	write := func(rel, content string) {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// claude: taken over, pointer intact.
	write(".claude/settings.json", `{"env":{"ANTHROPIC_BASE_URL":"`+proxyURL+`"}}`)
	// opencode: taken over, stale pointer with path + query — the audit record
	// must keep only the host.
	write(".config/opencode/opencode.json", `{"provider":{"model-proxy":{"options":{"baseURL":"http://127.0.0.1:9999/v1?session=abc"}}}}`)
	bakDir := filepath.Join(home, ".model-proxy")
	for _, name := range []string{"claude", "opencode"} {
		if err := os.MkdirAll(bakDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bakDir, name+".bak"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

const driftAuditStatusJSON = `{
  "uptime":"1m0s","version":"0.4.2","listen":%q,
  "health":{"aqp":{"circuit_state":"closed","available":true}},
  "quota":{"aqp":{"RemainingPct":0.9}},
  "schedule":{"models":{"glm-5.2":{"first":"aqp","ordered":[{"provider":"aqp","priority":1,"tier":"plan","surplus":1,"available":true}]}}},
  "model_locks":{}
}`

// TestRenderDoctorLiveDriftAudit: a drifted client is appended to the
// security audit log (kind=drift, agent=doctor) with hosts only — never the
// URL path or query string. The intact client writes nothing.
func TestRenderDoctorLiveDriftAudit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	statusJSON := func(addr string) string { return fmt.Sprintf(driftAuditStatusJSON, addr) }
	addr := doctorLiveTestServer(t, statusJSON, `{"enabled":false,"records":[]}`)
	cfg := doctorLiveTestCfg(t, addr, `providers:
  aqp: {provider_id: aqp, openai_base_url: https://x}
routes:
  glm-5.2:
    - {provider: aqp, model: glm-5.2, priority: 1}
`)
	proxyURL := "http://" + addr
	writeDriftScene(t, home, cfg, proxyURL)

	out, err := clidoctor.RenderDoctorLive(cfg, filepath.Join(home, "config.yaml"))
	if err != nil {
		t.Fatalf("renderDoctorLive: %v", err)
	}
	if !strings.Contains(out, "takeover drift: opencode") {
		t.Fatalf("diagnosis missing drift line:\n%s", out)
	}

	dir := filepath.Join(home, ".model-proxy")
	result, err := observeseclog.Query(dir, observeseclog.Filter{Kind: observeseclog.KindDrift})
	if err != nil {
		t.Fatalf("query audit log: %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("drift records = %d, want exactly 1 (only opencode drifted)", len(result.Records))
	}
	rec := result.Records[0]
	if rec.Agent != "doctor" || rec.Kind != observeseclog.KindDrift {
		t.Errorf("record kind/agent = %q/%q, want drift/doctor", rec.Kind, rec.Agent)
	}
	if rec.Ts == 0 {
		t.Error("record ts not stamped")
	}
	for _, want := range []string{"client=opencode", "expected=" + addr, "actual=127.0.0.1:9999"} {
		if !strings.Contains(rec.Detail, want) {
			t.Errorf("detail missing %q: %q", want, rec.Detail)
		}
	}
	// Red line: the detail carries hosts only — no URL path, no query string.
	for _, banned := range []string{"/v1", "session", "?"} {
		if strings.Contains(rec.Detail, banned) {
			t.Errorf("detail must not contain %q: %q", banned, rec.Detail)
		}
	}
}

// TestRenderDoctorLiveDriftAuditDeduplicatesSameDay: drift usually persists
// until the user fixes it, so repeated doctor --live runs must not append the
// same client's record over and over — one record per client per day.
func TestRenderDoctorLiveDriftAuditDeduplicatesSameDay(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	statusJSON := func(addr string) string { return fmt.Sprintf(driftAuditStatusJSON, addr) }
	addr := doctorLiveTestServer(t, statusJSON, `{"enabled":false,"records":[]}`)
	cfg := doctorLiveTestCfg(t, addr, `providers:
  aqp: {provider_id: aqp, openai_base_url: https://x}
routes:
  glm-5.2:
    - {provider: aqp, model: glm-5.2, priority: 1}
`)
	writeDriftScene(t, home, cfg, "http://"+addr)

	for run := 1; run <= 2; run++ {
		if _, err := clidoctor.RenderDoctorLive(cfg, filepath.Join(home, "config.yaml")); err != nil {
			t.Fatalf("renderDoctorLive run %d: %v", run, err)
		}
	}
	result, err := observeseclog.Query(filepath.Join(home, ".model-proxy"),
		observeseclog.Filter{Kind: observeseclog.KindDrift})
	if err != nil {
		t.Fatalf("query audit log: %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("drift records after 2 runs = %d, want 1 (same-day dedup)", len(result.Records))
	}
}

// TestRenderDoctorLiveDriftAuditDisabled: guard.audit=false suppresses the
// audit append; doctor output is unchanged.
func TestRenderDoctorLiveDriftAuditDisabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	statusJSON := func(addr string) string { return fmt.Sprintf(driftAuditStatusJSON, addr) }
	addr := doctorLiveTestServer(t, statusJSON, `{"enabled":false,"records":[]}`)
	cfg := doctorLiveTestCfg(t, addr, `guard:
  audit: false
providers:
  aqp: {provider_id: aqp, openai_base_url: https://x}
routes:
  glm-5.2:
    - {provider: aqp, model: glm-5.2, priority: 1}
`)
	writeDriftScene(t, home, cfg, "http://"+addr)

	out, err := clidoctor.RenderDoctorLive(cfg, filepath.Join(home, "config.yaml"))
	if err != nil {
		t.Fatalf("renderDoctorLive: %v", err)
	}
	if !strings.Contains(out, "takeover drift: opencode") {
		t.Fatalf("diagnosis missing drift line:\n%s", out)
	}
	entries, err := os.ReadDir(filepath.Join(home, ".model-proxy"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "security-") {
			t.Errorf("audit disabled but security log %q was written", e.Name())
		}
	}
}
