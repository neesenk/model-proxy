package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// defaultCodexClientVersion is the last-resort client_version sent to the codex
// /models endpoint when neither config, the codex CLI, nor the codex models
// cache yields a version. Bump manually; auto-detection normally wins.
const defaultCodexClientVersion = "0.144.1"

var semverRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

// resolveCodexClientVersion returns the first non-empty (trimmed) version from
// configVal, cliVer, cacheVer (in that order); if all are empty it falls back
// to defaultCodexClientVersion. cliVer/cacheVer are func params so tests can
// inject fakes without spawning processes or touching the filesystem.
func resolveCodexClientVersion(configVal string, cliVer, cacheVer func() string) string {
	for _, src := range []func() string{func() string { return configVal }, cliVer, cacheVer} {
		if v := strings.TrimSpace(src()); v != "" {
			return v
		}
	}
	return defaultCodexClientVersion
}

// parseSemver extracts the first MAJOR.MINOR.PATCH token from s (e.g. the
// output of `codex --version`, "codex 0.144.1"). Empty if none found.
func parseSemver(s string) string {
	if m := semverRe.FindString(s); m != "" {
		return m
	}
	return ""
}

// codexCLIVersion runs `codex --version` and returns its MAJOR.MINOR.PATCH
// version, or "" if the codex CLI is absent or unparsable.
func codexCLIVersion() string {
	out, err := exec.Command("codex", "--version").Output()
	if err != nil {
		return ""
	}
	return parseSemver(string(out))
}

// codexCacheVersion reads the client_version the codex CLI persisted in its
// models cache, or "" if the file is absent/unreadable.
func codexCacheVersion() string {
	data, err := os.ReadFile(codexModelsCachePath())
	if err != nil {
		return ""
	}
	var v struct {
		ClientVersion string `json:"client_version"`
	}
	if json.Unmarshal(data, &v) != nil {
		return ""
	}
	return v.ClientVersion
}

// codexHome returns the codex CLI config dir: $CODEX_HOME if set, else ~/.codex.
func codexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

func codexModelsCachePath() string {
	return filepath.Join(codexHome(), "models_cache.json")
}
