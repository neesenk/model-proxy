package protocol

import (
	"fmt"
	"sync"
)

// Diagnostic is one structured conversion observation: a stable machine
// readable Code plus the human Detail already carried by the legacy
// convertWarn log line. Codes are the contract consumed by the request log
// and the strict-lossy mode; Detail is informational.
type Diagnostic struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// Diagnostics collects the diagnostics of ONE conversion invocation. The zero
// value is ready; every method is nil-safe so call sites can thread a nil
// collector where diagnostics are not wanted (response/stream paths).
type Diagnostics struct {
	mu    sync.Mutex
	items []Diagnostic
}

// NewDiagnostics returns a ready collector.
func NewDiagnostics() *Diagnostics { return &Diagnostics{} }

// Add records one diagnostic (nil collector is a no-op).
func (d *Diagnostics) Add(code, detail string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.items = append(d.items, Diagnostic{Code: code, Detail: detail})
	d.mu.Unlock()
}

// Addf records one formatted diagnostic.
func (d *Diagnostics) Addf(code, format string, args ...any) {
	if d == nil {
		return
	}
	d.Add(code, fmt.Sprintf(format, args...))
}

// Items returns a detached copy, oldest first.
func (d *Diagnostics) Items() []Diagnostic {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Diagnostic(nil), d.items...)
}

// HasCode reports whether a diagnostic with the given code was collected.
func (d *Diagnostics) HasCode(code string) bool {
	for _, item := range d.Items() {
		if item.Code == code {
			return true
		}
	}
	return false
}

// warnDiag is the dual-write helper for migrated convertWarn sites: the
// collector (when present) receives the structured diagnostic, and the legacy
// log line keeps firing — the log remains the low-tech observability floor.
func warnDiag(d *Diagnostics, code, detail string) {
	d.Add(code, detail)
	convertWarn(detail)
}

// warnDiagf is warnDiag with formatting.
func warnDiagf(d *Diagnostics, code, format string, args ...any) {
	warnDiag(d, code, fmt.Sprintf(format, args...))
}
