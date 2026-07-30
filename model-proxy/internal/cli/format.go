package cli

import (
	"fmt"
	"strconv"
	"strings"

	"model-proxy/provider"
)

// truncate caps a string at n bytes, trailing "..." (provider display rules).
func truncate(s string, n int) string { return provider.Truncate(s, n) }

// CompactNum renders a count compactly: 0, 5, 567, 1k, 1.2k, 450k, 1.2M, 5.6B.
// Rounding that would carry a value up to 1000 of its unit promotes to the next
// unit instead (e.g. 999999 → "1M", not "1000k").
func CompactNum(n uint64) string {
	switch {
	case n >= 1_000_000_000:
		return scaleNum(float64(n)/1e9, "B", "")
	case n >= 1_000_000:
		return scaleNum(float64(n)/1e6, "M", "B")
	case n >= 1_000:
		return scaleNum(float64(n)/1e3, "k", "M")
	}
	return strconv.FormatUint(n, 10)
}

// scaleNum formats v (already divided into unit suf) to one decimal with a
// trailing ".0" stripped. If v rounds up to 1000 of this unit, promote to
// "1"+next instead ("1M" rather than "1000k"). next=="" at the top unit (B).
func scaleNum(v float64, suf, next string) string {
	r := int64(v*10 + 0.5) // rounded tenths
	if r >= 10000 {        // 1000.0 of this unit — carry to the next unit
		if next != "" {
			return "1" + next
		}
		return "1000" + suf // top unit (B): no larger unit to promote to
	}
	return trimNumZero(fmt.Sprintf("%.1f", float64(r)/10)) + suf
}

// trimNumZero strips a trailing ".0" from a "%.1f" number string.
func trimNumZero(s string) string {
	if i := strings.Index(s, "."); i >= 0 && strings.HasSuffix(s, "0") {
		return s[:i]
	}
	return s
}
