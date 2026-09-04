// Package display owns the terminal color state and text formatting
// utilities shared by the CLI, provider usage display, and log streams. It is
// a zero-dependency leaf (stdlib only) so any layer may import it.
package display

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// display.go holds the stdout color + format helpers shared by the CLI and the
// provider usage-display methods (Usage()), plus the stderr log-stream color
// helpers (LogColor/StatusColor) shared by CLI runtime logs.

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

// ColorEnabled controls stdout color for the provider display helpers. Decided
// at init from os.Stdout (tty -> on); NO_COLOR disables, CLICOLOR_FORCE=1
// forces on.
var ColorEnabled = decideColor(os.Stdout)

func decideColor(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("CLICOLOR_FORCE") != "" && os.Getenv("CLICOLOR_FORCE") != "0" {
		return true
	}
	return isTerminal(f)
}

// isTerminal approximates whether the fd is a terminal (char device).
// /dev/null is also a char device but never interactive — exclude it so
// redirecting stdout to /dev/null doesn't turn color escape codes on. Kept
// stdlib-only (this package is a zero-dependency leaf); interactive TTYs are
// char devices too, so this stays an approximation.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	if fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	// File.Stat basenames the path, so compare against /dev/null's base name
	// (os.DevNull is "/dev/null" on unix, "NUL" on windows).
	return fi.Name() != filepath.Base(os.DevNull) && fi.Name() != "NUL"
}

// SetColorEnabled lets tests and embedding callers override stdout color.
func SetColorEnabled(on bool) { ColorEnabled = on }

// C wraps s in a color; returns s unchanged when color is disabled.
func C(code, s string) string {
	if !ColorEnabled {
		return s
	}
	return code + s + ansiReset
}

// Semantic helpers.
func Dim(s string) string     { return C(ansiDim, s) }
func Bold(s string) string    { return C(ansiBold, s) }
func Green(s string) string   { return C(ansiGreen, s) }
func Yellow(s string) string  { return C(ansiYellow, s) }
func Red(s string) string     { return C(ansiRed, s) }
func Cyan(s string) string    { return C(ansiCyan, s) }
func Blue(s string) string    { return C(ansiBlue, s) }
func Magenta(s string) string { return C(ansiMagenta, s) }
func Gray(s string) string    { return C(ansiGray, s) }

// UsageRatioColor colors text by balance ratio: >=50% green, >=20% yellow, else red.
func UsageRatioColor(balance, total float64, s string) string {
	if total <= 0 {
		return Yellow(s)
	}
	ratio := balance / total
	switch {
	case ratio >= 0.5:
		return Green(s)
	case ratio >= 0.2:
		return Yellow(s)
	default:
		return Red(s)
	}
}

// ProgressBar renders a [██░░░] bar of given width, colored by remaining ratio.
// pct is the used percentage (0-100). The filled portion uses ratio coloring
// (green < 50%, yellow < 80%, red >= 80%), empty portion is dim.
func ProgressBar(pct, width int) string {
	if width < 4 {
		width = 4
	}
	filled := pct * width / 100
	if filled > width {
		filled = width
	}
	bar := ""
	for i := 0; i < width; i++ {
		if i < filled {
			bar += "█"
		} else {
			bar += "░"
		}
	}
	return UsageRatioColor(float64(100-pct), 100, "["+bar+"]")
}

// --- format helpers ---

// Money formats v as $X.XX.
func Money(v float64) string { return fmt.Sprintf("$%.2f", v) }

// FormatCredits formats a credit amount string (e.g. "330.258..." -> "330",
// "22500" -> "22,500"). Truncates decimals, adds thousands separators.
func FormatCredits(s string) string {
	f := 0.0
	fmt.Sscanf(s, "%f", &f)
	return FormatWithCommas(int(f))
}

// FormatWithCommas adds thousands separators to an integer.
func FormatWithCommas(n int) string {
	s := fmt.Sprintf("%d", n)
	if n < 0 {
		return "-" + FormatWithCommas(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// FormatDuration converts seconds to a compact human-readable string (e.g. "5h", "7d3h").
// Non-positive secs render as an em-dash, matching the pre-Phase-3 main helper.
func FormatDuration(secs int) string {
	if secs <= 0 {
		return "—"
	}
	d := secs / 86400
	h := (secs % 86400) / 3600
	m := (secs % 3600) / 60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd%dh", d, h)
	case h > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

// FormatResetAt formats a reset time (epoch ms) for display: if it falls on
// today's date, only HH:MM; otherwise MM-DD HH:MM.
func FormatResetAt(resetMs int64) string {
	t := time.UnixMilli(resetMs).Local()
	if t.Format("20060102") == time.Now().Format("20060102") {
		return t.Format("15:04")
	}
	return t.Format("01-02 15:04")
}

// Pad right-pads s with spaces to length n.
func Pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

// Or returns s if non-empty, else fallback.
func Or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// Truncate caps s at n bytes (trailing "...").
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// --- log-stream (stderr) color, shared by CLI runtime logs ---

// LogColorEnabled mirrors the root CLI's stderr color decision (NO_COLOR /
// CLICOLOR_FORCE / terminal detection), evaluated once at startup.
var LogColorEnabled = DecideLogColor(os.Stderr)

// DecideLogColor applies NO_COLOR / CLICOLOR_FORCE / terminal detection.
func DecideLogColor(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("CLICOLOR_FORCE") != "" && os.Getenv("CLICOLOR_FORCE") != "0" {
		return true
	}
	return IsTerminalLog(f)
}

// IsTerminalLog approximates whether the fd is a terminal (char device).
func IsTerminalLog(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

const (
	LogAnsiReset  = "\033[0m"
	LogAnsiGreen  = "\033[32m"
	LogAnsiYellow = "\033[33m"
	LogAnsiRed    = "\033[31m"
	LogAnsiGray   = "\033[90m"
)

// LogColor wraps s in code when the log color switch is on.
func LogColor(code, s string) string {
	if !LogColorEnabled {
		return s
	}
	return code + s + LogAnsiReset
}

// StatusColor colors text by upstream HTTP status: 2xx green, 3xx/4xx yellow,
// 5xx red, else gray.
func StatusColor(status int, s string) string {
	var code string
	switch {
	case status >= 200 && status < 300:
		code = LogAnsiGreen
	case status >= 300 && status < 500:
		code = LogAnsiYellow
	case status >= 500:
		code = LogAnsiRed
	default:
		code = LogAnsiGray
	}
	return LogColor(code, s)
}
