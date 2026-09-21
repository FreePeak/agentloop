package onegw

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The point of tiering: a call naming a tier goes to that tier's combo on
// the wire. Before this the combo was fixed at construction, so the loop's
// per-step tier reached nothing.
func TestChatTierRoutesToTheTierCombo(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		asked = append(asked, body.Model)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"leg-that-answered","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "dev").WithTiers(map[string]string{
		"planning":  "plan-combo",
		"execution": "exec-combo",
	})

	if _, err := c.ChatTier(context.Background(), "execution", Message{Role: "user", Content: "a"}); err != nil {
		t.Fatalf("ChatTier() error: %v", err)
	}
	if _, err := c.ChatTier(context.Background(), "planning", Message{Role: "user", Content: "b"}); err != nil {
		t.Fatalf("ChatTier() error: %v", err)
	}
	// No tier named -> the default combo.
	if _, err := c.Chat(context.Background(), Message{Role: "user", Content: "c"}); err != nil {
		t.Fatalf("Chat() error: %v", err)
	}
	want := []string{"exec-combo", "plan-combo", "dev"}
	if len(asked) != len(want) {
		t.Fatalf("calls = %v, want %v", asked, want)
	}
	for i := range want {
		if asked[i] != want[i] {
			t.Errorf("call %d went to combo %q, want %q", i, asked[i], want[i])
		}
	}
}

// An unmapped tier must degrade to the default combo, not fail: a
// misconfigured tier should cost the wrong model, not the run.
func TestUnknownTierFallsBackToTheDefaultCombo(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		asked = body.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"x","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "dev").WithTiers(map[string]string{"planning": "plan-combo"})
	if _, err := c.ChatTier(context.Background(), "no-such-tier", Message{Role: "user", Content: "a"}); err != nil {
		t.Fatalf("ChatTier() error: %v (an unmapped tier must not fail the call)", err)
	}
	if asked != "dev" {
		t.Errorf("combo = %q, want the default %q", asked, "dev")
	}
}

// A client with no tiers set behaves exactly as before — one combo.
func TestNoTiersMeansTheDefaultComboOnly(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		asked = append(asked, body.Model)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"x","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "harvey")
	for _, tier := range []string{"planning", "execution", "synthesis", ""} {
		if _, err := c.ChatTier(context.Background(), tier, Message{Role: "user", Content: "a"}); err != nil {
			t.Fatalf("ChatTier(%q) error: %v", tier, err)
		}
	}
	for i, got := range asked {
		if got != "harvey" {
			t.Errorf("call %d combo = %q, want harvey (no tiers configured)", i, got)
		}
	}
}

// The reply records the combo ASKED FOR, separately from the leg that
// answered — observing a fallback is the only way to judge whether tiering
// is doing anything.
func TestReplyRecordsTheComboAskedFor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"deepseek-v4.1-flash","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "dev").WithTiers(map[string]string{"execution": "exec-combo"})
	got, err := c.ChatTier(context.Background(), "execution", Message{Role: "user", Content: "a"})
	if err != nil {
		t.Fatalf("ChatTier() error: %v", err)
	}
	if got.Combo != "exec-combo" {
		t.Errorf("Combo = %q, want the combo asked for", got.Combo)
	}
	if got.Model != "deepseek-v4.1-flash" {
		t.Errorf("Model = %q, want the leg that answered", got.Model)
	}
}
