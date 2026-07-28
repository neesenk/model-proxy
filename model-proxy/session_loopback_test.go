package main

import "testing"

// --- LoopbackServer.Port ---

func TestLoopbackServer_Port(t *testing.T) {
	ls, err := NewLoopbackServer()
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Stop()
	if err := ls.Start(); err != nil {
		t.Fatal(err)
	}
	if ls.Port() == 0 {
		t.Error("Port()=0 want non-zero after Start")
	}
	if ls.CallbackURL() == "" {
		t.Error("CallbackURL() empty after Start")
	}
}
