package app

import (
	"bytes"
	"fmt"
	"io"
	"model-proxy/internal/observe/counters"
	"model-proxy/internal/observe/seclog"
	"model-proxy/internal/providerbuild"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ---- guard_integration_test.go ----

// Outbound secret guard (DLP-lite) integration: the scan runs once on the
// shared request body inside forward, before the cache lookup and any forward
// branch. All fixtures are synthetic strings shaped to match the patterns —
// never real credentials (AGENTS.md credential red line), and failure messages
// must not echo the fixture bytes either.

// guardFixtureKey is a synthetic AWS-shaped access key id (the AWS docs
// example suffix — high-entropy, as the rule requires; all-zeros no longer
// matches since the embedded rule gained an entropy floor).
const guardFixtureKey = "AKIA" + "IOSFODNN7EXAMPLE"

func guardRequestBody() string {
	return `{"model":"glm","messages":[{"role":"user","content":"here is my key ` + guardFixtureKey + `"}]}`
}

// newGuardTestProxy wires a one-route proxy (guard.secrets = secretsAction)
// in front of a raw-body-capturing upstream.
func newGuardTestProxy(t *testing.T, secretsAction string) (p *Proxy, proxyURL string, upstreamBodies func() []string) {
	t.Helper()
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
		Guard: GuardConfig{Secrets: secretsAction},
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

// guardEventDetails returns the Detail of every published "guard" event.
func guardEventDetails(p *Proxy) []string {
	var details []string
	for _, e := range p.events.Snapshot() {
		if e.Type == "guard" {
			details = append(details, e.Detail)
		}
	}
	return details
}

// Default (guard section absent) = log: forward the body unchanged, publish a
// guard event carrying only pattern type names, and count the hit.
func TestGuard_LogDefaultAllowsAndPublishesEvent(t *testing.T) {
	p, proxyURL, bodies := newGuardTestProxy(t, "")

	postOK(t, proxyURL+"/v1/chat/completions", guardRequestBody())

	got := bodies()
	if len(got) != 1 || !strings.Contains(got[0], guardFixtureKey) {
		t.Fatalf("log action must forward the unmodified body once (calls=%d)", len(got))
	}
	details := guardEventDetails(p)
	if len(details) != 1 {
		t.Fatalf("guard events = %v, want exactly 1", details)
	}
	if !strings.Contains(details[0], "aws_access_key_id") || !strings.Contains(details[0], "action=log") {
		t.Errorf("guard event detail = %q, want pattern name + action=log", details[0])
	}
	if strings.Contains(details[0], guardFixtureKey) {
		t.Errorf("guard event leaked matched content")
	}
	snap := p.metrics.Snapshot()
	if n := snap[counters.PMKey{Provider: "guard", Model: "aws_access_key_id"}].Requests; n != 1 {
		t.Errorf("guard hit counter = %d, want 1", n)
	}
}

func TestGuard_RedactRewritesBodyBeforeForwarding(t *testing.T) {
	p, proxyURL, bodies := newGuardTestProxy(t, "redact")

	postOK(t, proxyURL+"/v1/chat/completions", guardRequestBody())

	got := bodies()
	if len(got) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(got))
	}
	if strings.Contains(got[0], guardFixtureKey) {
		t.Errorf("redacted body still contains the secret")
	}
	if !strings.Contains(got[0], "[REDACTED]") {
		t.Errorf("redacted body lacks the [REDACTED] placeholder")
	}
	// The rest of the JSON must survive (model field intact).
	if !strings.Contains(got[0], `"model":"glm"`) {
		t.Errorf("redacted body lost surrounding JSON content")
	}
	details := guardEventDetails(p)
	if len(details) != 1 || !strings.Contains(details[0], "action=redact") {
		t.Errorf("guard events = %v, want 1 event with action=redact", details)
	}
}

func TestGuard_BlockRejectsWith400(t *testing.T) {
	p, proxyURL, bodies := newGuardTestProxy(t, "block")

	code, respBody := post(t, proxyURL+"/v1/chat/completions", guardRequestBody())
	if code != http.StatusBadRequest {
		t.Fatalf("block: status=%d body=%s, want 400", code, respBody)
	}
	if !strings.Contains(respBody, "aws_access_key_id") || !strings.Contains(respBody, "guard.secrets=block") {
		t.Errorf("block response = %q, want pattern name + reason", respBody)
	}
	if strings.Contains(respBody, guardFixtureKey) {
		t.Errorf("block response leaked matched content")
	}
	if got := bodies(); len(got) != 0 {
		t.Errorf("blocked request reached the upstream %d times, want 0", len(got))
	}
	// A blocked request is an early terminal: the live monitor needs its end
	// event pair (same contract as the other 400 terminals).
	var sawEnd400 bool
	for _, e := range p.events.Snapshot() {
		if e.Type == "end" && e.Status == http.StatusBadRequest {
			sawEnd400 = true
		}
	}
	if !sawEnd400 {
		t.Errorf("blocked request produced no 400 end event")
	}
}

func TestGuard_OffDisablesScanning(t *testing.T) {
	p, proxyURL, bodies := newGuardTestProxy(t, "off")

	postOK(t, proxyURL+"/v1/chat/completions", guardRequestBody())

	got := bodies()
	if len(got) != 1 || !strings.Contains(got[0], guardFixtureKey) {
		t.Errorf("off: upstream should receive the unmodified body once (calls=%d)", len(got))
	}
	if details := guardEventDetails(p); len(details) != 0 {
		t.Errorf("off: guard events = %v, want none", details)
	}
	snap := p.metrics.Snapshot()
	if n := snap[counters.PMKey{Provider: "guard", Model: "aws_access_key_id"}].Requests; n != 0 {
		t.Errorf("off: guard hit counter = %d, want 0", n)
	}
}

// Clean bodies take no guard action in any mode (no events, no rewrites).
func TestGuard_CleanBodyUnscathed(t *testing.T) {
	p, proxyURL, bodies := newGuardTestProxy(t, "block")

	postOK(t, proxyURL+"/v1/chat/completions", `{"model":"glm","messages":[{"role":"user","content":"explain the observer pattern"}]}`)

	got := bodies()
	if len(got) != 1 || !strings.Contains(got[0], "observer pattern") {
		t.Errorf("clean body should reach the upstream unmodified (calls=%d)", len(got))
	}
	if details := guardEventDetails(p); len(details) != 0 {
		t.Errorf("clean body: guard events = %v, want none", details)
	}
}

// ---- guard_refresh_test.go ----

// Guard OAuth known-secret re-sync (guard_wiring.go): codex/aqp rotate their
// tokens in place during serve, so the scanner's known-secret set must follow
// the on-disk auth files on the poll beat — without a reload and without
// losing the pool-secret base. Fixtures are synthetic token-shaped strings
// (no embedded-rule literal, >= minSecretLen) — never real credentials.

var (
	oauthTokV1 = "oakt-" + strings.Repeat("mV3x", 8) + "-v1"
	oauthTokV2 = "oakt-" + strings.Repeat("qZ7w", 8) + "-v2"
)

// writeCodexAuthFile rewrites the codex OAuth auth file under home the way the
// codex provider's in-process token refresh does.
func writeCodexAuthFile(t *testing.T, home, accessToken string) {
	t.Helper()
	if err := writeCodexAuthFileErr(home, accessToken); err != nil {
		t.Fatal(err)
	}
}

// writeCodexAuthFileErr is the goroutine-safe variant: it returns the error
// instead of calling t.Fatal, which is only legal on the test goroutine.
func writeCodexAuthFileErr(home, accessToken string) error {
	content := `{"tokens":{"access_token":"` + accessToken + `","refresh_token":"","id_token":"","account_id":"acct"}}`
	return os.WriteFile(filepath.Join(home, ".model-proxy", "codex_oauth_auth.json"), []byte(content), 0o600)
}

// guardScanNames runs the current generation's scanner over a body containing
// token and reports whether the known-secret channel fires.
func guardKnownSecretHits(p *Proxy, token string) bool {
	sc := p.SnapshotRuntime().Guard
	if sc == nil {
		return false
	}
	for _, name := range sc.Scan([]byte(`{"model":"gpt","messages":[{"role":"user","content":"token: ` + token + `"}]}`)) {
		if name == "known_secret" {
			return true
		}
	}
	return false
}

// newOAuthGuardProxy builds a proxy with a pool-backed static provider (pool
// secret base) and a codex provider (rotating OAuth secret), both routed.
func newOAuthGuardProxy(t *testing.T, home string, scheduling Scheduling) *Proxy {
	t.Helper()
	writePoolFile(t, "static", testProviderID, guardPoolKey)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(up.Close)
	cfg := &Config{
		Providers: map[string]Provider{
			"static": {OpenAIBaseURL: up.URL, Provider: testProviderID},
			"codex":  {OpenAIBaseURL: up.URL, Provider: "codex", ClientVersion: "1.0.0"},
		},
		Routes: map[string][]RouteTarget{
			"glm": {{Provider: "static", Model: "glm"}},
			"gpt": {{Provider: "codex", Model: "gpt"}},
		},
		Guard:      GuardConfig{Secrets: "log", KnownSecrets: true, Decode: true},
		Scheduling: scheduling,
	}
	return newTestProxy(t, cfg)
}

// One refresh pass after an in-place token rotation: the new token joins the
// known-secret set, the retired token drops out, and the pool-secret base
// survives the rebuild.
func TestGuardOAuthRefresh_RotatedTokenRescans(t *testing.T) {
	home := t.TempDir()
	setPoolHome(t, home)
	writeCodexAuthFile(t, home, oauthTokV1)
	p := newOAuthGuardProxy(t, home, Scheduling{})

	if !guardKnownSecretHits(p, oauthTokV1) {
		t.Fatal("boot scanner must match the v1 OAuth token")
	}
	if guardKnownSecretHits(p, oauthTokV2) {
		t.Fatal("v2 token must not be protected before it exists")
	}

	// The provider refreshes in place: same file, new token.
	writeCodexAuthFile(t, home, oauthTokV2)
	p.refreshGuardKnownSecrets()

	if !guardKnownSecretHits(p, oauthTokV2) {
		t.Error("post-refresh: rotated v2 token must hit known_secret")
	}
	if guardKnownSecretHits(p, oauthTokV1) {
		t.Error("post-refresh: retired v1 token must no longer hit known_secret")
	}
	if !guardKnownSecretHits(p, guardPoolKey) {
		t.Error("post-refresh: pool key must survive the scanner rebuild (current-generation pool base)")
	}

	// No file change → no rebuild (scanner pointer stable).
	before := p.SnapshotRuntime().Guard
	p.refreshGuardKnownSecrets()
	if after := p.SnapshotRuntime().Guard; after != before {
		t.Error("unchanged OAuth files must not rebuild the scanner")
	}
}

// The lifecycle loop picks up a rotation on the quota-poll beat
// (scheduling.quota_poll_interval), with no reload and no direct method call.
func TestGuardOAuthRefresh_LoopFollowsPollBeat(t *testing.T) {
	home := t.TempDir()
	setPoolHome(t, home)
	writeCodexAuthFile(t, home, oauthTokV1)
	p := newOAuthGuardProxy(t, home, Scheduling{QuotaPollInterval: "20ms"})

	writeCodexAuthFile(t, home, oauthTokV2)
	waitUntil(t, "guard scanner picks up the rotated OAuth token on the poll beat", func() bool {
		return guardKnownSecretHits(p, oauthTokV2)
	})
	if guardKnownSecretHits(p, oauthTokV1) {
		t.Error("after the beat swap: retired v1 token must no longer hit known_secret")
	}
}

// Refresh passes racing reloads must stay generation-consistent: a stale
// rebuild (files read before a reload landed) is dropped, never mixed into
// the new generation. Run under -race; the final convergence assertion pins
// the semantic outcome.
func TestGuardOAuthRefresh_ConcurrentReloadRace(t *testing.T) {
	home := t.TempDir()
	setPoolHome(t, home)
	writeCodexAuthFile(t, home, oauthTokV1)
	// Every reload schedules a catalog refresh; fail it fast instead of waiting
	// out the real models.dev client timeout at Close.
	t.Setenv("MP_MODELSDEV_URL", "http://127.0.0.1:1")

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(up.Close)
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "listen: 127.0.0.1:0\n" +
		"providers:\n  codex:\n    openai_base_url: " + up.URL + "\n    provider_id: codex\n    client_version: \"1.0.0\"\n" +
		"routes:\n  gpt:\n    - {provider: codex, model: gpt}\n" +
		"guard:\n  secrets: log\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	p := newTestProxy(t, mustLoadConfigFile(t, cfgPath))

	// Rotate the token mid-race so refreshes built on either side of a reload
	// observe different file contents.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := p.Reload(cfgPath); err != nil {
				t.Errorf("reload: %v", err)
				return
			}
			if i%5 == 4 {
				// t.Fatal is illegal off the test goroutine — report and bail.
				if err := writeCodexAuthFileErr(home, fmt.Sprintf("%s-r%d", oauthTokV2, i)); err != nil {
					t.Errorf("rotate auth file: %v", err)
					return
				}
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			p.refreshGuardKnownSecrets()
		}
	}()
	// Let the race run for a bounded number of reload generations, then stop.
	waitUntil(t, "several reload generations elapsed during the race", func() bool {
		return p.configGeneration.Load() >= 8
	})
	close(stop)
	wg.Wait()

	// Converge and assert the final scanner reflects the CURRENT generation's
	// config + the CURRENT auth file: the latest rotated token hits, the
	// long-retired v1 does not.
	p.refreshGuardKnownSecrets()
	current := providerbuild.CollectOAuthSecrets(p.cfgSnapshot(), testBuildOpts())
	if len(current) != 1 {
		t.Fatalf("current OAuth secret set = %d values, want 1", len(current))
	}
	if !guardKnownSecretHits(p, current[0]) {
		t.Error("post-race: scanner must match the current auth file's token")
	}
	if guardKnownSecretHits(p, oauthTokV1) {
		t.Error("post-race: retired v1 token must not survive in any generation's scanner")
	}
}

// --- provider in-memory secrets (provider.SecretReporter) ---

var (
	aqpMintedKeyV1 = "aqmk-" + strings.Repeat("zX8v", 8) + "-v1"
	aqpMintedKeyV2 = "aqmk-" + strings.Repeat("kQ2n", 8) + "-v2"
)

// newAqpMintRig builds the mock compass mint endpoint + SSO cookie store a
// real aqp provider needs. currentKey controls which key the next mint
// returns; flipped under a mutex so request-driving goroutines can race it.
func newAqpMintRig(t *testing.T, home string) (mintURL *string, setKey func(string)) {
	t.Helper()
	var mu sync.Mutex
	key := aqpMintedKeyV1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		k := key
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"retcode":0,"data":{"api_key":%q,"project_id":"p1"}}`, k)
	}))
	t.Cleanup(srv.Close)
	cookie := `{"sso_session_cookie":"SSO_C=test"}`
	if err := os.WriteFile(filepath.Join(home, ".model-proxy", "aqp_oauth_auth.json"), []byte(cookie), 0o600); err != nil {
		t.Fatal(err)
	}
	return &srv.URL, func(k string) { mu.Lock(); key = k; mu.Unlock() }
}

// newAqpGuardProxy builds a proxy routing one model to a REAL aqp provider
// (real AqpKeyProvider minting against the rig) behind a capture upstream.
func newAqpGuardProxy(t *testing.T, home, mintURL string) *Proxy {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(up.Close)
	cfg := &Config{
		Providers: map[string]Provider{
			"aqp": {OpenAIBaseURL: up.URL, Provider: "aqp", AqpMintURL: mintURL},
		},
		Routes: map[string][]RouteTarget{
			"aqp-m": {{Provider: "aqp", Model: "aqp-m"}},
		},
		Guard: GuardConfig{Secrets: "log", KnownSecrets: true, Decode: true},
	}
	return newTestProxy(t, cfg)
}

// The aqp managed key exists only in provider memory (minted at request time,
// stored in no file): one refresh pass after the first request must bring it
// into the known-secret set, and a re-mint must rotate it (old value out).
func TestGuardMemorySecrets_AqpMintedKeyRescans(t *testing.T) {
	home := t.TempDir()
	setPoolHome(t, home)
	mintURL, setKey := newAqpMintRig(t, home)
	p := newAqpGuardProxy(t, home, *mintURL)
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(px.Close)

	// Pre-mint: a re-sync finds nothing (the file carries only the SSO cookie,
	// which IS collected — but the managed key is not minted yet).
	p.refreshGuardKnownSecrets()
	if guardKnownSecretHits(p, aqpMintedKeyV1) {
		t.Fatal("pre-mint: the managed key must not be protected before it exists")
	}

	// First request mints the key (provider AuthHeaders → AqpKeyProvider).
	postOK(t, px.URL+"/v1/chat/completions",
		`{"model":"aqp-m","messages":[{"role":"user","content":"hi"}]}`)
	p.refreshGuardKnownSecrets()
	if !guardKnownSecretHits(p, aqpMintedKeyV1) {
		t.Error("post-mint refresh: the in-memory minted key must hit known_secret")
	}

	// Rotate: the mint endpoint now issues v2; force a provider re-mint, then
	// one refresh pass swaps the protected value.
	setKey(aqpMintedKeyV2)
	prov, ok := p.providers["aqp"]
	if !ok {
		t.Fatal("aqp provider missing from providers map")
	}
	if err := prov.Refresh(); err != nil {
		t.Fatalf("provider re-mint: %v", err)
	}
	p.refreshGuardKnownSecrets()
	if !guardKnownSecretHits(p, aqpMintedKeyV2) {
		t.Error("post-rotation refresh: the rotated minted key must hit known_secret")
	}
	if guardKnownSecretHits(p, aqpMintedKeyV1) {
		t.Error("post-rotation refresh: the retired minted key must no longer hit known_secret")
	}
}

// Refresh passes racing live request traffic (which mints/reads the cached key
// under the provider's own lock) must stay race-clean and converge on the
// current minted value. Run under -race; the final assertion pins convergence.
func TestGuardMemorySecrets_ConcurrentMintAndRefresh(t *testing.T) {
	home := t.TempDir()
	setPoolHome(t, home)
	mintURL, setKey := newAqpMintRig(t, home)
	p := newAqpGuardProxy(t, home, *mintURL)
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(px.Close)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // live traffic keeps the minted-key cache hot
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// t.Fatal is illegal off the test goroutine: use the raw client
			// and t.Errorf so a transport failure is a clean test failure.
			resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json",
				stringReader(`{"model":"aqp-m","messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Errorf("traffic during race: %v", err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("traffic during race: status = %d, want 200", resp.StatusCode)
				return
			}
		}
	}()
	go func() { // re-syncs + a mid-race key rotation
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			p.refreshGuardKnownSecrets()
			if i == 10 {
				setKey(aqpMintedKeyV2)
				if prov := p.providers["aqp"]; prov != nil {
					_ = prov.Refresh()
				}
			}
		}
	}()
	waitUntil(t, "the rotated minted key joins the known-secret set mid-race", func() bool {
		return guardKnownSecretHits(p, aqpMintedKeyV2)
	})
	close(stop)
	wg.Wait()

	// Converged: the current minted key is protected, the retired one is not.
	p.refreshGuardKnownSecrets()
	if !guardKnownSecretHits(p, aqpMintedKeyV2) {
		t.Error("post-race: current minted key must hit known_secret")
	}
	if guardKnownSecretHits(p, aqpMintedKeyV1) {
		t.Error("post-race: retired minted key must not survive in the scanner")
	}
}

// ---- session_scan_test.go ----

// Split-exfiltration (session scan) integration: a pool key fragmented across
// multiple requests of one x-claude-code-session-id session must fire
// known_secret_fragmented; single-request / cross-session / headerless cases
// must not. All fixtures are synthetic — never real credentials (AGENTS.md
// credential red line), and failure output must not echo fixture bytes.
// The store itself is owned and unit-tested by internal/guard/session.

// fragPoolKey is a synthetic 40-char pool key, splittable into fragments that
// each clear guard's minKnownFrag (8).
var fragPoolKey = "poolkey-" + strings.Repeat("zK7v", 8)

func fragBody(fragment string) string {
	return `{"model":"glm","messages":[{"role":"user","content":"note ` + fragment + `"}]}`
}

// postSession posts body with an optional x-claude-code-session-id header.
func postSession(t *testing.T, url, body, session string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("POST", url, stringReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	if session != "" {
		req.Header.Set("x-claude-code-session-id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b)
}

// fragmentedEvents returns the guard event Details naming
// known_secret_fragmented.
func fragmentedEvents(p *Proxy) []string {
	var out []string
	for _, d := range guardEventDetails(p) {
		if strings.Contains(d, "known_secret_fragmented") {
			out = append(out, d)
		}
	}
	return out
}

func fragmentedCount(p *Proxy) uint64 {
	return p.metrics.Snapshot()[counters.PMKey{Provider: "guard", Model: "known_secret_fragmented"}].Requests
}

func sessionGuardCfg(action string) GuardConfig {
	return GuardConfig{Secrets: action, KnownSecrets: true, Decode: true, SessionScan: true}
}

// (a) Two-request split: the second request completes the key → one
// fragmented event + counter, both bodies forwarded (log action), no secret
// material in the event.
func TestSessionScan_TwoFragmentSplitFires(t *testing.T) {
	p, proxyURL, bodies := newGuardPoolProxy(t, sessionGuardCfg("log"), fragPoolKey)

	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[:20]), "s1")
	if got := fragmentedEvents(p); len(got) != 0 {
		t.Fatalf("first fragment must not fire, events = %v", got)
	}
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[20:]), "s1")

	details := fragmentedEvents(p)
	if len(details) != 1 {
		t.Fatalf("fragmented events = %v, want exactly 1", details)
	}
	if !strings.Contains(details[0], "action=log") {
		t.Errorf("fragmented event detail = %q, want action=log", details[0])
	}
	if strings.Contains(details[0], fragPoolKey[:16]) {
		t.Errorf("fragmented event leaked key material")
	}
	if n := fragmentedCount(p); n != 1 {
		t.Errorf("fragmented counter = %d, want 1", n)
	}
	if got := bodies(); len(got) != 2 {
		t.Errorf("log action must forward both requests (calls=%d)", len(got))
	}
}

// (b) Three-request split fires only at the third request.
func TestSessionScan_ThreeFragmentSplitFiresAtThird(t *testing.T) {
	p, proxyURL, _ := newGuardPoolProxy(t, sessionGuardCfg("log"), fragPoolKey)

	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[:14]), "s1")
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[14:28]), "s1")
	if got := fragmentedEvents(p); len(got) != 0 {
		t.Fatalf("incomplete splits must not fire, events = %v", got)
	}
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[28:]), "s1")
	if got := fragmentedEvents(p); len(got) != 1 {
		t.Fatalf("fragmented events = %v, want exactly 1 after the third request", got)
	}
}

// (c) The full key in one request reports known_secret only — and a later
// clean request in the same session must NOT re-fire as fragmented just
// because the window still holds the key.
func TestSessionScan_FullKeySingleRequestNotFragmented(t *testing.T) {
	p, proxyURL, _ := newGuardPoolProxy(t, sessionGuardCfg("log"), fragPoolKey)

	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey), "s1")
	if got := fragmentedEvents(p); len(got) != 0 {
		t.Fatalf("complete key in one body must not fire fragmented, events = %v", got)
	}
	snap := p.metrics.Snapshot()
	if n := snap[counters.PMKey{Provider: "guard", Model: "known_secret"}].Requests; n != 1 {
		t.Errorf("known_secret counter = %d, want 1", n)
	}
	postSession(t, proxyURL+"/v1/chat/completions", fragBody("clean follow-up"), "s1")
	if got := fragmentedEvents(p); len(got) != 0 {
		t.Fatalf("window replay of an already-reported key must not fire fragmented, events = %v", got)
	}
}

// (d) Fragments in DIFFERENT sessions never reassemble; requests without the
// session header are not aggregated at all.
func TestSessionScan_SessionIsolation(t *testing.T) {
	p, proxyURL, _ := newGuardPoolProxy(t, sessionGuardCfg("log"), fragPoolKey)

	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[:20]), "session-a")
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[20:]), "session-b")
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[:20]), "")
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[20:]), "")
	if got := fragmentedEvents(p); len(got) != 0 {
		t.Fatalf("cross-session/headerless fragments must not fire, events = %v", got)
	}
	// Sanity: the same split in ONE session does fire (fixture is valid).
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[:20]), "session-c")
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[20:]), "session-c")
	if got := fragmentedEvents(p); len(got) != 1 {
		t.Fatalf("same-session split must fire, events = %v", got)
	}
}

// (e) Once >32KiB of later body traffic has pushed the first fragment out of
// the window, the split is NOT detected (documented bounded-window limit).
func TestSessionScan_WindowTruncationLosesFragments(t *testing.T) {
	p, proxyURL, _ := newGuardPoolProxy(t, sessionGuardCfg("log"), fragPoolKey)

	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[:20]), "s1")
	junk := `{"model":"glm","messages":[{"role":"user","content":"` + strings.Repeat("y", 40<<10) + `"}]}`
	postSession(t, proxyURL+"/v1/chat/completions", junk, "s1")
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[20:]), "s1")
	if got := fragmentedEvents(p); len(got) != 0 {
		t.Fatalf("fragments separated by >32KiB must not reassemble, events = %v", got)
	}
}

// (f) session_scan: false disables the whole pass (no events, no windows).
func TestSessionScan_ConfigFalseDisables(t *testing.T) {
	p, proxyURL, _ := newGuardPoolProxy(t,
		GuardConfig{Secrets: "log", KnownSecrets: true, Decode: true, SessionScan: false}, fragPoolKey)

	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[:20]), "s1")
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[20:]), "s1")
	if got := fragmentedEvents(p); len(got) != 0 {
		t.Fatalf("session_scan=false must not fire, events = %v", got)
	}
	if n := p.sessionScan.Len(); n != 0 {
		t.Errorf("session_scan=false must not retain windows, sessions = %d", n)
	}
}

// (g) redact cannot rewrite a secret spanning requests: a fragmented hit
// degrades to log semantics (event says action=log; the fragment reaches the
// upstream unredacted).
func TestSessionScan_RedactDegradesToLog(t *testing.T) {
	p, proxyURL, bodies := newGuardPoolProxy(t, sessionGuardCfg("redact"), fragPoolKey)

	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[:20]), "s1")
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[20:]), "s1")

	details := fragmentedEvents(p)
	if len(details) != 1 || !strings.Contains(details[0], "action=log") {
		t.Fatalf("redact must degrade to log for fragmented hits, events = %v", details)
	}
	got := bodies()
	if len(got) != 2 || !strings.Contains(got[1], fragPoolKey[20:]) {
		t.Errorf("fragmented request under redact must forward unchanged (cannot rewrite across requests)")
	}
}

// (h) block rejects the completing request with 400 naming the signal, never
// the key; the first fragment passed through (unavoidable — it was clean).
func TestSessionScan_BlockRejectsCompletingRequest(t *testing.T) {
	_, proxyURL, bodies := newGuardPoolProxy(t, sessionGuardCfg("block"), fragPoolKey)

	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[:20]), "s1")
	code, respBody := postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[20:]), "s1")
	if code != http.StatusBadRequest {
		t.Fatalf("block: status=%d body=%s, want 400", code, respBody)
	}
	if !strings.Contains(respBody, "known_secret_fragmented") {
		t.Errorf("block response = %q, want the signal name", respBody)
	}
	if strings.Contains(respBody, fragPoolKey[:16]) || strings.Contains(respBody, fragPoolKey[20:]) {
		t.Errorf("block response leaked key material")
	}
	if got := bodies(); len(got) != 1 {
		t.Errorf("blocked request reached the upstream %d times, want 0 (only the first fragment)", len(got))
	}
}

// (i) Fragmented hits persist a seclog record (kind=secret,
// names=[known_secret_fragmented]) with the effective action.
func TestSessionScan_AuditRecord(t *testing.T) {
	p, proxyURL, _ := newGuardPoolProxy(t, sessionGuardCfg("log"), fragPoolKey)
	dir := t.TempDir()
	logger, err := seclog.New(dir, seclog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	go logger.Run()
	p.secLog = logger

	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[:20]), "s1")
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[20:]), "s1")
	logger.Shutdown()

	result, err := seclog.Query(dir, seclog.Filter{Kind: seclog.KindSecret})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("secret records = %d, want 1", len(result.Records))
	}
	rec := result.Records[0]
	if len(rec.Names) != 1 || rec.Names[0] != "known_secret_fragmented" || rec.Action != "log" {
		t.Errorf("record = %+v, want names=[known_secret_fragmented] action=log", rec)
	}
	if rec.RequestID == "" || rec.Exposed != "glm" {
		t.Errorf("record missing request attribution: %+v", rec)
	}
}

// (j) Concurrent requests on one session: the completing fragment must fire
// at least once and the store must stay race-clean (run with -race).
func TestSessionScan_ConcurrentSameSession(t *testing.T) {
	p, proxyURL, _ := newGuardPoolProxy(t, sessionGuardCfg("log"), fragPoolKey)

	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[:20]), "s1")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fragBody(fmt.Sprintf("padding-%d", i))
			if i == 0 {
				body = fragBody(fragPoolKey[20:])
			}
			req, err := http.NewRequest("POST", proxyURL+"/v1/chat/completions", stringReader(body))
			if err != nil {
				t.Errorf("new request: %v", err)
				return
			}
			req.Header.Set("x-claude-code-session-id", "s1")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("post: %v", err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}(i)
	}
	wg.Wait()
	if n := fragmentedCount(p); n != 1 {
		t.Errorf("fragmented counter = %d, want exactly 1 (only the completing request fires; more = double-reporting)", n)
	}
}

// (k) After a split fires, a later request carrying only the suffix fragment
// again must NOT re-fire: the completing request reset the secret's fragment
// progress, and the store must honor that reset (the former element-wise max
// merge kept the stale progress and re-fired).
func TestSessionScan_NoRefireAfterCompletion(t *testing.T) {
	p, proxyURL, _ := newGuardPoolProxy(t, sessionGuardCfg("log"), fragPoolKey)

	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[:20]), "s1")
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[20:]), "s1")
	if got := fragmentedEvents(p); len(got) != 1 {
		t.Fatalf("split must fire exactly once, events = %v", got)
	}
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[20:]), "s1")
	if got := fragmentedEvents(p); len(got) != 1 {
		t.Fatalf("suffix fragment after completion must not re-fire, events = %v", got)
	}
}

// ---- session_test.go ----

// TestForwardThreadsSessionID verifies the session header becomes the live
// sticky key after a successful forward, rather than exposing a test-only
// scheduler callback from production code.
func TestForwardThreadsSessionID(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	keyFile := filepath.Join(dir, ".model-proxy", "zhipu_apikey.json")
	if err := os.WriteFile(keyFile, []byte(`{"api_key":"KEY-A"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	cfg := &Config{Listen: "127.0.0.1:1",
		Providers: map[string]Provider{"zhipu": {OpenAIBaseURL: srv.URL, Provider: "zhipu"}},
		Routes:    map[string][]RouteTarget{"glm-5": {{Provider: "zhipu", Model: "glm-5"}}}}
	p := newTestProxy(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"glm-5"}`)))
	req.Header.Set("x-claude-code-session-id", "sess-XYZ")
	w := httptest.NewRecorder()
	p.forward("openai", w, req, nextRequestID())
	if w.Code != http.StatusOK {
		t.Fatalf("forward status = %d, want %d", w.Code, http.StatusOK)
	}

	sticky, ok := p.runtimeState.Sticky("sess-XYZ")
	if !ok || sticky.Provider != "zhipu" {
		t.Fatalf("session sticky = %+v, present=%t; want provider zhipu for sess-XYZ", sticky, ok)
	}
}
