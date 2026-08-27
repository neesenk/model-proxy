package config

import (
	"strings"
	"testing"
)

// credentials: selects the apikey-pool storage backend (file|keychain). The
// closed set is enforced at validate; the accessor applies the file default.

const credModeTestProviders = `providers:
  x:
    openai_base_url: https://x
    provider_id: zhipu
`

func TestCredentialsMode_DefaultsToFile(t *testing.T) {
	for _, body := range []string{
		credModeTestProviders,
		"credentials: \"\"\n" + credModeTestProviders,
	} {
		cfg, err := LoadConfigFromBytes("test", []byte(body))
		if err != nil {
			t.Fatalf("LoadConfigFromBytes(%q): %v", body, err)
		}
		if got := cfg.CredentialsMode(); got != "file" {
			t.Fatalf("CredentialsMode() = %q, want file", got)
		}
	}
}

func TestCredentialsMode_KeychainAccepted(t *testing.T) {
	cfg, err := LoadConfigFromBytes("test", []byte("credentials: keychain\n"+credModeTestProviders))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if got := cfg.CredentialsMode(); got != "keychain" {
		t.Fatalf("CredentialsMode() = %q, want keychain", got)
	}
	// The rawConfig mirror must not silently drop the file-loaded value.
	if cfg.Credentials != "keychain" {
		t.Fatalf("Credentials = %q, want keychain", cfg.Credentials)
	}
}

func TestCredentialsMode_InvalidRejected(t *testing.T) {
	_, err := LoadConfigFromBytes("test", []byte("credentials: vault\n"+credModeTestProviders))
	if err == nil || !strings.Contains(err.Error(), `credentials "vault" invalid`) {
		t.Fatalf("err = %v, want closed-set rejection", err)
	}
}
