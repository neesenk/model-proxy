package models

import (
	"errors"
	"reflect"
	"sort"
	"testing"

	configdomain "model-proxy/internal/config"
	runtimewire "model-proxy/internal/runtime/wirecap"
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
		disabled: func(_ *configdomain.Config, _ string, ids []string) ([]string, []string) { return ids, nil },
		probe: func(gotCfg *configdomain.Config, provider string, ids, _ []string) ([]string, []DropReason, map[string]runtimewire.ModelProtocols, error) {
			if gotCfg != cfg || provider != "aqp" || !reflect.DeepEqual(ids, []string{"old-model", "candidate-b", "candidate-a"}) {
				t.Fatalf("probe args = (%p, %q, %v)", gotCfg, provider, ids)
			}
			return nil, []DropReason{{Model: "must-be-cleared", Status: 401}}, nil, probeErr
		},
		display: func(gotCfg *configdomain.Config, provider string, policyDropped []string, dropped []DropReason, protocols map[string]runtimewire.ModelProtocols, upstreamNames map[string]string, perr error, allFailed bool) {
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

	err := probeAndWriteModels(cfg, "aqp", []string{"old-model", "candidate-b", "candidate-a"}, []string{"old-model"}, nil, nil, "config.yaml", ops)
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
	probeMatrix := map[string]runtimewire.ModelProtocols{
		"old-model":   {Chat: runtimewire.Unknown, Anthropic: runtimewire.No, Responses: runtimewire.Unknown},
		"candidate-a": {Chat: runtimewire.Unknown, Anthropic: runtimewire.No, Responses: runtimewire.Unknown},
		"candidate-b": {Chat: runtimewire.Unknown, Anthropic: runtimewire.No, Responses: runtimewire.Unknown},
	}

	ops := probeAndWriteModelsOps{
		filter: func(*configdomain.Config, string, []string) ([]string, []string) {
			return []string{"old-model", "candidate-b", "candidate-a"}, nil
		},
		disabled: func(_ *configdomain.Config, _ string, ids []string) ([]string, []string) { return ids, nil },
		probe: func(_ *configdomain.Config, _ string, ids, _ []string) ([]string, []DropReason, map[string]runtimewire.ModelProtocols, error) {
			return nil, probeDrops, probeMatrix, nil
		},
		display: func(gotCfg *configdomain.Config, provider string, _ []string, dropped []DropReason, protocols map[string]runtimewire.ModelProtocols, _ map[string]string, perr error, allFailed bool) {
			if perr != nil || !allFailed {
				t.Fatalf("display state = perr %v allFailed %v", perr, allFailed)
			}
			if !reflect.DeepEqual(dropped, probeDrops) {
				t.Fatalf("display drops = %#v, want %#v", dropped, probeDrops)
			}
			// The probe's fresh matrix is handed to the display (refresh shows
			// the PROTOCOLS column without a file round-trip).
			if !reflect.DeepEqual(protocols, probeMatrix) {
				t.Fatalf("display protocols = %#v, want %#v", protocols, probeMatrix)
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

	err := probeAndWriteModels(cfg, "aqp", []string{"old-model", "candidate-a", "candidate-b"}, []string{"old-model"}, nil, nil, "config.yaml", ops)
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
		disabled: func(_ *configdomain.Config, _ string, ids []string) ([]string, []string) { return ids, nil },
		probe: func(_ *configdomain.Config, _ string, ids, _ []string) ([]string, []DropReason, map[string]runtimewire.ModelProtocols, error) {
			return []string{"new-model"}, nil, nil, nil
		},
		display: func(*configdomain.Config, string, []string, []DropReason, map[string]runtimewire.ModelProtocols, map[string]string, error, bool) {
		},
		write:  func(string, string, []string) error { return writeErr },
		reload: func([]string, *configdomain.Config) { reloadCalls++ },
	}

	err := probeAndWriteModels(cfg, "aqp", []string{"new-model"}, []string{"old-model"}, nil, nil, "config.yaml", ops)
	if !errors.Is(err, writeErr) {
		t.Fatalf("probeAndWriteModels error = %v, want %v", err, writeErr)
	}
	if reloadCalls != 0 {
		t.Fatalf("reload calls = %d, want 0 after write failure", reloadCalls)
	}
}

// TestProbeAndWriteModels_ReloadsAfterCapsOnlyPersist: a refresh whose kept
// set is unchanged still persisted fresh verdicts to model_caps.json — the
// daemon must be signalled so its reload re-reads the file (the split-brain
// fix). A probe error (no caps persisted, no config change) must NOT signal.
func TestProbeAndWriteModels_ReloadsAfterCapsOnlyPersist(t *testing.T) {
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"aqp": {Models: []string{"same-model"}},
	}}
	reloadCalls := 0
	ops := probeAndWriteModelsOps{
		filter: func(*configdomain.Config, string, []string) ([]string, []string) {
			return []string{"same-model"}, nil
		},
		disabled: func(_ *configdomain.Config, _ string, ids []string) ([]string, []string) { return ids, nil },
		probe: func(_ *configdomain.Config, _ string, ids, _ []string) ([]string, []DropReason, map[string]runtimewire.ModelProtocols, error) {
			return []string{"same-model"}, nil, map[string]runtimewire.ModelProtocols{
				"same-model": {Chat: runtimewire.Yes, Anthropic: runtimewire.No, Responses: runtimewire.No},
			}, nil
		},
		display: func(*configdomain.Config, string, []string, []DropReason, map[string]runtimewire.ModelProtocols, map[string]string, error, bool) {
		},
		write:  func(string, string, []string) error { t.Fatal("write must not run: models unchanged"); return nil },
		reload: func([]string, *configdomain.Config) { reloadCalls++ },
	}
	if err := probeAndWriteModels(cfg, "aqp", []string{"same-model"}, []string{"same-model"}, nil, nil, "config.yaml", ops); err != nil {
		t.Fatalf("probeAndWriteModels: %v", err)
	}
	if reloadCalls != 1 {
		t.Fatalf("reload calls = %d, want 1 (caps persisted → daemon must re-read model_caps.json)", reloadCalls)
	}
}

// TestProbeAndWriteModels_NoReloadOnProbeErrorWithoutChange: when the probe
// infra failed (nothing persisted to model_caps.json) AND the model list is
// unchanged, no daemon signal is needed.
func TestProbeAndWriteModels_NoReloadOnProbeErrorWithoutChange(t *testing.T) {
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"aqp": {Models: []string{"same-model"}},
	}}
	reloadCalls := 0
	ops := probeAndWriteModelsOps{
		filter: func(*configdomain.Config, string, []string) ([]string, []string) {
			return []string{"same-model"}, nil
		},
		disabled: func(_ *configdomain.Config, _ string, ids []string) ([]string, []string) { return ids, nil },
		probe: func(_ *configdomain.Config, _ string, ids, _ []string) ([]string, []DropReason, map[string]runtimewire.ModelProtocols, error) {
			return nil, nil, nil, errors.New("provider unavailable")
		},
		display: func(*configdomain.Config, string, []string, []DropReason, map[string]runtimewire.ModelProtocols, map[string]string, error, bool) {
		},
		write:  func(string, string, []string) error { t.Fatal("write must not run"); return nil },
		reload: func([]string, *configdomain.Config) { reloadCalls++ },
	}
	if err := probeAndWriteModels(cfg, "aqp", []string{"same-model"}, []string{"same-model"}, nil, nil, "config.yaml", ops); err != nil {
		t.Fatalf("probeAndWriteModels: %v", err)
	}
	if reloadCalls != 0 {
		t.Fatalf("reload calls = %d, want 0 (nothing persisted, nothing changed)", reloadCalls)
	}
}

// TestProbeAndWriteModels_DisabledRideAlongUnprobed: disabled ids are
// excluded from the probe input, kept unconditionally (a disable override is
// not a callability verdict), and CANNOT mask the all-probe-failed safety
// net — a total outage on the probed subset still keeps that subset
// unvalidated instead of wiping it from config.
func TestProbeAndWriteModels_DisabledRideAlongUnprobed(t *testing.T) {
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"aqp": {Models: []string{"dead-a", "dead-b", "off-a"}},
	}}
	var probeInput []string
	var written []string
	reloadCalls := 0
	ops := probeAndWriteModelsOps{
		filter: func(_ *configdomain.Config, _ string, ids []string) ([]string, []string) {
			return ids, nil
		},
		disabled: func(_ *configdomain.Config, _ string, ids []string) ([]string, []string) {
			probeIDs := make([]string, 0, len(ids))
			var disabledIDs []string
			for _, id := range ids {
				if id == "off-a" {
					disabledIDs = append(disabledIDs, id)
				} else {
					probeIDs = append(probeIDs, id)
				}
			}
			return probeIDs, disabledIDs
		},
		probe: func(_ *configdomain.Config, _ string, ids, disabledIDs []string) ([]string, []DropReason, map[string]runtimewire.ModelProtocols, error) {
			probeInput = ids
			// The probe seam still RECEIVES the disabled ids (the production
			// impl uses them only to carry frozen verdicts through the
			// persist); the not-probed invariant is CheckProviderModels'
			// contract, covered in models_check_test.go.
			if !reflect.DeepEqual(disabledIDs, []string{"off-a"}) {
				t.Errorf("probe disabled ids = %v, want [off-a]", disabledIDs)
			}
			// Total outage: every probed model fails.
			drops := []DropReason{{Model: ids[0], Status: 401}, {Model: ids[1], Status: 401}}
			matrix := map[string]runtimewire.ModelProtocols{
				ids[0]: {Chat: runtimewire.Unknown},
				ids[1]: {Chat: runtimewire.Unknown},
			}
			return nil, drops, matrix, nil
		},
		display: func(gotCfg *configdomain.Config, _ string, _ []string, dropped []DropReason, _ map[string]runtimewire.ModelProtocols, _ map[string]string, perr error, allFailed bool) {
			if perr != nil || !allFailed {
				t.Fatalf("display state = perr %v allFailed %v, want the outage recognized", perr, allFailed)
			}
			if len(dropped) != 2 {
				t.Fatalf("display drops = %v, want both probed failures surfaced", dropped)
			}
			if got := gotCfg.Providers["aqp"].Models; len(got) != 3 {
				t.Fatalf("display models = %v, want all three retained (outage fallback + disabled)", got)
			}
		},
		write: func(_, _ string, names []string) error {
			written = append([]string(nil), names...)
			return nil
		},
		reload: func([]string, *configdomain.Config) { reloadCalls++ },
	}

	if err := probeAndWriteModels(cfg, "aqp", []string{"dead-a", "dead-b", "off-a"}, []string{"off-a"}, nil, nil, "config.yaml", ops); err != nil {
		t.Fatalf("probeAndWriteModels: %v", err)
	}
	if !reflect.DeepEqual(probeInput, []string{"dead-a", "dead-b"}) {
		t.Errorf("probe input = %v, want the disabled id excluded", probeInput)
	}
	// The outage fallback keeps the PROBED set unvalidated AND the disabled id
	// rides along — nothing is wiped from config.
	sort.Strings(written)
	if !reflect.DeepEqual(written, []string{"dead-a", "dead-b", "off-a"}) {
		t.Errorf("written = %v, want all three (outage fallback + disabled)", written)
	}
	if reloadCalls != 1 {
		t.Errorf("reload calls = %d, want 1", reloadCalls)
	}
}
