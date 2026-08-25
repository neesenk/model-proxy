// Package presets owns the provider preset catalog and the guided `add`
// command. The catalog is DERIVED from the annotated built-in template
// (configdomain.DefaultConfigYAML) — the single authoritative source for
// provider endpoints and default model lists — filtered to provider ids with
// registered implementations. No endpoint or model knowledge is duplicated
// here; adding a preset means editing the template (and, when new, the
// provider implementation).
//
// `model-proxy presets list` prints the catalog. `model-proxy add <preset>`
// merges the template's provider block into an existing config.yaml (via
// configedit, preserving comments/structure), runs the provider's login flow,
// and hot-reloads a running daemon — one command from zero to usable.
package presets

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mattn/go-isatty"
	"gopkg.in/yaml.v3"

	"model-proxy/internal/cli/framework"
	clilogin "model-proxy/internal/cli/login"
	cliserve "model-proxy/internal/cli/serve"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/configedit"
	"model-proxy/internal/provider"
)

// Preset is one catalog entry: a ready-to-use config providers block.
type Preset struct {
	// Name is the preset id AND the config providers: key.
	Name       string   `json:"name"`
	ProviderID string   `json:"provider_id"`
	BaseURL    string   `json:"base_url"`
	UsageURL   string   `json:"usage_url,omitempty"`
	Billing    string   `json:"billing,omitempty"`
	Models     []string `json:"models"`
}

// excludedPresets are template entries that must not surface in the public
// catalog: aqp's SSO mint endpoint is internal-network only (design doc
// design-s3-preset-wizard.md §3.1). Everything else with a registered
// implementation is listed.
var excludedPresets = map[string]bool{"aqp": true}

// List derives the catalog from the built-in annotated template, sorted by
// name. Returns an error if the shipped template itself fails to load — that
// is a build-time defect, never user input.
func List() ([]Preset, error) {
	tpl, err := loadTemplate()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(tpl.Providers))
	for name := range tpl.Providers {
		if !excludedPresets[name] && provider.IsRegistered(tpl.Providers[name].Provider) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]Preset, 0, len(names))
	for _, name := range names {
		p := tpl.Providers[name]
		models := append([]string(nil), p.Models...)
		sort.Strings(models)
		out = append(out, Preset{
			Name:       name,
			ProviderID: p.Provider,
			BaseURL:    p.OpenAIBaseURL,
			UsageURL:   p.UsageURL,
			Billing:    p.Billing,
			Models:     models,
		})
	}
	return out, nil
}

func loadTemplate() (*configdomain.Config, error) {
	tpl, err := configdomain.LoadConfigFromBytes("config.yaml", []byte(configdomain.DefaultConfigYAML))
	if err != nil {
		return nil, fmt.Errorf("built-in config template is invalid: %w", err)
	}
	return tpl, nil
}

// CmdPresets implements `presets list` (the only subcommand today).
func CmdPresets(args []string, _ io.Reader, stdout io.Writer, _ io.Writer) int {
	presets, err := List()
	if err != nil {
		fmt.Fprintln(stdout, "✗ "+err.Error())
		return 1
	}
	fmt.Fprintln(stdout, "Available provider presets:")
	fmt.Fprintln(stdout)
	for _, p := range presets {
		billing := p.Billing
		if billing == "" {
			billing = "plan"
		}
		fmt.Fprintf(stdout, "  %-12s provider=%-14s %-13s %d models\n", p.Name, p.ProviderID, billing, len(p.Models))
	}
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Add one to your config:")
	fmt.Fprintf(stdout, "  model-proxy add <%s>\n", strings.Join(presetNames(presets), "|"))
	return 0
}

func presetNames(presets []Preset) []string {
	out := make([]string, len(presets))
	for i, p := range presets {
		out[i] = p.Name
	}
	return out
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

	tpl, err := loadTemplate()
	if err != nil {
		fmt.Fprintln(stderr, "✗ "+err.Error())
		return 1
	}
	catalog, lerr := List()
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
			strings.Join(presetNames(catalog), "|"))
		return 1
	}
	tplProv, ok := tpl.Providers[presetName]
	if !ok || !provider.IsRegistered(tplProv.Provider) {
		fmt.Fprintf(stderr, "unknown preset %q — available:\n", presetName)
		for _, p := range catalog {
			fmt.Fprintf(stderr, "  %s\n", p.Name)
		}
		return 1
	}

	// 1. Merge the template provider block into the user config.
	written, merr := mergePresetBlock(cfgPath, presetName)
	if merr != nil {
		fmt.Fprintf(stderr, "✗ merge %s into %s: %v\n", presetName, filepath.Base(cfgPath), merr)
		return 1
	}
	merged, err := configdomain.LoadConfig(cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "✗ reload merged config: %v\n", err)
		return 1
	}
	if _, ok := merged.Providers[presetName]; !ok {
		fmt.Fprintf(stderr, "✗ provider %s missing after merge (template drift)\n", presetName)
		return 1
	}

	// 2. Ambiguity gate BEFORE any login side effects: models this provider
	// serves that other configured providers also serve with no explicit route
	// would resolve via implicit routing to whichever provider sorts first.
	ambiguous := ambiguousModels(merged, presetName)
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
			// Non-TTY cannot confirm → fail closed, do NOT login.
			fmt.Fprintf(stderr, "✗ %s\nre-run with --yes to proceed\n", msg)
			return 1
		}
	} else if written {
		fmt.Fprintf(stdout, "✓ added provider %s to %s (%d models)\n", presetName, filepath.Base(cfgPath), len(tplProv.Models))
	}

	// 3. Login (key from env for scripts; prompt/stdin flow interactively).
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

	// 4. Hot-reload a running daemon so the new provider is live immediately.
	cliserve.MaybeReloadDaemon(args, merged)

	// 5. Next steps — suggest a model actually configured on this provider.
	testModel := ""
	if p := merged.Providers[presetName]; len(p.Models) > 0 {
		testModel = p.Models[0]
	}
	fmt.Fprintln(stdout, "\nNext steps:")
	if testModel != "" {
		fmt.Fprintf(stdout, "  model-proxy test %s\n", testModel)
	}
	fmt.Fprintln(stdout, "  model-proxy takeover claude|opencode|codex|pi   # point a client at the proxy")
	return 0
}

// mergePresetBlock copies the template's providers.<preset> node into the user
// config (creating the providers: mapping when missing), validates the merged
// YAML before writing (fail-closed), and writes back preserving file mode.
// Idempotent: an existing block is left untouched (login still runs).
func mergePresetBlock(cfgPath, presetName string) (bool, error) {
	userRoot, err := configedit.LoadNode(cfgPath)
	if err != nil {
		return false, err
	}
	if configedit.MapNode(userRoot) == nil {
		return false, errors.New("config is not a YAML mapping")
	}
	providersMap := configedit.ChildMap(userRoot, "providers")

	existing := configedit.LookupChildMap(providersMap, presetName)
	if existing != nil {
		return false, nil // already configured — nothing to merge
	}

	tplNode, err := loadTemplateProviderNode(presetName)
	if err != nil {
		return false, err
	}
	configedit.SetChildNode(providersMap, presetName, tplNode)

	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(userRoot); err != nil {
		return true, err
	}
	if err := encoder.Close(); err != nil {
		return true, err
	}
	// Validate BEFORE writing: a merged config that fails to load must never
	// hit disk (fail-closed).
	if _, verr := configdomain.LoadConfigFromBytes(cfgPath, buffer.Bytes()); verr != nil {
		return true, fmt.Errorf("merged config invalid: %w", verr)
	}
	mode := os.FileMode(0o644)
	if info, serr := os.Stat(cfgPath); serr == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(cfgPath), "."+filepath.Base(cfgPath)+".tmp-*")
	if err != nil {
		return true, err
	}
	tmpName := tmp.Name()
	if _, werr := tmp.Write(buffer.Bytes()); werr != nil {
		tmp.Close()
		os.Remove(tmpName)
		return true, werr
	}
	if cerr := tmp.Close(); cerr != nil {
		os.Remove(tmpName)
		return true, cerr
	}
	if cerr := os.Chmod(tmpName, mode); cerr != nil {
		os.Remove(tmpName)
		return true, cerr
	}
	return true, os.Rename(tmpName, cfgPath)
}

// loadTemplateProviderNode extracts providers.<preset> from the built-in
// template as a deep-copied yaml.Node (round-trip through bytes — yaml.Node
// has no Clone). The node carries the template's structure for that block.
func loadTemplateProviderNode(presetName string) (*yaml.Node, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(configdomain.DefaultConfigYAML), &root); err != nil {
		return nil, fmt.Errorf("parse built-in template: %w", err)
	}
	mapping := root.Content[0]
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != "providers" || mapping.Content[i+1].Kind != yaml.MappingNode {
			continue
		}
		providers := mapping.Content[i+1]
		for j := 0; j+1 < len(providers.Content); j += 2 {
			if providers.Content[j].Value == presetName {
				block := providers.Content[j+1]
				encoded, err := yaml.Marshal(block)
				if err != nil {
					return nil, err
				}
				var clone yaml.Node
				if err := yaml.Unmarshal(encoded, &clone); err != nil {
					return nil, err
				}
				return clone.Content[0], nil
			}
		}
	}
	return nil, fmt.Errorf("preset %q not found in built-in template", presetName)
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

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
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

func pickPresetInteractively(stdin io.Reader, stdout io.Writer, catalog []Preset) (string, error) {
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

func cfgOrHint(path string) string {
	if path == "" {
		return "./config.yaml"
	}
	return path
}

// ambiguousModels returns preset models that appear in OTHER configured
// providers' model lists while no explicit routes: entry maps them.
func ambiguousModels(merged *configdomain.Config, presetName string) []string {
	target := merged.Providers[presetName]
	others := map[string]bool{}
	for name, p := range merged.Providers {
		if name == presetName {
			continue
		}
		for _, m := range p.Models {
			others[m] = true
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range target.Models {
		if others[m] && !seen[m] {
			seen[m] = true
			if _, routed := merged.Routes[m]; !routed {
				out = append(out, m)
			}
		}
	}
	sort.Strings(out)
	return out
}
