//go:build darwin

package provider

import "syscall"

// osVersion returns the macOS product version (kern.osproductversion), used for
// the X-Os-Version fingerprint header. Darwin-only: syscall.Sysctl is undefined
// on other platforms, so this lives behind a build tag (see zcode_osversion_other.go).
func osVersion() string {
	v, err := syscall.Sysctl("kern.osproductversion")
	if err != nil {
		return ""
	}
	return printableASCII(v)
}
