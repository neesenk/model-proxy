package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// pin_cmd.go implements `model-proxy pin` / `unpin` — temporary runtime hot-
// switch of a route's provider WITHOUT editing config.yaml. It talks to the
// running daemon's /api/pin (the live effect is applied inside decideOrder, and
// visible in /debug/schedule), so the change is lost on daemon restart.
//
//	pin <route> <provider> [--ttl 1h]   force a route onto one provider (no failover)
//	pin                                 list active pins
//	unpin <route>                       remove a pin

// cmdPin: with a route+provider, set a pin; with no args, list active pins.
func cmdPin(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %s\n", cRed("✗"), err)
		os.Exit(1)
	}
	base := "http://" + cfg.Listen
	pos := positionalArgs(args)
	if len(pos) == 0 {
		out, err := doListPins(base)
		emitPinResult(os.Stdout, os.Stderr, out, err, cfg.Listen)
		return
	}
	if len(pos) < 2 {
		fmt.Fprintf(os.Stderr, "%s usage: model-proxy pin <route> <provider> [--ttl DUR]\n", cRed("✗"))
		os.Exit(1)
	}
	out, err := doPin(base, pos[0], pos[1], parsePinTTL(args))
	emitPinResult(os.Stdout, os.Stderr, out, err, cfg.Listen)
}

// cmdUnpin removes a route's pin.
func cmdUnpin(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %s\n", cRed("✗"), err)
		os.Exit(1)
	}
	pos := positionalArgs(args)
	if len(pos) == 0 {
		fmt.Fprintf(os.Stderr, "%s usage: model-proxy unpin <route>\n", cRed("✗"))
		os.Exit(1)
	}
	out, err := doUnpin("http://"+cfg.Listen, pos[0])
	emitPinResult(os.Stdout, os.Stderr, out, err, cfg.Listen)
}

// emitPinResult prints a do*/listPins result line to stdout, or a ✗ error to
// stderr + exit 1. Extracted so tests drive do* directly without os.Exit.
func emitPinResult(_ *os.File, stderr *os.File, out string, err error, listen string) {
	if err != nil {
		if isDaemonUnreachable(err) {
			fmt.Fprintf(stderr, "%s cannot reach daemon at %s: %v\nis `model-proxy serve` running?\n", cRed("✗"), listen, err)
		} else {
			fmt.Fprintf(stderr, "%s %s\n", cRed("✗"), err)
		}
		os.Exit(1)
	}
	fmt.Print(out)
}

// doPin sets a pin via POST /api/pin and returns the success line (or an error
// carrying the daemon's message on a non-200). base is "http://<listen>".
func doPin(base, route, provider string, ttl time.Duration) (string, error) {
	raw, _ := json.Marshal(map[string]any{
		"route": route, "provider": provider, "ttl_seconds": int64(ttl.Seconds()),
	})
	resp, err := daemonHTTPClient.Post(base+"/api/pin", "application/json", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("%s", truncate(strings.TrimSpace(string(rb)), 200))
	}
	var out struct {
		ExpiresAt string `json:"expires_at"`
	}
	json.Unmarshal(rb, &out)
	suffix := "no expiry (until unpin or restart)"
	if out.ExpiresAt != "" {
		suffix = "expires " + out.ExpiresAt
	}
	return fmt.Sprintf("%s pinned %s → %s (%s)\n", cGreen("✓"), route, provider, suffix), nil
}

// doUnpin removes a pin via DELETE /api/pin?route= and returns the result line.
func doUnpin(base, route string) (string, error) {
	req, _ := http.NewRequest(http.MethodDelete, base+"/api/pin?"+url.Values{"route": {route}}.Encode(), nil)
	resp, err := daemonHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("daemon returned HTTP %d: %s", resp.StatusCode, truncate(string(rb), 200))
	}
	var out struct {
		Removed bool `json:"removed"`
	}
	json.Unmarshal(rb, &out)
	if out.Removed {
		return fmt.Sprintf("%s unpinned %s\n", cGreen("✓"), route), nil
	}
	return fmt.Sprintf("%s no pin on %s\n", cDim("•"), route), nil
}

// doListPins fetches GET /api/pin and returns the rendered pin table (or the
// "(no active pins)" line).
func doListPins(base string) (string, error) {
	resp, err := daemonHTTPClient.Get(base + "/api/pin")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("daemon returned HTTP %d: %s", resp.StatusCode, truncate(string(rb), 200))
	}
	var out struct {
		Pins []struct {
			Route     string `json:"route"`
			Provider  string `json:"provider"`
			ExpiresAt string `json:"expires_at"`
		} `json:"pins"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", fmt.Errorf("parse pins response: %w", err)
	}
	if len(out.Pins) == 0 {
		return "(no active pins)\n", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-24s %-20s %s\n", "ROUTE", "PROVIDER", "EXPIRES")
	for _, p := range out.Pins {
		exp := p.ExpiresAt
		if exp == "" {
			exp = "never"
		}
		fmt.Fprintf(&b, "%-24.24s %-20.20s %s\n", p.Route, p.Provider, exp)
	}
	return b.String(), nil
}

// isDaemonUnreachable reports whether err is a connection failure to the daemon
// (so the CLI can append the "is model-proxy serve running?" hint).
func isDaemonUnreachable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "connection refused")
}

// parsePinTTL extracts a --ttl duration from args (e.g. "--ttl 1h" or "--ttl=1h").
// Zero (the default) means no expiry.
func parsePinTTL(args []string) time.Duration {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--ttl" && i+1 < len(args) {
			return parseTTLValue(args[i+1])
		}
		if strings.HasPrefix(a, "--ttl=") {
			return parseTTLValue(strings.TrimPrefix(a, "--ttl="))
		}
	}
	return 0
}

func parseTTLValue(s string) time.Duration {
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	fmt.Fprintf(os.Stderr, "%s invalid --ttl %q (use a Go duration like 1h, 30m, 2h45m)\n", cRed("✗"), s)
	os.Exit(1)
	return 0
}

// positionalArgs returns the non-flag tokens of args (the inverse of configPath:
// it drops --config PATH and every other --flag[ =val]). Used by pin/unpin to
// pull <route> [<provider>] out of a flag-laden argv.
func positionalArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--config" {
			i++ // skip its value
			continue
		}
		if strings.HasPrefix(a, "--") {
			if !strings.Contains(a, "=") {
				i++ // skip --flag value
			}
			continue
		}
		out = append(out, a)
	}
	return out
}
