//go:build !darwin

package provider

import (
	"os"
	"runtime"
)

// osVersion mirrors Node's os.release(), which ZCode feeds into the
// X-Os-Version fingerprint header. On linux os.release() is the kernel release
// (uname -r, /proc/sys/kernel/osrelease). On windows os.release() reports the
// kernel version via Node's internal OS metadata, which stdlib Go cannot read
// portably — we return "" there and ZCode's own "omit when unavailable" rule
// applies (ZCode omits the header when the value doesn't pass its
// printable-ASCII normalizer). The darwin implementation lives in
// zcode_osversion_darwin.go (syscall.Sysctl is darwin-only).
func osVersion() string {
	switch runtime.GOOS {
	case "linux":
		b, err := os.ReadFile("/proc/sys/kernel/osrelease")
		if err != nil {
			return ""
		}
		return printableASCII(string(b))
	default:
		return ""
	}
}
