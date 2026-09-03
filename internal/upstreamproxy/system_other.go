//go:build !darwin && !windows && !linux

package upstreamproxy

import "net/url"

// detectSystemProxy has no implementation on this platform; the chain falls
// back to direct when no env proxy is set.
func detectSystemProxy() *url.URL { return nil }
