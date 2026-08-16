package doctor_test

import (
	"fmt"
	clidoctor "model-proxy/internal/cli/doctor"
	configdomain "model-proxy/internal/config"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// doctorLiveTestServer serves the canned /api/status + /api/requests pair that
// the renderDoctorLive integration tests drive (same httptest pattern as
// TestRenderStatusIntegration).
func doctorLiveTestServer(t *testing.T, statusJSON func(addr string) string, requestsJSON string) (addr string) {
	t.Helper()
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	addr = ts.Listener.Addr().String()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, statusJSON(addr))
	})
	mux.HandleFunc("/api/requests", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, requestsJSON)
	})
	return addr
}

// doctorLiveTestCfg loads a config bound to the test server address (listen
// must point at the httptest listener — renderDoctorLive dials cfg.Listen).
func doctorLiveTestCfg(t *testing.T, addr, extra string) *configdomain.Config {
	t.Helper()
	cfg, err := configdomain.LoadConfigFromBytes("test", []byte("listen: "+addr+"\n"+extra))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

// TestRenderDoctorLiveRouteDown: every target of a route is cooling down
// (429-quota + circuit breaker) → the schedule's ordered list is empty, and
// the verdict must name the target count, the EARLIEST recovery across both
// cooldown kinds, and the unfreeze hint for that provider.
func TestRenderDoctorLiveRouteDown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	aqpUntil := time.Now().Add(20 * time.Minute).UTC().Format(time.RFC3339)
	zhipuUntil := time.Now().Add(45 * time.Minute).UTC().Format(time.RFC3339)
	statusJSON := func(addr string) string {
		return fmt.Sprintf(`{
		  "uptime":"3h12m0s","version":"0.4.2","listen":%q,
		  "health":{
		    "aqp":{"circuit_state":"closed","available":false,"rate_limited_until":%q,"rate_limit_kind":"quota"},
		    "zhipu":{"circuit_state":"open","available":false,"circuit_until":%q}
		  },
		  "quota":{},
		  "schedule":{"models":{"claude":{"first":"","ordered":[]}}},
		  "model_locks":{}
		}`, addr, aqpUntil, zhipuUntil)
	}
	addr := doctorLiveTestServer(t, statusJSON, `{"enabled":false,"records":[]}`)
	cfg := doctorLiveTestCfg(t, addr, `providers:
  aqp: {provider_id: aqp, openai_base_url: https://x}
  zhipu: {provider_id: zhipu, openai_base_url: https://y}
routes:
  claude:
    - {provider: aqp, model: glm-5.2, priority: 1}
    - {provider: zhipu, model: glm-5.2, priority: 2}
`)

	out, err := clidoctor.RenderDoctorLive(cfg, filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("renderDoctorLive: %v", err)
	}
	aqpLocal, _ := time.Parse(time.RFC3339, aqpUntil)
	for _, want := range []string{
		"model-proxy doctor --live",
		"daemon running (v0.4.2, uptime 3h12m0s)",
		"Diagnosis",
		`✗ route "claude": 2 targets all unavailable`,
		"earliest recovery " + aqpLocal.Local().Format("15:04") + " (aqp, quota cooldown)",
		"[可立即执行] model-proxy unfreeze aqp",
		// aqp is a single-account login -> no pool to grow; the quota fix is a
		// config change naming the exact route key.
		"[需要改配置] config.yaml 的 routes.claude",
		"request_log disabled",
		"not taken over",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// aqp is not pool-capable: the [需要凭据] login hint must NOT appear.
	if strings.Contains(out, "[需要凭据]") {
		t.Errorf("single-account provider must not get a login hint:\n%s", out)
	}
	// The zhipu circuit expires later — it must NOT be quoted as the earliest.
	if strings.Contains(out, "circuit breaker)") {
		t.Errorf("earliest recovery should pick the 20m quota cooldown, not the 45m circuit:\n%s", out)
	}
	// A fully-down route has no healthy landing line.
	if strings.Contains(out, `✓ route "claude"`) {
		t.Errorf("down route must not get a ✓ landing line:\n%s", out)
	}
}

// TestRenderDoctorLiveRouteDownModelLock: model locks (the third "target down"
// cause) don't appear in health at all — the verdict must still name the lock,
// and must match locks by the target's MODEL, not just the provider.
func TestRenderDoctorLiveRouteDownModelLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	lockUntil := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	otherUntil := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	statusJSON := func(addr string) string {
		return fmt.Sprintf(`{
		  "uptime":"1m0s","version":"0.4.2","listen":%q,
		  "health":{},
		  "quota":{},
		  "schedule":{"models":{"claude":{"first":"","ordered":[]}}},
		  "model_locks":{
		    "zhipu":[{"model":"other-model","until":%q}],
		    "aqp":[{"model":"glm-5.2","until":%q}]
		  }
		}`, addr, otherUntil, lockUntil)
	}
	addr := doctorLiveTestServer(t, statusJSON, `{"enabled":false,"records":[]}`)
	cfg := doctorLiveTestCfg(t, addr, `providers:
  aqp: {provider_id: aqp, openai_base_url: https://x}
  zhipu: {provider_id: zhipu, openai_base_url: https://y}
routes:
  claude:
    - {provider: aqp, model: glm-5.2, priority: 1}
    - {provider: zhipu, model: glm-5.2, priority: 2}
`)

	out, err := clidoctor.RenderDoctorLive(cfg, filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("renderDoctorLive: %v", err)
	}
	if !strings.Contains(out, "(aqp, model lock)") {
		t.Errorf("want (aqp, model lock) as the recovery cause:\n%s", out)
	}
	// zhipu's lock is on a DIFFERENT model (not the route's target) and expires
	// earlier — picking it would prove model-matching is broken.
	lockLocal, _ := time.Parse(time.RFC3339, lockUntil)
	if !strings.Contains(out, lockLocal.Local().Format("15:04")) {
		t.Errorf("want the 30m aqp lock time, not the earlier 5m unrelated lock:\n%s", out)
	}
}

// TestRenderDoctorLiveHealthy: all routes have an available target → the
// conclusion is clean, each route shows its current landing + quota, and the
// request_log section lists recent failures as context.
func TestRenderDoctorLiveHealthy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	failTs := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	statusJSON := func(addr string) string {
		return fmt.Sprintf(`{
		  "uptime":"2h15m3s","version":"0.4.2","listen":%q,
		  "health":{"volc":{"circuit_state":"closed","available":true}},
		  "quota":{"volc":{"Account":"work","RemainingPct":0.62,"Windows":[{"Label":"Daily","RemainingPct":0.62,"Ultimate":true}]}},
		  "schedule":{"models":{"pi":{"first":"volc","ordered":[{"provider":"volc","priority":1,"tier":"plan","surplus":1.2,"available":true}]}}},
		  "model_locks":{}
		}`, addr)
	}
	requests := fmt.Sprintf(`{"enabled":true,"records":[
	  {"ts":%q,"request_id":"1","protocol":"anthropic","path":"/v1/messages","exposed":"pi","provider":"volc","status":502,"latency_ms":1234},
	  {"ts":%q,"request_id":"2","protocol":"anthropic","path":"/v1/messages","exposed":"pi","provider":"volc","status":429,"latency_ms":56}
	]}`, failTs, failTs)
	addr := doctorLiveTestServer(t, statusJSON, requests)
	cfg := doctorLiveTestCfg(t, addr, `providers:
  volc: {provider_id: volcengine, openai_base_url: https://x}
routes:
  pi:
    - {provider: volc, model: glm-5.2, priority: 1}
`)

	out, err := clidoctor.RenderDoctorLive(cfg, filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("renderDoctorLive: %v", err)
	}
	for _, want := range []string{
		"✓ no problems found",
		`✓ route "pi" → volc (62% remaining)`,
		"Recent failures",
		"pi → volc",
		"502",
		"429",
		// Failure follow-ups: 429 -> unfreeze the cooling provider, 5xx ->
		// re-probe the route's links with test.
		"[可立即执行] model-proxy unfreeze volc",
		"[可立即执行] model-proxy test pi",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRenderDoctorLivePinQuotaWarnings: an active pin, a nearly-exhausted
// first-choice quota, and daemon warnings each produce their own ⚠ line.
func TestRenderDoctorLivePinQuotaWarnings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	statusJSON := func(addr string) string {
		return fmt.Sprintf(`{
		  "uptime":"1m0s","version":"0.4.2","listen":%q,
		  "health":{"aqp":{"circuit_state":"closed","available":true}},
		  "quota":{"aqp":{"RemainingPct":0.03}},
		  "schedule":{"models":{"claude":{"first":"aqp","pin":"aqp","pin_expires":"expires in 30m","ordered":[{"provider":"aqp","priority":1,"tier":"plan","surplus":0,"available":true}]}}},
		  "model_locks":{},
		  "warnings":["model \"glm\" served by 2 logged-in providers (a, b); auto-routing to a — add an explicit route to choose"]
		}`, addr)
	}
	addr := doctorLiveTestServer(t, statusJSON, `{"enabled":false,"records":[]}`)
	cfg := doctorLiveTestCfg(t, addr, `providers:
  aqp: {provider_id: aqp, openai_base_url: https://x}
routes:
  claude:
    - {provider: aqp, model: glm-5.2, priority: 1}
`)

	out, err := clidoctor.RenderDoctorLive(cfg, filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("renderDoctorLive: %v", err)
	}
	for _, want := range []string{
		`⚠ route "claude" pinned to aqp`,
		"no failover while pinned",
		"[可立即执行] model-proxy unpin claude",
		`⚠ route "claude": aqp quota nearly exhausted (3% remaining)`,
		// aqp = single-account provider -> config-key hint, no login hint.
		"[需要改配置] config.yaml 的 routes.claude",
		"served by 2 logged-in providers",
		// Daemon warnings are config-driven -> tagged config hint.
		"[需要改配置] 按提示修改 config.yaml，然后 model-proxy serve reload 生效",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// Warnings present → the clean-bill line must NOT appear.
	if strings.Contains(out, "no problems found") {
		t.Errorf("warnings must suppress the clean-bill line:\n%s", out)
	}
}

// TestRenderDoctorLiveDaemonDown mirrors TestRenderStatusDaemonDown: a refused
// connection is itself the diagnosis (the daemon isn't running).
func TestRenderDoctorLiveDaemonDown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // bind then close → guaranteed connection refused

	cfg := doctorLiveTestCfg(t, addr, "providers:\n  aqp: {provider_id: aqp, openai_base_url: https://x}\n")
	_, err = clidoctor.RenderDoctorLive(cfg, filepath.Join(t.TempDir(), "config.yaml"))
	if err == nil {
		t.Fatal("want error for unreachable daemon")
	}
	if !strings.Contains(err.Error(), "cannot reach daemon") || !strings.Contains(err.Error(), "model-proxy serve") {
		t.Errorf("want the daemon-down hint, got %v", err)
	}
}

// TestRenderDoctorLiveWebDisabled: a 404 on /api/status means the daemon runs
// without web.enabled — same hint as serve status.
func TestRenderDoctorLiveWebDisabled(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()
	cfg := doctorLiveTestCfg(t, ts.Listener.Addr().String(), "providers:\n  aqp: {provider_id: aqp, openai_base_url: https://x}\n")
	_, err := clidoctor.RenderDoctorLive(cfg, filepath.Join(t.TempDir(), "config.yaml"))
	if err == nil || !strings.Contains(err.Error(), "web.enabled") {
		t.Fatalf("want web.enabled error, got %v", err)
	}
}

// TestDoctorLiveFlag: the --live scan follows the parseStatusFlags convention —
// exact match only, --config and its value are ignored (configPath owns them).
func TestDoctorLiveFlag(t *testing.T) {
	if clidoctor.DoctorLive([]string{}) {
		t.Error("no args → false")
	}
	if !clidoctor.DoctorLive([]string{"--live"}) {
		t.Error("--live → true")
	}
	if !clidoctor.DoctorLive([]string{"--config", "x.yaml", "--live"}) {
		t.Error("--config must not swallow --live")
	}
	if clidoctor.DoctorLive([]string{"--lives"}) {
		t.Error("--lives must not match --live")
	}
}

// TestCheckTakeoverDrift: the three takeover states — not taken over (no .bak),
// ok (pointer matches what takeover would write today), drift (mismatch or
// missing file). opencode's expected pointer carries the /v1 suffix.
func TestCheckTakeoverDrift(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", t.TempDir()) // isolate any pool-file reads
	proxyURL := "http://127.0.0.1:8314"
	cfg, err := configdomain.LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:8314
takeover:
  claude: `+filepath.Join(home, "claude.json")+`
  opencode: `+filepath.Join(home, "opencode.json")+`
  codex: `+filepath.Join(home, "config.toml")+`
  pi: `+filepath.Join(home, "models.json")+`
providers:
  aqp: {provider_id: aqp, openai_base_url: https://x}
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	bakDir := filepath.Join(home, ".model-proxy")

	// claude: taken over, pointer intact.
	os.WriteFile(cfg.Takeover.Claude, []byte(`{"env":{"ANTHROPIC_BASE_URL":"`+proxyURL+`","ANTHROPIC_AUTH_TOKEN":"PROXY_MANAGED"}}`), 0o600)
	// opencode: taken over, but the config now points at a stale port (drift).
	os.WriteFile(cfg.Takeover.Opencode, []byte(`{"provider":{"model-proxy":{"options":{"baseURL":"http://127.0.0.1:9999/v1"}}}}`), 0o600)
	// codex: never taken over (no .bak, no file).
	// pi: taken over, but the config file vanished (client reinstall).
	for _, name := range []string{"claude", "opencode", "pi"} {
		if err := os.MkdirAll(bakDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bakDir, name+".bak"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	drift := clidoctor.CheckTakeoverDrift(cfg, bakDir)
	byClient := map[string]clidoctor.ClientDrift{}
	for _, d := range drift {
		byClient[d.Client] = d
	}
	if len(drift) != 4 {
		t.Fatalf("want 4 clients, got %d", len(drift))
	}
	if c := byClient["claude"]; !c.Taken || !c.OK {
		t.Errorf("claude = %+v, want taken+ok", c)
	}
	if c := byClient["opencode"]; !c.Taken || c.OK ||
		c.Current != "http://127.0.0.1:9999/v1" || c.Expected != proxyURL+"/v1" {
		t.Errorf("opencode = %+v, want drift 9999/v1 vs %s/v1", c, proxyURL)
	}
	if c := byClient["codex"]; c.Taken {
		t.Errorf("codex = %+v, want not taken over", c)
	}
	if c := byClient["pi"]; !c.Taken || c.OK || c.Current != "(file missing)" {
		t.Errorf("pi = %+v, want drift (file missing)", c)
	}
}

// TestCodexPointer: the TOML pointer check — ok only when model_provider
// selects our section AND the section's base_url equals the proxy URL.
func TestCodexPointer(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.toml")
	proxyURL := "http://127.0.0.1:8314"
	good := `model_provider = "model-proxy"

[model_providers."model-proxy"]
name = "model-proxy"
base_url = "http://127.0.0.1:8314"
wire_api = "responses"
`
	if err := os.WriteFile(file, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	if cur, exp := clidoctor.CodexPointer(file, "model-proxy", proxyURL); cur != exp {
		t.Errorf("good config: current=%q expected=%q, want equal", cur, exp)
	}

	// model_provider switched away (e.g. user edited back to openai).
	bad := strings.Replace(good, `model_provider = "model-proxy"`, `model_provider = "openai"`, 1)
	os.WriteFile(file, []byte(bad), 0o600)
	if cur, _ := clidoctor.CodexPointer(file, "model-proxy", proxyURL); cur != `model_provider = "openai"` {
		t.Errorf("wrong model_provider: current=%q", cur)
	}

	// Section intact but base_url stale (listen port changed).
	stale := strings.Replace(good, `base_url = "http://127.0.0.1:8314"`, `base_url = "http://127.0.0.1:9999"`, 1)
	os.WriteFile(file, []byte(stale), 0o600)
	if cur, exp := clidoctor.CodexPointer(file, "model-proxy", proxyURL); cur != "http://127.0.0.1:9999" || exp != proxyURL {
		t.Errorf("stale base_url: current=%q expected=%q", cur, exp)
	}

	if cur, _ := clidoctor.CodexPointer(filepath.Join(dir, "nope.toml"), "model-proxy", proxyURL); cur != "(file missing)" {
		t.Errorf("missing file: current=%q", cur)
	}
}

// TestRenderDoctorLiveTakeoverDrift: end-to-end — a drifted client shows up
// both as a ⚠ verdict line (current vs expected + fix hint) and in the
// Takeover section's one-line summary.
func TestRenderDoctorLiveTakeoverDrift(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // takeover paths default under $HOME
	statusJSON := func(addr string) string {
		return fmt.Sprintf(`{
		  "uptime":"1m0s","version":"0.4.2","listen":%q,
		  "health":{"aqp":{"circuit_state":"closed","available":true}},
		  "quota":{"aqp":{"RemainingPct":0.9}},
		  "schedule":{"models":{"glm-5.2":{"first":"aqp","ordered":[{"provider":"aqp","priority":1,"tier":"plan","surplus":1,"available":true}]}}},
		  "model_locks":{}
		}`, addr)
	}
	addr := doctorLiveTestServer(t, statusJSON, `{"enabled":false,"records":[]}`)
	cfg := doctorLiveTestCfg(t, addr, `providers:
  aqp: {provider_id: aqp, openai_base_url: https://x}
routes:
  glm-5.2:
    - {provider: aqp, model: glm-5.2, priority: 1}
`)
	proxyURL := "http://" + addr
	// claude: taken over, pointer intact → ✓.
	if err := os.MkdirAll(filepath.Dir(cfg.Takeover.Claude), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(cfg.Takeover.Claude, []byte(`{"env":{"ANTHROPIC_BASE_URL":"`+proxyURL+`"}}`), 0o600)
	// opencode: taken over, pointer stale → drift.
	if err := os.MkdirAll(filepath.Dir(cfg.Takeover.Opencode), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(cfg.Takeover.Opencode, []byte(`{"provider":{"model-proxy":{"options":{"baseURL":"http://127.0.0.1:9999/v1"}}}}`), 0o600)
	bakDir := filepath.Join(home, ".model-proxy")
	for _, name := range []string{"claude", "opencode"} {
		if err := os.MkdirAll(bakDir, 0o700); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(bakDir, name+".bak"), []byte("{}"), 0o600)
	}

	out, err := clidoctor.RenderDoctorLive(cfg, filepath.Join(home, "config.yaml"))
	if err != nil {
		t.Fatalf("renderDoctorLive: %v", err)
	}
	for _, want := range []string{
		"takeover drift: opencode",
		"http://127.0.0.1:9999/v1",
		proxyURL + "/v1",
		"[可立即执行] model-proxy takeover opencode",
		"model-proxy restore opencode",
		"claude ✓",
		"opencode ✗ drift",
		"codex not taken over",
		"pi not taken over",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRenderDoctorLiveQuotaHintsPoolProvider: a nearly-exhausted first choice
// on a POOL-capable provider (zhipu) gets the [需要凭据] login hint, and with
// another available target also the [可立即执行] temporary pin hint.
func TestRenderDoctorLiveQuotaHintsPoolProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	statusJSON := func(addr string) string {
		return fmt.Sprintf(`{
		  "uptime":"1m0s","version":"0.4.2","listen":%q,
		  "health":{"zhipu":{"circuit_state":"closed","available":true},"volc":{"circuit_state":"closed","available":true}},
		  "quota":{"zhipu":{"RemainingPct":0.04},"volc":{"RemainingPct":0.9}},
		  "schedule":{"models":{"claude":{"first":"zhipu","ordered":[
		    {"provider":"zhipu","priority":1,"tier":"plan","surplus":0,"available":true},
		    {"provider":"volc","priority":2,"tier":"plan","surplus":0,"available":true}
		  ]}}},
		  "model_locks":{}
		}`, addr)
	}
	addr := doctorLiveTestServer(t, statusJSON, `{"enabled":false,"records":[]}`)
	cfg := doctorLiveTestCfg(t, addr, `providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
  volc: {provider_id: volcengine, openai_base_url: https://y}
routes:
  claude:
    - {provider: zhipu, model: glm-5.2, priority: 1}
    - {provider: volc, model: glm-5.2, priority: 2}
`)

	out, err := clidoctor.RenderDoctorLive(cfg, filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("renderDoctorLive: %v", err)
	}
	for _, want := range []string{
		`⚠ route "claude": zhipu quota nearly exhausted (4% remaining)`,
		"[可立即执行] model-proxy pin claude volc",
		"[需要凭据] model-proxy login zhipu",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// Pool-capable provider -> no config-key hint for the quota finding.
	if strings.Contains(out, "[需要改配置] config.yaml 的 routes.claude") {
		t.Errorf("pool provider should get the login hint, not a config change:\n%s", out)
	}
}
