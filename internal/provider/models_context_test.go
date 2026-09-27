package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchModelsContextCancelsUpstream(t *testing.T) {
	started := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/models" {
			t.Errorf("unexpected model request %s %s", r.Method, r.URL.Path)
		}
		close(started)
		<-r.Context().Done()
	}))
	defer up.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p := newQwenPlanForTest(t, &Config{OpenAIBaseURL: up.URL})
	done := make(chan error, 1)
	go func() { _, err := FetchModelsContext(ctx, p); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("fetch did not reach upstream")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("fetch error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("model fetch did not cancel")
	}
}

func TestFetchModelsContextCapabilities(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := FetchModelsContext(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled fetch = %v", err)
	}
	if _, err := FetchModelsContext(context.Background(), &StaticProvider{}); !errors.Is(err, errNotSupported) {
		t.Fatalf("unsupported fetch = %v", err)
	}
	// Every HTTP-capable implementation must supply the cancellable capability.
	for _, impl := range []Provider{&AqpProvider{}, &ZhipuProvider{}, &ZCodeProvider{}, &DeepSeekProvider{}, &KimiCodeProvider{}, &QwenPlanProvider{}, &CodexProvider{}, &VolcengineProvider{}} {
		if _, ok := impl.(interface {
			FetchModelsContext(context.Context) ([]string, error)
		}); !ok {
			t.Errorf("%T has no cancellable model fetch", impl)
		}
	}
}

// TestFetchModelsContext_ProviderCoverage pins the CLI/webui refresh parity
// contract: EVERY provider with a real /models endpoint implements the
// cancellable FetchModelsContext — the daemon twin (POST /api/models/refresh)
// fetches the live list ONLY through it, and a provider that ships just the
// legacy FetchModels silently degrades the web refresh to re-validating
// route-configured models while the CLI discovers new ones (the mimo /
// opencode-go / openrouter / step-plan gap). Static is the deliberate
// exception (no /models endpoint at all; both paths return errNotSupported).
func TestFetchModelsContext_ProviderCoverage(t *testing.T) {
	build := func(providerID string) Provider {
		t.Helper()
		impl, err := New(&Config{ProviderID: providerID}, "test-"+providerID)
		if err != nil {
			t.Fatalf("%s: New: %v", providerID, err)
		}
		return impl
	}
	withLiveModelsEndpoint := []string{
		"aqp", "zhipu", "zcode", "deepseek", "kimi-code", "qwen-plan",
		"mimo", "opencode-go", "openrouter", "step-plan",
		"codex", "volcengine", "typesafe",
	}
	for _, providerID := range withLiveModelsEndpoint {
		t.Run(providerID, func(t *testing.T) {
			if _, ok := build(providerID).(interface {
				FetchModelsContext(context.Context) ([]string, error)
			}); !ok {
				t.Errorf("%s lacks FetchModelsContext — the daemon/webui models refresh cannot fetch its live model list (CLI parity gap)", providerID)
			}
		})
	}
}

// TestFetchModelsContext_BearerProviders tests the four context additions
// against a fake /models upstream: same ids as the legacy FetchModels, and
// the request is bound to the caller's context (cancellation propagates).
func TestFetchModelsContext_BearerProviders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("fetch path = %q, want /models", r.URL.Path)
		}
		w.Write([]byte(`{"data":[{"id":"new-hot-model"}]}`))
	}))
	defer srv.Close()

	mimo := newTestMiMo(t, &Config{OpenAIBaseURL: srv.URL})
	ocg := newTestOpenCodeGo(t, &Config{OpenAIBaseURL: srv.URL})
	orouter := newTestOpenRouter(t, &Config{OpenAIBaseURL: srv.URL})
	for _, p := range []interface{ SaveKey(string) error }{mimo, ocg, orouter} {
		if err := p.SaveKey("sk-testkey1234567890"); err != nil {
			t.Fatalf("SaveKey: %v", err)
		}
	}
	provs := []struct {
		name string
		p    Provider
	}{
		{"mimo", mimo},
		{"opencode-go", ocg},
		{"openrouter", orouter},
		{"step-plan", newStepPlanForTest(t, &Config{OpenAIBaseURL: srv.URL})},
	}
	for _, tc := range provs {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ids, err := FetchModelsContext(ctx, tc.p)
			if err != nil {
				t.Fatalf("FetchModelsContext: %v", err)
			}
			if len(ids) != 1 || ids[0] != "new-hot-model" {
				t.Errorf("ids = %v, want [new-hot-model]", ids)
			}
		})
	}
}
