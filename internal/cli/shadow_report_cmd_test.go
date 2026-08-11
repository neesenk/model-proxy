package cli_test

import (
	"testing"

	"model-proxy/internal/cli"
)

func TestMakeURLQuery(t *testing.T) {
	q := cli.MakeURLQuery([]string{"--from", "100", "--to", "200"})
	if q.Get("from") != "100" || q.Get("to") != "200" {
		t.Errorf("makeURLQuery: from=%q to=%q want 100/200", q.Get("from"), q.Get("to"))
	}
	q2 := cli.MakeURLQuery(nil)
	if q2.Encode() != "" {
		t.Errorf("cli.MakeURLQuery(nil) should be empty, got %q", q2.Encode())
	}
}
