package providerstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"

	"model-proxy/internal/config"
	"model-proxy/internal/configedit"
)

// LoadEffectiveConfig loads config.yaml with its providers resolved from the
// store under the process HOME (Path()); the HomeDir-port-aware variant is
// LoadEffectiveConfigFrom.
func LoadEffectiveConfig(configPath string, absorb bool) (*config.Config, error) {
	return LoadEffectiveConfigFrom(configPath, Path(), absorb)
}

// LoadEffectiveConfigFrom is LoadEffectiveConfig with an explicit store path
// (the admin service's HomeDir port injects its test dir through this).
func LoadEffectiveConfigFrom(configPath, storePath string, absorb bool) (*config.Config, error) {
	var providers map[string]config.Provider
	var err error
	if absorb {
		providers, err = Absorb(configPath, storePath)
	} else {
		providers, err = Resolve(configPath, storePath)
	}
	if err != nil {
		var warn *StripWarning
		if !(absorb && errors.As(err, &warn)) {
			return nil, err
		}
		// Strip failed after the store was updated: keep loading with the
		// merged map; the warning rides out with the cfg.
	}
	data, rerr := os.ReadFile(configPath)
	if rerr != nil {
		return nil, fmt.Errorf("read config %s: %w", configPath, rerr)
	}
	cfg, cerr := config.LoadConfigEffective(configPath, data, providers)
	if cerr != nil {
		return nil, cerr
	}
	if err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Mutate runs a locked read-modify-write cycle over the effective provider
// map: yaml entries are absorbed over the store first (so a stale yaml copy
// cannot clobber the edit via the next reload), fn mutates the map in place,
// the result is validated as a whole effective config (fail-closed — routes
// referencing a deleted provider reject the edit), then persisted to the
// store and the yaml section stripped. A no-op fn skips the write (no backup
// churn). The caller reloads the daemon AFTER Mutate returns.
func Mutate(configPath, storePath string, fn func(providers map[string]config.Provider) error) error {
	return MutateContext(context.Background(), configPath, storePath, fn)
}

// MutateContext is Mutate with a cancellation boundary honored up to the
// commit: once the store write starts, the cycle finishes (save + strip)
// even if the client disconnects — same contract as the admin yaml writers.
func MutateContext(ctx context.Context, configPath, storePath string, fn func(providers map[string]config.Provider) error) error {
	return configedit.WithConfigLockContext(ctx, configPath, func() error {
		return MutateContextUnderLock(ctx, configPath, storePath, fn)
	})
}

// MutateContextUnderLock is MutateContext for callers that already hold the
// config.yaml flock and must keep it through a subsequent reload or rollback.
func MutateContextUnderLock(ctx context.Context, configPath, storePath string, fn func(providers map[string]config.Provider) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	yamlProviders, err := readYAMLProviders(configPath)
	if err != nil {
		return err
	}
	store, err := Load(storePath)
	if err != nil {
		return err
	}
	// effective starts as a COPY of the absorbed state (yaml entries over
	// the store): fn mutates it in place, and the no-op guard below
	// compares it against the same absorbed baseline — aliasing the store
	// (or comparing against the store alone) would make the guard
	// always-equal during the yaml→store transition window and silently
	// drop edits.
	effective := mergeOver(store, yamlProviders)
	baseline := Clone(effective)
	if err := fn(effective); err != nil {
		return err
	}
	if reflect.DeepEqual(baseline, effective) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Whole-config validation with the new provider set: cross-provider
	// rules (routes/shadow targets, protocol-vs-base) must hold for the
	// state that the next reload will serve. A missing config file is an
	// error here (fail-closed, like the yaml writers this replaces);
	// only Absorb/Resolve tolerate absence for the read paths.
	data, rerr := os.ReadFile(configPath)
	if rerr != nil {
		return fmt.Errorf("read config %s: %w", configPath, rerr)
	}
	if _, verr := config.LoadConfigEffective(configPath, data, effective); verr != nil {
		return fmt.Errorf("refusing invalid provider change: %w", verr)
	}
	if err := Save(storePath, effective); err != nil {
		return err
	}
	if len(yamlProviders) > 0 {
		if err := stripYAMLProviders(configPath); err != nil {
			return &StripWarning{Err: err}
		}
	}
	return nil
}

// Clone deep-copies a provider map — callers that need to roll the store back
// after a failed reload snapshot the pre-mutation state with this.
func Clone(providers map[string]config.Provider) map[string]config.Provider {
	out := make(map[string]config.Provider, len(providers))
	for name, p := range providers {
		if p.Models != nil {
			p.Models = append([]string(nil), p.Models...)
		}
		if p.Headers != nil {
			p.Headers = copyStringMap(p.Headers)
		}
		if p.Alias != nil {
			p.Alias = copyStringMap(p.Alias)
		}
		if p.CatalogAlias != nil {
			p.CatalogAlias = copyStringMap(p.CatalogAlias)
		}
		if p.Capabilities != nil {
			caps := p.Capabilities
			p.Capabilities = make(map[string][]string, len(caps))
			for m, list := range caps {
				p.Capabilities[m] = append([]string(nil), list...)
			}
		}
		if p.PeakHours != nil {
			p.PeakHours = append(config.PeakConfig(nil), p.PeakHours...)
		}
		out[name] = p
	}
	return out
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
