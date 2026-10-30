// Package provider owns the `provider` command family: add / rm / set
// against the provider.json store — the CLI twin of the Accounts-page Add
// Provider form for CUSTOM endpoints (presets keep their own `presets add`
// with the template + login orchestration). Every mutation runs the same
// providerstore.Mutate pipeline as the Web (locked RMW, whole-effective
// validation fail-closed, absorbing any config.yaml providers: stragglers)
// and hot-reloads a running daemon.
package provider

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/mattn/go-isatty"

	"model-proxy/internal/cli/framework"
	clilogin "model-proxy/internal/cli/login"
	cliserve "model-proxy/internal/cli/serve"
	configdomain "model-proxy/internal/config"
	domainpresets "model-proxy/internal/presets"
	"model-proxy/internal/providerstore"
)

// valueFlags are the flags whose following token is a VALUE (not a
// positional) — framework.Positional only knows --config, so the provider
// family scans with its own full list.
var valueFlags = map[string]bool{
	"--config": true, "--base-url": true, "--anthropic-url": true, "--usage-url": true,
	"--billing": true, "--priority": true, "--api-key-env": true, "--label": true, "--model": true,
}

// positionals returns the non-flag tokens (subcommand, provider name).
func positionals(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if valueFlags[a] {
			i++ // skip the value token
			continue
		}
		if len(a) > 1 && a[0] == '-' && a[1] == '-' && strings.Contains(a, "=") {
			continue // --flag=value form
		}
		if len(a) > 1 && a[0] == '-' {
			continue // boolean flag
		}
		out = append(out, a)
	}
	return out
}

// modelFlags collects every `--model M` value (repeatable, order preserved).
func modelFlags(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--model" && i+1 < len(args) {
			out = append(out, args[i+1])
		} else if strings.HasPrefix(args[i], "--model=") {
			out = append(out, strings.TrimPrefix(args[i], "--model="))
		}
	}
	return out
}

// CmdProvider dispatches `provider <add|rm|set> ...`. Exit codes: 0 success,
// 1 usage/refusal/failure. Stream-parameterized so tests can drive the
// interactive prompts; the process entry is RunProvider (run.go).
func CmdProvider(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	pos := positionals(args)
	if len(pos) == 0 {
		usage(stderr)
		return 1
	}
	switch pos[0] {
	case "add":
		return runAdd(args, pos[1:], stdin, stdout, stderr)
	case "rm", "remove":
		return runRemove(args, pos[1:], stdin, stdout, stderr)
	case "set", "edit":
		return runSet(args, pos[1:], stdin, stdout, stderr)
	default:
		usage(stderr)
		return 1
	}
}

func usage(w io.Writer) {
	_, _ = fmt.Fprintln(w, `usage: model-proxy provider <add|rm|set> ...
  provider add <name> --base-url URL [--anthropic-url URL] [--usage-url URL]
               [--billing plan|pay-as-you-go] [--priority N] [--model M ...]
               [--api-key-env VAR] [--no-login]
  provider rm <name> [--yes]
  provider set <name> [--base-url URL] [--anthropic-url URL] [--usage-url URL]
               [--billing B] [--priority N] [--model M ...]
Providers live in ~/.model-proxy/provider.json; every change validates the
whole effective config (fail-closed) and hot-reloads a running daemon.`)
}

// configGuard guards the mutation pipeline: providerstore.Mutate needs a
// readable config file (routes and cross-checks validate against it).
func configGuard(args []string, stderr io.Writer) (string, bool) {
	cfgPath := framework.ConfigPath(args)
	if _, err := os.Stat(cfgPath); err != nil {
		_, _ = fmt.Fprintf(stderr, "no config.yaml found at %q — run `model-proxy config init` first\n", cfgPath)
		return "", false
	}
	return cfgPath, true
}

func stdinIsInteractive(stdin io.Reader) bool {
	f, ok := stdin.(*os.File)
	return ok && isatty.IsTerminal(f.Fd())
}

func askYesNo(r io.Reader, out io.Writer, prompt string) bool {
	_, _ = fmt.Fprint(out, prompt)
	var line string
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n == 0 || err != nil {
			return false
		}
		if buf[0] == '\n' {
			break
		}
		line += string(buf[0])
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// runAdd implements `provider add <name> ...` for custom OpenAI-compatible
// endpoints (provider_id static — vendor protocols come via `presets add`).
func runAdd(args, pos []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cfgPath, ok := configGuard(args, stderr)
	if !ok {
		return 1
	}
	if len(pos) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: provider add <name> --base-url URL [...]")
		return 1
	}
	name := pos[0]
	baseURL := framework.FlagStringValue(args, "--base-url")
	anthropicURL := framework.FlagStringValue(args, "--anthropic-url")
	if baseURL == "" && anthropicURL == "" {
		_, _ = fmt.Fprintln(stderr, "✗ --base-url (or --anthropic-url) is required — the endpoint the proxy forwards to")
		return 1
	}
	billing := framework.FlagStringValue(args, "--billing")
	if billing != "" && billing != "plan" && billing != "pay-as-you-go" {
		_, _ = fmt.Fprintf(stderr, "✗ --billing %q invalid — use plan or pay-as-you-go\n", billing)
		return 1
	}
	priority := 0
	if raw := framework.FlagStringValue(args, "--priority"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			_, _ = fmt.Fprintf(stderr, "✗ --priority %q must be a non-negative integer\n", raw)
			return 1
		}
		priority = n
	}
	models := modelFlags(args)
	entry := configdomain.Provider{
		Provider:         "static",
		OpenAIBaseURL:    baseURL,
		AnthropicBaseURL: anthropicURL,
		UsageURL:         framework.FlagStringValue(args, "--usage-url"),
		Billing:          billing,
		Priority:         priority,
		Models:           append([]string(nil), models...),
	}

	// Ambiguity gate BEFORE the write (the presets-add contract): models this
	// provider serves that other providers also serve without explicit routes
	// would resolve via implicit routing to whichever name sorts first.
	if ambiguous := ambiguityFor(cfgPath, name, entry); len(ambiguous) > 0 {
		msg := fmt.Sprintf("model(s) %s are also served by other configured providers without explicit routes — "+
			"implicit routing picks alphabetically. Use --priority or routes: to control failover.",
			strings.Join(ambiguous, ", "))
		interactive := stdinIsInteractive(stdin)
		switch {
		case interactive && !framework.HasFlagValue(args, "--yes"):
			if !askYesNo(stdin, stdout, "Proceed anyway? [y/N] ") {
				_, _ = fmt.Fprintln(stderr, "aborted")
				return 1
			}
			_, _ = fmt.Fprintf(stdout, "! %s\n", msg)
		case interactive:
			_, _ = fmt.Fprintf(stdout, "! %s\n", msg)
		default:
			_, _ = fmt.Fprintf(stderr, "✗ %s\nre-run with --yes to proceed\n", msg)
			return 1
		}
	}

	if err := providerstore.Mutate(cfgPath, providerstore.Path(), func(providers map[string]configdomain.Provider) error {
		if _, exists := providers[name]; exists {
			return fmt.Errorf("provider %q already exists — edit it with `provider set` or remove it first", name)
		}
		providers[name] = entry
		return nil
	}); err != nil {
		_, _ = fmt.Fprintf(stderr, "✗ add provider: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "✓ added provider %s to the provider store (%s, %d models)\n", name, baseURL, len(models))

	// Credential step: --api-key-env for scripts, interactive prompt on a TTY,
	// skip with a hint otherwise (OAuth-style flows are not part of custom
	// static endpoints — the pool holds plain API keys).
	skipLogin := framework.HasFlagValue(args, "--no-login")
	apiKeyEnv := framework.FlagStringValue(args, "--api-key-env")
	keyIn := ""
	if !skipLogin {
		if apiKeyEnv != "" {
			keyIn = os.Getenv(apiKeyEnv)
			if keyIn == "" {
				_, _ = fmt.Fprintf(stderr, "✗ environment variable %s is empty\n", apiKeyEnv)
				return 1
			}
		} else if !stdinIsInteractive(stdin) {
			_, _ = fmt.Fprintf(stdout, "! no credential added — run `model-proxy login %s` (or --api-key-env) to add one\n", name)
			skipLogin = true
		}
	}
	if !skipLogin {
		cfg, err := providerstore.LoadEffectiveConfig(cfgPath, false)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "✗ reload merged config: %v\n", err)
			return 1
		}
		if err := clilogin.RunProviderLogin(cfg, name, keyIn, framework.FlagStringValue(args, "--label"), false); err != nil {
			_, _ = fmt.Fprintf(stderr, "✗ login failed: %v\n", err)
			return 1
		}
	}

	// Hot-reload a running daemon; print the next step against the merged set.
	cfg, err := providerstore.LoadEffectiveConfig(cfgPath, false)
	if err == nil {
		cliserve.MaybeReloadDaemon(args, cfg)
		if p := cfg.Providers[name]; len(p.Models) > 0 {
			_, _ = fmt.Fprintf(stdout, "\nNext steps:\n  model-proxy test %s\n", p.Models[0])
		}
	}
	return 0
}

// runRemove implements `provider rm <name> [--yes]`. The credential pool is
// intentionally KEPT (rm is a routing/config removal; `logout` owns
// credentials) — the exit notes say so.
func runRemove(args, pos []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cfgPath, ok := configGuard(args, stderr)
	if !ok {
		return 1
	}
	if len(pos) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: provider rm <name> [--yes]")
		return 1
	}
	name := pos[0]
	cfg, err := providerstore.LoadEffectiveConfig(cfgPath, false)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "✗ %v\n", err)
		return 1
	}
	entry, exists := cfg.Providers[name]
	if !exists {
		_, _ = fmt.Fprintf(stderr, "✗ unknown provider %q; available: %s\n", name, cfg.ProviderNames())
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "removing provider %s (provider_id=%s, %d models)\n", name, entry.Provider, len(entry.Models))
	if !framework.HasFlagValue(args, "--yes") {
		if !stdinIsInteractive(stdin) {
			_, _ = fmt.Fprintln(stderr, "✗ non-interactive run needs --yes")
			return 1
		}
		if !askYesNo(stdin, stdout, fmt.Sprintf("Remove provider %q? [y/N] ", name)) {
			_, _ = fmt.Fprintln(stderr, "aborted")
			return 1
		}
	}
	if err := providerstore.Mutate(cfgPath, providerstore.Path(), func(providers map[string]configdomain.Provider) error {
		if _, exists := providers[name]; !exists {
			return fmt.Errorf("provider %q vanished during removal", name)
		}
		delete(providers, name)
		return nil
	}); err != nil {
		// A route still referencing the provider fails validation here with
		// the exact offending route named — surface it verbatim.
		_, _ = fmt.Fprintf(stderr, "✗ remove provider: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "✓ removed provider %s (credential pool kept — `model-proxy logout %s` clears it)\n", name, name)
	if cfg, err := providerstore.LoadEffectiveConfig(cfgPath, false); err == nil {
		cliserve.MaybeReloadDaemon(args, cfg)
	}
	return 0
}

// runSet implements `provider set <name> [--base-url ...] [--model M ...]`:
// only the PRESENT flags change the entry; --model replaces the whole list.
func runSet(args, pos []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cfgPath, ok := configGuard(args, stderr)
	if !ok {
		return 1
	}
	if len(pos) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: provider set <name> [--base-url URL] [--anthropic-url URL] [--usage-url URL] [--billing B] [--priority N] [--model M ...]")
		return 1
	}
	name := pos[0]
	changed := 0
	baseURL := framework.FlagStringValue(args, "--base-url")
	anthropicURL := framework.FlagStringValue(args, "--anthropic-url")
	usageURL := framework.FlagStringValue(args, "--usage-url")
	billing := framework.FlagStringValue(args, "--billing")
	priorityRaw := framework.FlagStringValue(args, "--priority")
	models := modelFlags(args)
	for _, v := range []string{baseURL, anthropicURL, usageURL, billing, priorityRaw} {
		if v != "" {
			changed++
		}
	}
	changed += len(models)
	if changed == 0 {
		_, _ = fmt.Fprintln(stderr, "✗ nothing to set — pass at least one of --base-url/--anthropic-url/--usage-url/--billing/--priority/--model")
		return 1
	}
	if billing != "" && billing != "plan" && billing != "pay-as-you-go" {
		_, _ = fmt.Fprintf(stderr, "✗ --billing %q invalid — use plan or pay-as-you-go\n", billing)
		return 1
	}
	priority := -1
	if priorityRaw != "" {
		n, err := strconv.Atoi(priorityRaw)
		if err != nil || n < 0 {
			_, _ = fmt.Fprintf(stderr, "✗ --priority %q must be a non-negative integer\n", priorityRaw)
			return 1
		}
		priority = n
	}
	if err := providerstore.Mutate(cfgPath, providerstore.Path(), func(providers map[string]configdomain.Provider) error {
		entry, exists := providers[name]
		if !exists {
			return fmt.Errorf("unknown provider %q — `provider add` it first", name)
		}
		if baseURL != "" {
			entry.OpenAIBaseURL = baseURL
		}
		if anthropicURL != "" {
			entry.AnthropicBaseURL = anthropicURL
		}
		if usageURL != "" {
			entry.UsageURL = usageURL
		}
		if billing != "" {
			entry.Billing = billing
		}
		if priority >= 0 {
			entry.Priority = priority
		}
		if len(models) > 0 {
			entry.Models = append([]string(nil), models...)
		}
		providers[name] = entry
		return nil
	}); err != nil {
		_, _ = fmt.Fprintf(stderr, "✗ set provider: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "✓ updated provider %s\n", name)
	if cfg, err := providerstore.LoadEffectiveConfig(cfgPath, false); err == nil {
		cliserve.MaybeReloadDaemon(args, cfg)
	}
	return 0
}

// ambiguityFor computes the implicit-routing ambiguity a NEW entry would add —
// the same check `presets add` gates on, against the would-be effective set.
func ambiguityFor(cfgPath, name string, entry configdomain.Provider) []string {
	cfg, err := providerstore.LoadEffectiveConfig(cfgPath, false)
	if err != nil {
		return nil
	}
	proposed := providerstore.Clone(cfg.Providers)
	proposed[name] = entry
	cfg.Providers = proposed
	return domainpresets.AmbiguousModels(cfg, name)
}
