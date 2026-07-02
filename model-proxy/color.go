package main

import (
	"os"
)

// Lightweight ANSI color helpers. Respects:
//   - NO_COLOR (disables color, https://no-color.org)
//   - CLICOLOR_FORCE=1 forces color on (even when not a tty, e.g. piped to `less -R`)
//   - non-tty without force -> auto-disable, so escape codes don't pollute piped output
//
// Standard library only, no external dependencies.

const (
	ansiReset   = "\033[0m"
	ansiBold    = "\033[1m"
	ansiDim     = "\033[2m"
	ansiRed     = "\033[31m"
	ansiGreen   = "\033[32m"
	ansiYellow  = "\033[33m"
	ansiBlue    = "\033[34m"
	ansiMagenta = "\033[35m"
	ansiCyan    = "\033[36m"
	ansiGray    = "\033[90m"
)

var colorEnabled = decideColor(os.Stdout)

// logColorEnabled controls whether runtime logs on stderr (log package, proxy
// request logs) get color. Separate from colorEnabled: when stderr alone is
// redirected to a file (e.g. model-proxy serve 2>proxy.log), only log color is
// disabled, leaving stdout status output unaffected. Files are never colored.
var logColorEnabled = decideColor(os.Stderr)

func decideColor(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("CLICOLOR_FORCE") != "" && os.Getenv("CLICOLOR_FORCE") != "0" {
		return true
	}
	return isTerminal(f)
}

// isTerminal approximates whether the fd is a terminal (char device). Standard library only.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// c wraps s in a color; returns s unchanged when color is disabled.
func c(code, s string) string {
	if !colorEnabled {
		return s
	}
	return code + s + ansiReset
}

// cl is like c but uses the log stream (stderr) color switch. For runtime logs.
func cl(code, s string) string {
	if !logColorEnabled {
		return s
	}
	return code + s + ansiReset
}

// statusColor colors text by upstream HTTP status: 2xx green, 3xx/4xx yellow, 5xx red, else gray.
func statusColor(status int, s string) string {
	var code string
	switch {
	case status >= 200 && status < 300:
		code = ansiGreen
	case status >= 300 && status < 500:
		code = ansiYellow
	case status >= 500:
		code = ansiRed
	default:
		code = ansiGray
	}
	return cl(code, s)
}

// Semantic helpers.
func cDim(s string) string    { return c(ansiDim, s) }
func cBold(s string) string   { return c(ansiBold, s) }
func cGreen(s string) string  { return c(ansiGreen, s) }
func cYellow(s string) string { return c(ansiYellow, s) }
func cRed(s string) string    { return c(ansiRed, s) }
func cCyan(s string) string   { return c(ansiCyan, s) }
func cBlue(s string) string   { return c(ansiBlue, s) }
func cMagenta(s string) string { return c(ansiMagenta, s) }
func cGray(s string) string   { return c(ansiGray, s) }

// usageRatioColor colors text by balance ratio: >=50% green, >=20% yellow, else red.
func usageRatioColor(balance, total float64, s string) string {
	if total <= 0 {
		return cYellow(s)
	}
	ratio := balance / total
	switch {
	case ratio >= 0.5:
		return cGreen(s)
	case ratio >= 0.2:
		return cYellow(s)
	default:
		return cRed(s)
	}
}
