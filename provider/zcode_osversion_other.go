//go:build !darwin

package provider

// osVersion returns "" on non-darwin platforms: the X-Os-Version fingerprint
// header is omitted (ZCode omits it when unavailable). syscall.Sysctl is
// darwin-only, so the real implementation lives in zcode_osversion_darwin.go.
func osVersion() string {
	return ""
}
