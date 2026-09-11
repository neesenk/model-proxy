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
