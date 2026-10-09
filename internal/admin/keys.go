package admin

// keys.go — forward-key management over the configured api_keys file: list
// with usage enrichment, mint+append, revoke by key id. The file stays the
// single source of truth (hand edits and `takeover`'s first-key embed read
// the same file); the serving gate's 10s TTL picks up writes without a
// reload. The full token is returned exactly once, at creation — listings
// carry only the sha256 id and a display-safe prefix.

import (
	"net/http"
	"time"

	"model-proxy/internal/appapi"
	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/webauth"
)

// keyPrefixLen is how many token characters a listing shows — enough for an
// operator to match against a client config by eye, never the whole token.
const keyPrefixLen = 12

// APIKeys lists the forward keys enriched with last-use and a 24h request
// count from the request-log index. No api_keys_file configured (loopback
// deployments) answers Enabled=false.
func (s *Service) APIKeys() (appapi.KeysResult, error) {
	path := s.keyFilePath()
	if path == "" {
		return appapi.KeysResult{Enabled: false, Keys: []appapi.APIKey{}}, nil
	}
	entries := webauth.ReadKeyEntries(path)
	keys := make([]appapi.APIKey, 0, len(entries))
	for _, e := range entries {
		keys = append(keys, appapi.APIKey{
			ID:        webauth.KeyID(e.Token),
			Label:     e.Label,
			Prefix:    displayPrefix(e.Token),
			CreatedAt: e.Created,
		})
	}
	s.enrichKeyUsage(keys)
	return appapi.KeysResult{Enabled: true, Keys: keys}, nil
}

// CreateAPIKey mints one new key and appends it with its label/created
// metadata. The write is atomic; the gate accepts the new key within its TTL.
func (s *Service) CreateAPIKey(label string) (appapi.APIKeyCreated, error) {
	path := s.keyFilePath()
	if path == "" {
		return appapi.APIKeyCreated{}, appapi.NewHTTPError(http.StatusConflict,
			"web.auth.api_keys_file is not configured — the forward surface is unauthenticated (loopback deployment); set it before managing keys")
	}
	token, err := webauth.GenerateToken()
	if err != nil {
		return appapi.APIKeyCreated{}, err
	}
	entries := webauth.ReadKeyEntries(path)
	entries = append(entries, webauth.KeyEntry{Token: token, Label: label, Created: webauth.Now().Format(time.RFC3339)})
	if err := webauth.WriteKeyEntries(path, entries); err != nil {
		return appapi.APIKeyCreated{}, err
	}
	return appapi.APIKeyCreated{
		APIKey: appapi.APIKey{
			ID:        webauth.KeyID(token),
			Label:     label,
			Prefix:    displayPrefix(token),
			CreatedAt: webauth.Now().Format(time.RFC3339),
		},
		Token: token,
	}, nil
}

// RevokeAPIKey removes the key whose sha256 id matches. Unknown ids are a 404
// (not a silent success — an operator revoking the wrong key must see it).
func (s *Service) RevokeAPIKey(id string) error {
	path := s.keyFilePath()
	if path == "" {
		return appapi.NewHTTPError(http.StatusConflict, "web.auth.api_keys_file is not configured")
	}
	entries := webauth.ReadKeyEntries(path)
	kept := make([]webauth.KeyEntry, 0, len(entries))
	removed := false
	for _, e := range entries {
		if webauth.KeyID(e.Token) == id {
			removed = true
			continue
		}
		kept = append(kept, e)
	}
	if !removed {
		return appapi.NewHTTPError(http.StatusNotFound, "no forward key with id "+id)
	}
	return webauth.WriteKeyEntries(path, kept)
}

// APIKeyUsage aggregates one key's per-model call volume over the window.
func (s *Service) APIKeyUsage(id string, from, to time.Time) ([]requestlog.KeyUsageEntry, error) {
	queries := s.RequestLogQueries()
	if queries == nil {
		return []requestlog.KeyUsageEntry{}, nil
	}
	filter := requestlog.Filter{From: from, To: to}
	rows, err := queries.KeyUsage(filter, id)
	if err != nil {
		return nil, err
	}
	out := make([]requestlog.KeyUsageEntry, 0, len(rows))
	for _, r := range rows {
		if r.APIKeyID == id {
			out = append(out, r)
		}
	}
	return out, nil
}

// keyFilePath resolves the configured forward-key file from the current
// config generation; "" when auth is off.
func (s *Service) keyFilePath() string {
	cfg := s.ports.Config()
	if cfg == nil {
		return ""
	}
	return cfg.Web.Auth.APIKeysFile
}

// enrichKeyUsage fills LastUsedMs and Requests24h. Two windows: all-time for
// last use (a key idle for days still shows when it was last seen) and a
// 24h-bounded pass for the call count — an all-time row would overstate it.
func (s *Service) enrichKeyUsage(keys []appapi.APIKey) {
	if len(keys) == 0 {
		return
	}
	queries := s.RequestLogQueries()
	if queries == nil {
		return
	}
	last := map[string]int64{}
	if rows, err := queries.KeyUsage(requestlog.Filter{}, ""); err == nil {
		for _, r := range rows {
			if r.LastUsedMs > last[r.APIKeyID] {
				last[r.APIKeyID] = r.LastUsedMs
			}
		}
	}
	dayAgo := time.Now().Add(-24 * time.Hour)
	calls24h := map[string]int64{}
	if rows, err := queries.KeyUsage(requestlog.Filter{From: dayAgo}, ""); err == nil {
		for _, r := range rows {
			calls24h[r.APIKeyID] += r.Requests
		}
	}
	for i := range keys {
		keys[i].LastUsedMs = last[keys[i].ID]
		keys[i].Requests24h = calls24h[keys[i].ID]
	}
}

func displayPrefix(token string) string {
	if len(token) <= keyPrefixLen {
		return token
	}
	return token[:keyPrefixLen] + "…"
}
