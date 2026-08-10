package main

import (
	"model-proxy/internal/app"
)

// Proxy is the application composition root; it lives in internal/app. The
// alias keeps root CLI/test code on the stable name.
type Proxy = app.Proxy

// reloadAppliedWarning aliases the app-owned reload warning type.
type reloadAppliedWarning = app.ReloadAppliedWarning
