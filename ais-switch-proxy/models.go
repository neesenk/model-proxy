package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Model entry as returned by the gateway's /models endpoint (OpenAI-style).
type ModelEntry struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	OwnedBy       string `json:"owned_by"`
	ContextWindow int64  `json:"context_window"`
}

// modelsCacheFile is the on-disk cache: the model list plus a fetch timestamp.
type modelsCacheFile struct {
	CachedAt time.Time    `json:"cached_at"`
	Data     []ModelEntry `json:"data"`
}

// defaultModelsRefreshInterval is used when config leaves it blank.
const defaultModelsRefreshInterval = 1 * time.Hour

// modelsRefreshInterval parses cfg.ModelsRefreshInterval (e.g. "1h", "30m"),
// falling back to 1h on blank/invalid.
func modelsRefreshInterval(cfg *Config) time.Duration {
	if cfg.ModelsRefreshInterval == "" {
		return defaultModelsRefreshInterval
	}
	d, err := time.ParseDuration(cfg.ModelsRefreshInterval)
	if err != nil || d <= 0 {
		return defaultModelsRefreshInterval
	}
	return d
}

// modelsCachePath resolves the cache file path: config models_cache_file, else
// next to the SSO cookie file (same dir), else next to the config file.
func modelsCachePath(cfg *Config) string {
	if cfg.ModelsCacheFile != "" {
		return expandPath(cfg.ModelsCacheFile)
	}
	for _, base := range []string{cfg.Auth.SSOCookieFile, cfg.LogFile} {
		if base != "" {
			return filepath.Join(filepath.Dir(base), "ais-switch-proxy-models.json")
		}
	}
	abs, err := filepath.Abs("config.yaml")
	if err == nil {
		return filepath.Join(filepath.Dir(abs), "ais-switch-proxy-models.json")
	}
	return "ais-switch-proxy-models.json"
}

// cqpUpstream returns the baseURL of the first cqp-authed provider, or "".
func cqpUpstream(cfg *Config) string {
	for _, prov := range cfg.Providers {
		if prov.Auth == "cqp" {
			return prov.BaseURL
		}
	}
	return ""
}

// fetchModels mints a CQP key and GETs <upstream>/models, returning the list.
func fetchModels(cfg *Config) ([]ModelEntry, error) {
	upstream := cqpUpstream(cfg)
	if upstream == "" {
		return nil, fmt.Errorf("no cqp route configured (need a route with auth: cqp)")
	}
	p := newCQPProvider(cfg.Auth)
	key, err := p.key()
	if err != nil {
		return nil, fmt.Errorf("mint cqp key: %w", err)
	}
	url := strings.TrimRight(upstream, "/") + "/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch models: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetch models: HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var v struct {
		Object string      `json:"object"`
		Data   []ModelEntry `json:"data"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("parse models response: %w", err)
	}
	return v.Data, nil
}

// loadModelsCache reads the cache; returns nil (no error) if absent.
func loadModelsCache(path string) (*modelsCacheFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var c modelsCacheFile
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, nil
}

// saveModelsCache writes the cache (0600, parent dir 0700).
func saveModelsCache(path string, entries []ModelEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	c := modelsCacheFile{CachedAt: time.Now(), Data: entries}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// refreshModelsCache fetches fresh models and writes the cache. Returns the
// entries. Errors are logged (in serve) or returned (in CLI).
func refreshModelsCache(cfg *Config, path string) ([]ModelEntry, error) {
	entries, err := fetchModels(cfg)
	if err != nil {
		return nil, err
	}
	if err := saveModelsCache(path, entries); err != nil {
		return nil, fmt.Errorf("write cache: %w", err)
	}
	return entries, nil
}

// startModelsRefresher runs a background loop (for `serve`) that refreshes the
// models cache on the configured interval. First refresh is immediate so the
// cache is warm before serving; subsequent ones are spaced. Never panics.
func startModelsRefresher(cfg *Config) {
	path := modelsCachePath(cfg)
	interval := modelsRefreshInterval(cfg)
	go func() {
		// Warm the cache immediately (best-effort).
		if _, err := refreshModelsCache(cfg, path); err != nil {
			log.Printf("[models] initial refresh failed: %v", err)
		} else {
			log.Printf("[models] cache refreshed (interval=%s, file=%s)", interval, path)
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			if _, err := refreshModelsCache(cfg, path); err != nil {
				log.Printf("[models] refresh failed: %v", err)
			}
		}
	}()
}

// cmdModels prints the gateway model list. If the cache is fresh (younger than
// the refresh interval), prints the cache; otherwise fetches fresh, updates the
// cache, and prints. --refresh forces a fresh fetch.
func cmdModels(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	force := false
	for _, a := range args {
		if a == "--refresh" {
			force = true
		}
	}
	path := modelsCachePath(cfg)
	interval := modelsRefreshInterval(cfg)

	var entries []ModelEntry
	var cachedAt time.Time
	if !force {
		c, err := loadModelsCache(path)
		if err != nil {
			log.Printf("warn: read cache %s: %v", path, err)
		}
		if c != nil && time.Since(c.CachedAt) < interval {
			entries, cachedAt = c.Data, c.CachedAt
		}
	}
	if entries == nil {
		fmt.Fprintln(os.Stderr, "Refreshing model list from gateway...")
		got, err := refreshModelsCache(cfg, path)
		if err != nil {
			log.Fatal(err)
		}
		entries = got
		cachedAt = time.Now()
	}

	printModels(entries, cachedAt, path)
}

// printModels renders the model list with color (stdout), enriched with pricing
// metadata (display name, per-million costs) from the embedded pricing table.
func printModels(entries []ModelEntry, cachedAt time.Time, path string) {
	if len(entries) == 0 {
		fmt.Println(cYellow("(no models)"))
		return
	}
	// Header
	fmt.Printf("%s  %s  %s  %s  %s\n",
		cDim(pad("ID", 22)), cDim(pad("NAME", 18)),
		cDim(pad("IN$/M", 8)), cDim(pad("OUT$/M", 8)), cDim(pad("CTX", 10)))
	for _, m := range entries {
		name, in, out := "—", "—", "—"
		if p := lookupPricing(m.ID); p != nil {
			if p.DisplayName != "" {
				name = p.DisplayName
			}
			if p.InputCostPerMillion != "" {
				in = p.InputCostPerMillion
			}
			if p.OutputCostPerMillion != "" {
				out = p.OutputCostPerMillion
			}
		}
		ctx := "—"
		if m.ContextWindow > 0 {
			ctx = fmt.Sprintf("%d", m.ContextWindow)
		}
		fmt.Printf("%s  %s  %s  %s  %s\n",
			cCyan(pad(m.ID, 22)), cGreen(pad(name, 18)),
			cGray(pad(in, 8)), cGray(pad(out, 8)), cGray(pad(ctx, 10)))
	}
	fmt.Printf("\n%s %d models  %s %s\n",
		cDim("count:"), len(entries),
		cDim("cached:"), cGray(cachedAt.Format("2006-01-02 15:04:05")))
	fmt.Printf("%s %s\n", cDim("cache:"), cGray(path))
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}
