package app

import (
	"fmt"
	"regexp"

	"model-proxy/internal/guard"
)

// buildGuardScanner compiles the per-generation outbound guard scanner from
// the resolved config and the credential values collected by BuildProviders.
// It runs OUTSIDE p.mu (regexp compilation + variant precomputation are not
// free); reload/startup then swap the immutable pointer under the lock.
//
// known_secrets: false drops the credential values (pattern table only);
// decode: false disables the encoded-form channels (base64/hex/url). Bad
// extra_patterns are rejected at config validate, so an error here means an
// unvalidated Config — fail-closed (reload keeps the old generation).
func buildGuardScanner(cfg *Config, secrets []string) (*guard.Scanner, error) {
	g := cfg.Guard
	var custom []guard.CustomPattern
	for _, ep := range g.ExtraPatterns {
		re, err := regexp.Compile(ep.Regex)
		if err != nil {
			return nil, fmt.Errorf("guard.extra_patterns[%s]: %w", ep.Name, err)
		}
		var literal []byte
		if ep.Literal != "" {
			literal = []byte(ep.Literal)
		}
		custom = append(custom, guard.CustomPattern{Name: ep.Name, RE: re, Literal: literal})
	}
	if !g.KnownSecretsEnabled() {
		secrets = nil
	}
	return guard.NewScannerWithOptions(custom, secrets, g.ExtraPaths, guard.Options{Decode: g.DecodeEnabled()})
}
