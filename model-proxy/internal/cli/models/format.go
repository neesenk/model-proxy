package models

import "model-proxy/provider"

// Terminal display helpers delegate to provider/display (single owner for
// color/format rules across CLI commands).
func cDim(s string) string            { return provider.Dim(s) }
func cBold(s string) string           { return provider.Bold(s) }
func cGreen(s string) string          { return provider.Green(s) }
func cYellow(s string) string         { return provider.Yellow(s) }
func cRed(s string) string            { return provider.Red(s) }
func cCyan(s string) string           { return provider.Cyan(s) }
func cBlue(s string) string           { return provider.Blue(s) }
func cGray(s string) string           { return provider.Gray(s) }
func pad(s string, n int) string      { return provider.Pad(s, n) }
func truncate(s string, n int) string { return provider.Truncate(s, n) }
