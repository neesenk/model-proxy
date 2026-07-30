package login

import (
	"os"

	"model-proxy/provider"
)

// truncate caps a string at n bytes (provider display rules).
func truncate(s string, n int) string { return provider.Truncate(s, n) }

// mask redacts a credential/id for logs: first 2 + … + last 2 (see
// model-proxy/util.go for the canonical CLI copy; this mirrors it).
func mask(s string) string {
	if s == "" {
		return "(empty)"
	}
	// Short secrets: don't reveal even partial — full mask.
	const minReveal = 8
	if len(s) < minReveal {
		return "****"
	}
	return s[:2] + "…" + s[len(s)-2:]
}

// HomeDir resolves the user home (credential files live under ~/.model-proxy).
func HomeDir() string {
	h, _ := os.UserHomeDir()
	return h
}
