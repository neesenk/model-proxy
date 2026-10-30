package providerstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"model-proxy/internal/config"
)

func validProvider() config.Provider {
	return config.Provider{
		Provider:      "static",
		OpenAIBaseURL: "https://api.example.com/v1",
		Models:        []string{"m1", "m2"},
	}
}

func TestSaveLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider.json")
	acme := validProvider()
	acme.Alias = map[string]string{"m1": "m1-exposed"}
	acme.CatalogAlias = map[string]string{"m1": "m1-catalog"}
	acme.Capabilities = map[string][]string{"m1": {"image", "tools"}}
	acme.PeakHours = config.PeakConfig{{Window: "09:00-18:00", Multiplier: 1.5}}
	acme.Headers = map[string]string{"x-trace": "literal"}
	acme.UsageURL = "https://api.example.com/usage"
	acme.Billing = "pay-as-you-go"
	acme.Priority = 3
	acme.ProxyURL = "off"
	in := map[string]config.Provider{"acme": acme}
	if err := Save(path, in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("roundtrip mismatch:\ngot  %#v\nwant %#v", got, in)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("provider.json mode = %v, want 0600", perm)
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if len(got) != 0 || got == nil {
		t.Fatalf("Load missing = %#v, want empty non-nil map", got)
	}
}

func TestLoadMalformedIsErrorNotReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load malformed succeeded — the authority store must never be silently discarded")
	}
}

func TestLoadFutureVersionIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider.json")
	if err := os.WriteFile(path, []byte(`{"version":99,"providers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "newer than this build supports") {
		t.Fatalf("Load future version err = %v, want version error", err)
	}
}

func TestLoadHandwrittenWithoutVersionAccepted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider.json")
	if err := os.WriteFile(path, []byte(`{"providers":{"acme":{"provider_id":"static","openai_base_url":"https://api.example.com/v1"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load versionless: %v", err)
	}
	if p, ok := got["acme"]; !ok || p.Provider != "static" || p.OpenAIBaseURL != "https://api.example.com/v1" {
		t.Fatalf("versionless load = %#v", got)
	}
}

func TestSaveRejectsInvalidProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider.json")
	bad := validProvider()
	bad.Provider = "not-a-provider-id"
	if err := Save(path, map[string]config.Provider{"acme": bad}); err == nil {
		t.Fatal("Save accepted unknown provider_id")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("rejected Save must not touch the store file (stat err = %v)", err)
	}
	missingBase := config.Provider{Provider: "static"}
	if err := Save(path, map[string]config.Provider{"acme": missingBase}); err == nil {
		t.Fatal("Save accepted provider without any base URL")
	}
}

func TestSaveKeepsBackupOfPreviousStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider.json")
	first := map[string]config.Provider{"acme": validProvider()}
	if err := Save(path, first); err != nil {
		t.Fatal(err)
	}
	second := map[string]config.Provider{"beta": validProvider()}
	if err := Save(path, second); err != nil {
		t.Fatal(err)
	}
	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	var doc fileDoc
	if err := json.Unmarshal(bak, &doc); err != nil {
		t.Fatalf("backup not json: %v", err)
	}
	if _, ok := doc.Providers["acme"]; !ok {
		t.Fatalf("backup = %#v, want previous store content", doc.Providers)
	}
}

func TestSaveNilProvidersWritesEmptyDoc(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider.json")
	if err := Save(path, nil); err != nil {
		t.Fatalf("Save(nil): %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("nil save roundtrip = %#v, want empty", got)
	}
}
