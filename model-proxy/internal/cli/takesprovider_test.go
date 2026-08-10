package cli

import "testing"

func TestTakesProvider(t *testing.T) {
	for _, cmd := range []string{"login", "logout", "usage"} {
		if !TakesProvider(cmd) {
			t.Errorf("TakesProvider(%q)=false want true", cmd)
		}
	}
	for _, cmd := range []string{"models", "serve", "schedule", "doctor", "config", "takeover", "help", ""} {
		if TakesProvider(cmd) {
			t.Errorf("TakesProvider(%q)=true want false", cmd)
		}
	}
}
