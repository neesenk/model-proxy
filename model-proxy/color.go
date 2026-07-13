package main

import (
	"os"

	"model-proxy/provider"
)

// Lightweight ANSI color helpers for the MAIN package. The stdout color +
// format helpers live in provider/display.go (shared with the provider usage
// display methods); this file re-exports them under the short main-package
// names (cDim, cBold, ...) so existing call sites are unchanged, and keeps the
// stderr LOG color helpers (cl, statusColor, logColorEnabled) here - they're
// log-stream-specific.

var colorEnabled = provider.ColorEnabled

// logColorEnabled controls whether runtime logs on stderr (log package, proxy
// request logs) get color. Separate from colorEnabled: when stderr alone is
// redirected to a file (e.g. model-proxy serve 2>proxy.log), only log color is
// disabled, leaving stdout status output unaffected. Files are never colored.
var logColorEnabled = decideLogColor(os.Stderr)

func decideLogColor(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("CLICOLOR_FORCE") != "" && os.Getenv("CLICOLOR_FORCE") != "0" {
		return true
	}
	return isTerminalLog(f)
}

// isTerminalLog approximates whether the fd is a terminal (char device).
func isTerminalLog(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// syncColorEnabled mirrors the main colorEnabled var into the provider package
// (tests force colorEnabled off during stdout capture; provider display must
// agree). Called when colorEnabled is toggled.
func syncColorEnabled() { provider.SetColorEnabled(colorEnabled) }

// cl is like c but uses the log stream (stderr) color switch. For runtime logs.
func cl(code, s string) string {
	if !logColorEnabled {
		return s
	}
	return code + s + "\033[0m"
}

const (
	logAnsiReset  = "\033[0m"
	logAnsiGreen  = "\033[32m"
	logAnsiYellow = "\033[33m"
	logAnsiRed    = "\033[31m"
	logAnsiGray   = "\033[90m"
)

// statusColor colors text by upstream HTTP status: 2xx green, 3xx/4xx yellow, 5xx red, else gray.
func statusColor(status int, s string) string {
	var code string
	switch {
	case status >= 200 && status < 300:
		code = logAnsiGreen
	case status >= 300 && status < 500:
		code = logAnsiYellow
	case status >= 500:
		code = logAnsiRed
	default:
		code = logAnsiGray
	}
	return cl(code, s)
}

// Stdout color helpers (delegate to provider/display.go).
func cDim(s string) string              { return provider.Dim(s) }
func cBold(s string) string             { return provider.Bold(s) }
func cGreen(s string) string            { return provider.Green(s) }
func cYellow(s string) string           { return provider.Yellow(s) }
func cRed(s string) string              { return provider.Red(s) }
func cCyan(s string) string             { return provider.Cyan(s) }
func cBlue(s string) string             { return provider.Blue(s) }
func cMagenta(s string) string          { return provider.Magenta(s) }
func cGray(s string) string             { return provider.Gray(s) }
func progressBar(pct, width int) string { return provider.ProgressBar(pct, width) }

// Format helpers (delegate to provider/display.go).
func money(v float64) string             { return provider.Money(v) }
func formatCredits(s string) string      { return provider.FormatCredits(s) }
func formatWithCommas(n int) string      { return provider.FormatWithCommas(n) }
func formatDuration(secs int) string     { return provider.FormatDuration(secs) }
func formatResetAt(resetMs int64) string { return provider.FormatResetAt(resetMs) }
func pad(s string, n int) string         { return provider.Pad(s, n) }
func or(s, fallback string) string       { return provider.Or(s, fallback) }
func truncate(s string, n int) string    { return provider.Truncate(s, n) }
