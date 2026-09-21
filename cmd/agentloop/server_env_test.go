package main

import "testing"

// The gateway is configured by environment, so this is the one place the
// wiring can break silently: NewServer must always carry a model client,
// with the documented defaults when nothing is set.
func TestNewServer_WiresModelFromEnv(t *testing.T) {
	s := NewServer()
	if s.model == nil {
		t.Fatal("model client is nil — runs would never reach onegw")
	}
	if s.model.Combo() != "dev" {
		t.Errorf("default combo = %q, want %q", s.model.Combo(), "dev")
	}

	t.Setenv("AGENTLOOP_ONEGW_COMBO", "harvey")
	s = NewServer()
	if s.model.Combo() != "harvey" {
		t.Errorf("combo = %q, want the env override %q", s.model.Combo(), "harvey")
	}
}
