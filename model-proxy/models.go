package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Model entry as returned by the gateway's /models endpoint (OpenAI-style).
type ModelEntry struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	OwnedBy       string `json:"owned_by"`
	ContextWindow int64  `json:"context_window"`
}

// cmdModels handles:
//
//	models              — list all models from all providers (config + models.dev supplement)
//	models <provider>   — list models for one provider
//	models pull         — force-refresh the models.dev catalog cache
//	models refresh <provider> — fetch live model list from a provider's server
func cmdModels(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	rest := nonFlagArgs(args)
	if len(rest) > 0 && rest[0] == "pull" {
		// models pull — force-refresh the global models.dev catalog cache.
		cat, ferr := ensureCatalogFresh(cachePath(), modelsDevEndpoint(), realModelsDevFetch, true)
		if ferr != nil {
			log.Fatal(ferr)
		}
		fmt.Printf("models.dev catalog refreshed: %d unique models, etag %s\n", len(cat.ByName), cat.Etag)
		return
	}
	if len(rest) > 0 && rest[0] == "refresh" {
		// models refresh <provider>
		if len(rest) < 2 {
			fmt.Println("usage: model-proxy models refresh <provider>")
			fmt.Println("available providers:")
			for name := range cfg.Providers {
				fmt.Printf("  %s\n", name)
			}
			return
		}
		provName := rest[1]
		if _, ok := cfg.Providers[provName]; !ok {
			log.Fatalf("unknown provider %q; available: %s", provName, providerNames(cfg))
		}
		fmt.Fprintf(os.Stderr, "Refreshing models from %s...\n", provName)
		entries, err := fetchProviderModels(cfg, provName)
		if err != nil {
			log.Fatal(err)
		}
		// Persist any newly-discovered model names into config.yaml (append-only;
		// never removes — the operator may have hand-added models). Metadata is
		// runtime-sourced from models.dev, so only names are written.
		existing := cfg.Providers[provName].Models
		have := make(map[string]bool, len(existing))
		for _, n := range existing {
			have[n] = true
		}
		var added []string
		for _, e := range entries {
			if !have[e.ID] {
				added = append(added, e.ID)
			}
		}
		if len(added) > 0 {
			sort.Strings(added)
			updated := append(append([]string{}, existing...), added...)
			if err := writeProviderModels(configPath(args), provName, updated); err != nil {
				log.Fatalf("writing new models to config: %v", err)
			}
			fmt.Fprintf(os.Stderr, "added %d new model(s) to config: %v\n", len(added), added)
		}
		printProviderModels(provName, entries)
		return
	}
	// models [provider] — config + models.dev supplement
	provFilter := ""
	if len(rest) > 0 {
		provFilter = rest[0]
		if _, ok := cfg.Providers[provFilter]; !ok {
			log.Fatalf("unknown provider %q; available: %s", provFilter, providerNames(cfg))
		}
	}
	cat, _ := ensureCatalogFresh(cachePath(), modelsDevEndpoint(), realModelsDevFetch, false)
	meta, sources := hydrateModels(cfg, cat)
	printAllModels(cfg, provFilter, meta, sources)
	// Warn about unrouted models auto-routed to one of several logged-in providers
	// (ambiguity). Single-provider implicit routes are silent.
	if _, warnings := synthesizeImplicitRoutes(cfg); len(warnings) > 0 {
		fmt.Fprintf(os.Stderr, "\n%s implicit-route warnings:\n", cYellow("⚠"))
		for _, w := range warnings {
			fmt.Fprintf(os.Stderr, "  %s\n", w)
		}
	}
}

// printAllModels prints all models with their hydrated metadata. `meta` maps
// provider→model→metadata (nil in legacy callers → names shown without ctx/out).
// `sources` drives a trailing SRC tag: models.dev / default.
func printAllModels(cfg *Config, provFilter string, meta map[string]map[string]ProviderModel, sources map[string]map[string]modelSource) {
	names := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		if provFilter != "" && n != provFilter {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)

	fmt.Printf("%s  %s  %s  %s  %s  %s  %s\n",
		cDim(pad("PROVIDER", 12)), cDim(pad("MODEL ID", 22)),
		cDim(pad("NAME", 20)), cDim(pad("CTX", 10)),
		cDim(pad("OUTPUT", 8)), cDim(pad("MODALITIES", 16)), cDim(pad("SRC", 10)))
	for _, pn := range names {
		// Effective model set: hydrated metadata keys ∪ the config name list
		// (config names show even when meta is nil — e.g. legacy callers).
		idSet := map[string]bool{}
		if meta != nil && meta[pn] != nil {
			for mid := range meta[pn] {
				idSet[mid] = true
			}
		}
		for _, mid := range cfg.Providers[pn].Models {
			idSet[mid] = true
		}
		modelIDs := make([]string, 0, len(idSet))
		for mid := range idSet {
			modelIDs = append(modelIDs, mid)
		}
		sort.Strings(modelIDs)
		for _, mid := range modelIDs {
			var m ProviderModel
			if meta != nil && meta[pn] != nil {
				m = meta[pn][mid]
			}
			name := mid
			ctx := "—"
			if m.Context > 0 {
				ctx = fmt.Sprintf("%d", m.Context)
			}
			out := "—"
			if m.Output > 0 {
				out = fmt.Sprintf("%d", m.Output)
			}
			mod := "text"
			if len(m.Modalities.Input) > 0 {
				mod = strings.Join(m.Modalities.Input, "/")
			}
			src := ""
			if sources != nil {
				switch sources[pn][mid] {
				case srcModelsDev:
					src = "models.dev"
				case srcDefault:
					src = "default"
				}
			}
			fmt.Printf("%s  %s  %s  %s  %s  %s  %s\n",
				cBlue(pad(pn, 12)), cCyan(pad(mid, 22)),
				cGreen(pad(name, 20)), cGray(pad(ctx, 10)),
				cGray(pad(out, 8)), cGray(pad(mod, 16)), cGray(pad(src, 10)))
		}
	}
}

// printProviderModels prints models fetched live from a provider's /models endpoint.
func printProviderModels(provName string, entries []ModelEntry) {
	if len(entries) == 0 {
		fmt.Println(cYellow("(no models)"))
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	fmt.Printf("%s  %s  %s  %s\n",
		cDim(pad("MODEL ID", 22)), cDim(pad("NAME", 20)),
		cDim(pad("CTX", 10)), cDim(pad("OWNED BY", 12)))
	for _, m := range entries {
		name := m.ID
		ctx := "—"
		if m.ContextWindow > 0 {
			ctx = fmt.Sprintf("%d", m.ContextWindow)
		}
		fmt.Printf("%s  %s  %s  %s\n",
			cCyan(pad(m.ID, 22)), cGreen(pad(name, 20)),
			cGray(pad(ctx, 10)), cGray(pad(m.OwnedBy, 12)))
	}
	fmt.Printf("\n%s %s: %d models\n", cDim("provider:"), provName, len(entries))
}

// fetchProviderModels fetches the live model list from a provider. Delegates to
// the provider's FetchModels() implementation (which lives in the provider/ layer).
func fetchProviderModels(cfg *Config, provName string) ([]ModelEntry, error) {
	ids, err := refreshProviderModels(cfg, provName)
	if err != nil {
		return nil, err
	}
	entries := make([]ModelEntry, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, ModelEntry{ID: id, Object: "model", OwnedBy: provName})
	}
	return entries, nil
}

// writeProviderModels rewrites providers.<provName>.models to `names` in
// configFile, preserving comments/order elsewhere via a yaml.Node round-trip
// (only the models sequence is re-encoded). Validates the result before writing
// and takes a best-effort .bak. CLI-safe (no daemon reload — `models` is not on
// the proxy hot path, so a running daemon picks up the list on its next reload).
func writeProviderModels(configFile, provName string, names []string) error {
	root, err := loadConfigNode(configFile)
	if err != nil {
		return err
	}
	p := childMap(childMap(root, "providers"), provName)
	if p == nil {
		return fmt.Errorf("provider %q not found in %s", provName, configFile)
	}
	setChildNode(p, "models", mustEncode(names))
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return err
	}
	enc.Close()
	data := buf.Bytes()
	if _, err := LoadConfigFromBytes(configFile, data); err != nil {
		return fmt.Errorf("rewritten config invalid: %w", err)
	}
	backupConfig(configFile, configFile+".bak")
	return atomicWrite(configFile, data)
}

// refreshProviderModels fetches the live model list for a provider exactly once.
// If `provName` is a pooled parent (≥2 accounts in its credential pool) it uses
// the pool's first virtual by account-id order; else it uses the plain provider
// name. The model list is per-upstream, not per-account, so one fetch is correct
// and sufficient — fanning out across the pool would multiply upstream calls
// without changing the result.
//
// Returns the raw model IDs; callers that need ModelEntry wrapping (e.g. the
// `models refresh` CLI display) use fetchProviderModels, which delegates here.
func refreshProviderModels(cfg *Config, provName string) ([]string, error) {
	if _, ok := cfg.Providers[provName]; !ok {
		return nil, fmt.Errorf("unknown provider %q", provName)
	}
	provMap, _, _ := buildProviders(cfg)
	target := provName
	if vids, pooled := poolVirtuals(cfg, provName); pooled {
		target = vids[0] // first virtual by account-id order
	}
	impl, ok := provMap[target]
	if !ok || impl == nil {
		return nil, fmt.Errorf("provider %q not available (not logged in?)", provName)
	}
	return impl.FetchModels()
}

// poolVirtuals returns the sorted virtual ids ("name#<accountID>") for a pooled
// parent and true when the provider has ≥2 accounts in its credential pool; or
// (nil, false) for a single-account / not-logged-in / unknown provider. It reads
// the pool file directly via loadPool (no Proxy required) so `models refresh`
// and `doctor` can resolve the pool without a running daemon. The returned ids
// are sorted so callers can deterministically pick the "first" virtual.
func poolVirtuals(cfg *Config, name string) ([]string, bool) {
	prov, ok := cfg.Providers[name]
	if !ok {
		return nil, false
	}
	pool, _ := loadPool(name, prov.Provider)
	if len(pool.Accounts) < 2 {
		return nil, false
	}
	vids := make([]string, 0, len(pool.Accounts))
	for _, a := range pool.Accounts {
		vids = append(vids, name+"#"+a.ID)
	}
	sort.Strings(vids)
	return vids, true
}

// listArkAgentPlanModelIDs calls the Volcengine signed OpenAPI ListArkAgentPlanModel
// via the provider's stored AK/SK and returns the Agent Plan's supported model IDs.
func listArkAgentPlanModelIDs(provName string) ([]string, error) {
	creds, err := loadVolcengineCreds(provName)
	if err != nil || creds.AccessKey == "" || creds.SecretKey == "" {
		return nil, fmt.Errorf("Agent Plan model list needs AK/SK — run `model-proxy login %s`", provName)
	}
	req, err := volcengineGet("ListArkAgentPlanModel", "2024-01-01", creds.AccessKey, creds.SecretKey, time.Now(), "")
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("ListArkAgentPlanModel: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("ListArkAgentPlanModel HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var wrap struct {
		ResponseMetadata json.RawMessage `json:"ResponseMetadata"`
		Result           struct {
			Datas []struct {
				ModelID string `json:"ModelID"`
			} `json:"Datas"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, fmt.Errorf("parse ListArkAgentPlanModel: %w", err)
	}
	ids := make([]string, 0, len(wrap.Result.Datas))
	for _, d := range wrap.Result.Datas {
		ids = append(ids, d.ModelID)
	}
	return ids, nil
}

// nonFlagArgs returns positional args (skipping --config and its value).
func nonFlagArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--config" || a == "-config" {
			i++
			continue
		}
		if strings.HasPrefix(a, "--config=") {
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		out = append(out, a)
	}
	return out
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}
