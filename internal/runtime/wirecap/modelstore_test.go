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
	nilStore.MarkResponsesUnsupported("a", "m", "fp", now)
	nilStore.Restore(nil, nil)
	if got := nilStore.Snapshot(); len(got) != 0 {
		t.Errorf("nil ModelStore Snapshot = %+v", got)
	}
}

func TestModelStorePruneModels(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := &ModelStore{}
	store.Put("aqp", "fp", "m1", ModelProtocols{Chat: Yes, Anthropic: No, Responses: No}, now)
	store.Put("aqp", "fp", "stale", ModelProtocols{Chat: Yes, Anthropic: Yes, Responses: Yes}, now)

	if pruned := store.PruneModels("aqp", map[string]bool{"m1": true}); !pruned {
		t.Fatal("PruneModels = false, want true (stale model removed)")
	}
	if _, ok := store.Get("aqp", "stale"); ok {
		t.Error("dropped model survived pruning")
	}
	if mp, ok := store.Get("aqp", "m1"); !ok || mp.Chat != Yes {
		t.Errorf("kept model = %+v, ok=%v — must survive pruning", mp, ok)
	}
	// Idempotent: nothing left to remove; unknown provider is a no-op.
	if store.PruneModels("aqp", map[string]bool{"m1": true}) {
		t.Error("second prune = true, want false (nothing to remove)")
	}
	if store.PruneModels("ghost", map[string]bool{}) {
		t.Error("pruning an unknown provider = true, want false")
	}
	var nilStore *ModelStore
	if nilStore.PruneModels("aqp", map[string]bool{}) {
		t.Error("nil ModelStore PruneModels = true, want false")
	}
}

func TestModelStoreReplaceProviderModels(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := &ModelStore{}
	store.Put("aqp", "fp", "old", ModelProtocols{Chat: Yes}, now)

	// Replace drops models absent from the fresh matrix and updates the stamp.
	fresh := map[string]ModelProtocols{
		"m1": {Chat: Yes, Anthropic: No, Responses: Yes},
		"m2": {Chat: No, Anthropic: No, Responses: Yes},
	}
	store.ReplaceProviderModels("aqp", "fp", fresh, now.Add(time.Minute))
	if _, ok := store.Get("aqp", "old"); ok {
		t.Error("replaced entry kept a model absent from the fresh matrix")
	}
	if mp, ok := store.Get("aqp", "m2"); !ok || mp.Responses != Yes {
		t.Errorf("replaced entry m2 = %+v, ok=%v", mp, ok)
	}
	if got := store.Snapshot()["aqp"].ProbedAt; !got.Equal(now.Add(time.Minute)) {
		t.Errorf("ProbedAt = %v, want the replace stamp", got)
	}
	// The input map must not alias the store.
	fresh["m1"] = ModelProtocols{}
	if mp, _ := store.Get("aqp", "m1"); mp.Chat != Yes {
		t.Error("ReplaceProviderModels aliased the caller's map")
	}

	// Stale-generation guard: a fingerprint mismatch against Restore's
	// expected map is dropped like a stale Put.
	store.Restore(store.Snapshot(), map[string]string{"aqp": "fp-new"})
	store.ReplaceProviderModels("aqp", "fp", map[string]ModelProtocols{"ghost": {Chat: Yes}}, now)
	if _, ok := store.Get("aqp", "ghost"); ok {
		t.Error("stale-generation replace wrote over the current generation")
	}
	var nilStore *ModelStore
	nilStore.ReplaceProviderModels("aqp", "fp", nil, now)
}

func TestModelStoreMarkResponsesUnsupported(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := &ModelStore{}
	store.Put("aqp", "fp", "m1", ModelProtocols{Chat: No, Anthropic: No, Responses: Yes}, now)
	store.MarkResponsesUnsupported("aqp", "m1", "fp", now.Add(time.Minute))
	mp, ok := store.Get("aqp", "m1")
	if !ok || mp.Responses != No {
		t.Fatalf("after correction = %+v, ok=%v", mp, ok)
	}
	// No-ops on missing entries.
	store.MarkResponsesUnsupported("aqp", "ghost", "fp", now)
	store.MarkResponsesUnsupported("ghost", "m1", "fp", now)
	if _, ok := store.Get("ghost", "m1"); ok {
		t.Error("correction created a phantom entry")
	}
	// No-ops on a fingerprint the requester cannot attribute: an empty
	// fingerprint and a mismatching (already-reloaded) generation must both be
	// dropped — a pre-reload in-flight 404 must never flip the new
	// generation's verdict (model-level "no" has no TTL to expire it).
	store.Put("aqp", "fp", "m2", ModelProtocols{Responses: Yes}, now)
	store.MarkResponsesUnsupported("aqp", "m2", "", now)
	store.MarkResponsesUnsupported("aqp", "m2", "other-fp", now)
	if mp, _ := store.Get("aqp", "m2"); mp.Responses != Yes {
		t.Fatalf("unattributable 404 flipped m2: %+v", mp)
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
				store.MarkResponsesUnsupported("p", "m", "fp", time.Now())
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

// TestModelStorePutRejectsSupersededGeneration pins the stale-generation write
// guard: after Restore installs expected fingerprints (boot/reload), a Put
// carrying a pre-reload fingerprint — a probe pass that captured the old
// config and finished after the swap — must be dropped, while a Put matching
// the current generation lands.
func TestModelStorePutRejectsSupersededGeneration(t *testing.T) {
	store := &ModelStore{}
	now := time.Unix(1_700_000_000, 0)
	store.Restore(nil, map[string]string{"p": "fp-old"})
	store.Put("p", "fp-old", "m", ModelProtocols{Chat: Yes}, now)
	if mp, ok := store.Get("p", "m"); !ok || mp.Chat != Yes {
		t.Fatalf("matching-generation Put dropped: %+v ok=%v", mp, ok)
	}

	// Reload: fingerprint changed, stale entry dropped, expectation re-armed.
	store.Restore(store.Snapshot(), map[string]string{"p": "fp-new"})
	if _, ok := store.Get("p", "m"); ok {
		t.Fatal("stale-fingerprint entry survived Restore")
	}
	store.Put("p", "fp-old", "m", ModelProtocols{Chat: No}, now)
	if mp, ok := store.Get("p", "m"); ok {
		t.Fatalf("superseded-generation Put resurrected stale verdict: %+v", mp)
	}
	store.Put("p", "fp-new", "m", ModelProtocols{Chat: Yes, Responses: No}, now)
	if mp, ok := store.Get("p", "m"); !ok || mp.Chat != Yes || mp.Responses != No {
		t.Fatalf("current-generation Put dropped: %+v ok=%v", mp, ok)
	}
}

// TestMergeOnUnknown pins the anti-flap merge rule: a transient Unknown leg
// (429/401/403/5xx/timeout — "no information") must retain the previously
// concluded verdict, while a concluded leg always overwrites. Without this,
// one rate-limited probe pass (the observed zcode/zhipu 429 storms) downgraded
// known-good yes verdicts to "? unknown" until a lucky later pass re-earned
// them.
func TestMergeOnUnknown(t *testing.T) {
	cases := []struct {
		name string
		old  ModelProtocols
		next ModelProtocols
		want ModelProtocols
	}{
		{
			name: "unknown keeps prior yes",
			old:  ModelProtocols{Chat: Yes, Anthropic: Yes, Responses: No},
			next: ModelProtocols{Chat: Unknown, Anthropic: Yes, Responses: Unknown},
			want: ModelProtocols{Chat: Yes, Anthropic: Yes, Responses: No},
		},
		{
			name: "concluded overwrites prior",
			old:  ModelProtocols{Chat: Yes, Anthropic: Yes, Responses: Yes},
			next: ModelProtocols{Chat: No, Anthropic: No, Responses: No},
			want: ModelProtocols{Chat: No, Anthropic: No, Responses: No},
		},
		{
			name: "unknown with no prior stays unknown",
			old:  ModelProtocols{},
			next: ModelProtocols{Chat: Unknown, Anthropic: Unknown, Responses: Unknown},
			want: ModelProtocols{Chat: Unknown, Anthropic: Unknown, Responses: Unknown},
		},
		{
			name: "mix: concluded wins where concluded, old kept where unknown",
			old:  ModelProtocols{Chat: No, Anthropic: Yes, Responses: No},
			next: ModelProtocols{Chat: Yes, Anthropic: Unknown, Responses: Unknown},
			want: ModelProtocols{Chat: Yes, Anthropic: Yes, Responses: No},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MergeOnUnknown(tc.old, tc.next); got != tc.want {
				t.Errorf("MergeOnUnknown(%+v, %+v) = %+v, want %+v", tc.old, tc.next, got, tc.want)
			}
		})
	}
}

// TestModelStorePutUnknownRetainsConcludedVerdict: a probe pass re-probing a
// provider (e.g. because one model gained an unknown leg, or a new model was
// added) must not downgrade previously concluded legs when the re-probe hits a
// transient failure.
func TestModelStorePutUnknownRetainsConcludedVerdict(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := &ModelStore{}
	store.Put("p", "fp", "m", ModelProtocols{Chat: Yes, Anthropic: Yes, Responses: No}, now)

	// Transient 429 on every leg: prior conclusions survive.
	store.Put("p", "fp", "m", ModelProtocols{Chat: Unknown, Anthropic: Unknown, Responses: Unknown}, now.Add(time.Minute))
	if mp, _ := store.Get("p", "m"); mp != (ModelProtocols{Chat: Yes, Anthropic: Yes, Responses: No}) {
		t.Errorf("after transient pass = %+v, want prior yes/yes/no retained", mp)
	}

	// A concluded correction still overwrites (the 404/no correction path).
	store.Put("p", "fp", "m", ModelProtocols{Chat: Yes, Anthropic: No, Responses: Unknown}, now.Add(2*time.Minute))
	if mp, _ := store.Get("p", "m"); mp != (ModelProtocols{Chat: Yes, Anthropic: No, Responses: No}) {
		t.Errorf("after concluded pass = %+v, want yes/no + retained no", mp)
	}

	// A verdict from a DIFFERENT fingerprint is a different endpoint's truth:
	// no merge, the fresh matrix wins wholesale.
	store.Put("p", "fp2", "m", ModelProtocols{Chat: Unknown, Anthropic: Unknown, Responses: Unknown}, now.Add(3*time.Minute))
	if mp, _ := store.Get("p", "m"); mp.Chat != Unknown || mp.Responses != Unknown {
		t.Errorf("after fingerprint change put = %+v, want fresh unknowns (no cross-fingerprint merge)", mp)
	}
}

// TestModelStoreReplaceProviderModelsMergesUnknown: models refresh replaces a
// provider's whole matrix, but transient-unknown legs in the fresh matrix
// retain the stored conclusions (same fingerprint), and models dropped from
// the candidate set still leave the store.
func TestModelStoreReplaceProviderModelsMergesUnknown(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := &ModelStore{}
	store.Put("p", "fp", "m1", ModelProtocols{Chat: Yes, Anthropic: Yes, Responses: No}, now)
	store.Put("p", "fp", "m2", ModelProtocols{Chat: Yes}, now)

	fresh := map[string]ModelProtocols{
		"m1": {Chat: Unknown, Anthropic: Unknown, Responses: Unknown}, // full 429 storm
		"m3": {Chat: Yes, Anthropic: Unknown, Responses: Unknown},     // new model
	}
	store.ReplaceProviderModels("p", "fp", fresh, now.Add(time.Minute))

	if mp, ok := store.Get("p", "m1"); !ok || mp != (ModelProtocols{Chat: Yes, Anthropic: Yes, Responses: No}) {
		t.Errorf("m1 = %+v (ok=%v), want prior verdicts retained through the unknown pass", mp, ok)
	}
	if mp, ok := store.Get("p", "m3"); !ok || mp.Chat != Yes || mp.Anthropic != Unknown {
		t.Errorf("m3 = %+v (ok=%v), want fresh conclusions as probed", mp, ok)
	}
	if _, ok := store.Get("p", "m2"); ok {
		t.Error("m2 absent from the fresh matrix must be dropped by the replace")
	}

	// Cross-fingerprint replace (config changed since the stored entry) is a
	// different endpoint's truth: no merge, unknowns land as unknowns.
	store.Put("p", "fp", "m1", ModelProtocols{Chat: Yes, Anthropic: Yes, Responses: No}, now)
	store.ReplaceProviderModels("p", "fp-other", map[string]ModelProtocols{
		"m1": {Chat: Unknown, Anthropic: Unknown, Responses: Unknown},
	}, now.Add(2*time.Minute))
	if mp, _ := store.Get("p", "m1"); mp.Chat != Unknown {
		t.Errorf("cross-fingerprint replace = %+v, want fresh unknowns (no merge)", mp)
	}
}
