package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
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

// cmdModels handles:
//
//	models              — list all models from all providers (from config)
//	models <provider>   — list models for one provider
//	models refresh <provider> — fetch live model list from a provider's server
func cmdModels(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	rest := nonFlagArgs(args)
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
		printProviderModels(provName, entries)
		return
	}
	// models [provider] — list from config
	provFilter := ""
	if len(rest) > 0 {
		provFilter = rest[0]
		if _, ok := cfg.Providers[provFilter]; !ok {
			log.Fatalf("unknown provider %q; available: %s", provFilter, providerNames(cfg))
		}
	}
	printAllModels(cfg, provFilter)
}

// printAllModels prints all models from config (optionally filtered by provider).
func printAllModels(cfg *Config, provFilter string) {
	names := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		if provFilter != "" && n != provFilter {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)

	fmt.Printf("%s  %s  %s  %s  %s  %s\n",
		cDim(pad("PROVIDER", 12)), cDim(pad("MODEL ID", 22)),
		cDim(pad("NAME", 20)), cDim(pad("CTX", 10)),
		cDim(pad("OUTPUT", 8)), cDim(pad("MODALITIES", 16)))
	for _, pn := range names {
		prov := cfg.Providers[pn]
		modelIDs := make([]string, 0, len(prov.Models))
		for mid := range prov.Models {
			modelIDs = append(modelIDs, mid)
		}
		sort.Strings(modelIDs)
		for _, mid := range modelIDs {
			m := prov.Models[mid]
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
			fmt.Printf("%s  %s  %s  %s  %s  %s\n",
				cBlue(pad(pn, 12)), cCyan(pad(mid, 22)),
				cGreen(pad(name, 20)), cGray(pad(ctx, 10)),
				cGray(pad(out, 8)), cGray(pad(mod, 16)))
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
