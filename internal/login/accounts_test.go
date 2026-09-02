package login

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
)

// setPoolHome isolates account storage for login tests.
func setPoolHome(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".model-proxy"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
}

// stubVolcengineValidator no-ops AK/SK validation for pool dedup/save tests.
func stubVolcengineValidator(t *testing.T) {
	t.Helper()
	orig := VolcengineAKSKValidator
	VolcengineAKSKValidator = func(string, string) error { return nil }
	t.Cleanup(func() { VolcengineAKSKValidator = orig })
}

// --- non-printing account cores (add/remove) ---

// TestAddApikeyAccountCore exercises the non-printing core extracted from
// runApiKeyLoginWithInput: validate + dedup + savePool under the lock, plus
// the symmetric removeApikeyAccount. No stdin, no printing — the web layer
// (Task 12) reuses this same path.
func TestAddApikeyAccountCore(t *testing.T) {
	setPoolHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer up.Close()
	cfg, _ := configdomain.LoadConfigFromBytes("test", []byte("providers:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://x\n    usage_url: "+up.URL+"\n"))
	prov := cfg.Providers["zhipu"]
	id, err := AddApikeyAccount(cfg, "zhipu", prov, accounts.Credentials{APIKey: "sk-test-1234567890"}, "my-label", false)
	if err != nil {
		t.Fatalf("addApikeyAccount: %v", err)
	}
	if id == "" {
		t.Fatal("empty id")
	}
	// ID must match the sha256[:16] of the key (non-volcengine).
	wantID := accounts.AccountID("zhipu", accounts.Credentials{APIKey: "sk-test-1234567890"})
	if id != wantID {
		t.Fatalf("id = %q, want %q", id, wantID)
	}
	pool, _ := accounts.NewStore(accounts.HomeDir()).Load("zhipu", "zhipu")
	if len(pool.Accounts) != 1 || pool.Accounts[0].Label != "my-label" {
		t.Fatalf("pool not written: %+v", pool.Accounts)
	}
	// Replace path: same id, new label, replace=true overwrites in place.
	if _, err := AddApikeyAccount(cfg, "zhipu", prov, accounts.Credentials{APIKey: "sk-test-1234567890"}, "renamed", true); err != nil {
		t.Fatalf("replace addApikeyAccount: %v", err)
	}
	pool2, _ := accounts.NewStore(accounts.HomeDir()).Load("zhipu", "zhipu")
	if len(pool2.Accounts) != 1 || pool2.Accounts[0].Label != "renamed" {
		t.Fatalf("replace should keep size 1 + update label: %+v", pool2.Accounts)
	}
	// Replace path with replace=false on an existing id aborts without prompting
	// (no stdin in core) and leaves the pool untouched.
	if _, err := AddApikeyAccount(cfg, "zhipu", prov, accounts.Credentials{APIKey: "sk-test-1234567890"}, "ignored", false); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("dup-no-replace should error 'cancelled', got %v", err)
	}
	// Remove: pool empties.
	if err := RemoveApikeyAccount("zhipu", "zhipu", id); err != nil {
		t.Fatalf("remove: %v", err)
	}
	pool3, _ := accounts.NewStore(accounts.HomeDir()).Load("zhipu", "zhipu")
	if len(pool3.Accounts) != 0 {
		t.Fatalf("pool not emptied: %+v", pool3.Accounts)
	}
}

// TestAddApikeyAccountCore_ValidationFail verifies the core rejects a 401 from
// the usage endpoint (no save).
func TestAddApikeyAccountCore_ValidationFail(t *testing.T) {
	setPoolHome(t, t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`unauthorized`))
	}))
	defer srv.Close()
	cfg, _ := configdomain.LoadConfigFromBytes("test", []byte("providers:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://x\n    usage_url: "+srv.URL+"\n"))
	prov := cfg.Providers["zhipu"]
	_, err := AddApikeyAccount(cfg, "zhipu", prov, accounts.Credentials{APIKey: "bad"}, "", false)
	if err == nil || !strings.Contains(err.Error(), "validation failed") {
		t.Fatalf("expected 'validation failed', got %v", err)
	}
	pool, _ := accounts.NewStore(accounts.HomeDir()).Load("zhipu", "zhipu")
	if len(pool.Accounts) != 0 {
		t.Fatalf("401 should not save: %+v", pool.Accounts)
	}
}

// TestAddVolcengineAccountCore verifies the AK/SK core: dedup by AccessKey,
// triple save, label, remove.
func TestAddVolcengineAccountCore(t *testing.T) {
	setPoolHome(t, t.TempDir())
	stubVolcengineValidator(t) // pool dedup/save logic; AK/SK validation tested elsewhere
	cfg, _ := configdomain.LoadConfigFromBytes("test", []byte("providers:\n  vol:\n    provider_id: volcengine\n    openai_base_url: https://x\n"))
	prov := cfg.Providers["vol"]
	cred := accounts.Credentials{APIKey: "ark-key", AccessKey: "AK9XYZ", SecretKey: "SK9"}
	id, err := AddVolcengineAccount(cfg, "vol", prov, cred, "volc-label", false)
	if err != nil {
		t.Fatalf("addVolcengineAccount: %v", err)
	}
	wantID := accounts.AccountID("volcengine", cred)
	if id != wantID || id == "AK9XYZ" {
		t.Fatalf("volcengine id = %q, want hashed %q (never the raw access key)", id, wantID)
	}
	pool, _ := accounts.NewStore(accounts.HomeDir()).Load("vol", "volcengine")
	if len(pool.Accounts) != 1 || pool.Accounts[0].Label != "volc-label" {
		t.Fatalf("pool not written: %+v", pool.Accounts)
	}
	if pool.Accounts[0].AccessKey != "AK9XYZ" || pool.Accounts[0].SecretKey != "SK9" || pool.Accounts[0].APIKey != "ark-key" {
		t.Fatalf("triple not saved: %+v", pool.Accounts[0])
	}
	// Same AccessKey with replace=false aborts; with replace=true overwrites the triple.
	if _, err := AddVolcengineAccount(cfg, "vol", prov, accounts.Credentials{APIKey: "ark2", AccessKey: "AK9XYZ", SecretKey: "SK-new"}, "", false); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("dup-no-replace should error 'cancelled', got %v", err)
	}
	if _, err := AddVolcengineAccount(cfg, "vol", prov, accounts.Credentials{APIKey: "ark2", AccessKey: "AK9XYZ", SecretKey: "SK-new"}, "renamed", true); err != nil {
		t.Fatalf("replace addVolcengineAccount: %v", err)
	}
	pool2, _ := accounts.NewStore(accounts.HomeDir()).Load("vol", "volcengine")
	if len(pool2.Accounts) != 1 {
		t.Fatalf("replace should keep size 1: %+v", pool2.Accounts)
	}
	if pool2.Accounts[0].APIKey != "ark2" || pool2.Accounts[0].SecretKey != "SK-new" || pool2.Accounts[0].Label != "renamed" {
		t.Fatalf("replace did not overwrite triple/label: %+v", pool2.Accounts[0])
	}
	if err := RemoveApikeyAccount("vol", "volcengine", id); err != nil {
		t.Fatalf("remove: %v", err)
	}
	pool3, _ := accounts.NewStore(accounts.HomeDir()).Load("vol", "volcengine")
	if len(pool3.Accounts) != 0 {
		t.Fatalf("pool not emptied: %+v", pool3.Accounts)
	}
}

func TestRemoveApikeyAccountCleansRestoredKeychainEntry(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	cred := accounts.Credentials{APIKey: "sk-restored-remove-core"}
	id := accounts.AccountID("zhipu", cred)
	pool := accounts.Pool{Version: 1, Accounts: []accounts.Account{{
		ID: id, Label: "restored", APIKey: cred.APIKey, AddedAt: "2026-08-26",
	}}}
	if err := accounts.NewStoreWithBackend(dir, accounts.BackendKeychain).Save("prov", "zhipu", pool); err != nil {
		t.Fatalf("seed keychain pool: %v", err)
	}
	fileStore := accounts.NewStoreWithBackend(dir, accounts.BackendFile)
	if _, err := fileStore.LoadSnapshot("prov", "zhipu"); err != nil {
		t.Fatalf("restore keychain pool to file: %v", err)
	}
	originalStoreEnv := accountStoreEnv
	accountStoreEnv = func() accounts.Store { return fileStore }
	t.Cleanup(func() { accountStoreEnv = originalStoreEnv })

	if err := RemoveApikeyAccount("prov", "zhipu", id); err != nil {
		t.Fatalf("RemoveApikeyAccount: %v", err)
	}
	if _, err := keyring.Get("model-proxy", "prov/"+id+"/api_key"); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatalf("restored keychain api_key after RemoveApikeyAccount: err = %v, want ErrNotFound", err)
	}
	after, err := fileStore.Load("prov", "zhipu")
	if err != nil || len(after.Accounts) != 0 {
		t.Fatalf("file pool after RemoveApikeyAccount = (%+v, %v)", after, err)
	}
}

// --- login validates API keys for kimi-code and volcengine ---

// TestValidateKeyBearerGET pins the shared key-validation gate's semantics: a
// no-op on empty url, nil on 200, an error on 401/403 (the two statuses that
// mean "key rejected"). Green-signal guard: getting the accept/reject boundary
// wrong would silently pass bad keys or reject valid ones.
func TestValidateKeyBearerGET(t *testing.T) {
	if err := ValidateKeyBearerGET("", "k"); err != nil {
		t.Errorf("empty url: want nil, got %v", err)
	}
	statusCase := func(code int) {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		defer srv.Close()
		err := ValidateKeyBearerGET(srv.URL, "k")
		switch code {
		case 200:
			if err != nil {
				t.Errorf("HTTP %d: want nil, got %v", code, err)
			}
		case 401, 403:
			if err == nil || !strings.Contains(err.Error(), "validation failed") {
				t.Errorf("HTTP %d: want 'validation failed', got %v", code, err)
			}
		}
	}
	statusCase(200)
	statusCase(401)
	statusCase(403)
}

// TestValidateKeyBearerGET_HugeBodyIsCapped pins the 16KB read cap: the error
// path only needs a short excerpt, and an endless/huge validation response
// must not be buffered in full. Without the cap this test hits the 15s client
// timeout and returns a transport error instead of the HTTP 401 verdict.
func TestValidateKeyBearerGET_HugeBodyIsCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		// Write far beyond the cap until the capped client stops reading.
		chunk := make([]byte, 64<<10)
		for {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	done := make(chan error, 1)
	go func() { done <- ValidateKeyBearerGET(srv.URL, "k") }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
			t.Fatalf("err = %v, want HTTP 401 verdict", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ValidateKeyBearerGET never returned — the body read is not capped")
	}
}

// TestAddVolcengineAccountCore_ArkKeyValidation401 pins that volcengine login
// validates the Ark API Key via usage_url (a Bearer GET to /models): a 401
// rejects before the triple is saved.
func TestAddVolcengineAccountCore_ArkKeyValidation401(t *testing.T) {
	setPoolHome(t, t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bad-ark" {
			t.Errorf("validation sent Authorization=%q, want Bearer bad-ark", r.Header.Get("Authorization"))
		}
		w.WriteHeader(401)
		w.Write([]byte(`unauthorized`))
	}))
	defer srv.Close()
	cfg, _ := configdomain.LoadConfigFromBytes("test", []byte("providers:\n  vol:\n    provider_id: volcengine\n    openai_base_url: https://x\n    usage_url: "+srv.URL+"\n"))
	prov := cfg.Providers["vol"]
	// No AK/SK — isolates the Ark-key path.
	_, err := AddVolcengineAccount(cfg, "vol", prov, accounts.Credentials{APIKey: "bad-ark"}, "", false)
	if err == nil || !strings.Contains(err.Error(), "validation failed") {
		t.Fatalf("Ark 401: err=%v want 'validation failed'", err)
	}
	pool, _ := accounts.NewStore(accounts.HomeDir()).Load("vol", "volcengine")
	if len(pool.Accounts) != 0 {
		t.Fatalf("Ark 401 should not save: %+v", pool.Accounts)
	}
}

// TestAddVolcengineAccountCore_ArkKeyValid pins that a 200 from /models accepts
// the Ark key and saves the triple (chat-only, no AK/SK).
func TestAddVolcengineAccountCore_ArkKeyValid(t *testing.T) {
	setPoolHome(t, t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	cfg, _ := configdomain.LoadConfigFromBytes("test", []byte("providers:\n  vol:\n    provider_id: volcengine\n    openai_base_url: https://x\n    usage_url: "+srv.URL+"\n"))
	prov := cfg.Providers["vol"]
	id, err := AddVolcengineAccount(cfg, "vol", prov, accounts.Credentials{APIKey: "good-ark"}, "lbl", false)
	if err != nil {
		t.Fatalf("Ark 200: %v", err)
	}
	if id == "" {
		t.Fatal("empty id")
	}
	pool, _ := accounts.NewStore(accounts.HomeDir()).Load("vol", "volcengine")
	if len(pool.Accounts) != 1 || pool.Accounts[0].APIKey != "good-ark" || pool.Accounts[0].Label != "lbl" {
		t.Fatalf("Ark 200 should save triple: %+v", pool.Accounts)
	}
}

// TestAddVolcengineAccountCore_AKSKValidationFail pins that the AK/SK pair is
// validated (via the VolcengineAKSKValidator seam, stubbed here to fail) when
// both are present, and that a failure rejects before save. Asserts the stub WAS
// called — a count-only check would miss a "never validated" bug.
func TestAddVolcengineAccountCore_AKSKValidationFail(t *testing.T) {
	setPoolHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer up.Close()
	cfg, _ := configdomain.LoadConfigFromBytes("test", []byte("providers:\n  vol:\n    provider_id: volcengine\n    openai_base_url: https://x\n    usage_url: "+up.URL+"\n"))
	prov := cfg.Providers["vol"]

	orig := VolcengineAKSKValidator
	defer func() { VolcengineAKSKValidator = orig }()
	called := false
	VolcengineAKSKValidator = func(ak, sk string) error {
		called = true
		if ak != "AK9" || sk != "SK9" {
			t.Errorf("validator got ak=%q sk=%q, want AK9/SK9", ak, sk)
		}
		return fmt.Errorf("GetAFPUsage HTTP 401: signature mismatch")
	}

	_, err := AddVolcengineAccount(cfg, "vol", prov, accounts.Credentials{APIKey: "good-ark", AccessKey: "AK9", SecretKey: "SK9"}, "", false)
	if err == nil || !strings.Contains(err.Error(), "validation failed") {
		t.Fatalf("AK/SK fail: err=%v want 'validation failed'", err)
	}
	if !called {
		t.Fatal("AK/SK validator was not called")
	}
	pool, _ := accounts.NewStore(accounts.HomeDir()).Load("vol", "volcengine")
	if len(pool.Accounts) != 0 {
		t.Fatalf("AK/SK fail should not save: %+v", pool.Accounts)
	}
}

// TestAddVolcengineAccountCore_AKSKEmptySkips pins the optional-AK/SK rule:
// when AK/SK are absent (chat-only), the validator is NOT called and a valid Ark
// key is saved. A sentinel stub fails the test if invoked.
func TestAddVolcengineAccountCore_AKSKEmptySkips(t *testing.T) {
	setPoolHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer up.Close()
	cfg, _ := configdomain.LoadConfigFromBytes("test", []byte("providers:\n  vol:\n    provider_id: volcengine\n    openai_base_url: https://x\n    usage_url: "+up.URL+"\n"))
	prov := cfg.Providers["vol"]

	orig := VolcengineAKSKValidator
	defer func() { VolcengineAKSKValidator = orig }()
	VolcengineAKSKValidator = func(ak, sk string) error {
		t.Fatal("validator must not be called when AK/SK absent")
		return nil
	}

	if _, err := AddVolcengineAccount(cfg, "vol", prov, accounts.Credentials{APIKey: "good-ark"}, "", false); err != nil {
		t.Fatalf("chat-only login should succeed: %v", err)
	}
	pool, _ := accounts.NewStore(accounts.HomeDir()).Load("vol", "volcengine")
	if len(pool.Accounts) != 1 || pool.Accounts[0].APIKey != "good-ark" {
		t.Fatalf("chat-only should save: %+v", pool.Accounts)
	}
}

// TestAddVolcengineAccountCore_PartialAKSKRejected pins the both-or-neither rule:
// a lone AccessKey or lone SecretKey is rejected up front with "both be set"
// (a partial pair can't sign GetAFPUsage yet previously saved silently and
// degraded to BillingUnknown). Neither-set still succeeds (chat-only). A sentinel
// stub also proves the validator never runs for partial or empty pairs.
func TestAddVolcengineAccountCore_PartialAKSKRejected(t *testing.T) {
	setPoolHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer up.Close()
	cfg, _ := configdomain.LoadConfigFromBytes("test", []byte("providers:\n  vol:\n    provider_id: volcengine\n    openai_base_url: https://x\n    usage_url: "+up.URL+"\n"))
	prov := cfg.Providers["vol"]

	orig := VolcengineAKSKValidator
	defer func() { VolcengineAKSKValidator = orig }()
	VolcengineAKSKValidator = func(string, string) error {
		t.Error("validator must not run for a partial or empty AK/SK pair")
		return nil
	}

	for _, cred := range []accounts.Credentials{
		{APIKey: "good-ark", AccessKey: "AK9"}, // lone AK
		{APIKey: "good-ark", SecretKey: "SK9"}, // lone SK
	} {
		_, err := AddVolcengineAccount(cfg, "vol", prov, cred, "", false)
		if err == nil || !strings.Contains(err.Error(), "both be set") {
			t.Errorf("partial %+v: err=%v want 'both be set'", cred, err)
		}
		pool, _ := accounts.NewStore(accounts.HomeDir()).Load("vol", "volcengine")
		if len(pool.Accounts) != 0 {
			t.Errorf("partial pair must not save: %+v", pool.Accounts)
		}
	}

	// Neither set → chat-only success; pool written.
	if _, err := AddVolcengineAccount(cfg, "vol", prov, accounts.Credentials{APIKey: "good-ark"}, "", false); err != nil {
		t.Fatalf("chat-only (no AK/SK): %v", err)
	}
	pool, _ := accounts.NewStore(accounts.HomeDir()).Load("vol", "volcengine")
	if len(pool.Accounts) != 1 || pool.Accounts[0].APIKey != "good-ark" {
		t.Fatalf("chat-only should save: %+v", pool.Accounts)
	}
}

func TestApiKeyValidationURL_Fallback(t *testing.T) {
	cases := []struct {
		name string
		prov configdomain.Provider
		want string
	}{
		{"usage_url wins", configdomain.Provider{UsageURL: "https://x/balance", OpenAIBaseURL: "https://x/v1"}, "https://x/balance"},
		{"openai_base_url/models when no usage_url", configdomain.Provider{UsageURL: "", OpenAIBaseURL: "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1"}, "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1/models"},
		{"trailing slash trimmed", configdomain.Provider{UsageURL: "", OpenAIBaseURL: "https://x/v1/"}, "https://x/v1/models"},
		{"empty when neither set", configdomain.Provider{UsageURL: "", OpenAIBaseURL: ""}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ApiKeyValidationURL(c.prov); got != c.want {
				t.Errorf("apiKeyValidationURL = %q, want %q", got, c.want)
			}
		})
	}
}
