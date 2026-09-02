package account

import (
	"errors"
	"github.com/zalando/go-keyring"
	"io"
	"model-proxy/internal/accounts"
	"model-proxy/internal/cli/clitest"
	"os"
	"strings"
	"testing"
)

// Test: `logout zhipu` interactive removes exactly the selected account.
// savePool sorts accounts by ID, so we compute which account is at index 0
// before the test, select it, and assert it's the one removed.
func TestCLI_LogoutInteractiveRemovesAccount(t *testing.T) {
	dir := t.TempDir()
	clitest.SetPoolHome(t, dir)
	clitest.WritePoolFile(t, "zhipu", "zhipu", "K1", "K2")
	cfgPath := clitest.WriteZhipuPoolConfig(t, "https://zhipu.invalid/u")

	pool, err := accounts.NewStore(accounts.HomeDir()).Load("zhipu", "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 2 {
		t.Fatalf("setup: want 2 accounts, got %d", len(pool.Accounts))
	}
	firstID := pool.Accounts[0].ID
	secondID := pool.Accounts[1].ID

	clitest.SetStdin(t, "1\n") // pick the first account (1-based)
	RunLogout([]string{"zhipu", "--config", cfgPath})

	pool2, _ := accounts.NewStore(accounts.HomeDir()).Load("zhipu", "zhipu")
	if len(pool2.Accounts) != 1 {
		t.Fatalf("after logout want 1 account, got %d: %+v", len(pool2.Accounts), pool2.Accounts)
	}
	if pool2.Accounts[0].ID != secondID {
		t.Errorf("remaining account id = %q, want %q (the unselected one)", pool2.Accounts[0].ID, secondID)
	}
	if pool2.Accounts[0].ID == firstID {
		t.Errorf("selected account %q was not removed", firstID)
	}
}

// Test: `logout zhipu --all` clears the entire pool and removes the plural
// pool file. The singular file is untouched (it never existed in pool mode).
func TestCLI_LogoutAllClearsPool(t *testing.T) {
	dir := t.TempDir()
	clitest.SetPoolHome(t, dir)
	clitest.WritePoolFile(t, "zhipu", "zhipu", "K1", "K2")
	// A stale legacy singular file from before pooling: it must NOT come back
	// to life after `logout --all` removes the pool accounts.
	if err := os.WriteFile(accounts.NewStore(accounts.HomeDir()).LegacyPath("zhipu"), []byte(`{"api_key":"ancient"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := clitest.WriteZhipuPoolConfig(t, "https://zhipu.invalid/u")

	RunLogout([]string{"zhipu", "--all", "--config", cfgPath})

	// The empty plural file stays as the authoritative credential tombstone
	// (provider-pools.md) — removing it would re-open the legacy fallback and
	// resurrect the old key.
	pool, err := accounts.NewStore(accounts.HomeDir()).Load("zhipu", "zhipu")
	if err != nil {
		t.Fatalf("tombstone pool unreadable: %v", err)
	}
	if len(pool.Accounts) != 0 {
		t.Errorf("pool accounts = %d, want 0", len(pool.Accounts))
	}
	snapshot, err := accounts.NewStore(accounts.HomeDir()).LoadSnapshot("zhipu", "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Source != accounts.SourcePlural {
		t.Errorf("source = %v, want SourcePlural (tombstone must override legacy)", snapshot.Source)
	}
}

// Test: `logout zhipu --all` on an EMPTY pool stays idempotent even when the
// keychain backend is unreachable. The empty pool already proves the user is
// logged out; RemoveAllAccounts only cleans stale restore provenance there,
// so its failure must surface as a warning, not a hard exit. The go-keyring
// mock is driven to always fail — no real keychain I/O. Not parallel: the
// mock is process-global.
func TestCLI_LogoutAllEmptyPoolCleanupFailureWarnsOnly(t *testing.T) {
	dir := t.TempDir()
	clitest.SetPoolHome(t, dir)
	// Empty plural pool (the tombstone) plus a stale keychain-restore marker
	// left by an older writer: file-mode RemoveAllAccounts follows that
	// provenance into keychain deletion.
	if err := accounts.NewStore(accounts.HomeDir()).Save("zhipu", "zhipu", accounts.Pool{Version: 1}); err != nil {
		t.Fatal(err)
	}
	markerPath := accounts.NewStore(accounts.HomeDir()).PoolPath("zhipu") + ".keychain-origin"
	if err := os.WriteFile(markerPath, []byte(`{"version":1,"account_ids":["stale-restore-id"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := clitest.WriteZhipuPoolConfig(t, "https://zhipu.invalid/u")

	keyring.MockInitWithError(errors.New("secret service unreachable"))
	defer keyring.MockInit() // restore a working mock for later tests

	// Capture both streams: the success message goes to stdout, the cleanup
	// warning to stderr. RunLogout must NOT os.Exit on this path.
	var stderr string
	stdout := clitest.GrabStdout(t, func() {
		stderr = captureStderr(t, func() {
			RunLogout([]string{"zhipu", "--all", "--config", cfgPath})
		})
	})

	if !strings.Contains(stdout, "Not logged in") {
		t.Errorf("stdout missing 'Not logged in':\n%s", stdout)
	}
	if !strings.Contains(stderr, "warning: stale credential cleanup failed (already logged out)") {
		t.Errorf("stderr missing cleanup warning:\n%s", stderr)
	}
	if !strings.Contains(stderr, "credential store unavailable") {
		t.Errorf("stderr warning missing the underlying keychain failure:\n%s", stderr)
	}
	// The failed cleanup leaves the provenance intact for a later retry.
	if _, err := os.Stat(markerPath); err != nil {
		t.Errorf("restore marker disappeared despite failed cleanup: %v", err)
	}
}

// Test: `logout zhipu --label K1` removes only K1, leaves K2.
func TestCLI_LogoutByLabel(t *testing.T) {
	dir := t.TempDir()
	clitest.SetPoolHome(t, dir)
	clitest.WritePoolFile(t, "zhipu", "zhipu", "K1", "K2")
	cfgPath := clitest.WriteZhipuPoolConfig(t, "https://zhipu.invalid/u")

	RunLogout([]string{"zhipu", "--label", "K1", "--config", cfgPath})

	pool, err := accounts.NewStore(accounts.HomeDir()).Load("zhipu", "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 1 {
		t.Fatalf("want 1 account after --label logout, got %d: %+v", len(pool.Accounts), pool.Accounts)
	}
	if pool.Accounts[0].APIKey != "K2" {
		t.Errorf("K1 should be removed; remaining key = %q want K2", pool.Accounts[0].APIKey)
	}
}

// Test: `logout zhipu --label nope` exits non-zero with "no account labeled"
// (subprocess — RunLogout calls log.Fatalf on missing label).
func TestCLI_LogoutLabelNotFoundExits(t *testing.T) {
	home := t.TempDir()
	clitest.SetPoolHome(t, home)
	clitest.WritePoolFile(t, "zhipu", "zhipu", "K1")
	cfgPath := clitest.WriteZhipuPoolConfig(t, "https://zhipu.invalid/u")

	_, stderr, code := clitest.RunCLIWithHome(t, home, "logout", cfgPath, "zhipu", "--label", "nope")
	if code == 0 {
		t.Error("logout --label nope: exit=0 want non-zero")
	}
	if !strings.Contains(stderr, "no account labeled") {
		t.Errorf("stderr missing 'no account labeled':\n%s", stderr)
	}
}

// Test: `logout zhipu` interactive with an out-of-range number exits non-zero
// with "invalid selection" (subprocess with stdin piped).
func TestCLI_LogoutInvalidSelectionExits(t *testing.T) {
	home := t.TempDir()
	clitest.SetPoolHome(t, home)
	clitest.WritePoolFile(t, "zhipu", "zhipu", "K1", "K2")
	cfgPath := clitest.WriteZhipuPoolConfig(t, "https://zhipu.invalid/u")

	_, stderr, code := clitest.RunCLIWithStdin(t, "99\n", home, "logout", cfgPath, "zhipu")
	if code == 0 {
		t.Error("logout invalid selection: exit=0 want non-zero")
	}
	if !strings.Contains(stderr, "invalid selection") {
		t.Errorf("stderr missing 'invalid selection':\n%s", stderr)
	}
}

// captureStderr captures everything written to os.Stderr during fn, mirroring
// grabStdout, and returns the captured text.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	var buf strings.Builder
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()
	fn()
	os.Stderr = orig
	_ = w.Close()
	<-done
	return buf.String()
}
