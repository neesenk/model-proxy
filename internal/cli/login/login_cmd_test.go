package login

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"model-proxy/internal/accounts"
	cliframework "model-proxy/internal/cli/framework"
	cliserve "model-proxy/internal/cli/serve"
	configdomain "model-proxy/internal/config"
	logincore "model-proxy/internal/login"
)

// login_cmd_test.go covers cmdLogin's error/help paths (subprocess) and
// runApiKeyLogin's stdin-driven validation (in-process with a redirected stdin).
// The non-printing account cores (AddApikeyAccount/AddVolcengineAccount/
// RemoveApikeyAccount/ValidateKeyBearerGET/ApiKeyValidationURL) moved to
// internal/login with their tests (accounts_test.go there).

// --- cmdLogin: no provider → usage + available providers ---

// --- cmdLogin: unknown provider → non-zero ---

// --- runApiKeyLogin: empty key → error (stdin redirected) ---

func TestRunApiKeyLogin_EmptyKey(t *testing.T) {
	// Redirect stdin to a pipe that yields an empty line.
	orig := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	defer func() { os.Stdin = orig }()
	w.Write([]byte("\n"))
	w.Close()

	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{"zhipu": {Provider: "zhipu"}}}
	err := RunApiKeyLoginWithInput(cfg, "zhipu", cfg.Providers["zhipu"], "", "", false)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty key: err=%v want 'empty' error", err)
	}
}

// --- runApiKeyLogin: valid key + mock validation → saved ---

func TestRunApiKeyLogin_ValidKeyMockValidation(t *testing.T) {
	// Stand up a mock usage endpoint that returns 200 (key accepted).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	// Redirect stdin to provide a key.
	orig := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	defer func() { os.Stdin = orig }()
	w.Write([]byte("test-api-key\n"))
	w.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"zhipu": {Provider: "zhipu", UsageURL: srv.URL},
		},
	}
	if err := RunApiKeyLoginWithInput(cfg, "zhipu", cfg.Providers["zhipu"], "", "", false); err != nil {
		t.Fatalf("runApiKeyLogin: %v", err)
	}
	// The key should have been saved to the PLURAL pool file
	// (<name>_apikeys.json). The legacy singular <name>_apikey.json is no
	// longer written by runApiKeyLogin — it now routes through the pool-aware
	// path. Assert via loadPool so we also verify the file is parseable.
	pool, err := accounts.NewStore(accounts.HomeDir()).Load("zhipu", "zhipu")
	if err != nil {
		t.Fatalf("load pool: %v", err)
	}
	if len(pool.Accounts) != 1 {
		t.Fatalf("want 1 account in pool, got %d: %+v", len(pool.Accounts), pool.Accounts)
	}
	if pool.Accounts[0].APIKey != "test-api-key" {
		t.Errorf("saved key = %q, want test-api-key", pool.Accounts[0].APIKey)
	}
	// ID must match the sha256[:16] of the key (zhipu is non-volcengine).
	wantID := accounts.AccountID("zhipu", accounts.Credentials{APIKey: "test-api-key"})
	if pool.Accounts[0].ID != wantID {
		t.Errorf("saved id = %q, want %q", pool.Accounts[0].ID, wantID)
	}
}

// --- runApiKeyLogin: 401 from validation → error ---

func TestRunApiKeyLogin_Validation401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`unauthorized`))
	}))
	defer srv.Close()

	t.Setenv("HOME", t.TempDir())
	orig := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	defer func() { os.Stdin = orig }()
	w.Write([]byte("bad-key\n"))
	w.Close()

	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{"zhipu": {Provider: "zhipu", UsageURL: srv.URL}}}
	err := RunApiKeyLoginWithInput(cfg, "zhipu", cfg.Providers["zhipu"], "", "", false)
	if err == nil || !strings.Contains(err.Error(), "validation failed") {
		t.Errorf("401 validation: err=%v want 'validation failed'", err)
	}
}

// --- pool-aware login: dedup by accountID, --label, --replace ---

// TestRunApiKeyLoginWithInput_DedupSameKey verifies that re-entering the SAME
// key with --replace keeps the pool at size 1 (idempotent replace, not append).
// The id is derived from the key (sha256[:16] for non-volcengine), so the same
// key always maps to the same id → the existing entry is overwritten in place.
func TestRunApiKeyLoginWithInput_DedupSameKey(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	cfg := &configdomain.Config{Listen: "127.0.0.1:1", Providers: map[string]configdomain.Provider{"zhipu": {Provider: "zhipu"}}}
	prov := cfg.Providers["zhipu"]
	writePoolFile(t, "zhipu", "zhipu", "DUP-KEY")
	RunApiKeyLoginWithInput(cfg, "zhipu", prov, "DUP-KEY", "renamed", true /*replace*/)
	pool, err := accounts.NewStore(accounts.HomeDir()).Load("zhipu", "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 1 {
		t.Fatalf("dup key should keep size 1, got %d: %+v", len(pool.Accounts), pool.Accounts)
	}
	// Replace must also update the label.
	if pool.Accounts[0].Label != "renamed" {
		t.Fatalf("replace should update label: got %q want %q", pool.Accounts[0].Label, "renamed")
	}
	if pool.Accounts[0].APIKey != "DUP-KEY" {
		t.Fatalf("replace should keep key: got %q", pool.Accounts[0].APIKey)
	}
}

// TestRunApiKeyLoginWithInput_DedupSameKey_NoReplace_Aborts verifies that when
// the same id already exists and replace is false, the function aborts with
// "login cancelled" (stdin says "n"). The pool is left untouched.
func TestRunApiKeyLoginWithInput_DedupSameKey_NoReplace_Aborts(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	cfg := &configdomain.Config{Listen: "127.0.0.1:1", Providers: map[string]configdomain.Provider{"zhipu": {Provider: "zhipu"}}}
	prov := cfg.Providers["zhipu"]
	writePoolFile(t, "zhipu", "zhipu", "DUP-KEY")

	// Redirect stdin to answer "n" to the replace prompt.
	orig := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	defer func() { os.Stdin = orig }()
	w.Write([]byte("n\n"))
	w.Close()

	err := RunApiKeyLoginWithInput(cfg, "zhipu", prov, "DUP-KEY", "", false /*replace*/)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("expected 'cancelled' error, got %v", err)
	}
	// Pool unchanged.
	pool, _ := accounts.NewStore(accounts.HomeDir()).Load("zhipu", "zhipu")
	if len(pool.Accounts) != 1 || pool.Accounts[0].APIKey != "DUP-KEY" {
		t.Fatalf("abort should leave pool untouched: %+v", pool.Accounts)
	}
}

// TestRunApiKeyLoginWithInput_DifferentKeyAppends verifies that a NEW key
// appends a fresh entry to the pool, with the provided label applied.
func TestRunApiKeyLoginWithInput_DifferentKeyAppends(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	cfg := &configdomain.Config{Listen: "127.0.0.1:1", Providers: map[string]configdomain.Provider{"zhipu": {Provider: "zhipu"}}}
	writePoolFile(t, "zhipu", "zhipu", "KEY-1")
	RunApiKeyLoginWithInput(cfg, "zhipu", cfg.Providers["zhipu"], "KEY-2", "team", false)
	pool, err := accounts.NewStore(accounts.HomeDir()).Load("zhipu", "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 2 {
		t.Fatalf("different key should append, got %d: %+v", len(pool.Accounts), pool.Accounts)
	}
	// Label applied to the new entry.
	var labeled *accounts.Account
	for i := range pool.Accounts {
		if pool.Accounts[i].Label == "team" {
			labeled = &pool.Accounts[i]
		}
	}
	if labeled == nil || labeled.APIKey != "KEY-2" {
		t.Fatalf("labeled entry wrong: %+v", labeled)
	}
}

// TestRunApiKeyLoginWithInput_EmptyKey verifies that an empty prompted key
// errors without touching the pool. The direct `in` arg is empty, so the
// function prompts on stdin; we feed it an empty line.
func TestRunApiKeyLoginWithInput_EmptyKey(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	orig := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	defer func() { os.Stdin = orig }()
	w.Write([]byte("\n"))
	w.Close()

	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{"zhipu": {Provider: "zhipu"}}}
	err := RunApiKeyLoginWithInput(cfg, "zhipu", cfg.Providers["zhipu"], "", "", false)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty key: err=%v want 'empty'", err)
	}
}

// TestRunApiKeyLoginWithInput_NoLabel_DefaultsToID verifies that when no label
// is provided, the entry's label defaults to the account id.
func TestRunApiKeyLoginWithInput_NoLabel_DefaultsToID(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	cfg := &configdomain.Config{Listen: "127.0.0.1:1", Providers: map[string]configdomain.Provider{"zhipu": {Provider: "zhipu"}}}
	prov := cfg.Providers["zhipu"]
	if err := RunApiKeyLoginWithInput(cfg, "zhipu", prov, "FRESH-KEY", "", false); err != nil {
		t.Fatal(err)
	}
	pool, _ := accounts.NewStore(accounts.HomeDir()).Load("zhipu", "zhipu")
	if len(pool.Accounts) != 1 {
		t.Fatalf("want 1 account, got %d", len(pool.Accounts))
	}
	wantID := accounts.AccountID("zhipu", accounts.Credentials{APIKey: "FRESH-KEY"})
	if pool.Accounts[0].ID != wantID {
		t.Fatalf("id = %q want %q", pool.Accounts[0].ID, wantID)
	}
	if pool.Accounts[0].Label != wantID {
		t.Fatalf("label should default to id: got %q want %q", pool.Accounts[0].Label, wantID)
	}
}

// --- runVolcengineLoginWithInput: writes the pool (triple), dedup by AccessKey ---
//
// These tests moved from internal/app/volcengine_creds_test.go with the login
// core extraction: they exercise the CLI wrapper's pool write/dedup behavior
// (all inputs passed directly, so no stdin prompts), which belongs to this
// package. The app package keeps the pool→virtuals unrolling test.

// stubVolcengineValidator no-ops AK/SK validation for pool dedup/save tests.
func stubVolcengineValidator(t *testing.T) {
	t.Helper()
	orig := logincore.VolcengineAKSKValidator
	logincore.VolcengineAKSKValidator = func(string, string) error { return nil }
	t.Cleanup(func() { logincore.VolcengineAKSKValidator = orig })
}

// TestRunVolcengineLoginWithInput_WritesPoolTriple verifies the pool-aware
// volcengine login writes a pool entry carrying the FULL triple
// {api_key, access_key, secret_key} with id keyed by the AccessKey HASH
// (account-level identity, no credential material — the id reaches logs and
// persisted state). Deleting any of the three fields from the saved entry, or
// keying by api_key, turns this red.
func TestRunVolcengineLoginWithInput_WritesPoolTriple(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	stubVolcengineValidator(t)
	cfg := &configdomain.Config{Listen: "127.0.0.1:1", Providers: map[string]configdomain.Provider{"volcengine": {Provider: "volcengine"}}}
	prov := cfg.Providers["volcengine"]
	if err := RunVolcengineLoginWithInput(cfg, "volcengine", prov,
		"ark-key-1", "AK-ONE", "SK-ONE", "acct-one", false); err != nil {
		t.Fatalf("runVolcengineLoginWithInput: %v", err)
	}
	pool, err := accounts.NewStore(accounts.HomeDir()).Load("volcengine", "volcengine")
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 1 {
		t.Fatalf("want 1 account, got %d: %+v", len(pool.Accounts), pool.Accounts)
	}
	a := pool.Accounts[0]
	// id keyed by the AccessKey hash (account-level identity), NOT the api_key
	// hash and never the raw access key.
	wantID := accounts.AccountID("volcengine", accounts.Credentials{APIKey: "ark-key-1", AccessKey: "AK-ONE", SecretKey: "SK-ONE"})
	if a.ID != wantID {
		t.Errorf("id = %q, want %q (access_key hash)", a.ID, wantID)
	}
	if a.ID == "AK-ONE" || strings.Contains(a.ID, "AK-ONE") {
		t.Errorf("id %q must not carry the raw access key", a.ID)
	}
	if a.APIKey != "ark-key-1" || a.AccessKey != "AK-ONE" || a.SecretKey != "SK-ONE" {
		t.Errorf("saved triple = %+v, want {api_key:ark-key-1 access_key:AK-ONE secret_key:SK-ONE}", a)
	}
	if a.Label != "acct-one" {
		t.Errorf("label = %q, want acct-one", a.Label)
	}
	// The legacy singular file must NOT be written by the pool-aware login.
	if _, err := os.Stat(filepath.Join(dir, ".model-proxy", "volcengine_apikey.json")); !os.IsNotExist(err) {
		t.Errorf("legacy singular file should not exist after pool login (err=%v)", err)
	}
}

// TestRunVolcengineLoginWithInput_DedupByAccessKey verifies that re-logging-in
// the SAME AccessKey with --replace keeps the pool at size 1 and overwrites the
// api_key + secret_key in place (idempotent replace, not append).
func TestRunVolcengineLoginWithInput_DedupByAccessKey(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	stubVolcengineValidator(t)
	cfg := &configdomain.Config{Listen: "127.0.0.1:1", Providers: map[string]configdomain.Provider{"volcengine": {Provider: "volcengine"}}}
	prov := cfg.Providers["volcengine"]
	// Seed an account with AccessKey=AK9.
	if err := RunVolcengineLoginWithInput(cfg, "volcengine", prov,
		"old-ark", "AK9", "old-sk", "first", false); err != nil {
		t.Fatal(err)
	}
	// Replace SAME AccessKey (AK9) with a new api_key + secret_key + label.
	if err := RunVolcengineLoginWithInput(cfg, "volcengine", prov,
		"new-ark", "AK9", "new-sk", "renamed", true /*replace*/); err != nil {
		t.Fatal(err)
	}
	pool, _ := accounts.NewStore(accounts.HomeDir()).Load("volcengine", "volcengine")
	if len(pool.Accounts) != 1 {
		t.Fatalf("replace same AccessKey should keep size 1, got %d: %+v", len(pool.Accounts), pool.Accounts)
	}
	a := pool.Accounts[0]
	if a.APIKey != "new-ark" || a.SecretKey != "new-sk" || a.Label != "renamed" {
		t.Errorf("replace did not update fields: got %+v", a)
	}
	if a.AccessKey != "AK9" {
		t.Errorf("AccessKey changed on replace: got %q want AK9", a.AccessKey)
	}
}

// TestRunVolcengineLoginWithInput_DedupNoReplace_Aborts: same AccessKey, no
// --replace, stdin says "n" → "login cancelled", pool untouched.
func TestRunVolcengineLoginWithInput_DedupNoReplace_Aborts(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	stubVolcengineValidator(t)
	cfg := &configdomain.Config{Listen: "127.0.0.1:1", Providers: map[string]configdomain.Provider{"volcengine": {Provider: "volcengine"}}}
	prov := cfg.Providers["volcengine"]
	if err := RunVolcengineLoginWithInput(cfg, "volcengine", prov,
		"old-ark", "AK9", "old-sk", "first", false); err != nil {
		t.Fatal(err)
	}
	// Redirect stdin to answer "n".
	orig := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	defer func() { os.Stdin = orig }()
	w.Write([]byte("n\n"))
	w.Close()

	err := RunVolcengineLoginWithInput(cfg, "volcengine", prov,
		"new-ark", "AK9", "new-sk", "", false /*replace*/)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("expected 'cancelled' error, got %v", err)
	}
	// Pool unchanged: old key intact.
	pool, _ := accounts.NewStore(accounts.HomeDir()).Load("volcengine", "volcengine")
	if len(pool.Accounts) != 1 || pool.Accounts[0].APIKey != "old-ark" {
		t.Fatalf("abort should leave pool untouched: %+v", pool.Accounts)
	}
}

// TestRunVolcengineLoginWithInput_DifferentAccessKeyAppends: a NEW AccessKey
// appends a fresh entry to the pool.
func TestRunVolcengineLoginWithInput_DifferentAccessKeyAppends(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	stubVolcengineValidator(t)
	cfg := &configdomain.Config{Listen: "127.0.0.1:1", Providers: map[string]configdomain.Provider{"volcengine": {Provider: "volcengine"}}}
	prov := cfg.Providers["volcengine"]
	if err := RunVolcengineLoginWithInput(cfg, "volcengine", prov,
		"ark-1", "AK1", "sk-1", "a", false); err != nil {
		t.Fatal(err)
	}
	if err := RunVolcengineLoginWithInput(cfg, "volcengine", prov,
		"ark-2", "AK2", "sk-2", "b", false); err != nil {
		t.Fatal(err)
	}
	pool, _ := accounts.NewStore(accounts.HomeDir()).Load("volcengine", "volcengine")
	if len(pool.Accounts) != 2 {
		t.Fatalf("different AccessKey should append, got %d: %+v", len(pool.Accounts), pool.Accounts)
	}
	// Both AccessKeys present.
	aks := map[string]bool{}
	for _, a := range pool.Accounts {
		aks[a.AccessKey] = true
	}
	if !aks["AK1"] || !aks["AK2"] {
		t.Fatalf("missing AccessKeys in pool: %+v", pool.Accounts)
	}
}

// --- flag parsing helpers ---

func TestFlagStringValue(t *testing.T) {
	cases := []struct {
		args []string
		flag string
		want string
	}{
		{[]string{"--label", "team"}, "--label", "team"},
		{[]string{"--label=team"}, "--label", "team"},
		{[]string{"login", "zhipu", "--label", "team"}, "--label", "team"},
		{[]string{"zhipu", "--label=team"}, "--label", "team"},
		{[]string{"zhipu"}, "--label", ""},
		{[]string{"zhipu", "--label"}, "--label", ""}, // no value after flag
		{[]string{}, "--label", ""},
	}
	for i, c := range cases {
		got := cliframework.FlagStringValue(c.args, c.flag)
		if got != c.want {
			t.Errorf("case %d: cliframework.FlagStringValue(%v, %q) = %q, want %q", i, c.args, c.flag, got, c.want)
		}
	}
}

func TestHasFlagValue(t *testing.T) {
	if !cliframework.HasFlagValue([]string{"--replace", "zhipu"}, "--replace") {
		t.Error("--replace present should be true")
	}
	if cliframework.HasFlagValue([]string{"zhipu"}, "--replace") {
		t.Error("missing --replace should be false")
	}
	// --replace=anything still counts as present
	if !cliframework.HasFlagValue([]string{"--replace=true"}, "--replace") {
		t.Error("--replace=true should be true")
	}
}

// --- maybeReloadDaemon is a no-op when no daemon/pid file exists ---

// TestMaybeReloadDaemon_NoOpWithoutPidFile pins the contract that
// maybeReloadDaemon returns silently (no error, no fatal) when no pid file
// exists — the foreground/test case. We point --log-file at an empty temp dir
// so resolveLogFile lands in a path with no pid file.
func TestMaybeReloadDaemon_NoOpWithoutPidFile(t *testing.T) {
	setPoolHome(t, t.TempDir())
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("maybeReloadDaemon panicked: %v", r)
		}
	}()
	cliserve.MaybeReloadDaemon([]string{"--log-file", filepath.Join(t.TempDir(), "none.log")}, nil)
}

// --- aqp/codex CLI login path-key regression ---
//
// Bug: cmdCodexLogin/runLogin previously used the provider_id (aqp/codex), so
// a renamed instance wrote a credential file that the forward path never read.
// Both interactive flows now share this behavior-tested path resolver.
func readLoginRepositoryFile(t *testing.T, relativePath string) []byte {
	t.Helper()
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate login_cmd_test.go")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(testFile), "..", "..", ".."))
	path := filepath.Join(repoRoot, filepath.FromSlash(relativePath))
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read repository file %s: %v", relativePath, err)
	}
	return src
}

func TestAqpCodexLogin_UsesConfigNameForAuthFile(t *testing.T) {
	home := filepath.Join("test-home", "user")
	for _, providerName := range []string{"aqp-work", "codex-work"} {
		want := filepath.Join(home, ".model-proxy", providerName+"_oauth_auth.json")
		if got := oauthAuthFilePath(home, providerName); got != want {
			t.Errorf("oauthAuthFilePath(%q, %q) = %q, want %q", home, providerName, got, want)
		}
	}
	assertLoginUsesOAuthAuthFilePath(t, "internal/cli/login/login.go", "RunLogin", "storePath")
	assertCodexLoginFlowUsesOAuthAuthFilePath(t, "internal/cli/login/login.go", "RunProviderLogin")
}

// assertCodexLoginFlowUsesOAuthAuthFilePath re-anchors the codex half of the
// wiring guard on the production dispatch: the runCodexLoginFlow call inside
// functionName must pass exactly oauthAuthFilePath(HomeDir(), provName) as its
// auth file (a renamed instance keeps writing <provName>_oauth_auth.json),
// never a directly constructed path.
func assertCodexLoginFlowUsesOAuthAuthFilePath(t *testing.T, relativePath, functionName string) {
	t.Helper()
	src := readLoginRepositoryFile(t, relativePath)
	file, err := parser.ParseFile(token.NewFileSet(), relativePath, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", relativePath, err)
	}
	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == functionName {
			body = fn.Body
			break
		}
	}
	if body == nil {
		t.Fatalf("%s: function %s not found", relativePath, functionName)
	}
	flowCalls := 0
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok || ident.Name != "runCodexLoginFlow" {
			return true
		}
		flowCalls++
		if len(call.Args) != 1 || !isExactOAuthAuthFilePathCall(call.Args[0]) {
			t.Errorf("%s %s: runCodexLoginFlow auth file must be oauthAuthFilePath(HomeDir(), provName)", relativePath, functionName)
		}
		return true
	})
	if flowCalls != 1 {
		t.Errorf("%s %s: runCodexLoginFlow calls = %d, want exactly 1", relativePath, functionName, flowCalls)
	}
}

// assertLoginUsesOAuthAuthFilePath is a structural wiring guard around the two
// interactive flows that are impractical to run as unit tests. It scopes the
// assertion to one function AST, requires the credential variable's sole
// assignment to use oauthAuthFilePath(HomeDir(), provName), and rejects the old
// direct path constructors. Comments and dead strings cannot satisfy it.
func assertLoginUsesOAuthAuthFilePath(t *testing.T, relativePath, functionName, variableName string) {
	t.Helper()
	src := readLoginRepositoryFile(t, relativePath)
	file, err := parser.ParseFile(token.NewFileSet(), relativePath, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", relativePath, err)
	}
	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == functionName {
			body = fn.Body
			break
		}
	}
	if body == nil {
		t.Fatalf("%s: function %s not found", relativePath, functionName)
	}

	helperCalls := 0
	variableAssignments := 0
	directAuthPath := false
	ast.Inspect(body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				if ident, ok := lhs.(*ast.Ident); ok && ident.Name == variableName {
					variableAssignments++
					if len(n.Rhs) != 1 || !isExactOAuthAuthFilePathCall(n.Rhs[0]) {
						t.Errorf("%s %s: %s assignment must be oauthAuthFilePath(HomeDir(), provName)", relativePath, functionName, variableName)
					}
				}
			}
		case *ast.CallExpr:
			if ident, ok := n.Fun.(*ast.Ident); ok && ident.Name == "oauthAuthFilePath" {
				helperCalls++
			}
			if selector, ok := n.Fun.(*ast.SelectorExpr); ok {
				pkg, _ := selector.X.(*ast.Ident)
				if pkg != nil && pkg.Name == "filepath" && selector.Sel.Name == "Join" && containsOAuthAuthSuffix(n) {
					directAuthPath = true
				}
				if selector.Sel.Name == "AuthFilePath" {
					directAuthPath = true
				}
			}
		}
		return true
	})
	if helperCalls != 1 {
		t.Errorf("%s %s: oauthAuthFilePath calls = %d, want exactly 1", relativePath, functionName, helperCalls)
	}
	if variableAssignments != 1 {
		t.Errorf("%s %s: %s assignments = %d, want exactly 1", relativePath, functionName, variableName, variableAssignments)
	}
	if directAuthPath {
		t.Errorf("%s %s: constructs an OAuth auth path directly instead of using oauthAuthFilePath", relativePath, functionName)
	}
}

func isExactOAuthAuthFilePathCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 2 {
		return false
	}
	fn, ok := call.Fun.(*ast.Ident)
	if !ok || fn.Name != "oauthAuthFilePath" {
		return false
	}
	homeCall, ok := call.Args[0].(*ast.CallExpr)
	if !ok || len(homeCall.Args) != 0 {
		return false
	}
	homeFn, ok := homeCall.Fun.(*ast.Ident)
	providerName, providerOK := call.Args[1].(*ast.Ident)
	return ok && homeFn.Name == "HomeDir" && providerOK && providerName.Name == "provName"
}

func containsOAuthAuthSuffix(call *ast.CallExpr) bool {
	found := false
	for _, arg := range call.Args {
		ast.Inspect(arg, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if ok && strings.Contains(literal.Value, "oauth_auth") {
				found = true
			}
			return !found
		})
	}
	return found
}

// TestRunApiKeyLogin_KimiCode_Validation401 pins that kimi-code login now
// validates the key (it routes through the generic addApikeyAccount, which gates
// on usage_url). A 401 from the usage endpoint rejects before the pool is written.
func TestRunApiKeyLogin_KimiCode_Validation401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`unauthorized`))
	}))
	defer srv.Close()

	t.Setenv("HOME", t.TempDir())
	orig := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	defer func() { os.Stdin = orig }()
	w.Write([]byte("bad-key\n"))
	w.Close()

	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{"kimi-code": {Provider: "kimi-code", UsageURL: srv.URL}}}
	err := RunApiKeyLoginWithInput(cfg, "kimi-code", cfg.Providers["kimi-code"], "", "", false)
	if err == nil || !strings.Contains(err.Error(), "validation failed") {
		t.Errorf("kimi-code 401: err=%v want 'validation failed'", err)
	}
	pool, _ := accounts.NewStore(accounts.HomeDir()).Load("kimi-code", "kimi-code")
	if len(pool.Accounts) != 0 {
		t.Errorf("kimi-code 401 should not save: %+v", pool.Accounts)
	}
}

// TestDefaultsHaveValidationURLs parses both user-facing default configs and
// asserts the URLs on their exact provider entries. A raw substring check can
// pass when a URL survives only in a comment or is attached to the wrong key.
func TestDefaultsHaveValidationURLs(t *testing.T) {
	sources := []struct {
		name string
		yaml []byte
	}{
		{name: "config.yaml", yaml: readLoginRepositoryFile(t, "config.yaml")},
		{name: "config init template", yaml: []byte(configdomain.DefaultConfigYAML)},
	}
	wants := map[string]string{
		"kimi-code":  "https://api.kimi.com/coding/v1/usages",
		"volcengine": "https://ark.cn-beijing.volces.com/api/plan/v3/models",
	}
	for _, source := range sources {
		cfg, err := configdomain.LoadConfigFromBytes(source.name, source.yaml)
		if err != nil {
			t.Fatalf("parse %s: %v", source.name, err)
		}
		for providerName, wantURL := range wants {
			prov, ok := cfg.Providers[providerName]
			if !ok {
				t.Errorf("%s: provider %q missing", source.name, providerName)
				continue
			}
			if prov.UsageURL != wantURL {
				t.Errorf("%s: provider %q usage_url = %q, want %q", source.name, providerName, prov.UsageURL, wantURL)
			}
		}
	}
}
