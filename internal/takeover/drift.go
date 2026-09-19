package takeover

import (
	"encoding/json"
	"os"
	"path/filepath"

	configdomain "model-proxy/internal/config"
)

// ClientDrift is the takeover state of one agent client: taken (a backup
// marker exists) or not; when taken, OK reports whether the client's config
// still points at this proxy. Current/Expected feed the drift detail line.
type ClientDrift struct {
	Client   string
	File     string
	Taken    bool
	OK       bool
	Current  string
	Expected string
}

// CheckDrift compares every taken-over client's proxy pointer against the
// value takeover would write today. Drift happens when a client upgrade
// rewrites its config or the proxy's listen address changes — the agent then
// silently talks to a dead endpoint, which looks exactly like "agent stuck".
// Local files only, read-only. The pointer is template-driven (drift probe
// declared by each takeover template; templatesDir "" = DefaultTemplatesDir).
// This is the single owner of the taken-over/drift computation: the CLI
// (`doctor`, post-takeover verification) and the Web admin takeover surface
// both consume it.
func CheckDrift(cfg *configdomain.Config, bakDir, templatesDir string) ([]ClientDrift, error) {
	out := []ClientDrift{}
	clients, err := ListClients(cfg, "", templatesDir)
	if err != nil {
		return nil, err
	}
	for _, c := range clients {
		d := ClientDrift{Client: c.Name, File: c.File}
		if _, err := os.Stat(filepath.Join(bakDir, c.Name+".bak")); err != nil {
			out = append(out, d) // no backup marker → not taken over
			continue
		}
		d.Taken = true
		d.Current, d.Expected = c.Template.Pointer(cfg)
		// A template without a drift probe is exempt from drift detection —
		// there is nothing to compare, and reporting it as drifted would be a
		// false alarm right after every takeover (mcp-only templates). The
		// same applies to an mcp-scope backup: its takeover never wrote the
		// model pointer the probe compares.
		scope := backupScope(filepath.Join(bakDir, c.Name+".bak.meta"))
		d.OK = scope == ScopeMCP || d.Current == d.Expected || d.Current == "(no drift probe)"
		out = append(out, d)
	}
	return out, nil
}

// backupScope reads the takeover scope a backup recorded (missing meta or
// field = ScopeAll — pre-scope backups were full takeovers).
func backupScope(metaPath string) RewriteScope {
	mb, err := os.ReadFile(metaPath)
	if err != nil {
		return ScopeAll
	}
	var meta struct {
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal(mb, &meta); err != nil {
		return ScopeAll
	}
	return RewriteScope(meta.Scope)
}
