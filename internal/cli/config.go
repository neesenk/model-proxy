package cli

import (
	"fmt"
	"log"
	"model-proxy/internal/app"
	cliframework "model-proxy/internal/cli/framework"
	displaypkg "model-proxy/internal/provider"
	"os"

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
