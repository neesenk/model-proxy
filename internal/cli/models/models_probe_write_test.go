package models

import (
	"errors"
	"reflect"
	"testing"

	configdomain "model-proxy/internal/config"
)

func TestProbeAndWriteModelsProbeErrorKeepsAndWritesCandidates(t *testing.T) {
	probeErr := errors.New("provider unavailable")
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"aqp": {Models: []string{"old-model"}},
	}}
	var written []string
	writeCalls := 0
	reloadCalls := 0
	displayCalls := 0

	ops := probeAndWriteModelsOps{
		filter: func(gotCfg *configdomain.Config, provider string, ids []string) ([]string, []string) {
			if gotCfg != cfg || provider != "aqp" || !reflect.DeepEqual(ids, []string{"old-model", "candidate-b", "candidate-a"}) {
				t.Fatalf("filter args = (%p, %q, %v)", gotCfg, provider, ids)
			}
			return []string{"old-model", "candidate-b", "candidate-a"}, []string{"policy-drop"}
		},
		probe: func(gotCfg *configdomain.Config, provider string, ids []string) ([]string, []DropReason, error) {
			if gotCfg != cfg || provider != "aqp" || !reflect.DeepEqual(ids, []string{"old-model", "candidate-b", "candidate-a"}) {
				t.Fatalf("probe args = (%p, %q, %v)", gotCfg, provider, ids)
			}
			return nil, []DropReason{{Model: "must-be-cleared", Status: 401}}, probeErr
		},
		display: func(gotCfg *configdomain.Config, provider string, policyDropped []string, dropped []DropReason, perr error, allFailed bool) {
			displayCalls++
			if gotCfg != cfg || provider != "aqp" {
				t.Fatalf("display target = (%p, %q)", gotCfg, provider)
			}
			if !reflect.DeepEqual(gotCfg.Providers[provider].Models, []string{"candidate-a", "candidate-b", "old-model"}) {
				t.Fatalf("display models = %v", gotCfg.Providers[provider].Models)
			}
			if !reflect.DeepEqual(policyDropped, []string{"policy-drop"}) || len(dropped) != 0 {
				t.Fatalf("display drops = policy %v probe %v", policyDropped, dropped)
			}
			if !errors.Is(perr, probeErr) || allFailed {
				t.Fatalf("display state = perr %v allFailed %v", perr, allFailed)
			}
		},
		write: func(path, provider string, names []string) error {
			writeCalls++
			if path != "config.yaml" || provider != "aqp" {
				t.Fatalf("write target = (%q, %q)", path, provider)
			}
			written = append([]string(nil), names...)
			return nil
		},
		reload: func(gotArgs []string, gotCfg *configdomain.Config) {
			reloadCalls++
			if gotCfg != cfg {
				t.Fatalf("reload cfg = %p, want %p", gotCfg, cfg)
			}
		},
	}

	err := probeAndWriteModels(cfg, "aqp", []string{"old-model", "candidate-b", "candidate-a"}, []string{"old-model"}, nil, "config.yaml", ops)
	if err != nil {
		t.Fatalf("probeAndWriteModels: %v", err)
	}
	if got := cfg.Providers["aqp"].Models; !reflect.DeepEqual(got, []string{"candidate-a", "candidate-b", "old-model"}) {
		t.Fatalf("config models = %v, want retained candidates", got)
	}
	if !reflect.DeepEqual(written, []string{"candidate-a", "candidate-b", "old-model"}) {
		t.Fatalf("written models = %v, want retained candidates", written)
	}
	if writeCalls != 1 || reloadCalls != 1 || displayCalls != 1 {
		t.Fatalf("calls = write %d reload %d display %d, want 1 each", writeCalls, reloadCalls, displayCalls)
	}
}

func TestProbeAndWriteModelsAllProbeFailedDoesNotWipe(t *testing.T) {
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"aqp": {Models: []string{"old-model"}},
	}}
	probeDrops := []DropReason{
		{Model: "old-model", Status: 401, Reason: "unauthorized"},
		{Model: "candidate-b", Status: 401, Reason: "unauthorized"},
		{Model: "candidate-a", Status: 503, Reason: "unavailable"},
	}
	var written []string
	reloadCalls := 0

	ops := probeAndWriteModelsOps{
		filter: func(*configdomain.Config, string, []string) ([]string, []string) {
			return []string{"old-model", "candidate-b", "candidate-a"}, nil
		},
		probe: func(*configdomain.Config, string, []string) ([]string, []DropReason, error) {
			return nil, probeDrops, nil
		},
		display: func(gotCfg *configdomain.Config, provider string, _ []string, dropped []DropReason, perr error, allFailed bool) {
			if perr != nil || !allFailed {
				t.Fatalf("display state = perr %v allFailed %v", perr, allFailed)
			}
			if !reflect.DeepEqual(dropped, probeDrops) {
				t.Fatalf("display drops = %#v, want %#v", dropped, probeDrops)
			}
			if got := gotCfg.Providers[provider].Models; !reflect.DeepEqual(got, []string{"candidate-a", "candidate-b", "old-model"}) {
				t.Fatalf("display models = %v, want retained candidates", got)
			}
		},
		write: func(_, _ string, names []string) error {
			written = append([]string(nil), names...)
			return nil
		},
		reload: func([]string, *configdomain.Config) { reloadCalls++ },
	}

	err := probeAndWriteModels(cfg, "aqp", []string{"old-model", "candidate-a", "candidate-b"}, []string{"old-model"}, nil, "config.yaml", ops)
	if err != nil {
		t.Fatalf("probeAndWriteModels: %v", err)
	}
	if !reflect.DeepEqual(written, []string{"candidate-a", "candidate-b", "old-model"}) {
		t.Fatalf("written models = %v, want retained candidates", written)
	}
	if reloadCalls != 1 {
		t.Fatalf("reload calls = %d, want 1", reloadCalls)
	}
}

func TestProbeAndWriteModelsWriteFailureDoesNotReload(t *testing.T) {
	writeErr := errors.New("disk full")
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"aqp": {Models: []string{"old-model"}},
	}}
	reloadCalls := 0
	ops := probeAndWriteModelsOps{
		filter: func(*configdomain.Config, string, []string) ([]string, []string) {
			return []string{"new-model"}, nil
		},
		probe: func(*configdomain.Config, string, []string) ([]string, []DropReason, error) {
			return []string{"new-model"}, nil, nil
		},
		display: func(*configdomain.Config, string, []string, []DropReason, error, bool) {},
		write:   func(string, string, []string) error { return writeErr },
		reload:  func([]string, *configdomain.Config) { reloadCalls++ },
	}

	err := probeAndWriteModels(cfg, "aqp", []string{"new-model"}, []string{"old-model"}, nil, "config.yaml", ops)
	if !errors.Is(err, writeErr) {
		t.Fatalf("probeAndWriteModels error = %v, want %v", err, writeErr)
	}
	if reloadCalls != 0 {
		t.Fatalf("reload calls = %d, want 0 after write failure", reloadCalls)
	}
}
