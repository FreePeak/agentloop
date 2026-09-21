package main

import (
	"testing"

	"github.com/FreePeak/agentloop/internal/experiments"
	"github.com/FreePeak/agentloop/internal/systemone"
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

// The screen is the second outbound dependency configured by environment,
// and it has one failure mode that matters: a deploy that believes it is
// screening while nothing is wired. Off is the default and must stay off —
// switching it on silently would fail-close every run against an endpoint
// nobody configured.
func TestNewServer_WiresSystemoneFromEnv(t *testing.T) {
	if s := NewServer(); s.screen != nil {
		t.Error("screen client is non-nil with no AGENTLOOP_SYSTEMONE_URL — runs would try to screen against nothing")
	}

	t.Setenv("AGENTLOOP_SYSTEMONE_URL", "http://127.0.0.1:8080")
	t.Setenv("AGENTLOOP_SYSTEMONE_MODEL", "jev-1.13.0")
	s := NewServer()
	if s.screen == nil {
		t.Fatal("screen client is nil with AGENTLOOP_SYSTEMONE_URL set")
	}
	c, ok := s.screen.(*systemone.Client)
	if !ok {
		t.Fatalf("screen client is %T, want *systemone.Client", s.screen)
	}
	if c.Model() != "jev-1.13.0" {
		t.Errorf("model = %q, want the env override", c.Model())
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
