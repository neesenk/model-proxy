package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/internal/cli/clitest"
	"model-proxy/internal/config"
	"model-proxy/internal/providerstore"
)

// newProviderFixture writes a minimal valid config (one provider, one routed
// model) and returns its path. Tests pass their own HOME via RunCLIWithHome
// so the provider.json store stays hermetic.
func newProviderFixture(t *testing.T) string {
	t.Helper()
	return clitest.WriteTempConfig(t, `listen: 127.0.0.1:15721
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://z.example.com/v4, models: [glm-5.2, glm-5.3]}
routes:
  glm-5.2: [{provider: zhipu, model: glm-5.2}]
`)
}

func storeFor(t *testing.T, home string) map[string]config.Provider {
	t.Helper()
	store, err := providerstore.Load(providerstore.PathUnder(home))
	if err != nil {
		t.Fatalf("load store: %v", err)
	}
	return store
}

func TestProviderAddRmSetLifecycle(t *testing.T) {
	cfgPath := newProviderFixture(t)
	home := t.TempDir()

	// add: custom static endpoint, no credential (non-TTY skips login).
	stdout, _, code := clitest.RunCLIWithHome(t, home, "provider", cfgPath,
		"add", "acme", "--base-url", "https://acme.example.com/v1",
		"--billing", "pay-as-you-go", "--priority", "3", "--model", "acme-1", "--model", "acme-2")
	if code != 0 {
		t.Fatalf("add exit=%d stdout=%s", code, stdout)
	}
	store := storeFor(t, home)
	entry, ok := store["acme"]
	if !ok || entry.Provider != "static" || entry.OpenAIBaseURL != "https://acme.example.com/v1" {
		t.Fatalf("store after add = %+v", store)
	}
	if entry.Billing != "pay-as-you-go" || entry.Priority != 3 || len(entry.Models) != 2 {
		t.Fatalf("entry fields = %+v", entry)
	}
	// The yaml fixture's zhipu block was absorbed and stripped in the same
	// mutation.
	body, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(body), "providers:") {
		t.Fatalf("yaml not stripped:\n%s", body)
	}
	if _, ok := storeFor(t, home)["zhipu"]; !ok {
		t.Fatal("yaml fixture provider not absorbed into the store")
	}

	// set: only the given flags change; --model replaces the list.
	_, _, code = clitest.RunCLIWithHome(t, home, "provider", cfgPath,
		"set", "acme", "--base-url", "https://acme2.example.com/v1", "--model", "acme-1", "--model", "acme-3")
	if code != 0 {
		t.Fatalf("set exit=%d", code)
	}
	entry = storeFor(t, home)["acme"]
	if entry.OpenAIBaseURL != "https://acme2.example.com/v1" || len(entry.Models) != 2 || entry.Models[1] != "acme-3" {
		t.Fatalf("entry after set = %+v", entry)
	}
	if entry.Billing != "pay-as-you-go" {
		t.Fatalf("set must not reset untouched fields: %+v", entry)
	}

	// The routed provider cannot be removed while a route references it
	// (checked while acme still exists — removing the LAST provider trips the
	// at-least-one rule instead).
	_, stderr, code := clitest.RunCLIWithHome(t, home, "provider", cfgPath, "rm", "zhipu", "--yes")
	if code == 0 || !strings.Contains(stderr, "not defined under providers:") {
		t.Fatalf("rm routed provider = (%d, %q), want route-referenced validation error", code, stderr)
	}
	// rm: non-interactive needs --yes.
	_, stderr, code = clitest.RunCLIWithHome(t, home, "provider", cfgPath, "rm", "acme")
	if code == 0 || !strings.Contains(stderr, "--yes") {
		t.Fatalf("rm without --yes = (%d, %q), want refusal", code, stderr)
	}
	_, _, code = clitest.RunCLIWithHome(t, home, "provider", cfgPath, "rm", "acme", "--yes")
	if code != 0 {
		t.Fatalf("rm exit=%d", code)
	}
	if _, still := storeFor(t, home)["acme"]; still {
		t.Fatal("provider still in store after rm")
	}
}

func TestProviderAddRefusals(t *testing.T) {
	cfgPath := newProviderFixture(t)
	home := t.TempDir()

	// No endpoint.
	_, stderr, code := clitest.RunCLIWithHome(t, home, "provider", cfgPath, "add", "acme")
	if code == 0 || !strings.Contains(stderr, "--base-url") {
		t.Fatalf("add without base-url = (%d, %q)", code, stderr)
	}
	// Ambiguity gate: glm-5.3 is served by zhipu WITHOUT an explicit route
	// (the fixture only routes glm-5.2), so serving it from a second provider
	// is ambiguous — non-TTY refuses without --yes.
	_, stderr, code = clitest.RunCLIWithHome(t, home, "provider", cfgPath,
		"add", "acme", "--base-url", "https://acme.example.com/v1", "--model", "glm-5.3")
	if code == 0 || !strings.Contains(stderr, "--yes") {
		t.Fatalf("ambiguous add = (%d, %q), want gate refusal", code, stderr)
	}
	// Duplicate name.
	_, _, code = clitest.RunCLIWithHome(t, home, "provider", cfgPath,
		"add", "zhipu", "--base-url", "https://x.example.com/v1")
	if code == 0 {
		t.Fatal("duplicate add must fail")
	}
}

func TestProviderSetRequiresTargetAndFlags(t *testing.T) {
	cfgPath := newProviderFixture(t)
	home := t.TempDir()

	_, stderr, code := clitest.RunCLIWithHome(t, home, "provider", cfgPath, "set", "ghost", "--billing", "plan")
	if code == 0 || !strings.Contains(stderr, "unknown provider") {
		t.Fatalf("set unknown = (%d, %q)", code, stderr)
	}
	_, stderr, code = clitest.RunCLIWithHome(t, home, "provider", cfgPath, "set", "zhipu")
	if code == 0 || !strings.Contains(stderr, "nothing to set") {
		t.Fatalf("set no flags = (%d, %q)", code, stderr)
	}
}

// TestProviderRmKeepsCredentialPool pins the documented semantics: rm is a
// routing removal, the account pool survives (logout owns credentials).
func TestProviderRmKeepsCredentialPool(t *testing.T) {
	cfgPath := newProviderFixture(t)
	home := t.TempDir()
	_, _, code := clitest.RunCLIWithHome(t, home, "provider", cfgPath,
		"add", "acme", "--base-url", "https://acme.example.com/v1", "--no-login")
	if code != 0 {
		t.Fatalf("add exit=%d", code)
	}
	if _, err := os.Stat(filepath.Join(home, ".model-proxy", "provider.json")); err != nil {
		t.Fatalf("store missing: %v", err)
	}
	stdout, _, code := clitest.RunCLIWithHome(t, home, "provider", cfgPath, "rm", "acme", "--yes")
	if code != 0 || !strings.Contains(stdout, "credential pool kept") {
		t.Fatalf("rm = (%d, %q)", code, stdout)
	}
}
