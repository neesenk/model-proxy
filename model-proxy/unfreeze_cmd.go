package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// unfreeze_cmd.go implements `model-proxy unfreeze [provider]` — clears frozen
// runtime health state (circuit-open / rate-limit cooldowns / model lockouts)
// via the running daemon's POST /api/health/reset, so the provider is retried
// immediately instead of waiting out a possibly hours-long cooldown. Operator
// escape hatch for abnormal edge cases (account topped up, misclassified 429,
// upstream window reset early). Sticky routes, pins, and learned param
// blocklists are NOT cleared. Like pins, the state lives in the daemon; the
// next quota poll re-persists the cleared (empty) snapshot.
//
//	unfreeze             clear frozen state for ALL providers
//	unfreeze <provider>  clear frozen state for one provider (pooled parent = all its accounts)

func cmdUnfreeze(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %s\n", cRed("✗"), err)
		os.Exit(1)
	}
	provider := ""
	if pos := positionalArgs(args); len(pos) > 0 {
		provider = pos[0]
	}
	out, err := doUnfreeze("http://"+cfg.Listen, provider)
	emitPinResult(os.Stdout, os.Stderr, out, err, cfg.Listen)
}

// doUnfreeze posts /api/health/reset and renders the result line. provider == ""
// resets all providers. Extracted for tests (mirrors doPin).
func doUnfreeze(base, provider string) (string, error) {
	raw, _ := json.Marshal(map[string]string{"provider": provider})
	resp, err := daemonHTTPClient.Post(base+"/api/health/reset", "application/json", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("%s", truncate(strings.TrimSpace(string(rb)), 200))
	}
	var out struct {
		Cleared           []string `json:"cleared"`
		ModelLocksCleared int      `json:"model_locks_cleared"`
	}
	json.Unmarshal(rb, &out)
	scope := "all providers"
	if provider != "" {
		scope = provider
	}
	if len(out.Cleared) == 0 && out.ModelLocksCleared == 0 {
		return fmt.Sprintf("%s no frozen state on %s\n", cDim("•"), scope), nil
	}
	names := strings.Join(out.Cleared, ", ")
	if names == "" {
		names = "(no provider cooldowns)"
	}
	return fmt.Sprintf("%s unfroze %s: %s (+%d model lock(s))\n", cGreen("✓"), scope, names, out.ModelLocksCleared), nil
}
