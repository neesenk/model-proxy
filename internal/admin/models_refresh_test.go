package admin

// models_refresh_test.go — behavior contract of POST /api/models/refresh's
// orchestration (the daemon twin of `models refresh <provider>`): fetch +
// probe + config write + reload + verdict-cache replace, plus the CLI's
// safety nets (all-failed probe and missing impl write unvalidated, never
// wipe) and reload-failure backup restore.

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
	runtimewire "model-proxy/internal/runtime/wirecap"
)

// refreshFakeProv is a provider.Provider stub: FetchModels/FilterModelIDs are
// scripted; the probe shape is the OpenAI default (matching baseProbe) so
// probe.ProbeModelProtocols exercises the real request-build path.
type refreshFakeProv struct {
	fetched  []string
	fetchErr error
}

func (f *refreshFakeProv) AuthHeaders(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer k")
	return nil
}
func (f *refreshFakeProv) Refresh() error { return nil }
func (f *refreshFakeProv) RewriteRequest(url string, body []byte, path string) (string, []byte) {
	return url, body
}
func (f *refreshFakeProv) Logout() error                           { return nil }
func (f *refreshFakeProv) Usage() error                            { return nil }
func (f *refreshFakeProv) Quota() (*provider.QuotaSnapshot, error) { return nil, nil }
func (f *refreshFakeProv) FetchModels() ([]string, error)          { return f.fetched, f.fetchErr }
func (f *refreshFakeProv) ExtraHeaders(*http.Request, string)      {}
func (f *refreshFakeProv) FilterModelIDs(ids []string) ([]string, []string) {
	return ids, nil
}
func (f *refreshFakeProv) ProbeRequest(modelID string) provider.ProbeRequest {
	return provider.ProbeRequest{
		Method: http.MethodPost,
		Path:   "/chat/completions",
		Body:   []byte(`{"model":"` + modelID + `","messages":[{"role":"user","content":"hi"}],"max_tokens":1}`),
	}
}

// refreshHarness wires a Service with scripted ports around one provider and
// records the side effects (config writes land on a real file; reload and
// cache-replace are spies).
type refreshHarness struct {
	service  *Service
	reload   *reloadSpy
	mu       sync.Mutex
	replaced map[string]runtimewire.ModelProtocols
	replaceN int
}

func newRefreshHarness(t *testing.T, cfg *configdomain.Config, configFile string, impl provider.Provider) *refreshHarness {
	t.Helper()
	h := &refreshHarness{reload: &reloadSpy{}}
	h.service = New(Ports{
		ConfigFile: func() string { return configFile },
		Config:     func() *configdomain.Config { return cfg },
		ProviderImpl: func(string) provider.Provider {
			return impl
		},
		Reload: h.reload.fn(),
		ModelCapsReplace: func(_ string, models map[string]runtimewire.ModelProtocols) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.replaceN++
			h.replaced = models
		},
	})
	return h
}

func (h *refreshHarness) replacedMatrix() (map[string]runtimewire.ModelProtocols, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.replaced, h.replaceN
}

func writeRefreshConfig(t *testing.T, baseURL, models string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := `listen: 127.0.0.1:8080
providers:
  zp: {provider_id: zhipu, openai_base_url: ` + baseURL + `, models: [` + models + `]}
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func refreshTestConfig(baseURL string, models []string) *configdomain.Config {
	return &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"zp": {Provider: "zhipu", OpenAIBaseURL: baseURL, Models: models},
		},
		Routes: map[string][]configdomain.RouteTarget{},
	}
}

// configModels reads providers.zp.models back from the written config file.
func configModels(t *testing.T, path string) []string {
	t.Helper()
	cfg, err := configdomain.LoadConfig(path)
	if err != nil {
		t.Fatalf("written config does not load: %v", err)
	}
	return cfg.Providers["zp"].Models
}

// bodyRulesUpstream answers probe legs by scanning the request body for model
// ids: ids in ok get 200, everything else 404.
func bodyRulesUpstream(t *testing.T, ok ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		for _, id := range ok {
			if strings.Contains(string(raw), id) {
				w.Write([]byte(`{}`))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"message":"model not supported"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRefreshModelsFetchesProbesWrites: the live list is fetched, every
// candidate is probed, only the callable subset lands in config, the daemon
// reloads, and the verdict cache is replaced with the fresh matrix.
func TestRefreshModelsFetchesProbesWrites(t *testing.T) {
	up := bodyRulesUpstream(t, "glm-new")
	configFile := writeRefreshConfig(t, up.URL, "glm-old")
	cfg := refreshTestConfig(up.URL, []string{"glm-old"})
	impl := &refreshFakeProv{fetched: []string{"glm-old", "glm-new"}}
	h := newRefreshHarness(t, cfg, configFile, impl)

	result, err := h.service.RefreshModels("zp")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Kept, []string{"glm-new"}) {
		t.Errorf("kept = %v, want [glm-new]", result.Kept)
	}
	if !reflect.DeepEqual(result.Added, []string{"glm-new"}) || !reflect.DeepEqual(result.Removed, []string{"glm-old"}) {
		t.Errorf("diff = +%v -%v, want +[glm-new] -[glm-old]", result.Added, result.Removed)
	}
	if len(result.ProbeDropped) != 1 || result.ProbeDropped[0].Model != "glm-old" {
		t.Errorf("probe dropped = %+v, want glm-old with a reason", result.ProbeDropped)
	}
	if !result.ConfigUpdated {
		t.Error("ConfigUpdated = false, want true (model set changed)")
	}
	if got := configModels(t, configFile); !reflect.DeepEqual(got, []string{"glm-new"}) {
		t.Errorf("config models = %v, want [glm-new]", got)
	}
	if len(h.reload.calls) != 1 || h.reload.calls[0] != configFile {
		t.Errorf("reload calls = %v, want exactly one for the config file", h.reload.calls)
	}
	matrix, n := h.replacedMatrix()
	if n != 1 {
		t.Fatalf("ModelCapsReplace calls = %d, want 1", n)
	}
	if mp := matrix["glm-new"]; mp.Chat != runtimewire.Yes {
		t.Errorf("fresh matrix glm-new chat = %s, want yes", mp.Chat)
	}
	if mp := matrix["glm-old"]; mp.Chat != runtimewire.No {
		t.Errorf("fresh matrix glm-old chat = %s, want no (404 probed)", mp.Chat)
	}
}

// TestRefreshModelsNoChangeSkipsWrite: an unchanged callable set leaves the
// config file and the reload untouched, but still refreshes the cache.
func TestRefreshModelsNoChangeSkipsWrite(t *testing.T) {
	up := bodyRulesUpstream(t, "glm")
	configFile := writeRefreshConfig(t, up.URL, "glm")
	cfg := refreshTestConfig(up.URL, []string{"glm"})
	before, _ := os.ReadFile(configFile)
	h := newRefreshHarness(t, cfg, configFile, &refreshFakeProv{fetched: []string{"glm"}})

	result, err := h.service.RefreshModels("zp")
	if err != nil {
		t.Fatal(err)
	}
	if result.ConfigUpdated {
		t.Error("ConfigUpdated = true, want false (no net change)")
	}
	if len(h.reload.calls) != 0 {
		t.Errorf("reload calls = %v, want none", h.reload.calls)
	}
	if after, _ := os.ReadFile(configFile); string(after) != string(before) {
		t.Error("config file rewritten despite an unchanged model set")
	}
	if _, n := h.replacedMatrix(); n != 1 {
		t.Errorf("ModelCapsReplace calls = %d, want 1 (verdicts still refreshed)", n)
	}
}

// TestRefreshModelsAllProbeFailedKeepsUnvalidated: an all-failed probe (5xx
// on every leg) must NOT wipe models: — the merged list is written
// unvalidated with a warning, and the failed matrix never reaches the cache.
func TestRefreshModelsAllProbeFailedKeepsUnvalidated(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(up.Close)
	configFile := writeRefreshConfig(t, up.URL, "glm-old")
	cfg := refreshTestConfig(up.URL, []string{"glm-old"})
	h := newRefreshHarness(t, cfg, configFile, &refreshFakeProv{fetched: []string{"glm-old", "glm-new"}})

	result, err := h.service.RefreshModels("zp")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Kept, []string{"glm-new", "glm-old"}) {
		t.Errorf("kept = %v, want the unvalidated merged set", result.Kept)
	}
	if !strings.Contains(result.Warning, "unvalidated") {
		t.Errorf("warning = %q, want an unvalidated-write warning", result.Warning)
	}
	if got := configModels(t, configFile); !reflect.DeepEqual(got, []string{"glm-new", "glm-old"}) {
		t.Errorf("config models = %v, want the merged set (never wiped)", got)
	}
	if _, n := h.replacedMatrix(); n != 0 {
		t.Errorf("ModelCapsReplace calls = %d, want 0 (failed matrix must not reach the cache)", n)
	}
}

// TestRefreshModelsImplUnavailable: a provider with no built impl (not
// logged in) cannot fetch or probe — refresh keeps the config ∪ route-target
// set unvalidated instead of failing or emptying the list.
func TestRefreshModelsImplUnavailable(t *testing.T) {
	configFile := writeRefreshConfig(t, "http://127.0.0.1:1", "glm")
	cfg := refreshTestConfig("http://127.0.0.1:1", []string{"glm"})
	cfg.Routes = map[string][]configdomain.RouteTarget{
		"glm2": {{Provider: "zp", Model: "glm2"}},
	}
	h := newRefreshHarness(t, cfg, configFile, nil)

	result, err := h.service.RefreshModels("zp")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Kept, []string{"glm", "glm2"}) {
		t.Errorf("kept = %v, want config ∪ route targets unvalidated", result.Kept)
	}
	if !strings.Contains(result.Warning, "unvalidated") {
		t.Errorf("warning = %q, want a not-available warning", result.Warning)
	}
	if got := configModels(t, configFile); !reflect.DeepEqual(got, []string{"glm", "glm2"}) {
		t.Errorf("config models = %v, want [glm glm2]", got)
	}
	if _, n := h.replacedMatrix(); n != 0 {
		t.Errorf("ModelCapsReplace calls = %d, want 0 (nothing probed)", n)
	}
}

// TestRefreshModelsUnknownProvider: an unconfigured provider is a 404, no
// side effects.
func TestRefreshModelsUnknownProvider(t *testing.T) {
	h := newRefreshHarness(t, refreshTestConfig("http://x", nil), "", nil)
	_, err := h.service.RefreshModels("ghost")
	if err == nil {
		t.Fatal("unknown provider returned nil error")
	}
	if status := httpErrorStatus(t, err); status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

// TestRefreshModelsReloadFailureRestoresConfig: when the hot reload fails to
// apply, the pre-write config is restored from the backup and the error
// surfaces — the saveAndReloadUnderLock contract.
func TestRefreshModelsReloadFailureRestoresConfig(t *testing.T) {
	up := bodyRulesUpstream(t, "glm-new")
	configFile := writeRefreshConfig(t, up.URL, "glm-old")
	cfg := refreshTestConfig(up.URL, []string{"glm-old"})
	h := newRefreshHarness(t, cfg, configFile, &refreshFakeProv{fetched: []string{"glm-new"}})
	h.reload.err = errors.New("boom")

	if _, err := h.service.RefreshModels("zp"); err == nil {
		t.Fatal("reload failure returned nil error")
	}
	if got := configModels(t, configFile); !reflect.DeepEqual(got, []string{"glm-old"}) {
		t.Errorf("config models after failed reload = %v, want restored [glm-old]", got)
	}
}
