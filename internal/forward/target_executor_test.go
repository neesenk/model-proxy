package forward

import (
	"net/http"
	"path/filepath"
	"testing"

	"model-proxy/internal/protocol"
	"model-proxy/internal/targetexec"
)

// TestTargetExecutorKeepsANilResponsesStateANilInterface: the executor guards
// its Responses continuation writes with `Responses != nil`. Assigning a nil
// *ResponsesStateStore into that interface field would make the guards true
// with nothing behind them — a typed-nil trap that is harmless only for as
// long as every guard has a second condition. With no store configured the
// executor must see a nil interface; with one, the store itself.
func TestTargetExecutorKeepsANilResponsesStateANilInterface(t *testing.T) {
	services := Services{
		Client:        &http.Client{},
		NewHealthGate: func(Snapshot) targetexec.HealthGate { return newFakeHealthGate() },
		NewEffects:    func(uint64) targetexec.Effects { return &fakeEffects{} },
	}
	build := func() targetexec.Executor {
		return pipeline{svc: services}.targetExecutor(targetexec.Runtime{}, Snapshot{}, "up")
	}
	if got := build().Responses; got != nil {
		t.Fatalf("executor.Responses = %#v with no state store, want a nil interface", got)
	}

	store := protocol.NewResponsesStateStore(filepath.Join(t.TempDir(), "responses_state.json"))
	t.Cleanup(store.Close)
	services.ResponsesState = store
	if got := build().Responses; got == nil {
		t.Fatal("executor.Responses is nil with a state store configured")
	}
}
