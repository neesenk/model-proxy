// security_explain.go — the admin security-explain port: on-demand
// re-location of recorded guard hits inside a persisted request body.
//
// This surface is the documented carve-out from the "guard hits never carry
// matched content" red line (decision 17): nothing is persisted, the caller
// is admin-authenticated, the source body is one the admin can already read
// via /api/requests/<id>, and secret-kind matches are masked in every
// snippet (path literals are shown verbatim — a path is not a credential,
// and "which file" is the whole point of a path hit).
package app

import (
	"fmt"

	"model-proxy/internal/appapi"
	"model-proxy/internal/guard"
)

// explainContextBytes is the number of body bytes kept on each side of a
// located match in the returned snippet.
const explainContextBytes = 80

// explainMaxPerName caps located occurrences reported per name; further
// occurrences of the same pattern carry no extra diagnostic value.
const explainMaxPerName = 5

// known-secret channel explanations (these channels have no regex — they
// match the proxy's own configured credential values).
var securityChannelNotes = map[string]string{
	"known_secret":            "Exact value of a credential configured on this proxy (a pool API key or OAuth token) appeared in the request.",
	"known_secret_encoded":    "A base64/hex/url-encoded form of a credential configured on this proxy appeared in the request.",
	"known_secret_fragmented": "A credential configured on this proxy was split into fragments across multiple requests of one session (split-exfiltration signal). This is a cross-request detection — it cannot be re-located inside this single request body.",
}

// sensitive-path category explanations.
var securityPathNotes = map[string]string{
	"ssh":       "Reference to ~/.ssh or a bare private-key file name (id_rsa / id_ed25519).",
	"aws_creds": "Reference to ~/.aws/credentials.",
	// proxy_creds is retired as a detection category (decision 35); the note
	// stays so explain can still describe historical audit records.
	"proxy_creds": "Reference to ~/.model-proxy — this proxy's own config/credential directory (category retired: managed credentials are matched by exact value via known_secret).",
	"gnupg":       "Reference to ~/.gnupg.",
	"kube":        "Reference to ~/.kube/config.",
	"docker":      "Reference to ~/.docker/config.json.",
	"gcloud":      "Reference to ~/.config/gcloud.",
	"dotenv":      "Reference to a .env file.",
	"custom_path": "A path from guard.extra_paths in your config.",
}

// locateGuardHits implements admin.Ports.LocateGuardHits: re-scan body with
// the CURRENT generation's guard scanner and project each requested name to
// display-ready matches. A name the current scanner no longer finds (rule
// removed, known secret rotated) still yields one entry with Located=false
// and its explanation, so the UI can say why nothing is highlighted.
func (p *Proxy) locateGuardHits(body []byte, kind string, names []string) ([]appapi.SecurityMatch, error) {
	p.mu.RLock()
	scanner := p.guardScanner
	p.mu.RUnlock()
	if scanner == nil {
		// Fail closed: no scanner means no analysis, not an empty one.
		return nil, appapi.ErrGuardScannerUnavailable
	}
	located := scanner.Locate(body, names)
	out := make([]appapi.SecurityMatch, 0, len(names))
	perName := map[string]int{}
	for _, m := range located {
		if perName[m.Name] >= explainMaxPerName {
			continue
		}
		perName[m.Name]++
		entry := appapi.SecurityMatch{
			Name:        m.Name,
			Strength:    m.Strength,
			Located:     true,
			Explanation: securityExplainNote(scanner, m.Name),
		}
		entry.Regex, entry.Source, _ = scanner.RuleInfo(m.Name)
		// MaskSnippet masks the hit itself for secret kind, and ALWAYS masks
		// any other secret overlapping the window — the context of a path hit
		// can carry an unrelated credential (the body is sensitive as a whole).
		entry.Pre, entry.Hit, entry.Post = scanner.MaskSnippet(body, m.Start, m.End, kind == "secret", explainContextBytes)
		out = append(out, entry)
	}
	for _, name := range names {
		if perName[name] > 0 {
			continue
		}
		entry := appapi.SecurityMatch{
			Name:        name,
			Explanation: securityExplainNote(scanner, name),
		}
		entry.Regex, entry.Source, _ = scanner.RuleInfo(name)
		if entry.Explanation == "" {
			entry.Explanation = "The current guard scanner no longer finds this name in the stored request body (the rule was removed or the credential rotated since the hit was recorded)."
		}
		out = append(out, entry)
	}
	return out, nil
}

// securityExplainNote returns the static explanation for known-secret
// channels and path categories; embedded/custom rules describe themselves
// through RuleInfo (regex + source).
func securityExplainNote(scanner *guard.Scanner, name string) string {
	if note, ok := securityChannelNotes[name]; ok {
		return note
	}
	if note, ok := securityPathNotes[name]; ok {
		return note
	}
	if _, source, ok := scanner.RuleInfo(name); ok {
		if source == "config" {
			return "Custom secret pattern from guard.extra_patterns in your config."
		}
		return fmt.Sprintf("Built-in secret pattern (source: %s).", source)
	}
	return ""
}
