// Package logx provides process-wide leveled logging on top of the standard
// log package. It is a leaf: it must never import other model-proxy packages.
//
// The level is set once at serve startup from config log_level (startup-only;
// reload does not re-read it) and defaults to info for every other process
// (CLI commands), so migrated call sites behave exactly like the previous
// bare log.Printf there.
package logx

import (
	"log"
	"sync/atomic"
)

// Level is a log severity; lower values are more verbose.
type Level int32

const (
	Debug Level = iota
	Info
	Warn
	Error
)

// current holds the active Level. Info is not the zero value, so it is
// stored explicitly at init to keep the out-of-box default info.
var current atomic.Int32

// unknownWarned gates the one-per-process warning for an unparseable level
// passed to SetLevel.
var unknownWarned atomic.Bool

func init() {
	current.Store(int32(Info))
}

// String returns the config/log name of the level.
func (l Level) String() string {
	switch l {
	case Debug:
		return "debug"
	case Info:
		return "info"
	case Warn:
		return "warn"
	case Error:
		return "error"
	}
	return "unknown"
}

// SetLevel parses s ("debug"|"info"|"warn"|"error"; "" means info) and makes
// it the process-wide level. An unknown value falls back to info and logs a
// single warning (once per process, no matter how many bad values follow).
func SetLevel(s string) {
	switch s {
	case "", "info":
		current.Store(int32(Info))
	case "debug":
		current.Store(int32(Debug))
	case "warn":
		current.Store(int32(Warn))
	case "error":
		current.Store(int32(Error))
	default:
		current.Store(int32(Info))
		if unknownWarned.CompareAndSwap(false, true) {
			log.Printf("model-proxy: unknown log_level %q — falling back to info", s)
		}
	}
}

// CurrentLevel returns the active level (test save/restore, status surfaces).
func CurrentLevel() Level {
	return Level(current.Load())
}

// Debugf logs at debug level; suppressed unless the level is debug.
func Debugf(format string, args ...any) {
	if CurrentLevel() <= Debug {
		log.Printf(format, args...)
	}
}

// Infof logs at info level; suppressed at warn/error.
func Infof(format string, args ...any) {
	if CurrentLevel() <= Info {
		log.Printf(format, args...)
	}
}

// Warnf logs at warn level; suppressed only at error.
func Warnf(format string, args ...any) {
	if CurrentLevel() <= Warn {
		log.Printf(format, args...)
	}
}

// Errorf logs at error level; never suppressed by the level.
func Errorf(format string, args ...any) {
	if CurrentLevel() <= Error {
		log.Printf(format, args...)
	}
}
