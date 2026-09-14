package admin

import (
	"fmt"
	"os"
	"strings"

	cliframework "model-proxy/internal/cli/framework"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/display"
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

func CmdUnfreeze(args []string, cfg *configdomain.Config) {
	provider := ""
	if pos := cliframework.PositionalArgs(args); len(pos) > 0 {
		provider = pos[0]
	}
	out, err := DoUnfreeze("http://"+cfg.Listen, provider)
	EmitPinResult(os.Stdout, os.Stderr, out, err, cfg.Listen)
}

// doUnfreeze posts /api/health/reset and renders the result line. provider == ""
// resets all providers. Extracted for tests (mirrors doPin).
func DoUnfreeze(base, provider string) (string, error) {
	var out struct {
		Cleared           []string `json:"cleared"`
		ModelLocksCleared int      `json:"model_locks_cleared"`
	}
	if err := postProviderOp(base, "/api/health/reset", provider, &out); err != nil {
		return "", err
	}
	scope := "all providers"
	if provider != "" {
		scope = provider
	}
	if len(out.Cleared) == 0 && out.ModelLocksCleared == 0 {
		return fmt.Sprintf("%s no frozen state on %s\n", display.Dim("•"), scope), nil
	}
	names := strings.Join(out.Cleared, ", ")
	if names == "" {
		names = "(no provider cooldowns)"
	}
	return fmt.Sprintf("%s unfroze %s: %s (+%d model lock(s))\n", display.Green("✓"), scope, names, out.ModelLocksCleared), nil
}
