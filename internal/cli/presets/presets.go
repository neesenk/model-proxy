// Package presets (cli) owns the `presets` and `add` CLI commands: terminal
// UX over the shared config-level catalog (internal/presets) plus the login
// orchestration and daemon hot-reload the web surface doesn't need.
package presets

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mattn/go-isatty"

	"model-proxy/internal/cli/framework"
	clilogin "model-proxy/internal/cli/login"
	cliserve "model-proxy/internal/cli/serve"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/login"
	domainpresets "model-proxy/internal/presets"
)

// CmdPresets implements `presets list` (the only subcommand today).
func CmdPresets(args []string, _ io.Reader, stdout io.Writer, _ io.Writer) int {
	catalog, err := domainpresets.List()
	if err != nil {
		fmt.Fprintln(stdout, "✗ "+err.Error())
		return 1
	}
	fmt.Fprintln(stdout, "Available provider presets:")
	fmt.Fprintln(stdout)
	for _, p := range catalog {
		billing := p.Billing
		if billing == "" {
			billing = "plan"
		}
		fmt.Fprintf(stdout, "  %-12s provider=%-14s %-13s %d models\n", p.Name, p.ProviderID, billing, len(p.Models))
	}
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Add one to your config:")
	fmt.Fprintf(stdout, "  model-proxy add <%s>\n", domainpresets.Names(catalog))
	return 0
}

// CmdAdd implements `add <preset> [--label NAME] [--replace]
// [--api-key-env ENV] [--yes]`. Exit codes: 0 success, 1 usage/refusal/failure.
func CmdAdd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cfgPath := framework.ConfigPath(args)
	presetName := firstPositional(args)
	label := framework.FlagStringValue(args, "--label")
	apiKeyEnv := framework.FlagStringValue(args, "--api-key-env")
	replace := framework.HasFlagValue(args, "--replace")
	assumeYes := framework.HasFlagValue(args, "--yes")

	if cfgPath == "" || !fileExists(cfgPath) {
		fmt.Fprintf(stderr, "no config.yaml found at %q — run `model-proxy config init` first\n", cfgOrHint(cfgPath))
		return 1
	}

	catalog, lerr := domainpresets.List()
	if lerr != nil {
		fmt.Fprintln(stderr, "✗ "+lerr.Error())
		return 1
	}

	// Interactive pick on a real terminal; otherwise require the positional.
	interactive := stdinIsInteractive(stdin)
	if presetName == "" && interactive {
		chosen, cerr := pickPresetInteractively(stdin, stdout, catalog)
		if cerr != nil {
			fmt.Fprintln(stderr, "✗ "+cerr.Error())
			return 1
		}
		presetName = chosen
	}
	if presetName == "" {
		fmt.Fprintf(stderr, "usage: model-proxy add <%s> [flags]\nrun `model-proxy presets list` for the catalog\n",
			domainpresets.Names(catalog))
		return 1
	}
	tplProv, ok := domainpresets.Lookup(presetName)
	if !ok {
		fmt.Fprintf(stderr, "unknown preset %q — available:\n", presetName)
		for _, p := range catalog {
			fmt.Fprintf(stderr, "  %s\n", p.Name)
		}
		return 1
	}

	// 1. Compute the would-be-merged config WITHOUT writing: the ambiguity
	// gate below runs against the merged result BEFORE anything hits disk,
	// so refusing the gate leaves config.yaml byte-identical (a credential-
	// less provider block must never reach the next reload).
	changed, mergedYAML, merr := domainpresets.PreviewMergeBlock(cfgPath, presetName)
	if merr != nil {
		fmt.Fprintf(stderr, "✗ merge %s into %s: %v\n", presetName, filepath.Base(cfgPath), merr)
		return 1
	}
	var merged *configdomain.Config
	if changed {
		merged, merr = configdomain.LoadConfigFromBytes(cfgPath, mergedYAML)
	} else {
		merged, merr = configdomain.LoadConfig(cfgPath)
	}
	if merr != nil {
		fmt.Fprintf(stderr, "✗ reload merged config: %v\n", merr)
		return 1
	}
	if _, ok := merged.Providers[presetName]; !ok {
		fmt.Fprintf(stderr, "✗ provider %s missing after merge (template drift)\n", presetName)
		return 1
	}

	// 2. Ambiguity gate BEFORE the write and any login side effects: models
	// this provider serves that other configured providers also serve with
	// no explicit route would resolve via implicit routing to whichever
	// provider sorts first.
	ambiguous := domainpresets.AmbiguousModels(merged, presetName)
	if len(ambiguous) > 0 {
		msg := fmt.Sprintf("model(s) %s are also served by other configured providers without explicit routes — "+
			"implicit routing picks alphabetically. Add routes: entries to control failover.",
			strings.Join(ambiguous, ", "))
		switch {
		case interactive && !assumeYes:
			if okConfirm := askYesNo(stdin, stdout, "Proceed anyway? [y/N] "); !okConfirm {
				fmt.Fprintln(stderr, "aborted")
				return 1
			}
			fmt.Fprintf(stdout, "! %s\n", msg)
		case interactive:
			fmt.Fprintf(stdout, "! %s\n", msg)
		default:
			// Non-TTY cannot confirm → fail closed, do NOT write or login.
			fmt.Fprintf(stderr, "✗ %s\nre-run with --yes to proceed\n", msg)
			return 1
		}
	}

	// 3. The gate passed — NOW persist the merged block (idempotent no-op
	// when the block already existed).
	written, werr := domainpresets.MergeBlock(cfgPath, presetName)
	if werr != nil {
		fmt.Fprintf(stderr, "✗ merge %s into %s: %v\n", presetName, filepath.Base(cfgPath), werr)
		return 1
	}
	if written && len(ambiguous) == 0 {
		fmt.Fprintf(stdout, "✓ added provider %s to %s (%d models)\n", presetName, filepath.Base(cfgPath), len(tplProv.Models))
	}

	// 4. Login (key from env for scripts; prompt/stdin flow interactively).
	keyIn := ""
	if apiKeyEnv != "" {
		keyIn = os.Getenv(apiKeyEnv)
		if keyIn == "" {
			fmt.Fprintf(stderr, "✗ environment variable %s is empty\n", apiKeyEnv)
			return 1
		}
	}
	if err := clilogin.RunProviderLogin(merged, presetName, keyIn, label, replace); err != nil {
		fmt.Fprintf(stderr, "✗ login failed: %v\n", err)
		return 1
	}

	// 4b. Model-visibility cross-check (apikey providers only): fetch the
	// account's visible models and warn when configured models are missing —
	// a stale preset (upstream renamed models) must surface, not route 404s.
	// Best-effort: probe errors stay silent (the endpoint may legitimately
	// not exist; validation already accepted the key).
	if login.ApiKeyLike(merged.Providers[presetName].Provider) {
		reportModelVisibility(stdout, merged, presetName)
	}

	// 5. Hot-reload a running daemon so the new provider is live immediately.
	cliserve.MaybeReloadDaemon(args, merged)

	// 6. Next steps — suggest a model actually configured on this provider.
	testModel := ""
	if p := merged.Providers[presetName]; len(p.Models) > 0 {
		testModel = p.Models[0]
	}
	fmt.Fprintln(stdout, "\nNext steps:")
	if testModel != "" {
		fmt.Fprintf(stdout, "  model-proxy test %s\n", testModel)
	}
	fmt.Fprintln(stdout, "  model-proxy takeover claude|opencode|codex|pi|kimi   # point a client at the proxy")
	return 0
}

// reportModelVisibility cross-checks the provider's configured models against
// the account's visible /models list and prints a warning for configured
// models the account cannot serve (stale preset / wrong plan). Best-effort:
// any probe error is silent — validation already accepted the key, and an
// endpoint that 404s (or an OAuth provider) says nothing about visibility.
func reportModelVisibility(stdout io.Writer, merged *configdomain.Config, provName string) {
	prov, ok := merged.Providers[provName]
	if !ok || len(prov.Models) == 0 {
		return
	}
	key := login.LatestAPIKey(provName, prov.Provider)
	if key == "" {
		return
	}
	visible, err := login.FetchVisibleModels(prov, key)
	if err != nil || len(visible) == 0 {
		return
	}
	set := make(map[string]bool, len(visible))
	for _, m := range visible {
		set[m] = true
	}
	var missing []string
	for _, m := range prov.Models {
		if !set[m] {
			missing = append(missing, m)
		}
	}
	if len(missing) == 0 {
		return
	}
	sortStrings(missing)
	fmt.Fprintf(stdout, "! %d configured model(s) not visible to this account (stale preset or different plan): %s\n",
		len(missing), strings.Join(missing, ", "))
	fmt.Fprintln(stdout, "  run `model-proxy models refresh "+provName+"` after fixing, or edit models: in config.yaml")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func cfgOrHint(path string) string {
	if path == "" {
		return "./config.yaml"
	}
	return path
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// firstPositional returns the first non-flag positional argument, skipping
// --config and its value, and the value tokens of value-taking flags so
// "--label x" doesn't swallow the preset name.
func firstPositional(args []string) string {
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		switch {
		case a == "--config":
			skipNext = true
			continue
		case strings.HasPrefix(a, "--config="):
			continue
		}
		if strings.HasPrefix(a, "-") {
			switch a {
			case "--label", "--api-key-env":
				skipNext = true
			}
			continue
		}
		return a
	}
	return ""
}

func stdinIsInteractive(stdin io.Reader) bool {
	f, ok := stdin.(*os.File)
	if !ok {
		return false
	}
	fd := f.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}

func askYesNo(r io.Reader, out io.Writer, prompt string) bool {
	fmt.Fprint(out, prompt)
	line, err := bufioReadLine(r)
	if err != nil && line == "" {
		return false
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

// bufioReadLine reads one \n-terminated line without buffering past it (the
// caller may keep reading from the same stream afterwards).
func bufioReadLine(r io.Reader) (string, error) {
	buf := make([]byte, 0, 64)
	one := make([]byte, 1)
	for {
		n, err := r.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				return strings.TrimSuffix(string(buf), "\r"), nil
			}
			buf = append(buf, one[0])
		}
		if err != nil {
			return string(buf), err
		}
	}
}

func pickPresetInteractively(stdin io.Reader, stdout io.Writer, catalog []domainpresets.Preset) (string, error) {
	fmt.Fprintln(stdout, "Which provider do you want to add?")
	for i, p := range catalog {
		fmt.Fprintf(stdout, "  %d. %-12s %d models\n", i+1, p.Name, len(p.Models))
	}
	fmt.Fprint(stdout, "number: ")
	line, err := bufioReadLine(stdin)
	line = strings.TrimSpace(line)
	if err != nil && line == "" {
		return "", errors.New("read selection: EOF")
	}
	idx := 0
	if _, serr := fmt.Sscanf(line, "%d", &idx); serr != nil || idx < 1 || idx > len(catalog) {
		return "", fmt.Errorf("invalid selection %q", line)
	}
	return catalog[idx-1].Name, nil
}
