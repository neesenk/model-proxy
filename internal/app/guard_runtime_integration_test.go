package app

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"model-proxy/internal/accounts"
	"model-proxy/internal/observe/counters"
	"model-proxy/internal/observe/seclog"
	"model-proxy/internal/providerbuild"
)

// Runtime-wired guard integration: the per-generation scanner carries the
// credential pool's known secrets (Build.Secrets → reload/startup swap →
// RuntimeSnapshot.Guard), the sensitive-path signal, and the security audit
// log. All fixtures are synthetic — never real credentials (AGENTS.md
// credential red line), and assertions must not echo fixture bytes into
// failure output beyond what the test itself constructed.

// guardPoolKey is a synthetic pool key that matches NO embedded rule (no
// issuer literal) so only the known-secret channel can catch it.
var guardPoolKey = "poolkey-" + strings.Repeat("wX9q", 8)

func guardPoolRequestBody(secret string) string {
	return `{"model":"glm","messages":[{"role":"user","content":"token: ` + secret + `"}]}`
}

// newGuardPoolProxy wires a one-route proxy in front of a raw-body-capturing
// upstream, with a real plural pool (one account per key) feeding the guard
// known-secret set. guardCfg is taken verbatim (tests choose the toggles).
func newGuardPoolProxy(t *testing.T, guardCfg GuardConfig, poolKeys ...string) (p *Proxy, proxyURL string, upstreamBodies func() []string) {
	t.Helper()
	setPoolHome(t, t.TempDir())
	writePoolFile(t, "static", testProviderID, poolKeys...)
	var mu sync.Mutex
	var bodies []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(up.Close)
	cfg := &Config{
		Providers: map[string]Provider{
			"static": {OpenAIBaseURL: up.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"glm": {{Provider: "static", Model: "glm"}},
		},
		Guard: guardCfg,
	}
	p = newProxyWithStatic(t, cfg, map[string]string{"static": "k"})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(px.Close)
	return p, px.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

// (a) A pool key appearing verbatim in the request body hits the known-secret
// exact channel and is INTERCEPTED regardless of the configured action: the
// request 400s, the event carries the TYPE NAME only (never key bytes), the
// counter is exact, and nothing reaches the upstream.
func TestGuardKnownSecret_PoolKeyPlaintextHit(t *testing.T) {
	p, proxyURL, bodies := newGuardPoolProxy(t,
		GuardConfig{Secrets: "log", KnownSecrets: true, Decode: true}, guardPoolKey)

	code, respBody := post(t, proxyURL+"/v1/chat/completions", guardPoolRequestBody(guardPoolKey))
	if code != http.StatusBadRequest || !strings.Contains(respBody, "known_secret") {
		t.Fatalf("exact-match interception: status=%d body=%s, want 400 naming known_secret", code, respBody)
	}
	if got := bodies(); len(got) != 0 {
		t.Fatalf("intercepted request reached the upstream %d times, want 0", len(got))
	}
	details := guardEventDetails(p)
	if len(details) != 1 {
		t.Fatalf("guard events = %v, want exactly 1", details)
	}
	if !strings.Contains(details[0], "known_secret") || !strings.Contains(details[0], "action=block") {
		t.Errorf("guard event detail = %q, want known_secret + action=block", details[0])
	}
	if strings.Contains(details[0], guardPoolKey) {
		t.Errorf("guard event leaked the pool key")
	}
	snap := p.metrics.Snapshot()
	if n := snap[counters.PMKey{Provider: "guard", Model: "known_secret"}].Requests; n != 1 {
		t.Errorf("known_secret counter = %d, want 1", n)
	}
}

// (b) The base64 form of a pool key hits the encoded known-secret channel and
// is intercepted the same way — redact cannot soften an exact hit either.
func TestGuardKnownSecret_Base64FormRedacted(t *testing.T) {
	p, proxyURL, bodies := newGuardPoolProxy(t,
		GuardConfig{Secrets: "redact", KnownSecrets: true, Decode: true}, guardPoolKey)

	b64 := base64.StdEncoding.EncodeToString([]byte(guardPoolKey))
	code, respBody := post(t, proxyURL+"/v1/chat/completions", guardPoolRequestBody(b64))
	if code != http.StatusBadRequest {
		t.Fatalf("encoded exact-match interception: status=%d body=%s, want 400", code, respBody)
	}
	if got := bodies(); len(got) != 0 {
		t.Fatalf("intercepted request reached the upstream %d times, want 0", len(got))
	}
	details := guardEventDetails(p)
	if len(details) != 1 || !strings.Contains(details[0], "known_secret_encoded") {
		t.Errorf("guard events = %v, want 1 event naming known_secret_encoded", details)
	}
	if strings.Contains(details[0], b64[:16]) {
		t.Errorf("guard event leaked encoded key bytes")
	}
}

// (c) Sensitive paths, context-aware: a path inside a tool-call position
// (STRONG) gets the configured action — log forwards + publishes a paths
// event, block rejects with 400 naming the category.
func TestGuardPaths_LogThenBlock(t *testing.T) {
	body := `{"model":"glm","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"~/.ssh/id_rsa\"}"}}]}]}`

	p, proxyURL, bodies := newGuardPoolProxy(t,
		GuardConfig{Secrets: "off", KnownSecrets: true, Decode: true, Paths: "log"})
	postOK(t, proxyURL+"/v1/chat/completions", body)
	if got := bodies(); len(got) != 1 {
		t.Fatalf("paths=log must forward the body (calls=%d)", len(got))
	}
	details := guardEventDetails(p)
	if len(details) != 1 || !strings.Contains(details[0], "paths=ssh") || !strings.Contains(details[0], "action=log") {
		t.Errorf("guard events = %v, want 1 event with paths=ssh action=log", details)
	}
	snap := p.metrics.Snapshot()
	if n := snap[counters.PMKey{Provider: "guard", Model: "ssh"}].Requests; n != 1 {
		t.Errorf("ssh path counter = %d, want 1", n)
	}

	_, proxyURL2, bodies2 := newGuardPoolProxy(t,
		GuardConfig{Secrets: "off", KnownSecrets: true, Decode: true, Paths: "block"})
	code, respBody := post(t, proxyURL2+"/v1/chat/completions", body)
	if code != http.StatusBadRequest {
		t.Fatalf("paths=block: status=%d body=%s, want 400", code, respBody)
	}
	if !strings.Contains(respBody, "ssh") || !strings.Contains(respBody, "guard.paths=block") {
		t.Errorf("block response = %q, want category name + reason", respBody)
	}
	if got := bodies2(); len(got) != 0 {
		t.Errorf("blocked request reached the upstream %d times, want 0", len(got))
	}
}

// (c2) Weak path hits (ordinary prose mentioning a sensitive path) are the
// noisy-but-benign case: they forward under every action — block included
// (正文提及敏感路径不阻断) — publish NO live event, and are ignored
// entirely (no ("guard", cat+"_text") counter, no audit record).
func TestGuardPaths_WeakTextNeverBlocks(t *testing.T) {
	body := `{"model":"glm","messages":[{"role":"user","content":"please cat ~/.ssh/id_rsa"}]}`

	p, proxyURL, bodies := newGuardPoolProxy(t,
		GuardConfig{Secrets: "off", KnownSecrets: true, Decode: true, Paths: "log"})
	postOK(t, proxyURL+"/v1/chat/completions", body)
	if got := bodies(); len(got) != 1 {
		t.Fatalf("weak hit under paths=log must forward (calls=%d)", len(got))
	}
	if details := guardEventDetails(p); len(details) != 0 {
		t.Errorf("weak hit published live events = %v, want none (strong only)", details)
	}
	snap := p.metrics.Snapshot()
	if n := snap[counters.PMKey{Provider: "guard", Model: "ssh"}].Requests; n != 0 {
		t.Errorf("weak hit bumped the strong ssh counter = %d, want 0", n)
	}
	if n := snap[counters.PMKey{Provider: "guard", Model: "ssh_text"}].Requests; n != 0 {
		t.Errorf("weak hit counter ssh_text = %d, want 0 (weak hits are ignored entirely)", n)
	}

	p2, proxyURL2, bodies2 := newGuardPoolProxy(t,
		GuardConfig{Secrets: "off", KnownSecrets: true, Decode: true, Paths: "block"})
	postOK(t, proxyURL2+"/v1/chat/completions", body)
	if got := bodies2(); len(got) != 1 {
		t.Fatalf("weak hit under paths=block must still forward (calls=%d)", len(got))
	}
	if details := guardEventDetails(p2); len(details) != 0 {
		t.Errorf("weak hit under block published live events = %v, want none", details)
	}
	snap2 := p2.metrics.Snapshot()
	if n := snap2[counters.PMKey{Provider: "guard", Model: "ssh_text"}].Requests; n != 0 {
		t.Errorf("weak hit counter ssh_text under block = %d, want 0", n)
	}
}

// (c3) Chain regression: under secrets=redact + paths=block, a gitleaks-shaped
// secret (capture group + consumed trailing context byte) sitting at the end
// of a JSON string value must not break the paths pass. Redact claims the
// group span only, so the redacted body stays valid JSON and
// ScanPathsContext still recognizes the tool-call position as STRONG —
// paths=block must reject. (Before the group-span fix, Redact ate the closing
// quote, the structure walk failed, and the strong hit silently downgraded to
// weak — which never blocks.)
func TestGuardRedactKeepsPathBlockChain(t *testing.T) {
	// 8-symbol period → Shannon entropy 3.0 ≥ the stripe rule's 2.0 gate.
	stripeKey := "sk_live_" + strings.Repeat("aB3xY9zQ", 3)
	body := `{"model":"glm","messages":[` +
		`{"role":"user","content":"stripe key ` + stripeKey + `"},` +
		`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"~/.ssh/id_rsa\"}"}}]}]}`

	p, proxyURL, bodies := newGuardPoolProxy(t,
		GuardConfig{Secrets: "redact", KnownSecrets: true, Decode: true, Paths: "block"})
	code, respBody := post(t, proxyURL+"/v1/chat/completions", body)
	if code != http.StatusBadRequest {
		t.Fatalf("paths=block after redact: status=%d body=%s, want 400 (strong tool-call path hit must survive redaction)", code, respBody)
	}
	if !strings.Contains(respBody, "ssh") || !strings.Contains(respBody, "guard.paths=block") {
		t.Errorf("block response = %q, want category name + guard.paths=block reason", respBody)
	}
	if strings.Contains(respBody, "guard.secrets") {
		t.Errorf("block response = %q, secrets=redact must not be the blocking reason", respBody)
	}
	if strings.Contains(respBody, stripeKey) {
		t.Errorf("block response leaked the secret")
	}
	if got := bodies(); len(got) != 0 {
		t.Errorf("blocked request reached the upstream %d times, want 0", len(got))
	}
	details := guardEventDetails(p)
	if len(details) != 2 {
		t.Fatalf("guard events = %v, want secrets(stripe) + paths(ssh) events", details)
	}

	// Same body under paths=log forwards, and the forwarded body is the
	// redacted-but-valid-JSON form: no secret bytes, placeholders inside the
	// string, structure intact.
	p2, proxyURL2, bodies2 := newGuardPoolProxy(t,
		GuardConfig{Secrets: "redact", KnownSecrets: true, Decode: true, Paths: "log"})
	postOK(t, proxyURL2+"/v1/chat/completions", body)
	got := bodies2()
	if len(got) != 1 {
		t.Fatalf("paths=log must forward the redacted body once (calls=%d)", len(got))
	}
	if strings.Contains(got[0], stripeKey) {
		t.Errorf("forwarded body still carries the stripe key")
	}
	if !json.Valid([]byte(got[0])) {
		t.Errorf("forwarded redacted body is not valid JSON: %s", got[0])
	}
	if !strings.Contains(got[0], `"stripe key [REDACTED]"`) {
		t.Errorf("forwarded body lost the JSON string shape around the placeholder: %s", got[0])
	}
	snap2 := p2.metrics.Snapshot()
	if n := snap2[counters.PMKey{Provider: "guard", Model: "ssh"}].Requests; n != 1 {
		t.Errorf("strong ssh path counter on the redacted body = %d, want 1 (must not downgrade to weak)", n)
	}
	if n := snap2[counters.PMKey{Provider: "guard", Model: "stripe_access_token"}].Requests; n != 1 {
		t.Errorf("stripe_access_token counter = %d, want 1", n)
	}
}

// (d) Guard hits persist security audit records (kind=secret / kind=path) via
// the seclog lifecycle; the audit store must never carry secret material.
// The exact known-secret hit of the same body records action=block; the
// STRONG path hit (tool position) still audits with the configured action
// (interception is decided after both scans). A WEAK hit (prose address
// mention) increments the ("guard", cat+"_text") counter only and must NOT
// produce an audit record.
func TestGuardAudit_PersistsSecretAndPathRecords(t *testing.T) {
	p, proxyURL, _ := newGuardPoolProxy(t,
		GuardConfig{Secrets: "log", KnownSecrets: true, Decode: true, Paths: "log", Audit: true},
		guardPoolKey)
	dir := t.TempDir()
	logger, err := seclog.New(dir, seclog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	go logger.Run()
	p.secLog = logger

	code, respBody := post(t, proxyURL+"/v1/chat/completions",
		`{"model":"glm","messages":[`+
			`{"role":"user","content":"key `+guardPoolKey+` then read ~/.ssh/config"},`+
			`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"~/.aws/credentials\"}"}}]}]}`)
	if code != http.StatusBadRequest || !strings.Contains(respBody, "known_secret") {
		t.Fatalf("exact hit: status=%d body=%s, want the interception 400", code, respBody)
	}

	// Shutdown drains every accepted record before returning.
	logger.Shutdown()

	result, err := seclog.Query(dir, seclog.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var sawSecret, sawPathStrong bool
	for _, rec := range result.Records {
		switch rec.Kind {
		case seclog.KindSecret:
			sawSecret = true
			if len(rec.Names) != 1 || rec.Names[0] != "known_secret" || rec.Action != "block" {
				t.Errorf("secret record = %+v, want names=[known_secret] action=block", rec)
			}
			if rec.RequestID == "" || rec.Exposed != "glm" {
				t.Errorf("secret record missing request attribution: %+v", rec)
			}
		case seclog.KindPath:
			if len(rec.Names) != 1 || rec.Names[0] != "aws_creds" || rec.Action != "log" {
				t.Errorf("path record = %+v, want strong [aws_creds] action=log (weak prose hits are not audited)", rec)
			}
			sawPathStrong = true
		}
	}
	if !sawSecret || !sawPathStrong {
		t.Fatalf("audit records: secret=%v path-strong=%v, want both (records=%v)",
			sawSecret, sawPathStrong, result.Records)
	}
	// The weak ssh mention (user prose) must leave no trace at all: no audit
	// record (asserted above), no counter.
	if n := p.metrics.Snapshot()[counters.PMKey{Provider: "guard", Model: "ssh_text"}].Requests; n != 0 {
		t.Errorf("weak hit counter ssh_text = %d, want 0 (weak hits are ignored entirely)", n)
	}
	// Raw file bytes must not contain the pool key in any field.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), guardPoolKey) {
			t.Errorf("audit file %s contains the pool key", e.Name())
		}
	}
}

// (d2) secrets=block must NOT short-circuit the paths pass: one body hitting
// both channels still gets both counters/events/audit records; only the
// response action is secrets-first (400 names the secret patterns only).
func TestGuardBlock_SecretsBlockStillScansPaths(t *testing.T) {
	p, proxyURL, bodies := newGuardPoolProxy(t,
		GuardConfig{Secrets: "block", KnownSecrets: true, Decode: true, Paths: "log", Audit: true},
		guardPoolKey)
	dir := t.TempDir()
	logger, err := seclog.New(dir, seclog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	go logger.Run()
	p.secLog = logger

	code, respBody := post(t, proxyURL+"/v1/chat/completions",
		`{"model":"glm","messages":[`+
			`{"role":"user","content":"key `+guardPoolKey+`"},`+
			`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"~/.ssh/config\"}"}}]}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("secrets=block: status=%d body=%s, want 400", code, respBody)
	}
	// The exact-match interception outranks the config secrets=block: the
	// response names the credential and the unblock path, not the config
	// action (paths action stays subordinate).
	if !strings.Contains(respBody, "credential configured on this proxy") || strings.Contains(respBody, "guard.paths=block") {
		t.Errorf("block response = %q, want the exact-match reason only", respBody)
	}
	if strings.Contains(respBody, guardPoolKey) {
		t.Errorf("block response leaked matched content")
	}
	if got := bodies(); len(got) != 0 {
		t.Errorf("blocked request reached the upstream %d times, want 0", len(got))
	}

	// Both channels counted + published, even though the request is
	// intercepted (interception is decided after both scans).
	snap := p.metrics.Snapshot()
	if n := snap[counters.PMKey{Provider: "guard", Model: "known_secret"}].Requests; n != 1 {
		t.Errorf("known_secret counter = %d, want 1", n)
	}
	if n := snap[counters.PMKey{Provider: "guard", Model: "ssh"}].Requests; n != 1 {
		t.Errorf("ssh path counter = %d, want 1 (paths must scan even when secrets blocks)", n)
	}
	details := guardEventDetails(p)
	if len(details) != 2 {
		t.Fatalf("guard events = %v, want secrets + paths events", details)
	}

	// Both audit records persist.
	logger.Shutdown()
	result, err := seclog.Query(dir, seclog.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var sawSecret, sawPath bool
	for _, rec := range result.Records {
		switch rec.Kind {
		case seclog.KindSecret:
			sawSecret = true
			if rec.Action != "block" {
				t.Errorf("secret record action = %q, want block", rec.Action)
			}
		case seclog.KindPath:
			sawPath = true
			if len(rec.Names) != 1 || rec.Names[0] != "ssh" {
				t.Errorf("path record = %+v, want names=[ssh]", rec)
			}
		}
	}
	if !sawSecret || !sawPath {
		t.Fatalf("audit records: secret=%v path=%v, want both (records=%v)", sawSecret, sawPath, result.Records)
	}
}

// (e) Toggles: known_secrets:false stops pool-key matching; decode:false
// stops encoded forms while plaintext still hits.
func TestGuardToggles_KnownSecretsAndDecode(t *testing.T) {
	p, proxyURL, bodies := newGuardPoolProxy(t,
		GuardConfig{Secrets: "log", KnownSecrets: false, Decode: true}, guardPoolKey)
	postOK(t, proxyURL+"/v1/chat/completions", guardPoolRequestBody(guardPoolKey))
	if got := bodies(); len(got) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(got))
	}
	if details := guardEventDetails(p); len(details) != 0 {
		t.Errorf("known_secrets=false: guard events = %v, want none", details)
	}
	snap := p.metrics.Snapshot()
	if n := snap[counters.PMKey{Provider: "guard", Model: "known_secret"}].Requests; n != 0 {
		t.Errorf("known_secrets=false: known_secret counter = %d, want 0", n)
	}

	p2, proxyURL2, _ := newGuardPoolProxy(t,
		GuardConfig{Secrets: "log", KnownSecrets: true, Decode: false}, guardPoolKey)
	b64 := base64.StdEncoding.EncodeToString([]byte(guardPoolKey))
	postOK(t, proxyURL2+"/v1/chat/completions", guardPoolRequestBody(b64))
	if details := guardEventDetails(p2); len(details) != 0 {
		t.Errorf("decode=false: base64 form must not hit, events = %v", details)
	}
	// Plaintext still hits — and is intercepted (exact-match channel).
	if code, respBody := post(t, proxyURL2+"/v1/chat/completions", guardPoolRequestBody(guardPoolKey)); code != http.StatusBadRequest {
		t.Fatalf("decode=false plaintext: status=%d body=%s, want the interception 400", code, respBody)
	}
	details := guardEventDetails(p2)
	if len(details) != 1 || !strings.Contains(details[0], "known_secret") {
		t.Errorf("decode=false: plaintext must still hit, events = %v", details)
	}
}

// (f) A pool key added by reload joins the protected set immediately (the
// scanner is rebuilt and swapped with the same config generation).
func TestGuardKnownSecret_ReloadProtectsNewPoolKey(t *testing.T) {
	setPoolHome(t, t.TempDir())
	keyV1 := guardPoolKey + "-v1x"
	keyV2 := guardPoolKey + "-v2x"
	writePoolFile(t, "zhipu", "zhipu", keyV1)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(up.Close)

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "listen: 127.0.0.1:0\n" +
		"providers:\n  zhipu:\n    openai_base_url: " + up.URL + "\n    provider_id: zhipu\n" +
		"routes:\n  glm:\n    - {provider: zhipu, model: glm}\n" +
		"guard:\n  secrets: log\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := mustLoadConfigFile(t, cfgPath)
	p := newTestProxy(t, cfg)
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(px.Close)

	// keyV2 is not in the pool yet: no hit.
	postOK(t, px.URL+"/v1/chat/completions", guardPoolRequestBody(keyV2))
	if details := guardEventDetails(p); len(details) != 0 {
		t.Fatalf("pre-reload: keyV2 must not be protected yet, events = %v", details)
	}

	// Add keyV2 to the pool and reload the same config file.
	writePoolFile(t, "zhipu", "zhipu", keyV1, keyV2)
	if err := p.Reload(cfgPath); err != nil {
		t.Fatal(err)
	}

	// keyV2 now hits the exact channel — the request is intercepted.
	if code, respBody := post(t, px.URL+"/v1/chat/completions", guardPoolRequestBody(keyV2)); code != http.StatusBadRequest {
		t.Fatalf("post-reload: status=%d body=%s, want the interception 400", code, respBody)
	}
	details := guardEventDetails(p)
	if len(details) != 1 || !strings.Contains(details[0], "known_secret") {
		t.Fatalf("post-reload: keyV2 must hit known_secret, events = %v", details)
	}
	if strings.Contains(details[0], keyV2) {
		t.Errorf("guard event leaked the pool key")
	}
	snap := p.metrics.Snapshot()
	if n := snap[counters.PMKey{Provider: "guard", Model: "known_secret"}].Requests; n != 1 {
		t.Errorf("post-reload known_secret counter = %d, want 1", n)
	}
}

// (g) codex/aqp OAuth files join the known-secret set best-effort: a valid
// file contributes its tokens; a missing or corrupt file is skipped silently
// (no panic, build still succeeds).
func TestGuardOAuthSecrets_BestEffortCollection(t *testing.T) {
	home := t.TempDir()
	setPoolHome(t, home)
	credDir := filepath.Join(home, ".model-proxy")

	writeFile := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(credDir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &Config{
		Providers: map[string]Provider{
			"codex": {Provider: "codex", OpenAIBaseURL: "http://x"},
			"aqp":   {Provider: "aqp", OpenAIBaseURL: "http://x", AqpMintURL: "http://x/mint"},
		},
	}
	collect := func() []providerbuild.Secret {
		t.Helper()
		return providerbuild.BuildProviders(cfg, accounts.NewStore(accounts.HomeDir()), testBuildOpts()).Secrets
	}

	// Missing files: nothing to protect, no error.
	if got := collect(); len(got) != 0 {
		t.Errorf("missing OAuth files: Secrets = %d values, want 0", len(got))
	}

	// Corrupt files: skipped silently, build still succeeds.
	writeFile("codex_oauth_auth.json", `{not json`)
	writeFile("aqp_oauth_auth.json", `{"sso_session_cookie": 42}`)
	if got := collect(); len(got) != 0 {
		t.Errorf("corrupt OAuth files: Secrets = %d values, want 0 (skip silently)", len(got))
	}

	// Valid files: every token/cookie value is collected.
	writeFile("codex_oauth_auth.json", `{"tokens":{"access_token":"`+guardPoolKey+`-at","refresh_token":"`+guardPoolKey+`-rt","id_token":"`+guardPoolKey+`-it","account_id":"acct"}}`)
	writeFile("aqp_oauth_auth.json", `{"account_id":"a","sso_session_cookie":"SSO_C=`+guardPoolKey+`-cookie"}`)
	got := collect()
	want := []string{guardPoolKey + "-at", guardPoolKey + "-rt", guardPoolKey + "-it", "SSO_C=" + guardPoolKey + "-cookie"}
	if len(got) != len(want) {
		t.Fatalf("valid OAuth files: Secrets = %d values, want %d", len(got), len(want))
	}
	set := map[string]bool{}
	for _, s := range got {
		set[s.Value] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("valid OAuth files: expected secret value missing from collected set")
		}
	}
}
