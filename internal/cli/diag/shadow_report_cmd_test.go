package diag_test

import (
	"testing"

	diag "model-proxy/internal/cli/diag"
)

func TestMakeURLQuery(t *testing.T) {
	q := diag.MakeURLQuery([]string{"--from", "100", "--to", "200"})
	if q.Get("from") != "100" || q.Get("to") != "200" {
		t.Errorf("makeURLQuery: from=%q to=%q want 100/200", q.Get("from"), q.Get("to"))
	}
	q2 := diag.MakeURLQuery(nil)
	if q2.Encode() != "" {
		t.Errorf("diag.MakeURLQuery(nil) should be empty, got %q", q2.Encode())
	}
}
