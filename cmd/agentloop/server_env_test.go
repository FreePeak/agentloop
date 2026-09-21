package main

import (
	"testing"

	"github.com/FreePeak/agentloop/internal/experiments"
)

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

// The guardrail client is wired by environment, and its one dangerous
// failure mode is a deploy that believes it is screening while nothing is
// wired. Unset must stay nil — the runner records screen_errors for that,
// which is the difference between an unscreened run and a clean one.
func TestNewServer_WiresGuardrailFromEnv(t *testing.T) {
	if s := NewServer(); s.guardrail != nil {
		t.Error("guardrail client is non-nil with no AGENTLOOP_GUARDRAIL_URL")
	}
	// No client means no screen function: the runner's "not configured"
	// path, not a screen that always passes.
	if fn := NewServer().screenFunc(); fn != nil {
		t.Error("screenFunc is non-nil with no guardrail client — every run would claim to be screened")
	}

	t.Setenv("AGENTLOOP_GUARDRAIL_URL", "http://127.0.0.1:8080")
	s := NewServer()
	if s.guardrail == nil {
		t.Fatal("guardrail client is nil with AGENTLOOP_GUARDRAIL_URL set")
	}
	if s.screenFunc() == nil {
		t.Error("screenFunc is nil with a guardrail client configured")
	}
}

// A policy typo must not loosen a screen: anything but "permissive" is the
// measured strict default (PRD §17).
func TestPolicyFromEnv(t *testing.T) {
	t.Setenv("AGENTLOOP_GUARDRAIL_POLICY", "permissive")
	if got := policyFromEnv(); got != experiments.Permissive {
		t.Errorf("policy = %+v, want Permissive", got)
	}
	t.Setenv("AGENTLOOP_GUARDRAIL_POLICY", "PERMISSIVE")
	if got := policyFromEnv(); got != experiments.Permissive {
		t.Errorf("policy = %+v, want Permissive (case-insensitive)", got)
	}
	t.Setenv("AGENTLOOP_GUARDRAIL_POLICY", "permissiv")
	if got := policyFromEnv(); got != experiments.Strict {
		t.Errorf("policy = %+v, want Strict on an unrecognised value", got)
	}
	t.Setenv("AGENTLOOP_GUARDRAIL_POLICY", "")
	if got := policyFromEnv(); got != experiments.Strict {
		t.Errorf("policy = %+v, want Strict by default", got)
	}
}
