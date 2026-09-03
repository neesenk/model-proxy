//go:build darwin

package upstreamproxy

import (
	"net/url"
	"os/exec"
)

// detectSystemProxy reads macOS system proxy settings via scutil. Any failure
// (scutil missing, unparsable output) means "no system proxy" — detection is
// best-effort and must never break startup.
func detectSystemProxy() *url.URL {
	output, err := exec.Command("scutil", "--proxy").Output()
	if err != nil {
		return nil
	}
	return parseScutilProxy(string(output))
}
