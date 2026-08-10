package models

import (
	"testing"
)

func TestNonFlagArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"simple", []string{"models", "zhipu"}, []string{"models", "zhipu"}},
		{"--config value skipped", []string{"--config", "x.yaml", "models"}, []string{"models"}},
		{"--config= skipped", []string{"--config=x.yaml", "models"}, []string{"models"}},
		{"flags skipped", []string{"--verbose", "models", "--refresh"}, []string{"models"}},
		{"empty", []string{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := NonFlagArgs(tc.args)
			if len(got) != len(tc.want) {
				t.Fatalf("NonFlagArgs(%v)=%v want %v", tc.args, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("NonFlagArgs(%v)[%d]=%q want %q", tc.args, i, got[i], tc.want[i])
				}
			}
		})
	}
}
