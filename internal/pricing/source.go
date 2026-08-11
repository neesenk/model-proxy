package pricing

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// FetchFunc retrieves a catalog, optionally conditioned on a cached ETag.
type FetchFunc func(endpoint, etag string) (status int, body []byte, newEtag string, err error)

// RefreshOptions describes one synchronous catalog refresh.
type RefreshOptions struct {
	CacheFile string
	Endpoint  string
	Fetch     FetchFunc
	Force     bool
	TTL       time.Duration
	Warnings  io.Writer
}

// FetchHTTP retrieves the OpenRouter catalog. The default Transport owns
// compression negotiation so callers always receive a decoded body.
func FetchHTTP(endpoint, etag string) (int, []byte, string, error) {
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, nil, "", err
	}
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return 0, nil, "", err
	}
	defer response.Body.Close()

	newEtag := response.Header.Get("ETag")
	if response.StatusCode == http.StatusNotModified {
		return http.StatusNotModified, nil, newEtag, nil
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return response.StatusCode, nil, newEtag, err
	}
	return response.StatusCode, body, newEtag, nil
}

func loadCache(path string) (*Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var catalog Catalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, err
	}
	if catalog.ByModel == nil {
		catalog.ByModel = map[string]Entry{}
	}
	return &catalog, nil
}

func saveCache(path string, catalog *Catalog) error {
	data, err := json.Marshal(catalog)
	if err != nil {
		return err
	}
	return writeAtomic(path, data)
}

type atomicWriteOps struct {
	createTemp func(dir, pattern string) (*os.File, error)
	rename     func(oldPath, newPath string) error
}

// writeAtomic uses a unique temporary file in the target directory. Unique
// names keep independent model-proxy processes from clobbering one another's
// in-progress cache writes; rename ensures readers observe a complete JSON file.
func writeAtomic(path string, data []byte) error {
	return writeAtomicWith(path, data, atomicWriteOps{
		createTemp: os.CreateTemp,
		rename:     os.Rename,
	})
}

func writeAtomicWith(path string, data []byte, ops atomicWriteOps) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := ops.createTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return ops.rename(tmpPath, path)
}

// EnsureFresh returns a usable catalog while preserving stale-cache fallback:
// fresh uses the cache; stale or forced sends a conditional request; 304
// refreshes metadata; 200 rebuilds the catalog. A fetch error uses stale data
// when available, otherwise returns an empty catalog together with the error.
func EnsureFresh(options RefreshOptions) (*Catalog, error) {
	cached, _ := loadCache(options.CacheFile)
	if !options.Force && cached != nil && time.Since(cached.FetchedAt) < options.TTL {
		return cached, nil
	}

	etag := ""
	if cached != nil {
		etag = cached.Etag
	}
	if options.Fetch == nil {
		return Empty(), fmt.Errorf("pricing fetch function is nil")
	}
	status, body, newEtag, err := options.Fetch(options.Endpoint, etag)
	if err != nil {
		if cached != nil {
			if options.Warnings != nil {
				fmt.Fprintf(options.Warnings, "model-proxy: pricing source unreachable (%v); using catalog cached %s ago\n", err, ageString(cached.FetchedAt))
			}
			return cached, nil
		}
		return Empty(), fmt.Errorf("pricing source unreachable and no cached catalog: %w", err)
	}

	switch status {
	case http.StatusNotModified:
		if cached == nil {
			return Empty(), nil
		}
		cached.FetchedAt = time.Now()
		if newEtag != "" {
			cached.Etag = newEtag
		}
		_ = saveCache(options.CacheFile, cached)
		return cached, nil
	case http.StatusOK:
		catalog := parseOpenRouter(body)
		catalog.FetchedAt = time.Now()
		catalog.Etag = newEtag
		_ = saveCache(options.CacheFile, catalog)
		return catalog, nil
	default:
		if cached != nil {
			return cached, nil
		}
		return Empty(), nil
	}
}

func ageString(fetchedAt time.Time) string {
	elapsed := time.Since(fetchedAt)
	switch {
	case elapsed < time.Minute:
		return "just now"
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm", int(elapsed.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(elapsed.Hours()))
	}
}
