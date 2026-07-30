package main

import (
	"fmt"
	"log"
	"os"

	configdomain "model-proxy/internal/config"
)

func cmdConfig(args []string) {
	if len(args) == 0 {
		fmt.Println("usage: model-proxy config [init|print|check]")
		os.Exit(1)
	}
	switch args[0] {
	case "init":
		if err := os.WriteFile("config.yaml", []byte(configdomain.DefaultConfigYAML), 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Println("wrote config.yaml")
	case "print":
		cfg, err := LoadConfig(configPath(args[1:]))
		if err != nil {
			log.Fatal(err)
		}
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
		cfg, err := LoadConfig(configPath(args[1:]))
		if err != nil {
			fmt.Println(cRed("✗ config invalid") + ": " + err.Error())
			os.Exit(1)
		}
		fmt.Println(cGreen("✓ config valid"))
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
		for _, w := range configRoutingWarnings(cfg, cfg.Routes) {
			fmt.Println(cYellow("  ⚠ " + w))
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown config subcommand: %s\n", args[0])
		os.Exit(1)
	}
}
