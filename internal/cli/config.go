package cli

import (
	"fmt"
	"log"
	"model-proxy/internal/app"
	cliframework "model-proxy/internal/cli/framework"
	displaypkg "model-proxy/internal/provider"
	"os"
	"strings"

	configdomain "model-proxy/internal/config"
)

func CmdConfig(args []string, cfg *configdomain.Config) {
	sub := cliframework.Positional(args)
	if sub == "" {
		fmt.Println("usage: model-proxy config [init|print|check]")
		os.Exit(1)
	}
	switch sub {
	case "init":
		// Refuse to clobber an existing ./config.yaml: init is a one-shot
		// template writer, and overwriting would silently destroy the user's
		// live config (the file `serve` loads by default).
		if _, err := os.Stat("config.yaml"); err == nil {
			fmt.Fprintln(os.Stderr, "config.yaml already exists — refusing to overwrite it (move it aside or delete it first)")
			os.Exit(1)
		} else if !os.IsNotExist(err) {
			log.Fatal(err)
		}
		// Interactive terminal: guided wizard (client detection, provider
		// selection, optional takeover). Pipes/scripts keep the static template.
		if configInitInteractive() {
			if err := runConfigInitWizard(os.Stdin, os.Stdout); err != nil {
				log.Fatal(err)
			}
			return
		}
		if err := os.WriteFile("config.yaml", []byte(configdomain.DefaultConfigYAML), 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Println("wrote config.yaml")
	case "print":
		fmt.Printf("listen: %s\n", cfg.Listen)
		for name, prov := range cfg.Providers {
			fmt.Printf("provider %s: openai_base_url=%s provider_id=%s (%d models)\n", name, prov.OpenAIBaseURL, prov.Provider, len(prov.Models))
		}
		for exposed, targets := range cfg.Routes {
			fmt.Printf("route %s: %d targets\n", exposed, len(targets))
		}
		if len(cfg.ClaudeMapping) > 0 {
			fmt.Printf("claude_mapping: %d aliases\n", len(cfg.ClaudeMapping))
		}
	case "check":
		fmt.Println(displaypkg.Green("✓ config valid"))
		fmt.Printf("  listen:    %s\n", cfg.Listen)
		fmt.Printf("  log_file:  %s\n", cfg.LogFile)
		fmt.Printf("  providers: %d\n", len(cfg.Providers))
		for name, prov := range cfg.Providers {
			fmt.Printf("    %s: %s (%s, %d models)\n", name, prov.OpenAIBaseURL, prov.Provider, len(prov.Models))
		}
		fmt.Printf("  routes:    %d\n", len(cfg.Routes))
		for exposed, targets := range cfg.Routes {
			fmt.Printf("    %s: %d targets\n", exposed, len(targets))
		}
		fmt.Printf("  claude_mapping: %d\n", len(cfg.ClaudeMapping))
		s := cfg.Scheduling
		fmt.Printf("  scheduling: threshold=%d cooldown=%s rate_backoff=%s timeout=%s dwell=%s\n",
			s.Threshold(), s.Cooldown(), s.RateBackoff(), s.Timeout(), s.Dwell())
		fmt.Print(RenderGuardSummary(cfg))
		// Config-time routing hazards (explicit routes only — implicit routes are
		// a daemon-side concept; the daemon logs these at boot/reload).
		for _, w := range app.ConfigRoutingWarnings(cfg, cfg.Routes) {
			fmt.Println(displaypkg.Yellow("  ⚠ " + w))
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown config subcommand: %s\n", sub)
		os.Exit(1)
	}
}

// RenderGuardSummary renders the effective outbound secret-guard settings for
// `config check`. The built-in rule tables are embedded in the binary
// (internal/guard) — reporting only their presence keeps the CLI free of a
// dependency edge on the guard package; custom extensions from
// guard.extra_patterns / guard.extra_paths are counted and named. All values
// are effective values (load-time defaults applied), same as scheduling.
func RenderGuardSummary(cfg *configdomain.Config) string {
	g := cfg.Guard
	var b strings.Builder
	fmt.Fprintf(&b, "  guard: secrets=%s known_secrets=%t decode=%t paths=%s audit=%t\n",
		g.SecretsAction(), g.KnownSecretsEnabled(), g.DecodeEnabled(), g.PathsAction(), g.AuditEnabled())
	fmt.Fprintf(&b, "    audit_path: %s\n", g.AuditPathValue(cliframework.HomeDir()))
	names := make([]string, 0, len(g.ExtraPatterns))
	for _, p := range g.ExtraPatterns {
		names = append(names, p.Name)
	}
	fmt.Fprintf(&b, "    patterns: built-in tables (embedded) + %d custom", len(g.ExtraPatterns))
	if len(names) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(names, ", "))
	}
	b.WriteByte('\n')
	fmt.Fprintf(&b, "    extra_paths: %d\n", len(g.ExtraPaths))
	return b.String()
}

// CmdConfigRun is the process-level entry: it owns config loading (init skips
// it) and delegates to CmdConfig.
func CmdConfigRun(args []string) {
	// config init writes the template without loading config; print/check need
	// a loaded config. The subcommand comes from Positional and the config path
	// from a FULL-args scan (like every other command), so
	// `model-proxy config --config X check` works regardless of flag position.
	if cliframework.Positional(args) == "init" {
		CmdConfig(args, nil)
		return
	}
	cfg, err := configdomain.LoadConfig(cliframework.ConfigPath(args))
	if err != nil {
		log.Fatal(err)
	}
	CmdConfig(args, cfg)
}
