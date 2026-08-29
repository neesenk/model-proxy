package logx

import (
	"bytes"
	"log"
	"strings"
	"sync"
	"testing"
)

// capture redirects the standard logger to a buffer for the duration of fn
// and restores both the output and the logx level afterwards. Tests using it
// must not run in parallel (process-global logger).
func capture(t *testing.T, fn func()) string {
	t.Helper()
	savedLevel := CurrentLevel()
	var buf bytes.Buffer
	savedWriter := log.Writer()
	savedFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(savedWriter)
		log.SetFlags(savedFlags)
		SetLevel(savedLevel.String())
	})
	fn()
	return buf.String()
}

func TestDefaultLevelIsInfo(t *testing.T) {
	// No SetLevel call before this test may leak: capture restores the level,
	// and package init stores info.
	out := capture(t, func() {
		Debugf("debug-msg")
		Infof("info-msg")
	})
	if strings.Contains(out, "debug-msg") {
		t.Errorf("default level must suppress debug, got %q", out)
	}
	if !strings.Contains(out, "info-msg") {
		t.Errorf("default level must pass info, got %q", out)
	}
}

func TestLevelFilterMatrix(t *testing.T) {
	cases := []struct {
		level string
		want  []string // substrings that must appear
		drop  []string // substrings that must NOT appear
	}{
		{"debug", []string{"d", "i", "w", "e"}, nil},
		{"info", []string{"i", "w", "e"}, []string{"d"}},
		{"warn", []string{"w", "e"}, []string{"d", "i"}},
		{"error", []string{"e"}, []string{"d", "i", "w"}},
		{"", []string{"i", "w", "e"}, []string{"d"}}, // empty = info
	}
	for _, tc := range cases {
		out := capture(t, func() {
			SetLevel(tc.level)
			Debugf("d")
			Infof("i")
			Warnf("w")
			Errorf("e")
		})
		for _, want := range tc.want {
			if !strings.Contains(out, want) {
				t.Errorf("level %q: output %q missing %q", tc.level, out, want)
			}
		}
		for _, drop := range tc.drop {
			if strings.Contains(out, drop) {
				t.Errorf("level %q: output %q must not contain %q", tc.level, out, drop)
			}
		}
	}
}

func TestSetLevelUnknownFallsBackToInfoAndWarnsOnce(t *testing.T) {
	out := capture(t, func() {
		unknownWarned.Store(false) // reset the once-gate for this test
		SetLevel("verbose")
		SetLevel("chatty")
		Debugf("dbg-token")
		Infof("inf-token")
	})
	if CurrentLevel() != Info {
		t.Errorf("CurrentLevel() = %v after unknown value, want info", CurrentLevel())
	}
	if n := strings.Count(out, "unknown log_level"); n != 1 {
		t.Errorf("unknown-level warning count = %d, want exactly 1 (output %q)", n, out)
	}
	if !strings.Contains(out, `"verbose"`) {
		t.Errorf("warning must name the offending value, got %q", out)
	}
	if strings.Contains(out, "dbg-token") || !strings.Contains(out, "inf-token") {
		t.Errorf("fallback must behave as info, got %q", out)
	}
}

func TestCurrentLevelRoundTrip(t *testing.T) {
	saved := CurrentLevel()
	t.Cleanup(func() { SetLevel(saved.String()) })
	for _, level := range []Level{Debug, Info, Warn, Error} {
		SetLevel(level.String())
		if CurrentLevel() != level {
			t.Errorf("CurrentLevel() = %v, want %v", CurrentLevel(), level)
		}
	}
}

func TestConcurrentSetLevelAndLog(t *testing.T) {
	// Race-detector target: writers log while another goroutine flips levels.
	// No assertion on content — the contract is data-race freedom and no panic.
	saved := CurrentLevel()
	t.Cleanup(func() { SetLevel(saved.String()) })
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 200 {
				SetLevel("debug")
				SetLevel("error")
			}
		}()
		go func() {
			defer wg.Done()
			for range 200 {
				Debugf("d")
				Infof("i")
				Warnf("w")
				Errorf("e")
				_ = CurrentLevel()
			}
		}()
	}
	wg.Wait()
}
