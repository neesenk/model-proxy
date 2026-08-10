package main

import (
	"go/ast"
	cliserve "model-proxy/internal/cli/serve"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestApplicationRuntimeConcreteAssemblyContract(t *testing.T) {
	file, _ := parseGoFile(t, "app_assembly.go")
	fields := namedStructFields(t, file, "applicationRuntime")
	for _, field := range []string{"configPath", "startupConfig", "proxy", "handler", "transportTasks"} {
		if _, ok := fields[field]; !ok {
			t.Errorf("applicationRuntime is missing concrete owner field %q", field)
		}
	}

	constructor := namedFunction(t, file, "newApplicationRuntime")
	if got := namedCallCountInNode(constructor.Body, "NewProxy"); got != 1 {
		t.Errorf("newApplicationRuntime NewProxy calls = %d, want exactly 1", got)
	}
	if got := callCountOnIdentInNode(constructor.Body, "proxy", "startRuntimeServices"); got != 1 {
		t.Errorf("newApplicationRuntime proxy.startRuntimeServices calls = %d, want exactly 1", got)
	}
	if got := namedCallCountInNode(constructor.Body, "NewServeMux"); got != 1 {
		t.Errorf("newApplicationRuntime http.NewServeMux calls = %d, want exactly 1", got)
	}
	if got := namedCallCountInNode(constructor.Body, "newWebServer"); got != 1 {
		t.Errorf("newApplicationRuntime newWebServer calls = %d, want exactly 1 guarded Web assembly", got)
	}

	literals := 0
	ast.Inspect(constructor.Body, func(node ast.Node) bool {
		unary, ok := node.(*ast.UnaryExpr)
		if !ok || unary.Op.String() != "&" {
			return true
		}
		literal, ok := unary.X.(*ast.CompositeLit)
		if !ok || !identIs(literal.Type, "applicationRuntime") {
			return true
		}
		literals++
		want := map[string]string{
			"configPath":    "args.Config",
			"startupConfig": "cfg",
			"proxy":         "proxy",
			"handler":       "mux",
		}
		got := map[string]string{}
		for _, element := range literal.Elts {
			entry, ok := element.(*ast.KeyValueExpr)
			if !ok {
				t.Fatalf("applicationRuntime literal element = %T, want keyed field", element)
			}
			key, ok := entry.Key.(*ast.Ident)
			if !ok {
				t.Fatalf("applicationRuntime literal key = %T, want identifier", entry.Key)
			}
			got[key.Name] = expressionName(entry.Value)
		}
		for field, value := range want {
			if got[field] != value {
				t.Errorf("applicationRuntime.%s initializer = %q, want %q", field, got[field], value)
			}
		}
		return true
	})
	if literals != 1 {
		t.Errorf("newApplicationRuntime concrete literals = %d, want exactly 1", literals)
	}
}

func TestApplicationRuntimeOwnsIsolatedLifecycle(t *testing.T) {
	for _, test := range []struct {
		name      string
		web       bool
		uiStatus  int
		taskCount int
	}{
		{name: "Web disabled", uiStatus: http.StatusBadGateway},
		{name: "Web enabled", web: true, uiStatus: http.StatusOK, taskCount: 1},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			writeFreshApplicationCatalog(t, home)
			cfg := &Config{
				Listen:    "127.0.0.1:0",
				Providers: map[string]Provider{},
				Stats:     StatsConfig{DBPath: filepath.Join(home, "stats.db")},
				Web:       WebConfig{Enabled: test.web},
			}
			runtime := newApplicationRuntime(cfg, cliserve.Args{Config: "test-config.yaml"})
			t.Cleanup(runtime.Close)

			if runtime.proxy == nil || runtime.startupConfig != cfg || runtime.handler == nil {
				t.Fatal("applicationRuntime did not retain its concrete proxy/config/mux owners")
			}
			if runtime.configPath != "test-config.yaml" {
				t.Fatalf("applicationRuntime configPath = %q, want test-config.yaml", runtime.configPath)
			}
			if runtime.proxy.stats == nil || runtime.proxy.flusher == nil {
				t.Fatal("newApplicationRuntime did not start persisted runtime services")
			}
			catalog := runtime.proxy.catalogSnapshot()
			if catalog == nil || catalog.Count() != 1 {
				t.Fatalf("newApplicationRuntime catalog count = %v, want 1 from isolated cache", catalog)
			}
			if got := len(runtime.transportTasks); got != test.taskCount {
				t.Fatalf("applicationRuntime transport tasks = %d, want %d", got, test.taskCount)
			}

			health := httptest.NewRecorder()
			runtime.handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
			if health.Code != http.StatusOK {
				t.Fatalf("assembled root handler /health status = %d, want 200", health.Code)
			}
			ui := httptest.NewRecorder()
			runtime.handler.ServeHTTP(ui, httptest.NewRequest(http.MethodGet, "/ui/", nil))
			if ui.Code != test.uiStatus {
				t.Fatalf("assembled /ui/ status = %d, want %d", ui.Code, test.uiStatus)
			}

			if test.taskCount != 0 {
				stop := make(chan struct{})
				done := make(chan struct{})
				go func() {
					runtime.transportTasks[0](stop)
					close(done)
				}()
				close(stop)
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("Web transport task did not stop and join")
				}
			}

			stopped := make(chan struct{})
			if admitted := runtime.proxy.lifecycle.run(func(stop <-chan struct{}) {
				<-stop
				close(stopped)
			}); !admitted {
				t.Fatal("isolated Proxy lifecycle did not admit owned task")
			}
			runtime.Close()
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("applicationRuntime.Close did not stop and join its Proxy lifecycle")
			}
			// Proxy.Close is idempotent; applicationRuntime must preserve that
			// property rather than layering a competing callback/task owner.
			runtime.Close()
			if runtime.proxy.lifecycle.run(func(<-chan struct{}) {}) {
				t.Fatal("applicationRuntime.Close admitted work after closing its Proxy")
			}
		})
	}
}

func TestApplicationRuntimeReloadSummaryContract(t *testing.T) {
	file, _ := parseGoFile(t, "app_assembly.go")
	reload := namedMethod(t, file, "applicationRuntime", "reload")
	if got := namedCallCountInNode(reload.Body, "reload"); got != 1 {
		t.Errorf("applicationRuntime.reload nested reload calls = %d, want exactly runtime.proxy.reload", got)
	}
	if got := namedCallCountInNode(reload.Body, "snapshotRuntime"); got != 1 {
		t.Errorf("applicationRuntime.reload snapshotRuntime calls = %d, want exactly 1 successful reload summary", got)
	}
	if got := selectorCallCountInNode(reload.Body, "ProviderNames"); got != 1 {
		t.Errorf("applicationRuntime.reload cliframework.ProviderNames calls = %d, want exactly 1 successful reload summary", got)
	}
	if got := selectorCallCountInNode(reload.Body, "RouteNames"); got != 1 {
		t.Errorf("applicationRuntime.reload cliframework.RouteNames calls = %d, want exactly 1 successful reload summary", got)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFreshApplicationCatalog(t, home)
	configPath := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(configPath, []byte(`listen: 127.0.0.1:17834
providers:
  demo:
    provider_id: zhipu
    openai_base_url: https://example.invalid/v1
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Listen:    "127.0.0.1:17833",
		Providers: map[string]Provider{},
		Stats:     StatsConfig{DBPath: filepath.Join(home, "stats.db")},
	}
	runtime := newApplicationRuntime(cfg, cliserve.Args{Config: configPath})
	t.Cleanup(runtime.Close)
	runtime.reload()
	snapshot := runtime.proxy.snapshotRuntime()
	if snapshot.cfg.Listen != "127.0.0.1:17834" {
		t.Fatalf("reload runtime listen = %q, want 127.0.0.1:17834", snapshot.cfg.Listen)
	}
	if provider := snapshot.cfg.Providers["demo"]; provider.Provider != "zhipu" {
		t.Fatalf("reload provider demo = %#v, want provider_id zhipu", provider)
	}
	if runtime.startupConfig.Listen != "127.0.0.1:17833" {
		t.Fatalf("startupConfig listen mutated to %q during reload", runtime.startupConfig.Listen)
	}
}

func writeFreshApplicationCatalog(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, ".model-proxy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"fetched_at":"` + time.Now().UTC().Format(time.RFC3339Nano) +
		`","etag":"","by_name":{"test-model":{"ctx":4096,"out":1024,"in":["text"],"out_mod":["text"]}}}`)
	if err := os.WriteFile(filepath.Join(dir, "models_cache.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func callCountOnIdentInNode(node ast.Node, receiver, method string) int {
	count := 0
	ast.Inspect(node, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != method || !identIs(selector.X, receiver) {
			return true
		}
		count++
		return true
	})
	return count
}

// selectorCallCountInNode counts <x>.<name>(...) call sites under n.
func selectorCallCountInNode(n ast.Node, name string) int {
	count := 0
	ast.Inspect(n, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
			count++
		}
		return true
	})
	return count
}
