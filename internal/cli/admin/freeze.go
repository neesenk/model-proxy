package admin

import (
	"fmt"
	"os"
	"strings"

	cliframework "model-proxy/internal/cli/framework"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/display"
)

// freeze_cmd.go implements `model-proxy freeze <provider>` — the manual
// counterpart of `unfreeze`: marks a provider operator-frozen via the running
// daemon's POST /api/health/freeze, so it is excluded from scheduling (never
// selected as a target) until `unfreeze` clears the entry. Unlike the
// circuit/rate-limit freeze this state is explicit, has no expiry, is NOT
// cleared by success/failure recording, and persists across restarts (same
// config-fingerprint gate as the rest of quota_state.json health). Sticky
// routes, pins, quotas, and learned param blocklists are NOT touched. Freeze
// ALWAYS requires an explicit provider (pooled parent = all its accounts);
// only unfreeze offers the no-arg = all form.
//
//	freeze <provider>  freeze one provider (pooled parent = all its accounts)

func CmdFreeze(args []string, cfg *configdomain.Config) {
	pos := cliframework.PositionalArgs(args)
	if len(pos) == 0 {
		fmt.Fprintf(os.Stderr, "%s usage: model-proxy freeze <provider>\n", display.Red("✗"))
		os.Exit(1)
	}
	out, err := DoFreeze("http://"+cfg.Listen, pos[0])
	EmitPinResult(os.Stdout, os.Stderr, out, err, cfg.Listen)
}

// DoFreeze posts /api/health/freeze and renders the result line. provider must
// be non-empty (the daemon rejects an empty provider with 400). Extracted for
// tests (mirrors DoUnfreeze).
func DoFreeze(base, provider string) (string, error) {
	var out struct {
		Frozen []string `json:"frozen"`
	}
	if err := postProviderOp(base, "/api/health/freeze", provider, &out); err != nil {
		return "", err
	}
	if len(out.Frozen) == 0 {
		return fmt.Sprintf("%s no matching provider for %s\n", display.Dim("•"), provider), nil
	}
	return fmt.Sprintf("%s froze %s: %s\n", display.Green("✓"), provider, strings.Join(out.Frozen, ", ")), nil
}
