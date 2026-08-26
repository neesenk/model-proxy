package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

// buildProviders creates provider.Provider instances from config. Each provider
// owns its auth/usage/quota/logout (Phase 1-5 + aqp-fetch migration); buildOne
// only wires the remaining callback (FetchModelsFn: volcengine's V4-signed
// ListArkAgentPlanModel) + the per-provider config fields.
//
// A provider whose credential snapshot has ≥2 accounts is UNROLLED into
// one virtual provider per account, keyed "name#<accountID>"; the parent name
// is NOT a key (only the virtuals are). A 1-entry PLURAL pool (the file `login`
// writes) is bound in-memory under the plain name. Only the no-plural-file /
// not-logged-in case stays file-backed (reading the legacy singular
// <name>_apikey.json via Store.LoadSnapshot's fallback) — binding the cred
// there would break the embedded ApiKeyBase, which reads the (non-existent)
// singular file.
//
// It also derives the credential-pool index maps in the SAME pass:
//   - poolIndex[parent] = its sorted virtual ids ("name#<accountID>")
//   - parentOf[vid]    = the parent name
//
// Loading the pool once (rather than separately in buildPoolIndex) closes a
// TOCTOU window across reload and guarantees the index only references virtual
// ids that actually exist in the returned providers map: a virtual whose
// buildOne returned nil (provider.New error) is skipped in BOTH the providers
// map AND the index. Single-account / not-logged-in providers appear in
// neither index map (their id == the plain name).
type Build struct {
	Providers map[string]provider.Provider
	PoolIndex map[string][]string
	ParentOf  map[string]string
	Eligible  map[string]bool
	// Secrets are the proxy-managed credential values collected in the SAME
	// pass (pool APIKey/AccessKey/SecretKey + codex/aqp OAuth tokens), handed
	// to the guard known-secret scanner. Memory only: never logged, persisted,
	// or serialized (credential red line).
	Secrets []string
	// PoolSecrets is the API-key-pool subset of Secrets. It is stable within a
	// config generation (pool files are only re-read by BuildProviders), so the
	// guard OAuth refresh loop reuses it as the base when re-collecting the
	// rotating OAuth subset.
	PoolSecrets []string
	// OAuthSecrets is the codex/aqp subset of Secrets. OAuth providers rotate
	// their tokens in place during serve (rewriting <name>_oauth_auth.json),
	// so this is the only part of Secrets that can change WITHOUT a reload —
	// the guard refresh loop re-collects it on the quota-poll beat.
	OAuthSecrets []string
}

// BuildOptions carries the process-environment seams buildProviders needs:
// home directory (credential files), codex version probes, and the volcengine
// signed model-list call. Tests inject fakes; production wires the real ones.
type BuildOptions struct {
	HomeDir                  string
	CodexCLIVersion          func() string
	CodexCacheVersion        func() string
	ListArkAgentPlanModelIDs func(provName string) ([]string, error)
}

// BuildProviders creates provider.Provider instances from config, unrolling
// multi-account credential pools into virtual providers ("name#<accountID>").
func BuildProviders(cfg *configdomain.Config, store accounts.Store, opts BuildOptions) Build {
	m := map[string]provider.Provider{}
	poolIndex := map[string][]string{}
	parentOf := map[string]string{}
	eligible := map[string]bool{}
	var poolSecrets, oauthSecrets []string
	for name, prov := range cfg.Providers {
		if prov.Provider == "aqp" || prov.Provider == "codex" {
			// OAuth/SSO providers own separate auth stores and never consult the
			// API-key account pool namespace. Their tokens still join the guard
			// known-secret set (best-effort, memory only).
			oauthSecrets = append(oauthSecrets, collectOAuthSecrets(opts, name, prov.Provider)...)
			if p := BuildOne(cfg, opts, name, prov, accounts.Credentials{}); p != nil {
				m[name] = p
			}
			continue
		}
		// One storage read decides both credentials and authority. An unreadable
		// plural file is never downgraded to the legacy singular key.
		snapshot, poolErr := store.LoadSnapshot(name, prov.Provider)
		if poolErr != nil {
			log.Printf("[proxy] pool %s unreadable: %v; disabling provider", name, poolErr)
			continue
		}
		pool := snapshot.Pool
		// Guard known-secret collection: every account credential the proxy
		// manages is a value a leak-out attempt would carry. Non-empty values
		// only; length/dup filtering happens in guard.NewScanner. Legacy
		// singular keys arrive as a wrapped 1-entry pool, so this one loop
		// covers both sources without a second storage read.
		for _, a := range pool.Accounts {
			cred := a.Credentials()
			poolSecrets = appendNonEmpty(poolSecrets, cred.APIKey, cred.AccessKey, cred.SecretKey)
		}
		if snapshot.Source != accounts.SourcePlural {
			// Missing or legacy: API-key providers keep their historical
			// file-backed path. static is plural-only and has no safe file-backed
			// AuthHeaders implementation, so an unbound instance must not exist.
			if prov.Provider == "static" {
				continue
			}
			if p := BuildOne(cfg, opts, name, prov, accounts.Credentials{}); p != nil {
				m[name] = p
				if snapshot.Source == accounts.SourceLegacy {
					eligible[name] = true
				}
			}
			continue
		}
		if len(pool.Accounts) == 0 {
			// An empty plural file is an authoritative credential tombstone.
			// Do not construct an unbound provider that could re-read legacy.
			log.Printf("[proxy] pool %s exists but has 0 accounts; disabling provider", name)
			continue
		}
		if len(pool.Accounts) == 1 {
			// 1-entry PLURAL pool (from `login`): bind the account's cred under
			// the plain name, no virtuals. The singular file does not exist in
			// this case, so binding in-memory is required for the provider to
			// authenticate at all.
			if p := BuildOne(cfg, opts, name, prov, pool.Accounts[0].Credentials()); p != nil {
				m[name] = p
				eligible[name] = true
			}
			continue
		}
		vids := make([]string, 0, len(pool.Accounts))
		for _, a := range pool.Accounts {
			vid := name + "#" + a.ID
			p := BuildOne(cfg, opts, name, prov, a.Credentials())
			if p == nil {
				// provider.New failed for this account — skip it in BOTH the
				// providers map and the index, so poolIndex never lists an id
				// that isn't runnable (which would make expandedRoutes produce
				// a target that forward can't serve).
				continue
			}
			m[vid] = p
			vids = append(vids, vid)
			parentOf[vid] = name
		}
		if len(vids) > 0 {
			sort.Strings(vids)
			poolIndex[name] = vids
			eligible[name] = true
		}
	}
	return Build{
		Providers: m,
		PoolIndex: poolIndex,
		ParentOf:  parentOf,
		Eligible:  eligible,
		// Fresh slices: the caller stores PoolSecrets/OAuthSecrets as
		// reload-owned state and later concatenates them for scanner rebuilds,
		// so Secrets must not share a backing array with either subset.
		Secrets:      append(append([]string(nil), poolSecrets...), oauthSecrets...),
		PoolSecrets:  poolSecrets,
		OAuthSecrets: oauthSecrets,
	}
}

// appendNonEmpty appends the non-empty values of vals to dst.
func appendNonEmpty(dst []string, vals ...string) []string {
	for _, v := range vals {
		if v != "" {
			dst = append(dst, v)
		}
	}
	return dst
}

// CollectOAuthSecrets re-reads every codex/aqp provider's OAuth auth file and
// returns the current token/cookie values. It is the refreshable counterpart
// of the OAuth pass inside BuildProviders: OAuth providers rotate their tokens
// in place during serve, so the guard refresh loop calls this on a beat to
// re-sync the known-secret set without a full provider rebuild. Best-effort
// like collectOAuthSecrets — missing/corrupt files contribute nothing.
func CollectOAuthSecrets(cfg *configdomain.Config, opts BuildOptions) []string {
	var out []string
	for name, prov := range cfg.Providers {
		if prov.Provider == "aqp" || prov.Provider == "codex" {
			out = append(out, collectOAuthSecrets(opts, name, prov.Provider)...)
		}
	}
	return out
}

// collectOAuthSecrets best-effort reads one codex/aqp provider's OAuth/SSO
// auth file (<home>/.model-proxy/<name>_oauth_auth.json — the same path
// buildOne injects as OAuthAuthFile) and returns the token/cookie values for
// the guard known-secret set. A missing or unreadable file means "not logged
// in" — silently skip (there is no credential to protect). Parsed with the
// provider package's own file types; values stay in memory only.
func collectOAuthSecrets(opts BuildOptions, name, providerID string) []string {
	b, err := os.ReadFile(filepath.Join(opts.HomeDir, ".model-proxy", name+"_oauth_auth.json"))
	if err != nil {
		return nil
	}
	switch providerID {
	case "codex":
		var af provider.CodexAuthFile
		if err := json.Unmarshal(b, &af); err != nil {
			return nil
		}
		return appendNonEmpty(nil, af.Tokens.AccessToken, af.Tokens.RefreshToken, af.Tokens.IDToken)
	case "aqp":
		var a provider.AqpAccountData
		if err := json.Unmarshal(b, &a); err != nil {
			return nil
		}
		return appendNonEmpty(nil, a.SSOSessionCookie)
	}
	return nil
}

// buildOne constructs a single provider instance (a real provider for the
// single-account path, or a virtual for one credential-pool entry) bound to
// cred. When cred is non-empty the key is bound via pcfg.BoundAPIKey, which the
// apikey constructors (zhipu/deepseek/volcengine) feed into a bound ApiKeyBase
// whose LoadKey/AuthHeaders use the in-memory key. This covers all three paths
// (forward AuthHeaders, FetchModels via p.AuthHeaders, and Usage/Quota fetches
// which call p.AuthHeaders) - one binding point, no separate cfg.Auth needed.
//
// When cred is empty all three fall back to the legacy file-backed behavior
// (identical to the pre-pool buildProviders).
func BuildOne(cfg *configdomain.Config, opts BuildOptions, name string, prov configdomain.Provider, cred accounts.Credentials) provider.Provider {
	pcfg := &provider.Config{
		ProviderID:    prov.Provider,
		ProviderName:  name,
		OpenAIBaseURL: prov.OpenAIBaseURL,
		Headers:       prov.Headers,
		UsageURL:      prov.UsageURL,
		AqpMintURL:    prov.AqpMintURL, // aqp: without this the key mint POSTs to ""
		BoundAPIKey:   cred.APIKey,     // binding point #1 (forward path)
		Models:        prov.Models,     // for the usage-display fallback (listConfigModels)
		OAuthAuthFile: filepath.Join(opts.HomeDir, ".model-proxy", name+"_oauth_auth.json"),
	}
	// Wire callbacks by provider type. aqp needs none: its Quota/Usage fetch
	// monthly_usage directly (AqpProvider.fetchMonthlyUsage reads the SSO-cookie
	// store at OAuthAuthFile + POSTs), like the other providers.
	switch prov.Provider {
	case "codex":
		pcfg.ClientVersion = ResolveCodexClientVersion(prov.ClientVersion, opts.CodexCLIVersion, opts.CodexCacheVersion)
	case "volcengine":
		pcfg.FetchModelsFn = func() ([]string, error) { return opts.ListArkAgentPlanModelIDs(name) }
		// GetAFPUsage is V4-signed with the virtual's own AK/SK (bound here so
		// each pooled account queries its own Agent Plan quota); falls back to
		// the legacy <name>_apikey.json when unbound (single-account path).
		pcfg.AccessKey = cred.AccessKey
		pcfg.SecretKey = cred.SecretKey
		pcfg.VolcengineCredFile = filepath.Join(opts.HomeDir, ".model-proxy", name+"_apikey.json")
	}
	p, err := provider.New(pcfg, name)
	if err != nil {
		log.Printf("[proxy] failed to build provider %s: %v (using auth-only)", name, err)
		return nil
	}
	return p
}

// HealthConfigFingerprint identifies the exact provider config that frozen
// health state belongs to. Persisted cooldowns restore only on an exact match
// — health is keyed by provider NAME, so without this gate a different config
// (or a test binary sharing ~/.model-proxy/quota_state.json) would "restore"
// cooldowns onto unrelated same-named providers.
func HealthConfigFingerprint(cfg *configdomain.Config) string {
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		p := cfg.Providers[name]
		fmt.Fprintf(h, "%s|%s|%s|%s\n", name, p.Provider, p.OpenAIBaseURL, p.AnthropicBaseURL)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// buildOpts wires the production environment seams (home dir, codex version
// probes, volcengine signed model list) for BuildProviders.
func buildOpts() BuildOptions {
	return BuildOptions{
		HomeDir:                  accounts.HomeDir(),
		CodexCLIVersion:          CodexCLIVersion,
		CodexCacheVersion:        CodexCacheVersion,
		ListArkAgentPlanModelIDs: ListArkAgentPlanModelIDs,
	}
}

// listArkAgentPlanModelIDs calls the Volcengine signed OpenAPI ListArkAgentPlanModel
// via the provider's stored AK/SK and returns the Agent Plan's supported model IDs.
func ListArkAgentPlanModelIDs(provName string) ([]string, error) {
	creds, err := LoadVolcengineCreds(accounts.HomeDir(), provName)
	if err != nil || creds.AccessKey == "" || creds.SecretKey == "" {
		return nil, fmt.Errorf("Agent Plan model list needs AK/SK — run `model-proxy login %s`", provName)
	}
	req, err := provider.VolcengineSignedGet("ListArkAgentPlanModel", "2024-01-01", creds.AccessKey, creds.SecretKey, time.Now(), "")
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
		return nil, fmt.Errorf("ListArkAgentPlanModel HTTP %d: %s", resp.StatusCode, provider.Truncate(string(body), 300))
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
