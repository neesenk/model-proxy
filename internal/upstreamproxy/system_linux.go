//go:build linux

package upstreamproxy

import (
	"net/url"
	"os/exec"
	"strconv"
	"strings"
)

// detectSystemProxy reads GNOME proxy settings via gsettings (best-effort).
// Non-GNOME desktops, missing gsettings, or non-manual mode all mean "no
// system proxy" — env vars remain the supported Linux mechanism there.
func detectSystemProxy() *url.URL {
	get := func(keys ...string) string {
		out, err := exec.Command("gsettings", append([]string{"get"}, keys...)...).Output()
		if err != nil {
			return ""
		}
		return strings.Trim(strings.TrimSpace(string(out)), "'")
	}
	mode := get("org.gnome.system.proxy", "mode")
	host := get("org.gnome.system.proxy.http", "host")
	port, _ := strconv.Atoi(get("org.gnome.system.proxy.http", "port"))
	return parseGnomeProxy(mode, host, port)
}
