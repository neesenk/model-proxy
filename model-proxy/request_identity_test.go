package main

import (
	"strings"
	"testing"
)

func TestNewRequestID(t *testing.T) {
	first, second := newRequestID(), newRequestID()
	if len(first) != 32 || len(second) != 32 {
		t.Fatalf("request ID lengths = %d/%d, want 32 hex characters each", len(first), len(second))
	}
	for _, id := range []string{first, second} {
		for _, char := range id {
			if !strings.ContainsRune("0123456789abcdef", char) {
				t.Fatalf("request ID %q contains non-hex character %q", id, char)
			}
		}
	}
	if first == second {
		t.Fatalf("request IDs collided: %s", first)
	}
}
