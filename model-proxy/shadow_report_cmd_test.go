package main

import "testing"

func TestMakeURLQuery(t *testing.T) {
	q := makeURLQuery([]string{"--from", "100", "--to", "200"})
	if q.Get("from") != "100" || q.Get("to") != "200" {
		t.Errorf("makeURLQuery: from=%q to=%q want 100/200", q.Get("from"), q.Get("to"))
	}
	q2 := makeURLQuery(nil)
	if q2.Encode() != "" {
		t.Errorf("makeURLQuery(nil) should be empty, got %q", q2.Encode())
	}
}
