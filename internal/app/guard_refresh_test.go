package app

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Guard OAuth known-secret re-sync (guard_refresh.go): codex/aqp rotate their
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
	current := CollectOAuthSecrets(p.cfgSnapshot(), buildOpts())
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
