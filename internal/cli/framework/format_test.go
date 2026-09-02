package framework

import (
	"testing"
)

func TestPositional(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"simple", []string{"login", "codex"}, "login"},
		{"after --config value", []string{"--config", "x.yaml", "login"}, "login"},
		{"--config= form", []string{"--config=x.yaml", "login"}, "login"},
		{"-config single dash", []string{"-config", "x.yaml", "usage"}, "usage"},
		{"flag only", []string{"--foo", "--bar"}, ""},
		{"empty", []string{}, ""},
		{"positional after flag", []string{"--verbose", "models", "zhipu"}, "models"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Positional(tc.args); got != tc.want {
				t.Errorf("Positional(%v)=%q want %q", tc.args, got, tc.want)
			}
		})
	}
}
