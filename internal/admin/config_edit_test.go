package admin

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"model-proxy/internal/appapi"
	configdomain "model-proxy/internal/config"
)

func TestSaveConfigWritesAndReloads(t *testing.T) {
	path := writeTestConfig(t)
	spy := &reloadSpy{}
	service := New(Ports{
		ConfigFile: func() string { return path },
		Reload:     spy.fn(),
	})
	next := []byte("listen: 127.0.0.1:9090\nproviders:\n  zhipu: {provider_id: zhipu, openai_base_url: https://example.test}\n")
	if err := service.SaveConfig(next); err != nil {
		t.Fatal(err)
	}
	if len(spy.calls) != 1 || spy.calls[0] != path {
		t.Errorf("reload calls = %v", spy.calls)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(next) {
		t.Errorf("saved config = %q, %v", data, err)
	}
}

func TestSaveConfigInvalidSkipsReloadAndWrite(t *testing.T) {
	path := writeTestConfig(t)
	before, _ := os.ReadFile(path)
	spy := &reloadSpy{}
	service := New(Ports{
		ConfigFile: func() string { return path },
		Reload:     spy.fn(),
	})
	if err := service.SaveConfig([]byte("providers: [")); err == nil {
		t.Fatal("invalid config must error")
	}
	if len(spy.calls) != 0 {
		t.Errorf("invalid save reloaded %d time(s)", len(spy.calls))
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Error("invalid save changed the config file")
	}
}

func TestSaveConfigReloadFailureRestoresBackup(t *testing.T) {
	path := writeTestConfig(t)
	before, _ := os.ReadFile(path)
	spy := &reloadSpy{err: errors.New("build failed")}
	service := New(Ports{
		ConfigFile: func() string { return path },
		Reload:     spy.fn(),
	})
	next := []byte("listen: 127.0.0.1:9090\nproviders:\n  zhipu: {provider_id: zhipu, openai_base_url: https://example.test}\n")
	if err := service.SaveConfig(next); err == nil || err.Error() != "build failed" {
		t.Fatalf("SaveConfig err = %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Error("failed reload must restore the previous config file")
	}
}

func TestSaveConfigAppliedWarningKeepsNewFile(t *testing.T) {
	path := writeTestConfig(t)
	spy := &reloadSpy{err: &ReloadAppliedWarning{Err: errors.New("quota persist failed")}}
	service := New(Ports{
		ConfigFile: func() string { return path },
		Reload:     spy.fn(),
	})
	next := []byte("listen: 127.0.0.1:9090\nproviders:\n  zhipu: {provider_id: zhipu, openai_base_url: https://example.test}\n")
	err := service.SaveConfig(next)
	if err == nil || err.Error() != "reload applied with warning: quota persist failed" {
		t.Fatalf("SaveConfig err = %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(next) {
		t.Error("applied-with-warning reload must keep the new config file")
	}
}

func editService(t *testing.T, path string, spy *reloadSpy) *Service {
	t.Helper()
	// HomeDir keeps the provider.json store hermetic — provider edits persist
	// there, never against the real ~/.model-proxy. One stable dir per service:
	// t.TempDir() inside the closure would return a NEW dir on every call.
	home := t.TempDir()
	return New(Ports{
		ConfigFile: func() string { return path },
		HomeDir:    func() string { return home },
		Reload:     spy.fn(),
	})
}

func readConfig(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestEditConfigGeneral(t *testing.T) {
	path := writeTestConfig(t)
	spy := &reloadSpy{}
	service := editService(t, path, spy)
	err := service.EditConfig(appapi.EditRequest{
		Kind: "general",
		Data: map[string]any{"listen": "127.0.0.1:18000", "log_level": "debug"},
	})
	if err != nil {
		t.Fatal(err)
	}
	content := readConfig(t, path)
	if !strings.Contains(content, "listen: 127.0.0.1:18000") || !strings.Contains(content, "log_level: debug") {
		t.Errorf("edited config:\n%s", content)
	}
	if len(spy.calls) != 1 {
		t.Errorf("reload calls = %v", spy.calls)
	}
}

func TestEditConfigScheduling(t *testing.T) {
	path := writeTestConfig(t)
	spy := &reloadSpy{}
	service := editService(t, path, spy)
	if err := service.EditConfig(appapi.EditRequest{
		Kind: "scheduling",
		Data: map[string]any{"circuit_threshold": 5, "sticky_dwell": "2m"},
	}); err != nil {
		t.Fatal(err)
	}
	content := readConfig(t, path)
	if !strings.Contains(content, "circuit_threshold: 5") || !strings.Contains(content, "sticky_dwell: 2m") {
		t.Errorf("edited config:\n%s", content)
	}
}

func TestEditConfigProvider(t *testing.T) {
	path := writeTestConfig(t)
	spy := &reloadSpy{}
	service := editService(t, path, spy)
	if err := service.EditConfig(appapi.EditRequest{
		Kind: "provider",
		Name: "zhipu",
		Data: map[string]any{
			"billing": "plan",
			"models":  []any{"glm", "glm-air"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Provider edits land in the provider.json store (the yaml fixture's
	// providers: block is absorbed and stripped by the same mutation).
	store := readProviderStore(t, service)
	zhipu, ok := store["zhipu"]
	if !ok || zhipu.Billing != "plan" || len(zhipu.Models) != 2 || zhipu.Models[1] != "glm-air" {
		t.Errorf("store after edit = %+v", store)
	}
	if content := readConfig(t, path); strings.Contains(content, "providers:") {
		t.Errorf("yaml must have its providers: section stripped after absorption:\n%s", content)
	}
}

// TestEditConfigProviderAddDefaultsToStatic: the structured editor's form no
// longer submits provider_id — a NEW provider defaults to the static
// implementation, and an edit that omits the key keeps the existing one's
// implementation untouched.
func TestEditConfigProviderAddDefaultsToStatic(t *testing.T) {
	path := writeTestConfig(t)
	spy := &reloadSpy{}
	service := editService(t, path, spy)
	if err := service.EditConfig(appapi.EditRequest{
		Kind: "provider",
		Name: "gateway2",
		Data: map[string]any{"openai_base_url": "https://gw2.example.com/v1", "models": []any{"m1"}},
	}); err != nil {
		t.Fatal(err)
	}
	store := readProviderStore(t, service)
	if got := store["gateway2"].Provider; got != "static" {
		t.Fatalf("new provider provider_id = %q, want the static default", got)
	}
	if err := service.EditConfig(appapi.EditRequest{
		Kind: "provider",
		Name: "zhipu",
		Data: map[string]any{"billing": "pay-as-you-go"},
	}); err != nil {
		t.Fatal(err)
	}
	store = readProviderStore(t, service)
	if got := store["zhipu"].Provider; got != "zhipu" {
		t.Fatalf("existing provider implementation = %q, want unchanged zhipu", got)
	}
	if store["zhipu"].Billing != "pay-as-you-go" {
		t.Fatalf("billing edit lost: %+v", store["zhipu"])
	}
}

// TestEditConfigProviderCatalogAlias pins the Model Matching write path: the
// mapping lands in the provider.json store (validate accepts it because the
// key names a configured model), and sending an empty map clears the override
// instead of leaving an empty mapping behind.
func TestEditConfigProviderCatalogAlias(t *testing.T) {
	path := writeTestConfig(t)
	spy := &reloadSpy{}
	service := editService(t, path, spy)
	if err := service.EditConfig(appapi.EditRequest{
		Kind: "provider",
		Name: "zhipu",
		Data: map[string]any{"catalog_alias": map[string]any{"glm": "glm-4.6"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := readProviderStore(t, service)["zhipu"].CatalogAlias; len(got) != 1 || got["glm"] != "glm-4.6" {
		t.Errorf("catalog_alias write missing: %+v", got)
	}
	// A key that is not in the provider's models list is rejected by validate
	// and must not touch the store.
	before := readProviderStore(t, service)
	beforeYAML := readConfig(t, path)
	err := service.EditConfig(appapi.EditRequest{
		Kind: "provider",
		Name: "zhipu",
		Data: map[string]any{"catalog_alias": map[string]any{"typo-model": "glm-4.6"}},
	})
	if err == nil || !strings.Contains(err.Error(), "not in its models: list") {
		t.Errorf("typo key: want validate error, got %v", err)
	}
	if after := readProviderStore(t, service); !reflect.DeepEqual(after, before) {
		t.Error("rejected catalog_alias edit changed the provider store")
	}
	if readConfig(t, path) != beforeYAML {
		t.Error("rejected catalog_alias edit changed the config file")
	}
	// Empty map deletes the key.
	if err := service.EditConfig(appapi.EditRequest{
		Kind: "provider",
		Name: "zhipu",
		Data: map[string]any{"catalog_alias": map[string]any{}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := readProviderStore(t, service)["zhipu"].CatalogAlias; len(got) != 0 {
		t.Errorf("empty catalog_alias map must clear the override: %+v", got)
	}
}

// TestEditConfigProviderAlias pins the Status→Models provider editor's alias
// write path: the rename mapping lands in the provider.json store (validate
// accepts it because the key names a configured model and the exposed name is
// unique), a typo key is rejected without touching the store, an exposed-name
// collision inside one provider is rejected, and an empty map clears the
// override.
func TestEditConfigProviderAlias(t *testing.T) {
	path := writeTestConfig(t)
	spy := &reloadSpy{}
	service := editService(t, path, spy)
	if err := service.EditConfig(appapi.EditRequest{
		Kind: "provider",
		Name: "zhipu",
		Data: map[string]any{
			"models": []any{"glm", "glm-air"},
			"alias":  map[string]any{"glm": "glm-4.6"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if got := readProviderStore(t, service)["zhipu"].Alias; len(got) != 1 || got["glm"] != "glm-4.6" {
		t.Fatalf("alias write missing: %+v", got)
	}
	// A key that is not in the provider's models list is rejected by validate
	// and must not touch the store.
	before := readProviderStore(t, service)
	beforeYAML := readConfig(t, path)
	err := service.EditConfig(appapi.EditRequest{
		Kind: "provider",
		Name: "zhipu",
		Data: map[string]any{"alias": map[string]any{"typo-model": "glm-4.6"}},
	})
	if err == nil || !strings.Contains(err.Error(), "not in its models: list") {
		t.Errorf("typo key: want validate error, got %v", err)
	}
	if after := readProviderStore(t, service); !reflect.DeepEqual(after, before) {
		t.Error("rejected alias edit changed the provider store")
	}
	if readConfig(t, path) != beforeYAML {
		t.Error("rejected alias edit changed the config file")
	}
	// An exposed name that collides with a sibling model's native name is
	// rejected: one exposed name must map to one upstream model.
	err = service.EditConfig(appapi.EditRequest{
		Kind: "provider",
		Name: "zhipu",
		Data: map[string]any{
			"models": []any{"glm", "glm-air"},
			"alias":  map[string]any{"glm": "glm-air"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "one exposed name must map to one upstream model") {
		t.Errorf("exposed collision: want validate error, got %v", err)
	}
	if after := readProviderStore(t, service); !reflect.DeepEqual(after, before) {
		t.Error("rejected collision edit changed the provider store")
	}
	// Empty map deletes the key.
	if err := service.EditConfig(appapi.EditRequest{
		Kind: "provider",
		Name: "zhipu",
		Data: map[string]any{"models": []any{"glm", "glm-air"}, "alias": map[string]any{}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := readProviderStore(t, service)["zhipu"].Alias; len(got) != 0 {
		t.Errorf("empty alias map must clear the override: %+v", got)
	}
}

func TestEditConfigRoute(t *testing.T) {
	path := writeTestConfig(t)
	spy := &reloadSpy{}
	service := editService(t, path, spy)
	if err := service.EditConfig(appapi.EditRequest{
		Kind: "route",
		Name: "glm-air",
		Data: map[string]any{"targets": []any{
			map[string]any{"provider": "zhipu", "model": "glm-air", "priority": 1},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	content := readConfig(t, path)
	if !strings.Contains(content, "glm-air") {
		t.Errorf("edited config:\n%s", content)
	}
}

// Bare `routes:` (null value, only comments below) must be promoted in place;
// appending a second routes key used to fail validation with "mapping key
// already defined".
func TestEditConfigRouteBareNullRoutesKey(t *testing.T) {
	path := writeTestConfig(t)
	if err := os.WriteFile(path, []byte(`listen: 127.0.0.1:8080
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://example.test, models: [glm]}
routes:
# legacy commented examples stay below the bare key
`), 0o600); err != nil {
		t.Fatal(err)
	}
	service := editService(t, path, &reloadSpy{})
	if err := service.EditConfig(appapi.EditRequest{
		Kind: "route",
		Name: "glm-air",
		Data: map[string]any{"targets": []any{
			map[string]any{"provider": "zhipu", "model": "glm-air"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	content := readConfig(t, path)
	if strings.Count(content, "\nroutes:")+boolToInt(strings.HasPrefix(content, "routes:")) != 1 {
		t.Errorf("edited config must keep exactly one routes key:\n%s", content)
	}
	if !strings.Contains(content, "glm-air") || !strings.Contains(content, "# legacy commented examples") {
		t.Errorf("edited config:\n%s", content)
	}
}

// TestEditConfigRouteStrategy covers the data.strategy edit semantics: set
// (list form → map form), switch back to quota (explicit), quota on an
// undeclared route (list form untouched), strip via null, strategy-only edit
// (targets node kept verbatim), and preservation on targets-only edits.
func TestEditConfigRouteStrategy(t *testing.T) {
	t.Run("set wraps list form into map form", func(t *testing.T) {
		path := writeTestConfig(t) // glm: [{provider: zhipu, model: glm, priority: 1}]
		service := editService(t, path, &reloadSpy{})
		if err := service.EditConfig(appapi.EditRequest{
			Kind: "route",
			Name: "glm",
			Data: map[string]any{
				"strategy": configdomain.RouteStrategyLoadBalance,
				"targets":  []any{map[string]any{"provider": "zhipu", "model": "glm"}},
			},
		}); err != nil {
			t.Fatal(err)
		}
		cfg, err := configdomain.LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.RouteStrategyFor("glm"); got != configdomain.RouteStrategyLoadBalance {
			t.Fatalf("RouteStrategyFor(glm) = %q, want load_balance", got)
		}
		if content := readConfig(t, path); !strings.Contains(content, "strategy: load_balance") {
			t.Errorf("map form not written:\n%s", content)
		}
	})

	t.Run("switch back to quota writes explicit quota", func(t *testing.T) {
		path := writeRouteStrategyConfig(t)
		service := editService(t, path, &reloadSpy{})
		if err := service.EditConfig(appapi.EditRequest{
			Kind: "route",
			Name: "glm",
			Data: map[string]any{
				"strategy": configdomain.RouteStrategyQuota,
				"targets":  []any{map[string]any{"provider": "zhipu", "model": "glm"}},
			},
		}); err != nil {
			t.Fatal(err)
		}
		cfg, err := configdomain.LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.RouteStrategyFor("glm"); got != configdomain.RouteStrategyQuota {
			t.Fatalf("RouteStrategyFor(glm) = %q, want quota", got)
		}
		if content := readConfig(t, path); !strings.Contains(content, "strategy: quota") {
			t.Errorf("explicit quota not written:\n%s", content)
		}
	})

	t.Run("quota on undeclared route keeps list form", func(t *testing.T) {
		path := writeTestConfig(t)
		service := editService(t, path, &reloadSpy{})
		if err := service.EditConfig(appapi.EditRequest{
			Kind: "route",
			Name: "glm",
			Data: map[string]any{
				"strategy": configdomain.RouteStrategyQuota,
				"targets":  []any{map[string]any{"provider": "zhipu", "model": "glm"}},
			},
		}); err != nil {
			t.Fatal(err)
		}
		content := readConfig(t, path)
		if strings.Contains(content, "strategy") {
			t.Errorf("quota on an undeclared route must not add a strategy key:\n%s", content)
		}
	})

	t.Run("null strips the declaration", func(t *testing.T) {
		path := writeRouteStrategyConfig(t)
		service := editService(t, path, &reloadSpy{})
		if err := service.EditConfig(appapi.EditRequest{
			Kind: "route",
			Name: "glm",
			Data: map[string]any{
				"strategy": nil,
				"targets":  []any{map[string]any{"provider": "zhipu", "model": "glm"}},
			},
		}); err != nil {
			t.Fatal(err)
		}
		cfg, err := configdomain.LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.RouteStrategyFor("glm"); got != configdomain.RouteStrategyQuota {
			t.Fatalf("RouteStrategyFor(glm) = %q, want default quota", got)
		}
		if content := readConfig(t, path); strings.Contains(content, "strategy") {
			t.Errorf("null strategy must strip the declaration:\n%s", content)
		}
	})

	t.Run("strategy-only edit keeps targets verbatim", func(t *testing.T) {
		path := writeTestConfig(t)
		service := editService(t, path, &reloadSpy{})
		if err := service.EditConfig(appapi.EditRequest{
			Kind: "route",
			Name: "glm",
			Data: map[string]any{"strategy": configdomain.RouteStrategyLoadBalance},
		}); err != nil {
			t.Fatal(err)
		}
		content := readConfig(t, path)
		if !strings.Contains(content, "strategy: load_balance") {
			t.Errorf("strategy-only edit did not declare:\n%s", content)
		}
		// The original targets node (priority comment and all) stays verbatim.
		if !strings.Contains(content, "priority: 1") {
			t.Errorf("targets node was re-encoded:\n%s", content)
		}
		cfg, err := configdomain.LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if targets := cfg.Routes["glm"]; len(targets) != 1 || targets[0].Priority != 1 {
			t.Errorf("targets after strategy-only edit = %v", targets)
		}
	})

	t.Run("strategy-only edit on missing route errors", func(t *testing.T) {
		path := writeTestConfig(t)
		service := editService(t, path, &reloadSpy{})
		err := service.EditConfig(appapi.EditRequest{
			Kind: "route",
			Name: "nope",
			Data: map[string]any{"strategy": configdomain.RouteStrategyLoadBalance},
		})
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("strategy-only edit on missing route err = %v", err)
		}
	})
}

// writeRouteStrategyConfig seeds a config whose glm route already declares
// the load_balance strategy (map form).
func writeRouteStrategyConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`listen: 127.0.0.1:8080
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://example.test, models: [glm]}
routes:
  glm:
    strategy: load_balance
    targets: [{provider: zhipu, model: glm}]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// A route in the map form ({strategy, targets}) must keep its declared
// strategy when the structured editor rewrites the target list — re-encoding
// a bare list would silently reset the route to the default quota strategy.
func TestEditConfigRoutePreservesStrategy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`listen: 127.0.0.1:8080
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://example.test, models: [glm]}
routes:
  glm:
    strategy: load_balance
    targets: [{provider: zhipu, model: glm}]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	service := editService(t, path, &reloadSpy{})
	if err := service.EditConfig(appapi.EditRequest{
		Kind: "route",
		Name: "glm",
		Data: map[string]any{"targets": []any{
			map[string]any{"provider": "zhipu", "model": "glm", "priority": 2},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	content := readConfig(t, path)
	if !strings.Contains(content, "strategy: load_balance") {
		t.Errorf("route edit dropped the declared strategy:\n%s", content)
	}
	if !strings.Contains(content, "priority: 2") {
		t.Errorf("route edit did not apply the new targets:\n%s", content)
	}
	cfg, err := configdomain.LoadConfig(path)
	if err != nil {
		t.Fatalf("reload edited config: %v", err)
	}
	if got := cfg.RouteStrategyFor("glm"); got != configdomain.RouteStrategyLoadBalance {
		t.Fatalf("RouteStrategyFor(glm) = %q, want load_balance", got)
	}
}

func TestEditConfigDelete(t *testing.T) {
	path := writeTestConfig(t)
	spy := &reloadSpy{}
	service := editService(t, path, spy)
	if err := service.EditConfig(appapi.EditRequest{
		Kind: "route",
		Name: "glm",
		Data: map[string]any{"delete": true},
	}); err != nil {
		t.Fatal(err)
	}
	if content := readConfig(t, path); strings.Contains(content, "routes:\n    glm") {
		t.Errorf("route delete did not apply:\n%s", content)
	}
}

func TestEditConfigUnknownKind(t *testing.T) {
	service := New(Ports{ConfigFile: func() string { return "config.yaml" }})
	err := service.EditConfig(appapi.EditRequest{Kind: "bogus"})
	if httpErrorStatus(t, err) != http.StatusBadRequest ||
		!strings.Contains(err.Error(), "unknown edit kind: bogus") {
		t.Errorf("err = %v", err)
	}
}

func TestEditConfigNonMappingConfig(t *testing.T) {
	path := writeTestConfig(t)
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	service := editService(t, path, &reloadSpy{})
	err := service.EditConfig(appapi.EditRequest{Kind: "general", Data: map[string]any{"listen": "x"}})
	if err == nil || !strings.Contains(err.Error(), "not a YAML mapping") {
		t.Errorf("err = %v, want mapping error", err)
	}
}

// TestEditConfigScalarBlocks covers the request_log/stats/cache kinds the
// Config tab's settings form drives: each kind mutates only its own block, and
// JSON numbers (float64) land as plain YAML integers, not scientific notation.
func TestEditConfigScalarBlocks(t *testing.T) {
	path := writeTestConfig(t)
	spy := &reloadSpy{}
	service := editService(t, path, spy)

	if err := service.EditConfig(appapi.EditRequest{
		Kind: "request_log",
		Data: map[string]any{
			"enabled":       true,
			"dir":           "~/requests",
			"max_file_size": float64(1073741824),
			"retention":     "720h",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.EditConfig(appapi.EditRequest{
		Kind: "stats",
		Data: map[string]any{"retention": "720h"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.EditConfig(appapi.EditRequest{
		Kind: "cache",
		Data: map[string]any{"enabled": false, "ttl": "60m", "max_entries": float64(6000)},
	}); err != nil {
		t.Fatal(err)
	}
	content := readConfig(t, path)
	for _, want := range []string{
		"request_log:", "enabled: true", "dir: ~/requests",
		"max_file_size: 1073741824", "retention: 720h",
		"stats:", "retention: 720h",
		"cache:", "enabled: false", "ttl: 60m", "max_entries: 6000",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("edited config missing %q:\n%s", want, content)
		}
	}
	if len(spy.calls) != 3 {
		t.Errorf("reload calls = %v", spy.calls)
	}
}

// TestEditConfigNilDeletesKey pins the form's "clear = revert to default"
// contract: a nil data value removes the key so the code default applies again.
func TestEditConfigNilDeletesKey(t *testing.T) {
	path := writeTestConfig(t)
	if err := os.WriteFile(path, []byte("listen: 127.0.0.1:8080\nproviders:\n  zhipu: {provider_id: zhipu, openai_base_url: https://example.test, models: [glm]}\nscheduling:\n  circuit_threshold: 7\n  sticky_dwell: 2m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := editService(t, path, &reloadSpy{})
	if err := service.EditConfig(appapi.EditRequest{
		Kind: "scheduling",
		Data: map[string]any{"circuit_threshold": nil, "sticky_dwell": "2m"},
	}); err != nil {
		t.Fatal(err)
	}
	content := readConfig(t, path)
	if strings.Contains(content, "circuit_threshold") {
		t.Errorf("nil value must delete the key:\n%s", content)
	}
	if !strings.Contains(content, "sticky_dwell: 2m") {
		t.Errorf("unchanged key must survive:\n%s", content)
	}
}

// TestEditConfigGuard covers the guard form kind: scalar switches, the two
// rule lists (present replaces wholesale, nil deletes, absent untouched), and
// that the projection round-trips through configSettings for the form.
func TestEditConfigGuard(t *testing.T) {
	path := writeTestConfig(t)
	if err := os.WriteFile(path, []byte(`listen: 127.0.0.1:8080
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://example.test, models: [glm]}
guard:
  secrets: log
  extra_paths: [~/.company/secrets]
  extra_patterns:
    - {name: old_rule, regex: 'old-[0-9]+'}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	spy := &reloadSpy{}
	service := editService(t, path, spy)
	err := service.EditConfig(appapi.EditRequest{
		Kind: "guard",
		Data: map[string]any{
			"paths": "off",
			"extra_patterns": []any{
				map[string]any{"name": "myvendor_key", "regex": `\bmv-[A-Za-z0-9]{32,}`, "literal": "mv-"},
			},
			// extra_paths absent → the existing list must survive.
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	content := readConfig(t, path)
	for _, want := range []string{
		"guard:", "secrets: log", "paths: off",
		"name: myvendor_key", `regex: \bmv-[A-Za-z0-9]{32,}`, "literal: mv-",
		"~/.company/secrets",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("edited config missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "old_rule") {
		t.Errorf("a present extra_patterns must replace the list wholesale:\n%s", content)
	}

	// The projection the form reads round-trips the edited state.
	doc, err := service.ConfigDocument()
	if err != nil {
		t.Fatal(err)
	}
	g := doc.Settings.Guard
	if g.Paths != "off" || g.Secrets != "log" {
		t.Errorf("settings.guard actions = %s/%s", g.Secrets, g.Paths)
	}
	if len(g.ExtraPatterns) != 1 || g.ExtraPatterns[0].Name != "myvendor_key" || g.ExtraPatterns[0].Literal != "mv-" {
		t.Errorf("settings.guard.extra_patterns = %+v", g.ExtraPatterns)
	}
	if len(g.ExtraPaths) != 1 || g.ExtraPaths[0] != "~/.company/secrets" {
		t.Errorf("settings.guard.extra_paths = %v", g.ExtraPaths)
	}

	// nil deletes a rule list entirely (revert-to-none signal).
	if err := service.EditConfig(appapi.EditRequest{
		Kind: "guard",
		Data: map[string]any{"extra_paths": nil},
	}); err != nil {
		t.Fatal(err)
	}
	if content := readConfig(t, path); strings.Contains(content, "extra_paths") {
		t.Errorf("nil extra_paths must delete the key:\n%s", content)
	}
	// A malformed row value is rejected with 400 and leaves the key alone.
	err = service.EditConfig(appapi.EditRequest{
		Kind: "guard",
		Data: map[string]any{"extra_patterns": "not-a-list"},
	})
	if httpErrorStatus(t, err) != http.StatusBadRequest {
		t.Fatalf("malformed extra_patterns must return 400, got %v", err)
	}
	if content := readConfig(t, path); !strings.Contains(content, "myvendor_key") {
		t.Errorf("malformed list value must leave the key untouched:\n%s", content)
	}
}
