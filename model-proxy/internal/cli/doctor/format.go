package doctor

import "model-proxy/provider"

// Terminal display helpers delegate to provider/display (same owner as
// internal/cli format helpers).
func cRed(s string) string            { return provider.Red(s) }
func cGreen(s string) string          { return provider.Green(s) }
func cYellow(s string) string         { return provider.Yellow(s) }
func cDim(s string) string            { return provider.Dim(s) }
func cBold(s string) string           { return provider.Bold(s) }
func cCyan(s string) string           { return provider.Cyan(s) }
func cGray(s string) string           { return provider.Gray(s) }
func pad(s string, n int) string      { return provider.Pad(s, n) }
func truncate(s string, n int) string { return provider.Truncate(s, n) }

// plural returns sing for n==1 else plur.
func plural(n int, sing, plur string) string {
	if n == 1 {
		return sing
	}
	return plur
}
