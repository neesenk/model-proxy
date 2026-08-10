package main

import "model-proxy/internal/httpx"

// newRequestID delegates to internal/httpx; kept as a one-line seam while the
// proxy front door migrates into internal/app.
func newRequestID() string { return httpx.NewRequestID() }
