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

func TestEditConfigRouteAndClaudeMapping(t *testing.T) {
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
	if err := service.EditConfig(appapi.EditRequest{
		Kind: "claude_mapping",
		Data: map[string]any{"alias": "sonnet", "route": "glm"},
	}); err != nil {
		t.Fatal(err)
	}
	content := readConfig(t, path)
	if !strings.Contains(content, "glm-air") || !strings.Contains(content, "sonnet: glm") {
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

	// claude_mapping deletes by alias, not by request name.
	if err := service.EditConfig(appapi.EditRequest{
		Kind: "claude_mapping",
		Data: map[string]any{"delete": true, "alias": "sonnet"},
	}); err != nil {
		t.Fatal(err)
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
