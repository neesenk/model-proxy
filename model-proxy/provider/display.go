package provider

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// display.go holds the stdout color + format helpers shared by the provider
// usage-display methods (Usage()) and re-exported by the main package. The
// stderr log color helpers (cl/statusColor/logColorEnabled) stay in the main
// package - they're log-stream-specific.

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
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
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
