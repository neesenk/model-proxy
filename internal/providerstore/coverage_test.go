package providerstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/internal/config"
)

// TestStripWarningErrorShape pins the error surface (message prefix + Unwrap)
// the daemon boot/reload logs depend on.
func TestStripWarningErrorShape(t *testing.T) {
	inner := errors.New("disk full")
	warn := &StripWarning{Err: inner}
	if !strings.HasPrefix(warn.Error(), "provider store migrated, but stripping providers: from config.yaml failed:") {
		t.Errorf("message = %q", warn.Error())
	}
	if !errors.Is(warn, inner) {
		t.Error("Unwrap must expose the inner error")
	}
}

// TestReadYAMLProvidersErrorPaths: a missing file yields nil providers (the
// pre-bootstrap state), a malformed providers block is a hard error.
func TestReadYAMLProvidersErrorPaths(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.yaml")
	if got, err := readYAMLProviders(missing); err != nil || got != nil {
		t.Fatalf("missing file = (%#v, %v), want (nil, nil)", got, err)
	}
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("providers:\n  x:\n    models: {a: 1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readYAMLProviders(bad); err == nil {
		t.Fatal("old map-form models must fail with the hint")
	} else if !strings.Contains(err.Error(), "hint:") {
		t.Fatalf("err = %v, want the map-form hint", err)
	}
	// A directory in place of the file is a non-ENOENT read error (EISDIR),
	// unlike a missing parent which reads as plain ENOENT above.
	unreadable := filepath.Join(dir, "asdir")
	if err := os.MkdirAll(unreadable, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readYAMLProviders(unreadable); err == nil {
		t.Fatal("non-ENOENT read error must surface")
	}
}

// TestLoadEffectiveConfigFromAbsorbStripWarning: when the store write succeeds
// but the yaml strip fails, the cfg stays usable and the StripWarning rides
// out with it — the daemon logs the warning and keeps booting.
func TestLoadEffectiveConfigFromAbsorbStripWarning(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based strip failure does not apply when running as root")
	}
	cfgDir := t.TempDir()
	cfgPath := writeSampleConfig(t, cfgDir, sampleConfig)
	// Pre-create the lock so the read-only config dir only breaks the STRIP.
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfgDir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(cfgDir, 0o700) }()
	storePath := filepath.Join(t.TempDir(), "provider.json")

	cfg, err := LoadEffectiveConfigFrom(cfgPath, storePath, true)
	var warn *StripWarning
	if err == nil || !errors.As(err, &warn) {
		t.Fatalf("err = %v, want StripWarning", err)
	}
	if cfg == nil || cfg.Providers["zhipu"].Provider != "zhipu" {
		t.Fatalf("cfg must stay usable: %#v", cfg.Providers)
	}
	if _, lerr := Load(storePath); lerr != nil {
		t.Fatalf("store must be persisted despite the strip failure: %v", lerr)
	}
}

// TestMutateSaveFailureSurfaces: a store path whose parent cannot be created
// fails the mutation without touching the yaml.
func TestMutateSaveFailureSurfaces(t *testing.T) {
	cfgDir := t.TempDir()
	cfgPath := writeSampleConfig(t, cfgDir, sampleConfig)
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(blocker, "nested", "provider.json")
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	mutateErr := Mutate(cfgPath, storePath, func(providers map[string]config.Provider) error {
		providers["extra"] = validProvider()
		return nil
	})
	if mutateErr == nil {
		t.Fatal("unwritable store path must fail the mutation")
	}
	if after, rerr := os.ReadFile(cfgPath); rerr != nil || string(after) != string(before) {
		t.Fatalf("failed mutation must not touch the yaml (err=%v)", rerr)
	}
}
