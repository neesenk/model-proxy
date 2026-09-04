package wirecap

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestModelStorePutGetSnapshotRestore(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := &ModelStore{}
	store.Put("aqp", "fp-a", "m1", ModelProtocols{Chat: Yes, Anthropic: No, Responses: Unknown}, now)
	store.Put("aqp", "fp-a", "m2", ModelProtocols{Chat: Yes, Anthropic: Yes, Responses: Yes}, now)
	store.Put("gone", "fp-g", "m1", ModelProtocols{Chat: Yes}, now)

	mp, ok := store.Get("aqp", "m1")
	if !ok || mp.Chat != Yes || mp.Anthropic != No || mp.Responses != Unknown {
		t.Fatalf("Get = %+v, ok=%v", mp, ok)
	}
	if _, ok := store.Get("aqp", "missing"); ok {
		t.Error("Get hit for unknown model")
	}
	if fp, ok := store.ProviderFingerprint("aqp"); !ok || fp != "fp-a" {
		t.Errorf("ProviderFingerprint = %q, %v", fp, ok)
	}

	// Snapshot must be detached (mutating it must not leak back).
	snap := store.Snapshot()
	snap["aqp"].Models["m1"] = ModelProtocols{}
	if again, _ := store.Get("aqp", "m1"); again.Chat != Yes {
		t.Error("Snapshot aliased the live store")
	}

	// Restore keeps only providers whose fingerprint still matches.
	store.Restore(snap, map[string]string{"aqp": "fp-a", "gone": "fp-changed"})
	if _, ok := store.Get("gone", "m1"); ok {
		t.Error("fingerprint-mismatched provider survived Restore")
	}
	if _, ok := store.Get("aqp", "m2"); !ok {
		t.Error("matching provider lost models in Restore")
	}

	// nil-safety.
	var nilStore *ModelStore
	if _, ok := nilStore.Get("a", "m"); ok {
		t.Error("nil ModelStore Get succeeded")
	}
	if _, ok := nilStore.ProviderFingerprint("a"); ok {
		t.Error("nil ModelStore ProviderFingerprint succeeded")
	}
	nilStore.Put("a", "fp", "m", ModelProtocols{}, now)
	nilStore.MarkResponsesUnsupported("a", "m", now)
	nilStore.Restore(nil, nil)
	if got := nilStore.Snapshot(); len(got) != 0 {
		t.Errorf("nil ModelStore Snapshot = %+v", got)
	}
}

func TestModelStoreMarkResponsesUnsupported(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := &ModelStore{}
	store.Put("aqp", "fp", "m1", ModelProtocols{Chat: No, Anthropic: No, Responses: Yes}, now)
	store.MarkResponsesUnsupported("aqp", "m1", now.Add(time.Minute))
	mp, ok := store.Get("aqp", "m1")
	if !ok || mp.Responses != No {
		t.Fatalf("after correction = %+v, ok=%v", mp, ok)
	}
	// No-ops on missing entries.
	store.MarkResponsesUnsupported("aqp", "ghost", now)
	store.MarkResponsesUnsupported("ghost", "m1", now)
	if _, ok := store.Get("ghost", "m1"); ok {
		t.Error("correction created a phantom entry")
	}
}

func TestModelStoreConcurrentAccess(t *testing.T) {
	store := &ModelStore{}
	start := make(chan struct{})
	done := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			for iteration := 0; iteration < 100; iteration++ {
				store.Put("p", "fp", "m", ModelProtocols{Chat: Yes, Responses: Yes}, time.Unix(int64(iteration), 0))
				store.MarkResponsesUnsupported("p", "m", time.Now())
				store.Get("p", "m")
				store.Snapshot()
			}
			done <- struct{}{}
		}()
	}
	close(start)
	<-done
	<-done
	if _, ok := store.Get("p", "m"); !ok {
		t.Fatal("concurrent writers lost the entry")
	}
}

func TestModelCapsFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "model_caps.json")
	want := map[string]ProviderModelCaps{
		"aqp": {
			Fingerprint: "fp-a",
			ProbedAt:    time.Unix(1_700_000_000, 0).UTC(),
			Models: map[string]ModelProtocols{
				"m1": {Chat: Yes, Anthropic: No, Responses: Yes},
			},
		},
	}
	if err := SaveModelCapsFile(path, want); err != nil {
		t.Fatalf("SaveModelCapsFile: %v", err)
	}
	got, err := LoadModelCapsFile(path)
	if err != nil {
		t.Fatalf("LoadModelCapsFile: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}

	// Missing file → nil, nil (first boot).
	missing, err := LoadModelCapsFile(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || missing != nil {
		t.Errorf("missing file = %+v, %v; want nil, nil", missing, err)
	}

	// Malformed file → error (caller warns + starts empty).
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadModelCapsFile(bad); err == nil {
		t.Error("malformed file must return an error")
	}
}

func TestModelCapsPath(t *testing.T) {
	got := ModelCapsPath("/home/u/.model-proxy/quota_state.json")
	want := "/home/u/.model-proxy/model_caps.json"
	if got != want {
		t.Errorf("ModelCapsPath = %q, want %q", got, want)
	}
}

func TestModelProtocolsConcluded(t *testing.T) {
	if (ModelProtocols{Chat: Yes, Anthropic: No, Responses: Unknown}).Concluded() {
		t.Error("unknown leg must not be concluded")
	}
	if !(ModelProtocols{Chat: Yes, Anthropic: No, Responses: Yes}).Concluded() {
		t.Error("all-final matrix must be concluded")
	}
}

func TestLoadModelCapsFileVersionMismatchDiscards(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model_caps.json")
	// v1 file (pre tools-attached probe legs): verdict semantics changed, so
	// a version mismatch must read as ABSENT (re-probe), not as data and not
	// as corruption.
	legacy := `{"version":1,"providers":{"aqp":{"fingerprint":"fp","models":{"m":{"chat":"yes","anthropic":"yes","responses":"yes"}}}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadModelCapsFile(path)
	if err != nil {
		t.Fatalf("version mismatch must not error, got %v", err)
	}
	if loaded != nil {
		t.Errorf("v1 file under v%d semantics must be discarded, got %v", ModelCapsFileVersion, loaded)
	}
	// Current version round-trips.
	now := time.Now()
	if err := SaveModelCapsFile(path, map[string]ProviderModelCaps{
		"aqp": {Fingerprint: "fp", ProbedAt: now, Models: map[string]ModelProtocols{"m": {Chat: Yes}}},
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err = LoadModelCapsFile(path)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("current-version round-trip failed: loaded=%v err=%v", loaded, err)
	}
}
