package admin

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"model-proxy/internal/appapi"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/providerstore"
)

// readProviderStore loads the service-under-test's provider.json store (the
// HomeDir the test injected) for assertions on provider mutations.
func readProviderStore(t *testing.T, service *Service) map[string]configdomain.Provider {
	t.Helper()
	store, err := readStoreForHome(t, service)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func readStoreForHome(t *testing.T, service *Service) (map[string]configdomain.Provider, error) {
	t.Helper()
	return providerstore.Load(providerstore.PathUnder(service.homeDir()))
}

// readTestFile returns a config file's bytes as a string for stability
// comparisons across mutations.
func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// setTestHome points accounts/credstore home lookups at a temp dir.
func setTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".model-proxy"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	return home
}

// writeTestConfig seeds a valid config file the edit/save flows can mutate.
func writeTestConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`listen: 127.0.0.1:8080
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://example.test, models: [glm]}
routes:
  glm: [{provider: zhipu, model: glm, priority: 1}]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// reloadSpy records Reload calls and replays a configured error.
type reloadSpy struct {
	calls []string
	err   error
}

func (spy *reloadSpy) fn() func(string) error {
	return func(configFile string) error {
		spy.calls = append(spy.calls, configFile)
		return spy.err
	}
}

func httpErrorStatus(t *testing.T, err error) int {
	t.Helper()
	var httpErr *appapi.HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error %v (%T) is not an appapi.HTTPError", err, err)
	}
	return httpErr.Status
}
