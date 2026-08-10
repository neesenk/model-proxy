package main

import (
	"model-proxy/internal/app"
)

// Proxy is the application composition root; it lives in internal/app. The
// alias keeps root CLI/test code on the stable name.
type Proxy = app.Proxy

// newProxyWithStatePath is the injectable constructor used by tests so every
// Proxy owns an isolated state file before the tracker loads or starts.
func newProxyWithStatePath(cfg *Config, qpath string) *Proxy {
	return app.NewProxyWithStatePath(cfg, qpath)
}

// webServer aliases the app-owned Web adapter.
type webServer = app.WebServer

// reloadAppliedWarning aliases the app-owned reload warning type.
type reloadAppliedWarning = app.ReloadAppliedWarning
