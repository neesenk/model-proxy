//go:build darwin

package provider

import "syscall"

// osVersion mirrors Node's os.release(), which ZCode feeds into the
// X-Os-Version fingerprint header (host: import { version as Gje } from "os").
// On darwin os.release() is the kernel version (kern.osrelease, e.g. "25.6.0"
// for macOS 26.x) — NOT the product version (kern.osproductversion, e.g.
// "26.6.2"); the 3.3.6-era implementation sent the product version and drifted.
// Darwin-only: syscall.Sysctl is undefined on other platforms, so this lives
// behind a build tag (see zcode_osversion_other.go).
func osVersion() string {
	v, err := syscall.Sysctl("kern.osrelease")
	if err != nil {
		return ""
	}
	return printableASCII(v)
}
