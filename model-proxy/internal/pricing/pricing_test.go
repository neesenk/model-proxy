package pricing

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const openRouterFixture = `{
  "data": [
    {"id": "openrouter/deepseek-v4-pro", "pricing": {
      "prompt": "0.0000099", "completion": "0.0000099"
    }},
    {"id": "deepseek/deepseek-v4-pro", "pricing": {
      "prompt": "0.0000011", "completion": "0.0000028",
      "input_cache_read": "0.0000001", "input_cache_write": "0.0000002"
    }},
    {"id": "z-ai/glm-4.6", "pricing": {
      "prompt": "0.0000009", "completion": "0.0000018"
    }},
    {"id": "~openai/gpt-5.6-luna", "pricing": {
      "prompt": "0.000005", "completion": "0.000015"
    }}
  ]
}`

func TestParseOpenRouterCanonicalVendorAndAliases(t *testing.T) {
	catalog := parseOpenRouter([]byte(openRouterFixture))
	if len(catalog.ByModel) != 3 {
		t.Fatalf("catalog keys = %v, want 3 entries", catalogKeys(catalog.ByModel))
	}
	deepseek := catalog.ByModel["deepseek-v4-pro"]
	if deepseek != (Entry{
		Prompt: 1.1e-6, Completion: 2.8e-6,
		CacheRead: 1e-7, CacheWrite: 2e-7,
	}) {
		t.Errorf("canonical vendor price = %+v", deepseek)
	}
	if _, ok := catalog.ByModel["gpt-5.6-luna"]; !ok {
		t.Errorf("dynamic alias was not normalized: %v", catalogKeys(catalog.ByModel))
	}
	if _, ok := catalog.ByModel["~openai/gpt-5.6-luna"]; ok {
		t.Error("catalog retained an unnormalized model id")
	}
}

func TestParseOpenRouterMalformedInputs(t *testing.T) {
	if got := parseOpenRouter([]byte(`not-json`)); got == nil || len(got.ByModel) != 0 {
		t.Errorf("malformed JSON = %+v, want non-nil empty catalog", got)
	}
	catalog := parseOpenRouter([]byte(`{
	  "data": [
	    {"id": "vendor/", "pricing": {"prompt": "1"}},
	    {"id": "vendor/model", "pricing": {
	      "prompt": "bad", "completion": " ", "input_cache_read": "also-bad"
	    }}
	  ]
	}`))
	if len(catalog.ByModel) != 1 {
		t.Fatalf("catalog after empty-name skip = %+v", catalog.ByModel)
	}
	if got := catalog.ByModel["model"]; got != (Entry{}) {
		t.Errorf("malformed decimal fields = %+v, want zero entry", got)
	}
}

func TestCatalogLookupIsExactAndNilSafe(t *testing.T) {
	catalog := parseOpenRouter([]byte(openRouterFixture))
	if entry, ok := catalog.Lookup("glm-4.6"); !ok || entry.Prompt != 0.9e-6 {
		t.Errorf("exact lookup: entry=%+v ok=%v", entry, ok)
	}
	if _, ok := catalog.Lookup("z-ai/glm-4.6"); ok {
		t.Error("lookup must not apply vendor-prefix or suffix fallback")
	}
	var nilCatalog *Catalog
	if _, ok := nilCatalog.Lookup("glm-4.6"); ok {
		t.Error("nil catalog lookup should be unpriced")
	}
}

func TestResolveOverridePrecedenceAndUnits(t *testing.T) {
	catalog := parseOpenRouter([]byte(openRouterFixture))
	overrides := map[string]Override{
		"glm-4.6": {Input: 9, Output: 18, CacheRead: 1, CacheWrite: 2},
	}
	entry, ok := Resolve(overrides, catalog, "glm-4.6")
	if !ok || entry != (Entry{
		Prompt: 9e-6, Completion: 18e-6, CacheRead: 1e-6, CacheWrite: 2e-6,
	}) {
		t.Errorf("override resolution: entry=%+v ok=%v", entry, ok)
	}
	if entry, ok = Resolve(overrides, catalog, "deepseek-v4-pro"); !ok || entry.Prompt != 1.1e-6 {
		t.Errorf("catalog fallback: entry=%+v ok=%v", entry, ok)
	}
	if _, ok = Resolve(overrides, catalog, "unknown"); ok {
		t.Error("unknown model should remain unpriced")
	}
}

func TestComputeCostIncludesEveryTokenClass(t *testing.T) {
	entry := Entry{Prompt: 1.1e-6, Completion: 2.8e-6, CacheRead: 0.1e-6, CacheWrite: 0.2e-6}
	got := ComputeCost(1_000_000, 500_000, 200_000, 100_000, entry)
	if got < 2.539999 || got > 2.540001 {
		t.Errorf("cost = %.9f, want 2.54", got)
	}
}

func TestCacheRoundTripAndMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pricing_cache.json")
	want := &Catalog{
		FetchedAt: time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC),
		Etag:      `"abc"`,
		ByModel:   map[string]Entry{"glm-4.6": {Prompt: 0.9e-6, Completion: 1.8e-6}},
	}
	if err := saveCache(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Etag != want.Etag || !got.FetchedAt.Equal(want.FetchedAt) ||
		got.ByModel["glm-4.6"] != want.ByModel["glm-4.6"] {
		t.Errorf("cache round trip = %+v, want %+v", got, want)
	}

	missing, err := loadCache(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil || missing != nil {
		t.Errorf("missing cache = (%+v, %v), want (nil, nil)", missing, err)
	}
}

func TestConcurrentCacheReplacementRemainsWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pricing_cache.json")
	catalogs := []*Catalog{
		{FetchedAt: time.Now(), Etag: `"a"`, ByModel: map[string]Entry{"model": {Prompt: 1}}},
		{FetchedAt: time.Now(), Etag: `"b"`, ByModel: map[string]Entry{"model": {Prompt: 2}}},
	}
	if err := saveCache(path, catalogs[0]); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errs := make(chan error, 8)
	var wait sync.WaitGroup
	for writer := range 6 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			for iteration := range 40 {
				if err := saveCache(path, catalogs[(writer+iteration)%len(catalogs)]); err != nil {
					errs <- err
					return
				}
				loaded, err := loadCache(path)
				if err != nil {
					errs <- err
					return
				}
				prompt := loaded.ByModel["model"].Prompt
				if prompt != 1 && prompt != 2 {
					errs <- errors.New("reader observed a partial or foreign catalog")
					return
				}
			}
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("temporary cache files leaked: %v", matches)
	}
}

func TestAtomicWriteKeepsOldCacheUntilRenameAndOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pricing_cache.json")
	oldCatalog := &Catalog{
		FetchedAt: time.Now(), Etag: `"old"`,
		ByModel: map[string]Entry{"model": {Prompt: 1}},
	}
	newCatalog := &Catalog{
		FetchedAt: time.Now(), Etag: `"new"`,
		ByModel: map[string]Entry{"model": {Prompt: 2}},
	}
	if err := saveCache(path, oldCatalog); err != nil {
		t.Fatal(err)
	}
	newData, err := json.Marshal(newCatalog)
	if err != nil {
		t.Fatal(err)
	}

	renameStarted := make(chan string, 1)
	releaseRename := make(chan struct{})
	writeResult := make(chan error, 1)
	go func() {
		writeResult <- writeAtomicWith(path, newData, atomicWriteOps{
			createTemp: os.CreateTemp,
			rename: func(oldPath, newPath string) error {
				renameStarted <- oldPath
				<-releaseRename
				return os.Rename(oldPath, newPath)
			},
		})
	}()

	tmpPath := <-renameStarted
	if tmpPath == path+".tmp" || filepath.Dir(tmpPath) != filepath.Dir(path) {
		t.Errorf("temporary path = %q, want unique sibling of target", tmpPath)
	}
	duringWrite, err := loadCache(path)
	if err != nil || duringWrite.Etag != `"old"` || duringWrite.ByModel["model"].Prompt != 1 {
		t.Errorf("target before rename = %+v, err=%v; want complete old catalog", duringWrite, err)
	}
	tmpCatalog, err := loadCache(tmpPath)
	if err != nil || tmpCatalog.Etag != `"new"` || tmpCatalog.ByModel["model"].Prompt != 2 {
		t.Errorf("temporary file before rename = %+v, err=%v; want complete new catalog", tmpCatalog, err)
	}
	close(releaseRename)
	if err := <-writeResult; err != nil {
		t.Fatal(err)
	}
	afterRename, err := loadCache(path)
	if err != nil || afterRename.Etag != `"new"` || afterRename.ByModel["model"].Prompt != 2 {
		t.Errorf("target after rename = %+v, err=%v; want complete new catalog", afterRename, err)
	}

	if err := saveCache(path, oldCatalog); err != nil {
		t.Fatal(err)
	}
	renameFailure := errors.New("injected rename failure")
	err = writeAtomicWith(path, newData, atomicWriteOps{
		createTemp: os.CreateTemp,
		rename: func(_, _ string) error {
			return renameFailure
		},
	})
	if !errors.Is(err, renameFailure) {
		t.Fatalf("rename failure = %v, want %v", err, renameFailure)
	}
	afterFailure, err := loadCache(path)
	if err != nil || afterFailure.Etag != `"old"` || afterFailure.ByModel["model"].Prompt != 1 {
		t.Errorf("target after failed rename = %+v, err=%v; want complete old catalog", afterFailure, err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("temporary cache files leaked after rename failure: %v", matches)
	}
}

func TestFetchHTTPSendsETagAndReturnsResponseMetadata(t *testing.T) {
	seenETag := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		seenETag <- request.Header.Get("If-None-Match")
		response.Header().Set("ETag", `"new"`)
		response.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()

	status, body, etag, err := FetchHTTP(server.URL, `"old"`)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusNotModified || body != nil || etag != `"new"` {
		t.Errorf("fetch result = status=%d body=%q etag=%q", status, body, etag)
	}
	if got := <-seenETag; got != `"old"` {
		t.Errorf("If-None-Match = %q, want old ETag", got)
	}
	if _, _, _, err := FetchHTTP("://invalid", ""); err == nil {
		t.Error("invalid endpoint should fail request construction")
	}
}

func TestEnsureFreshLifecycle(t *testing.T) {
	t.Run("200 rebuilds and persists exact catalog", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pricing_cache.json")
		fetch := assertingFetch(t, "http://prices.test", "", http.StatusOK, []byte(openRouterFixture), `"new"`)
		catalog, err := EnsureFresh(RefreshOptions{
			CacheFile: path, Endpoint: "http://prices.test", Fetch: fetch, TTL: DefaultTTL,
		})
		if err != nil {
			t.Fatal(err)
		}
		entry := catalog.ByModel["deepseek-v4-pro"]
		if catalog.Etag != `"new"` || entry != (Entry{
			Prompt: 1.1e-6, Completion: 2.8e-6, CacheRead: 1e-7, CacheWrite: 2e-7,
		}) {
			t.Errorf("rebuilt catalog = %+v, entry=%+v", catalog, entry)
		}
		persisted, err := loadCache(path)
		if err != nil || persisted == nil || persisted.Etag != `"new"` {
			t.Errorf("persisted catalog = %+v, err=%v", persisted, err)
		}
	})

	t.Run("fresh cache avoids fetch", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pricing_cache.json")
		want := &Catalog{FetchedAt: time.Now(), Etag: `"fresh"`, ByModel: map[string]Entry{"m": {Prompt: 1}}}
		if err := saveCache(path, want); err != nil {
			t.Fatal(err)
		}
		called := false
		catalog, err := EnsureFresh(RefreshOptions{
			CacheFile: path,
			Endpoint:  "unused",
			TTL:       DefaultTTL,
			Fetch: func(_, _ string) (int, []byte, string, error) {
				called = true
				return 0, nil, "", nil
			},
		})
		if err != nil || called || catalog.Etag != want.Etag {
			t.Errorf("fresh result = %+v, err=%v, fetchCalled=%v", catalog, err, called)
		}
	})

	t.Run("stale fetch error warns and preserves cache", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pricing_cache.json")
		before := time.Now().Add(-2 * DefaultTTL)
		want := &Catalog{FetchedAt: before, Etag: `"old"`, ByModel: map[string]Entry{"m": {Prompt: 1}}}
		if err := saveCache(path, want); err != nil {
			t.Fatal(err)
		}
		var warning bytes.Buffer
		catalog, err := EnsureFresh(RefreshOptions{
			CacheFile: path, Endpoint: "http://prices.test", TTL: DefaultTTL, Warnings: &warning,
			Fetch: func(endpoint, etag string) (int, []byte, string, error) {
				if endpoint != "http://prices.test" || etag != `"old"` {
					t.Errorf("refresh inputs = endpoint=%q etag=%q", endpoint, etag)
				}
				return 0, nil, "", errors.New("offline")
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if catalog.Etag != `"old"` || !catalog.FetchedAt.Equal(before) || catalog.ByModel["m"].Prompt != 1 {
			t.Errorf("stale fallback changed catalog: %+v", catalog)
		}
		if !strings.Contains(warning.String(), "pricing source unreachable") ||
			!strings.Contains(warning.String(), "using catalog cached") {
			t.Errorf("stale fallback warning = %q", warning.String())
		}
	})

	t.Run("304 refreshes time and optional ETag", func(t *testing.T) {
		for _, test := range []struct {
			name    string
			newETag string
			want    string
		}{
			{name: "replace ETag", newETag: `"new"`, want: `"new"`},
			{name: "preserve ETag", newETag: "", want: `"old"`},
		} {
			t.Run(test.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "pricing_cache.json")
				before := time.Now().Add(-2 * DefaultTTL)
				stale := &Catalog{FetchedAt: before, Etag: `"old"`, ByModel: map[string]Entry{"m": {Prompt: 1}}}
				if err := saveCache(path, stale); err != nil {
					t.Fatal(err)
				}
				catalog, err := EnsureFresh(RefreshOptions{
					CacheFile: path, Endpoint: "http://prices.test", TTL: DefaultTTL,
					Fetch: assertingFetch(t, "http://prices.test", `"old"`, http.StatusNotModified, nil, test.newETag),
				})
				if err != nil {
					t.Fatal(err)
				}
				if !catalog.FetchedAt.After(before) || catalog.Etag != test.want ||
					catalog.ByModel["m"].Prompt != 1 {
					t.Errorf("304 result = %+v", catalog)
				}
				persisted, err := loadCache(path)
				if err != nil || persisted == nil || !persisted.FetchedAt.After(before) || persisted.Etag != test.want {
					t.Errorf("persisted 304 result = %+v, err=%v", persisted, err)
				}
			})
		}
	})

	t.Run("304 without cache is empty", func(t *testing.T) {
		catalog, err := EnsureFresh(RefreshOptions{
			CacheFile: filepath.Join(t.TempDir(), "missing.json"),
			Endpoint:  "http://prices.test",
			TTL:       DefaultTTL,
			Fetch:     assertingFetch(t, "http://prices.test", "", http.StatusNotModified, nil, `"new"`),
		})
		if err != nil || catalog == nil || len(catalog.ByModel) != 0 {
			t.Errorf("304 without cache = %+v, err=%v", catalog, err)
		}
	})

	t.Run("force bypasses fresh cache", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pricing_cache.json")
		fresh := &Catalog{FetchedAt: time.Now(), Etag: `"old"`, ByModel: map[string]Entry{"glm-4.6": {Prompt: 99}}}
		if err := saveCache(path, fresh); err != nil {
			t.Fatal(err)
		}
		catalog, err := EnsureFresh(RefreshOptions{
			CacheFile: path, Endpoint: "http://prices.test", TTL: DefaultTTL, Force: true,
			Fetch: assertingFetch(t, "http://prices.test", `"old"`, http.StatusOK, []byte(openRouterFixture), `"new"`),
		})
		if err != nil || catalog.ByModel["glm-4.6"].Prompt != 0.9e-6 || catalog.Etag != `"new"` {
			t.Errorf("forced refresh = %+v, err=%v", catalog, err)
		}
	})

	t.Run("unexpected status keeps stale or returns empty", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pricing_cache.json")
		stale := &Catalog{FetchedAt: time.Now().Add(-2 * DefaultTTL), Etag: `"old"`, ByModel: map[string]Entry{"m": {Prompt: 1}}}
		if err := saveCache(path, stale); err != nil {
			t.Fatal(err)
		}
		catalog, err := EnsureFresh(RefreshOptions{
			CacheFile: path, Endpoint: "http://prices.test", TTL: DefaultTTL,
			Fetch: assertingFetch(t, "http://prices.test", `"old"`, http.StatusServiceUnavailable, nil, ""),
		})
		if err != nil || catalog.Etag != `"old"` || catalog.ByModel["m"].Prompt != 1 {
			t.Errorf("unexpected status with stale cache = %+v, err=%v", catalog, err)
		}
		catalog, err = EnsureFresh(RefreshOptions{
			CacheFile: filepath.Join(t.TempDir(), "missing.json"),
			Endpoint:  "http://prices.test",
			TTL:       DefaultTTL,
			Fetch:     assertingFetch(t, "http://prices.test", "", http.StatusServiceUnavailable, nil, ""),
		})
		if err != nil || catalog == nil || len(catalog.ByModel) != 0 {
			t.Errorf("unexpected status without cache = %+v, err=%v", catalog, err)
		}
	})

	t.Run("initial fetch failure returns usable empty plus error", func(t *testing.T) {
		catalog, err := EnsureFresh(RefreshOptions{
			CacheFile: filepath.Join(t.TempDir(), "missing.json"),
			Endpoint:  "http://prices.test",
			TTL:       DefaultTTL,
			Fetch: func(_, _ string) (int, []byte, string, error) {
				return 0, nil, "", errors.New("offline")
			},
		})
		if err == nil || catalog == nil || len(catalog.ByModel) != 0 {
			t.Errorf("initial failure = %+v, err=%v", catalog, err)
		}
	})

	t.Run("nil fetch is rejected", func(t *testing.T) {
		catalog, err := EnsureFresh(RefreshOptions{
			CacheFile: filepath.Join(t.TempDir(), "missing.json"),
			TTL:       DefaultTTL,
		})
		if err == nil || catalog == nil || len(catalog.ByModel) != 0 {
			t.Errorf("nil fetch = %+v, err=%v", catalog, err)
		}
	})
}

func assertingFetch(
	t *testing.T,
	wantEndpoint string,
	wantETag string,
	status int,
	body []byte,
	newETag string,
) FetchFunc {
	t.Helper()
	return func(endpoint, etag string) (int, []byte, string, error) {
		t.Helper()
		if endpoint != wantEndpoint || etag != wantETag {
			t.Errorf("fetch inputs = endpoint=%q etag=%q, want endpoint=%q etag=%q",
				endpoint, etag, wantEndpoint, wantETag)
		}
		return status, body, newETag, nil
	}
}

func catalogKeys(entries map[string]Entry) []string {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	return keys
}
