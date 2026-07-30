package main

import (
	"fmt"
	climodels "model-proxy/internal/cli/models"
	"os"
	"sort"
	"strconv"
	"strings"

	configdomain "model-proxy/internal/config"
	"model-proxy/provider"
)

// cmdDoctor runs an OFFLINE diagnostic of the scheduling setup from config (no
// daemon needed): per-provider tier/quota source/peak_hours, per-route dry-run
// order (no live quota → all unknown → tier then priority), and warnings.
// Credential pools (≥2 accounts) are expanded inline: the parent is shown with
// its account count + the per-account virtual ids, plus a note that new sessions
// round-robin across the pool (offline: no live quota → falls back to priority).
func cmdDoctor(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		fmt.Println(cRed("✗ config invalid: ") + err.Error())
		os.Exit(1)
	}
	// --live replaces the offline report with the live daemon diagnosis; the
	// two never print together (the live report re-derives everything from
	// /api/status + local takeover state).
	if doctorLive(args) {
		out, err := renderDoctorLive(cfg, configPath(args))
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s %s\n", cRed("✗"), err.Error())
			os.Exit(1)
		}
		fmt.Print(out)
		return
	}
	doctorWithCfg(cfg)
}

// doctorWithCfg renders the doctor diagnostic for an already-loaded config.
// Extracted from cmdDoctor so tests can drive it in-process with a hand-built
// Config (no temp config file needed). Writes to stdout; returns the warning
// count.
func doctorWithCfg(cfg *Config) int {
	fmt.Println(cGreen("✓ config valid"))

	fmt.Printf("\n%s\n", cBold("Providers"))
	pnames := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		pnames = append(pnames, n)
	}
	sort.Strings(pnames)
	for _, name := range pnames {
		prov := cfg.Providers[name]
		tier := "plan"
		if prov.Billing == "pay-as-you-go" {
			tier = "pay-as-you-go"
		}
		extra := ""
		if vids, pooled := climodels.PoolVirtuals(cfg, name); pooled {
			extra = fmt.Sprintf("  pool: %d accounts", len(vids))
		}
		fmt.Printf("  %s %s  quota=%s  peak=%s%s\n",
			pad(name, 12), cCyan(pad(tier, 13)), cGray(quotaSourceLabel(prov.Provider)), peakSummary(prov.PeakHours), extra)
	}

	fmt.Printf("\n%s\n", cBold("Routes (dry-run: no live quota → tier then priority)"))
	warns := 0
	rnames := make([]string, 0, len(cfg.Routes))
	for n := range cfg.Routes {
		rnames = append(rnames, n)
	}
	sort.Strings(rnames)
	for _, exposed := range rnames {
		targets := cfg.Routes[exposed]
		fmt.Printf("  %s\n", exposed)
		hasPlan := false
		for _, t := range dryRunOrder(cfg, targets) {
			prov := cfg.Providers[t.Provider]
			tier := "plan"
			if prov.Billing == "pay-as-you-go" {
				tier = "pay-as-you-go"
			}
			if tier == "plan" {
				hasPlan = true
			}
			fmt.Printf("    %s %s  p%d\n", pad(t.Provider, 12), cCyan(pad(tier, 13)), t.Priority)
			// Wire-protocol note: a provider our protocol system can neither
			// passthrough nor convert (none today — codex/Responses is now
			// converted) would get the HONEST marker — which client families
			// can't be served — not a conversion suggestion.
			if note := provider.WireProtocolNote(prov.Provider); note != "" {
				fmt.Printf("        %s %s\n", cYellow("⚠"), note)
				warns++
			} else if t.Protocol != "" {
				// Protocol conversion (#11): a target declaring a backend protocol
				// converts client↔backend when they differ. Surface it + the fixed
				// set of fields conversion drops (so an operator wiring tools/images
				// knows what's lossy before traffic flows).
				fmt.Printf("        %s target protocol %s — converts when client protocol differs; remaining lossy: anthropic↔chat thinking, unsupported server tools, input_audio (cache breakpoints/documents/tool-result media preserved)\n",
					cYellow("↔"), t.Protocol)
				// Reasoning-replay marker (#9): for models that REQUIRE reasoning
				// content echoed back, the dropped thinking/reasoning is fatal to
				// multi-turn tool calls, not just lossy.
				if reasoningReplayModel(t.Model) && t.Protocol == "openai" {
					fmt.Printf("        %s reasoning-required model behind openai-chat conversion — Anthropic thinking is dropped; multi-turn tool calls may 400 upstream (replay cache not implemented)\n",
						cYellow("⚠"))
					warns++
				}
			} else if hint := provider.ProtocolHint(prov.Provider, t.Model); hint != "" {
				fmt.Printf("        %s no protocol: declared, but %s speaks %s — clients of the other protocol will send malformed bodies; add protocol: %s\n",
					cYellow("⚠"), prov.Provider, hint, hint)
				warns++
			}
			// Expand a pooled parent inline: show its account count + the
			// per-account virtual ids. Offline (no live quota) so we can't show
			// per-account surplus — note the session-sticky round-robin so an
			// operator understands how traffic spreads at runtime.
			if vids, pooled := climodels.PoolVirtuals(cfg, t.Provider); pooled {
				fmt.Printf("        %s %d accounts (round-robin session-sticky; no live quota → falls back to priority)\n",
					cDim("pool:"), len(vids))
				for _, vid := range vids {
					fmt.Printf("        %s\n", cGray(vid))
				}
			}
		}
		if !hasPlan {
			fmt.Printf("    %s no plan provider — only pay-as-you-go\n", cYellow("⚠"))
			warns++
		}
	}

	// Shadow evaluation: each route's candidate backend + the global sampling
	// knobs. Omitted entirely when no shadow is configured.
	if len(cfg.Shadow) > 0 {
		fmt.Printf("\n%s\n", cBold("Shadow"))
		rate := 1.0
		if cfg.ShadowSampleRate != nil {
			rate = *cfg.ShadowSampleRate
		}
		maxConc := cfg.ShadowMaxConcurrent
		if maxConc <= 0 {
			maxConc = 4
		}
		sroutes := make([]string, 0, len(cfg.Shadow))
		for r := range cfg.Shadow {
			sroutes = append(sroutes, r)
		}
		sort.Strings(sroutes)
		for _, route := range sroutes {
			sh := cfg.Shadow[route]
			proto := sh.Protocol
			if proto == "" {
				proto = "same-as-primary"
			}
			fmt.Printf("  %s → %s/%s  protocol=%s  sample_rate=%s  max_concurrent=%d\n",
				pad(route, 12), sh.Provider, sh.Model, proto, strconv.FormatFloat(rate, 'f', -1, 64), maxConc)
		}
	}

	// Fusion orchestration: one line per recipe — panel size/quorum/synthesizer
	// plus the cost knobs (budget, first_turn_only) and quality knobs (judge,
	// custom instruction). Omitted entirely when no fusion recipe is configured.
	if len(cfg.Fusion) > 0 {
		fmt.Printf("\n%s\n", cBold("Fusion"))
		fnames := make([]string, 0, len(cfg.Fusion))
		for n := range cfg.Fusion {
			fnames = append(fnames, n)
		}
		sort.Strings(fnames)
		for _, name := range fnames {
			f := cfg.Fusion[name]
			quorum := f.MinPanel
			if quorum <= 0 {
				quorum = 2
			}
			if quorum > len(f.Panel) {
				quorum = len(f.Panel)
			}
			budget := "unlimited"
			if f.MaxRunsPerDay > 0 {
				budget = strconv.Itoa(f.MaxRunsPerDay) + "/day"
			}
			extra := ""
			if f.FirstTurnOnly {
				extra += "  first_turn_only"
			}
			if f.Judge != nil {
				extra += fmt.Sprintf("  judge=%s/%s", f.Judge.Provider, f.Judge.Model)
			}
			if f.Instruction != "" {
				extra += "  custom instruction"
			}
			fmt.Printf("  %s panel=%d quorum=%d  synthesizer=%s/%s  budget=%s%s\n",
				pad(name, 12), len(f.Panel), quorum, f.Synthesizer.Provider, f.Synthesizer.Model, budget, extra)
		}
	}

	s := cfg.Scheduling
	fmt.Printf("\n%s\n", cBold("Scheduling"))
	fmt.Printf("  sticky_dwell=%s  quota_poll_interval=%s  quota_switch_margin=%d pts  circuit=(threshold %d, cooldown %s)\n",
		s.Dwell(), s.PollInterval(), s.QuotaSwitchMargin, s.Threshold(), s.Cooldown())
	if warns > 0 {
		fmt.Printf("\n%s %d warning(s)\n", cYellow("⚠"), warns)
	} else {
		fmt.Printf("\n%s no warnings\n", cGreen("✓"))
	}
	return warns
}

// quotaSourceLabel returns a short label for where a provider's quota comes from
// (by provider_id), or "(none → unknown at runtime)" for ids without a Quota parser.
func quotaSourceLabel(providerID string) string {
	switch providerID {
	case "aqp":
		return "monthly_usage"
	case "codex":
		return "wham/usage"
	case "zhipu":
		return "quota/limit"
	case "volcengine":
		return "GetAFPUsage (AK/SK)"
	case "deepseek":
		return "user/balance"
	case "kimi-code":
		return "usages"
	case "zcode":
		return "quota/limit"
	default:
		return "(none → unknown at runtime)"
	}
}

// peakSummary renders a PeakConfig as "09:00-12:00(×2), 14:00-18:00(×2)" or "-".
func peakSummary(ph PeakConfig) string {
	if len(ph) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(ph))
	for _, seg := range ph {
		mult := seg.Multiplier
		if mult == 0 {
			mult = configdomain.DefaultPeakMultiplier
		}
		parts = append(parts, fmt.Sprintf("%s(×%g)", seg.Window, mult))
	}
	return strings.Join(parts, ", ")
}

// dryRunOrder sorts targets by the offline schedule order: billing tier (plan
// before pay-as-you-go), then priority asc. With no live quota all plan-intent
// providers are equal-surplus, so tier + priority decide.
func dryRunOrder(cfg *Config, targets []RouteTarget) []RouteTarget {
	out := append([]RouteTarget(nil), targets...)
	sort.SliceStable(out, func(i, j int) bool {
		ti, tj := 0, 0
		if cfg.Providers[out[i].Provider].Billing == "pay-as-you-go" {
			ti = 1
		}
		if cfg.Providers[out[j].Provider].Billing == "pay-as-you-go" {
			tj = 1
		}
		if ti != tj {
			return ti < tj
		}
		return out[i].Priority < out[j].Priority
	})
	return out
}
