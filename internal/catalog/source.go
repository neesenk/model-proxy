package catalog

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	// DefaultTTL is the maximum age of a fresh on-disk models.dev projection.
	DefaultTTL = 24 * time.Hour
	// DefaultEndpoint is the public models.dev metadata endpoint.
	DefaultEndpoint = "https://models.dev/api.json"
)

// FetchFunc fetches a catalog response, preserving the HTTP status and ETag.
type FetchFunc func(endpoint, etag string) (status int, body []byte, newETag string, err error)

// RefreshOptions supplies all application-specific refresh dependencies.
type RefreshOptions struct {
	CacheFile string
	Endpoint  string
	Fetch     FetchFunc
	Force     bool
	TTL       time.Duration
	Warnings  io.Writer
}

// FetchHTTP is the production HTTP fetcher. Do not set Accept-Encoding: Go's
// transport automatically requests and transparently decodes gzip otherwise.
func FetchHTTP(endpoint, etag string) (int, []byte, string, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, nil, "", err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	newETag := resp.Header.Get("ETag")
	if resp.StatusCode == http.StatusNotModified {
		return http.StatusNotModified, nil, newETag, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, newETag, err
	}
	return resp.StatusCode, body, newETag, nil
}

// EnsureFresh loads the cached catalog when fresh, otherwise conditionally
// refreshes it. A stale cache remains usable on fetch, HTTP, or parse failures;
// without a usable cache those failures are returned together with an empty
// catalog so callers can choose their explicit degraded behavior.
func EnsureFresh(options RefreshOptions) (*Catalog, error) {
	if options.CacheFile == "" {
		return empty(), fmt.Errorf("catalog cache file is empty")
	}
	if options.Endpoint == "" {
		options.Endpoint = DefaultEndpoint
	}
	if options.Fetch == nil {
		options.Fetch = FetchHTTP
	}
	if options.TTL <= 0 {
		options.TTL = DefaultTTL
	}

	cached, cacheErr := load(options.CacheFile)
	if cacheErr != nil {
		warn(options.Warnings, "model-proxy: models.dev cache unreadable (%v); refreshing\n", cacheErr)
	}
	if !options.Force && cached != nil && time.Since(cached.fetchedAt) < options.TTL {
		return cached, nil
	}

	etag := ""
	if cached != nil {
		etag = cached.etag
	}
	status, body, newETag, err := options.Fetch(options.Endpoint, etag)
	if err != nil {
		return fallback(options, cached, fmt.Errorf("models.dev unreachable: %w", err))
	}
	switch status {
	case http.StatusNotModified:
		if cached == nil {
			return empty(), fmt.Errorf("models.dev returned 304 without a cached catalog")
		}
		cached.fetchedAt = time.Now()
		if newETag != "" {
			cached.etag = newETag
		}
		if err := save(options.CacheFile, cached); err != nil {
			warn(options.Warnings, "model-proxy: cannot persist refreshed models.dev cache: %v\n", err)
		}
		return cached, nil
	case http.StatusOK:
		fresh, err := parse(body)
		if err != nil {
			return fallback(options, cached, fmt.Errorf("models.dev returned malformed JSON: %w", err))
		}
		fresh.fetchedAt = time.Now()
		fresh.etag = newETag
		if err := save(options.CacheFile, fresh); err != nil {
			warn(options.Warnings, "model-proxy: cannot persist refreshed models.dev cache: %v\n", err)
		}
		return fresh, nil
	default:
		return fallback(options, cached, fmt.Errorf("models.dev returned HTTP %d", status))
	}
}

func fallback(options RefreshOptions, cached *Catalog, cause error) (*Catalog, error) {
	if cached != nil {
		warn(options.Warnings, "model-proxy: %v; using catalog cached %s ago\n", cause, age(cached.fetchedAt))
		return cached, nil
	}
	return empty(), cause
}

func warn(w io.Writer, format string, args ...any) {
	if w != nil {
		_, _ = fmt.Fprintf(w, format, args...)
	}
}

func age(fetchedAt time.Time) string {
	d := time.Since(fetchedAt)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

func load(path string) (*Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return unmarshalCatalog(data)
}

func save(path string, catalog *Catalog) error {
	data, err := catalog.marshalJSON()
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}

type atomicWriteOps struct {
	createTemp func(dir, pattern string) (*os.File, error)
	sync       func(*os.File) error
	rename     func(oldPath, newPath string) error
}

// fileSync flushes file contents to stable storage (power-loss safety).
func fileSync(f *os.File) error { return f.Sync() }

func atomicWrite(path string, data []byte) error {
	return atomicWriteWith(path, data, atomicWriteOps{
		createTemp: os.CreateTemp,
		rename:     os.Rename,
	})
}

func atomicWriteWith(path string, data []byte, ops atomicWriteOps) error {
	if ops.sync == nil {
		ops.sync = fileSync
	}
	dir := filepath.Dir(path)
	// 0700 matches the credential-store standard (internal/accounts): the
	// cache lives under ~/.model-proxy beside credential files.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := ops.createTemp(dir, ".models_cache-*")
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
	// Sync before rename: a rename alone may be reordered/replayed after a
	// crash with unflushed data, exposing a truncated cache.
	if err := ops.sync(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return ops.rename(tmpPath, path)
}
