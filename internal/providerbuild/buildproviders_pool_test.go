package providerbuild

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
)

// buildproviders_pool_test.go covers the credential-pool unrolling and the
// guard known-secret collection at the BuildProviders level (no Proxy): pool
// fixtures go through accounts.Store on a temp HOME, assertions check the
// Build result maps and the credentials each virtual binds. All keys are
// synthetic.

func poolStore(t *testing.T) accounts.Store {
	t.Helper()
	return accounts.NewStore(accounts.HomeDir())
}

func writePool(t *testing.T, name, providerID string, keys ...string) {
	t.Helper()
	p := accounts.Pool{Version: 1}
	for _, k := range keys {
		c := accounts.Credentials{APIKey: k}
		p.Accounts = append(p.Accounts, accounts.Account{
			ID:      accounts.AccountID(providerID, c),
			Label:   k,
			APIKey:  k,
			AddedAt: "2026-07-08",
		})
	}
	if err := poolStore(t).Save(name, providerID, p); err != nil {
		t.Fatal(err)
	}
}

func poolConfig(providers ...string) *configdomain.Config {
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{}}
	for _, id := range providers {
		cfg.Providers[id] = configdomain.Provider{OpenAIBaseURL: "http://x", Provider: id}
	}
	return cfg
}

// TestBuildProviders_UnrollsMultiAccountPool: a ≥2-account pool becomes sorted
// virtuals "name#<id>"; the parent is not a runnable key; each virtual binds
// its own key; the pool secrets join the guard sets.
func TestBuildProviders_UnrollsMultiAccountPool(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writePool(t, "zhipu", "zhipu", "KEY-A", "KEY-B", "KEY-C")

	built := BuildProviders(poolConfig("zhipu"), poolStore(t), testBuildOpts())
	if _, ok := built.Providers["zhipu"]; ok {
		t.Fatal("parent zhipu must not be a runnable provider key when pooled")
	}
	vids := built.PoolIndex["zhipu"]
	if len(vids) != 3 {
		t.Fatalf("poolIndex[zhipu] len = %d, want 3 (%v)", len(vids), vids)
	}
	if !sort.StringsAreSorted(vids) {
		t.Fatalf("poolIndex[zhipu] not sorted: %v", vids)
	}
	if len(built.Providers) != 3 {
		t.Fatalf("providers map len = %d, want 3", len(built.Providers))
	}
	if !built.Eligible["zhipu"] {
		t.Error("pooled zhipu must be eligible")
	}
	want := map[string]bool{"Bearer KEY-A": true, "Bearer KEY-B": true, "Bearer KEY-C": true}
	for _, vid := range vids {
		if built.ParentOf[vid] != "zhipu" {
			t.Errorf("parentOf[%s] = %q, want zhipu", vid, built.ParentOf[vid])
		}
		req := httptest.NewRequest(http.MethodGet, "http://x/m", nil)
		if err := built.Providers[vid].AuthHeaders(req); err != nil {
			t.Fatalf("virtual %s AuthHeaders: %v", vid, err)
		}
		tok := req.Header.Get("Authorization")
		if !want[tok] {
			t.Errorf("virtual %s injected %q, want one of Bearer KEY-A/B/C", vid, tok)
		}
		delete(want, tok)
	}
	if len(want) != 0 {
		t.Errorf("virtual keys not distinct, missing: %v", want)
	}
	if len(built.PoolSecrets) != 3 || len(built.Secrets) != 3 {
		t.Errorf("PoolSecrets/Secrets = %d/%d values, want 3/3", len(built.PoolSecrets), len(built.Secrets))
	}
	if len(built.OAuthSecrets) != 0 {
		t.Errorf("OAuthSecrets = %d values, want 0 (no OAuth providers)", len(built.OAuthSecrets))
	}
}

// TestBuildProviders_SingleEntryPluralPoolBindsPlainName: a 1-entry PLURAL
// pool (what `login` writes) binds the key in-memory under the plain name.
func TestBuildProviders_SingleEntryPluralPoolBindsPlainName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writePool(t, "zhipu", "zhipu", "ONLY")

	built := BuildProviders(poolConfig("zhipu"), poolStore(t), testBuildOpts())
	p := built.Providers["zhipu"]
	if p == nil {
		t.Fatal("1-entry plural pool must keep the plain name runnable")
	}
	if len(built.PoolIndex) != 0 || len(built.ParentOf) != 0 {
		t.Fatalf("1-entry pool must not populate poolIndex/parentOf: %v / %v", built.PoolIndex, built.ParentOf)
	}
	if !built.Eligible["zhipu"] {
		t.Error("1-entry plural pool must be eligible")
	}
	req := httptest.NewRequest(http.MethodGet, "http://x/m", nil)
	if err := p.AuthHeaders(req); err != nil {
		t.Fatalf("AuthHeaders: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer ONLY" {
		t.Errorf("Authorization = %q, want Bearer ONLY (bound in-memory)", got)
	}
}

// TestBuildProviders_LegacySingularStaysFileBacked: with only the legacy
// singular <name>_apikey.json the provider keeps its plain name, stays
// file-backed, and is eligible (SourceLegacy).
func TestBuildProviders_LegacySingularStaysFileBacked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "zhipu_apikey.json"), []byte(`{"api_key":"SOLO"}`), 0o600)

	built := BuildProviders(poolConfig("zhipu"), poolStore(t), testBuildOpts())
	p := built.Providers["zhipu"]
	if p == nil {
		t.Fatal("legacy singular account must keep the plain name runnable")
	}
	if len(built.PoolIndex) != 0 || len(built.ParentOf) != 0 {
		t.Fatalf("legacy singular must not populate poolIndex/parentOf: %v / %v", built.PoolIndex, built.ParentOf)
	}
	if !built.Eligible["zhipu"] {
		t.Error("legacy singular source must be eligible")
	}
	req := httptest.NewRequest(http.MethodGet, "http://x/m", nil)
	if err := p.AuthHeaders(req); err != nil {
		t.Fatalf("AuthHeaders: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer SOLO" {
		t.Errorf("Authorization = %q, want Bearer SOLO (file-backed)", got)
	}
	// The legacy key still joins the guard known-secret set via the snapshot.
	if len(built.PoolSecrets) != 1 {
		t.Errorf("PoolSecrets = %d values, want 1 (legacy key wrapped as 1-entry pool)", len(built.PoolSecrets))
	}
}

// TestBuildProviders_EmptyPluralPoolDisablesProvider: an empty plural file is
// an authoritative credential tombstone — no unbound fallback to legacy.
func TestBuildProviders_EmptyPluralPoolDisablesProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writePool(t, "zhipu", "zhipu")

	built := BuildProviders(poolConfig("zhipu"), poolStore(t), testBuildOpts())
	if len(built.Providers) != 0 || len(built.PoolIndex) != 0 || built.Eligible["zhipu"] {
		t.Errorf("empty plural pool must disable the provider: %+v", built)
	}
}

// TestBuildProviders_UnreadablePoolDisablesProvider: a corrupt plural file is
// never downgraded to the legacy singular key.
func TestBuildProviders_UnreadablePoolDisablesProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "zhipu_apikeys.json"), []byte(`{not json`), 0o600)
	os.WriteFile(filepath.Join(credDir, "zhipu_apikey.json"), []byte(`{"api_key":"SOLO"}`), 0o600)

	built := BuildProviders(poolConfig("zhipu"), poolStore(t), testBuildOpts())
	if len(built.Providers) != 0 {
		t.Errorf("unreadable plural pool must disable the provider, got %v", built.Providers)
	}
}

// TestBuildProviders_StaticRequiresPluralPool: static is plural-only — with no
// pool file no unbound instance may exist.
func TestBuildProviders_StaticRequiresPluralPool(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	built := BuildProviders(poolConfig("static"), poolStore(t), testBuildOpts())
	if len(built.Providers) != 0 {
		t.Errorf("static without a plural pool must not be built, got %v", built.Providers)
	}
}

// TestBuildProviders_OAuthSecretsBestEffort: codex/aqp OAuth auth files join
// the OAuth secret subset; missing/corrupt files are skipped silently.
func TestBuildProviders_OAuthSecretsBestEffort(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"codex": {Provider: "codex", OpenAIBaseURL: "http://x"},
		"aqp":   {Provider: "aqp", OpenAIBaseURL: "http://x", AqpMintURL: "http://x/mint"},
	}}
	collect := func() []Secret {
		t.Helper()
		return BuildProviders(cfg, poolStore(t), testBuildOpts()).OAuthSecrets
	}

	// Missing files: nothing to protect, no error.
	if got := collect(); len(got) != 0 {
		t.Errorf("missing OAuth files: OAuthSecrets = %d values, want 0", len(got))
	}
	// Corrupt files: skipped silently, build still succeeds.
	os.WriteFile(filepath.Join(credDir, "codex_oauth_auth.json"), []byte(`{not json`), 0o600)
	os.WriteFile(filepath.Join(credDir, "aqp_oauth_auth.json"), []byte(`{"sso_session_cookie": 42}`), 0o600)
	if got := collect(); len(got) != 0 {
		t.Errorf("corrupt OAuth files: OAuthSecrets = %d values, want 0", len(got))
	}
	// Valid files: every token/cookie value is collected.
	os.WriteFile(filepath.Join(credDir, "codex_oauth_auth.json"),
		[]byte(`{"tokens":{"access_token":"syn-at","refresh_token":"syn-rt","id_token":"syn-it","account_id":"acct"}}`), 0o600)
	os.WriteFile(filepath.Join(credDir, "aqp_oauth_auth.json"),
		[]byte(`{"account_id":"a","sso_session_cookie":"SSO_C=syn-cookie"}`), 0o600)
	got := collect()
	want := map[string]bool{"syn-at": true, "syn-rt": true, "syn-it": true, "SSO_C=syn-cookie": true}
	if len(got) != len(want) {
		t.Fatalf("valid OAuth files: OAuthSecrets = %d values, want %d (%v)", len(got), len(want), got)
	}
	for _, s := range got {
		if !want[s.Value] {
			t.Errorf("unexpected OAuth secret collected")
		}
		delete(want, s.Value)
	}
	if len(want) != 0 {
		t.Errorf("OAuth secret values missing from the collected set")
	}
}

// TestCollectOAuthSecrets mirrors the BuildProviders OAuth pass: the refresh
// loop re-collects the same values without a provider rebuild.
func TestCollectOAuthSecrets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "codex_oauth_auth.json"),
		[]byte(`{"tokens":{"access_token":"syn-at","account_id":"acct"}}`), 0o600)
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"codex": {Provider: "codex", OpenAIBaseURL: "http://x"},
		"zhipu": {Provider: "zhipu", OpenAIBaseURL: "http://x"},
	}}
	got := CollectOAuthSecrets(cfg, testBuildOpts())
	if len(got) != 1 || got[0].Value != "syn-at" {
		t.Errorf("CollectOAuthSecrets = %d values, want [syn-at]", len(got))
	}
}

// TestHealthConfigFingerprint: stable per config content, order-independent,
// and changes when the provider set changes.
func TestHealthConfigFingerprint(t *testing.T) {
	a := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"b": {Provider: "zhipu", OpenAIBaseURL: "http://b"},
		"a": {Provider: "codex", OpenAIBaseURL: "http://a"},
	}}
	b := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"a": {Provider: "codex", OpenAIBaseURL: "http://a"},
		"b": {Provider: "zhipu", OpenAIBaseURL: "http://b"},
	}}
	c := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"a": {Provider: "codex", OpenAIBaseURL: "http://a"},
	}}
	fa, fb, fc := HealthConfigFingerprint(a), HealthConfigFingerprint(b), HealthConfigFingerprint(c)
	if fa != fb {
		t.Errorf("fingerprint must be map-order independent: %q vs %q", fa, fb)
	}
	if fa == fc {
		t.Error("fingerprint must change with the provider set")
	}
	if len(fa) != 16 {
		t.Errorf("fingerprint len = %d, want 16", len(fa))
	}
}

// TestBuildOpts wires the production seams against the process environment.
func TestBuildOpts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	opts := BuildOpts()
	if opts.HomeDir != accounts.HomeDir() {
		t.Errorf("BuildOpts HomeDir = %q, want %q", opts.HomeDir, accounts.HomeDir())
	}
	if opts.CodexCLIVersion == nil || opts.CodexCacheVersion == nil || opts.ListArkAgentPlanModelIDs == nil {
		t.Error("BuildOpts must wire all three seams")
	}
}

// TestCodexCacheVersion reads the codex CLI's models cache under CODEX_HOME.
func TestCodexCacheVersion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if got := CodexCacheVersion(); got != "" {
		t.Errorf("missing cache: got %q, want empty", got)
	}
	os.WriteFile(filepath.Join(home, "models_cache.json"), []byte(`not-json`), 0o600)
	if got := CodexCacheVersion(); got != "" {
		t.Errorf("corrupt cache: got %q, want empty", got)
	}
	os.WriteFile(filepath.Join(home, "models_cache.json"), []byte(`{"client_version":"1.2.3"}`), 0o600)
	if got := CodexCacheVersion(); got != "1.2.3" {
		t.Errorf("valid cache: got %q, want 1.2.3", got)
	}
}

// TestListArkAgentPlanModelIDs_Unreachable: with AK/SK on disk but an
// unreachable upstream the signed call errors out (no panic, no fake success).
func TestListArkAgentPlanModelIDs_Unreachable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "volcengine_apikey.json"),
		[]byte(`{"api_key":"ark","access_key":"ak","secret_key":"sk"}`), 0o600)
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := dead.Addr().String()
	dead.Close()
	t.Setenv("HTTPS_PROXY", "http://"+addr)
	t.Setenv("HTTP_PROXY", "http://"+addr)
	// Empty AK/SK: the on-disk legacy creds feed the signed call, which dies on
	// the unreachable upstream.
	if _, err := ListArkAgentPlanModelIDs(context.Background(), "volcengine", "", ""); err == nil {
		t.Error("unreachable upstream: want error, got nil")
	}
}

// fakeListSeam records the (provider|AK|SK) of each signed model-list call made
// through the BuildOptions seam. FetchModels runs synchronously, so no locking
// is needed.
type fakeListSeam struct {
	calls int
	last  string
}

func (f *fakeListSeam) opts(home string) BuildOptions {
	return BuildOptions{
		HomeDir: home,
		ListArkAgentPlanModelIDs: func(ctx context.Context, provName, ak, sk string) ([]string, error) {
			f.calls++
			f.last = provName + "|" + ak + "|" + sk
			return []string{"model-a"}, nil
		},
	}
}

// TestBuildOne_Volcengine_FetchModels_UsesVirtualOwnCreds (A12 regression):
// the FetchModelsFn closure must sign with THIS virtual's bound AK/SK, not
// re-read the legacy single-account file (in a pooled setup that file is gone
// or archived as .migrated.bak, so the pre-fix path could never succeed for
// pooled virtuals).
func TestBuildOne_Volcengine_FetchModels_UsesVirtualOwnCreds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seam := &fakeListSeam{}
	cfg := poolConfig("volcengine")
	p := BuildOne(cfg, seam.opts(accounts.HomeDir()), "volcengine", cfg.Providers["volcengine"],
		accounts.Credentials{APIKey: "ark-1", AccessKey: "AKV", SecretKey: "SKV"})
	if p == nil {
		t.Fatal("BuildOne volcengine returned nil")
	}
	ids, err := p.FetchModels()
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(ids) != 1 || ids[0] != "model-a" {
		t.Errorf("ids=%v want the seam's [model-a]", ids)
	}
	if seam.calls != 1 || seam.last != "volcengine|AKV|SKV" {
		t.Errorf("seam calls=%d last=%q, want 1 call with volcengine|AKV|SKV", seam.calls, seam.last)
	}
}

// TestBuildOne_Volcengine_FetchModels_ChatOnlyNeedsAKSK (A12/A06 parity): a
// chat-only virtual (bound Ark key, no AK/SK) must report needs-AK/SK instead
// of signing with a legacy single-account file left on disk by a pre-pool
// login — its keys belong to a DIFFERENT account. The seam must never run.
func TestBuildOne_Volcengine_FetchModels_ChatOnlyNeedsAKSK(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "volcengine_apikey.json"),
		[]byte(`{"api_key":"ark","access_key":"AKLEGACY-MARKER","secret_key":"SKLEGACY-MARKER"}`), 0o600)

	seam := &fakeListSeam{}
	cfg := poolConfig("volcengine")
	p := BuildOne(cfg, seam.opts(home), "volcengine", cfg.Providers["volcengine"],
		accounts.Credentials{APIKey: "ark-chat-only"})
	if p == nil {
		t.Fatal("BuildOne volcengine returned nil")
	}
	_, err := p.FetchModels()
	if err == nil || !strings.Contains(err.Error(), "AK/SK") {
		t.Fatalf("chat-only FetchModels: err=%v, want needs-AK/SK error", err)
	}
	if strings.Contains(err.Error(), "AKLEGACY-MARKER") {
		t.Errorf("error must not carry the legacy file's access key marker: %q", err)
	}
	if seam.calls != 0 {
		t.Errorf("seam calls=%d, want 0 (chat-only virtual must not sign a model list)", seam.calls)
	}
}

// TestBuildOne_Volcengine_FetchModels_UnboundLegacyFallback: the unbound
// single-account instance passes empty AK/SK through the seam so the real
// implementation falls back to the legacy store via credstore.
func TestBuildOne_Volcengine_FetchModels_UnboundLegacyFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seam := &fakeListSeam{}
	cfg := poolConfig("volcengine")
	p := BuildOne(cfg, seam.opts(accounts.HomeDir()), "volcengine", cfg.Providers["volcengine"], accounts.Credentials{})
	if p == nil {
		t.Fatal("BuildOne volcengine returned nil")
	}
	if _, err := p.FetchModels(); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if seam.calls != 1 || seam.last != "volcengine||" {
		t.Errorf("seam calls=%d last=%q, want 1 call with empty AK/SK", seam.calls, seam.last)
	}
}
