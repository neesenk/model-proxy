// model_disable_store.go — composition-root wiring for the operator
// disabled-model override's persistence: the file format and atomic save
// live with the runtime state (internal/runtime disabled_file.go, the
// model_caps.json discipline); this file owns the Proxy lifecycle — seeding
// the Manager at construction (restart survival) and rewriting the file
// after every toggle (refresh/restart survival). See disabled_file.go for
// the entry semantics (self-validating pairs, no fingerprint gate).
package app

import (
	"model-proxy/internal/observe/logx"
	runtimestate "model-proxy/internal/runtime"
)

// seedDisabledModels restores the persisted override into the runtime
// Manager. Called once at construction; a missing file is the first-run
// no-op, an unreadable/malformed file degrades to "no overrides" with a
// warning (a bad state file must not take the proxy down; the next
// successful toggle rewrites it whole).
func (p *Proxy) seedDisabledModels() {
	entries, err := runtimestate.LoadDisabledModelsFile(p.disabledModelsPath)
	if err != nil {
		logx.Warnf("[startup] disabled models store unreadable: %v; starting with none", err)
		return
	}
	if len(entries) == 0 {
		return
	}
	p.runtimeState.RestoreDisabledModels(entries)
	restored := 0
	for _, models := range entries {
		restored += len(models)
	}
	logx.Infof("[startup] restored %d disabled-model override(s) from %s", restored, p.disabledModelsPath)
}

// persistDisabledModels writes the CURRENT disable override projection to
// disabled_models.json. Called after every toggle via the admin port. The
// read happens inside disabledModelsMu together with the write, so the
// last-completed persist always reflects every prior mutation (each toggle's
// own persist re-reads under the same mutex). A failure surfaces to the API
// caller while the in-memory toggle stays live — routing listens to memory;
// the file only feeds the NEXT process.
func (p *Proxy) persistDisabledModels() error {
	p.disabledModelsMu.Lock()
	defer p.disabledModelsMu.Unlock()
	return runtimestate.SaveDisabledModelsFile(p.disabledModelsPath, p.runtimeState.DisabledModels())
}
