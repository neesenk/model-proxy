package admin

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"model-proxy/internal/appapi"
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
	return New(Ports{
		ConfigFile: func() string { return path },
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
	content := readConfig(t, path)
	if !strings.Contains(content, "billing: plan") || !strings.Contains(content, "glm-air") {
		t.Errorf("edited config:\n%s", content)
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
	// A malformed row value leaves the key alone instead of corrupting it.
	if err := service.EditConfig(appapi.EditRequest{
		Kind: "guard",
		Data: map[string]any{"extra_patterns": "not-a-list"},
	}); err != nil {
		t.Fatal(err)
	}
	if content := readConfig(t, path); !strings.Contains(content, "myvendor_key") {
		t.Errorf("malformed list value must leave the key untouched:\n%s", content)
	}
}
