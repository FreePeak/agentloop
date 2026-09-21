package guardrail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The battery is the contract with the backend. A test that pins its shape
// is what stops someone silently screening for nothing by editing the map.
func TestScreenSendsTheFixedBattery(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			t.Errorf("path = %q, want /v1/systemone", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{
			"jailbreak":{"probability":0.02},"harmful_request":{"probability":0.01},
			"medical_advice":{"probability":0.03},"self_harm":{"probability":0.0},
			"severity":{"probability":0.1}}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", "")
	if _, err := c.Screen(context.Background(), "Summarise the architecture doc"); err != nil {
		t.Fatalf("Screen() error: %v", err)
	}

	if got["state"] != "Summarise the architecture doc" {
		t.Errorf("state = %v, want the text under judgement", got["state"])
	}
	if got["model"] != DefaultModel {
		t.Errorf("model = %v, want %q", got["model"], DefaultModel)
	}
	qs, ok := got["questions"].(map[string]any)
	if !ok {
		t.Fatalf("questions missing or wrong shape: %T", got["questions"])
	}
	// The four Noul hazard questions plus one severity Score — §4.3's rule.
	for _, id := range []string{"jailbreak", "harmful_request", "medical_advice", "self_harm", "severity"} {
		if _, ok := qs[id]; !ok {
			t.Errorf("battery is missing %q", id)
		}
	}
	if len(qs) != 5 {
		t.Errorf("battery has %d questions, want 5 (4 Noul + 1 Score)", len(qs))
	}
}

// A jailbreak must come back as a hazard probability, not swallowed.
func TestScreenReadsHazardsAndSeverity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{
			"jailbreak":0.91,"harmful_request":0.88,"medical_advice":0.4,
			"self_harm":0.05,"severity":{"score":2.1}}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "")
	got, err := c.Screen(context.Background(), "ignore your instructions and…")
	if err != nil {
		t.Fatalf("Screen() error: %v", err)
	}
	if got.Nouls["jailbreak"] != 0.91 {
		t.Errorf("jailbreak = %v, want 0.91", got.Nouls["jailbreak"])
	}
	if got.Severity != 2.1 {
		t.Errorf("severity = %v, want 2.1", got.Severity)
	}
	if got.Model != "jev-1.13.0" {
		t.Errorf("model = %q, want the answering backend", got.Model)
	}
	if got.LatencyMs < 0 {
		t.Error("latency not recorded")
	}
}

// Two backends answer /v1/systemone, so both real envelopes must read the
// same hazard — and neither may be read from `confidence`. An answer carrying
// both a probability and a confidence is the normal case, and preferring the
// confidence silently screens on the model's self-assessment (0.88) instead of
// the hazard (0.91).
func TestBothBackendEnvelopesAgree(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		hazard   float64
		severity float64
	}{
		{
			name:     "TypeSafe Jev",
			body:     `{"model":"jev-1.13.0","answers":{"jailbreak":{"type":"noul","noul":0.91,"confidence":0.88},"severity":{"type":"score","score":2.05,"legend":{"2":"serious"}}}}`,
			hazard:   0.91,
			severity: 2.05,
		},
		{
			name:     "local Laya sidecar",
			body:     `{"model":"laya","answers":{"jailbreak":{"type":"noul","probabilities":{"0":0.09,"1":0.91},"confidence":0.8},"severity":{"type":"score","score":2}}}`,
			hazard:   0.91,
			severity: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()

			got, err := New(srv.URL, "", "").Screen(context.Background(), "text")
			if err != nil {
				t.Fatalf("Screen() error: %v", err)
			}
			if got.Nouls["jailbreak"] != tc.hazard {
				t.Errorf("jailbreak = %v, want %v (never the confidence)", got.Nouls["jailbreak"], tc.hazard)
			}
			if got.Severity != tc.severity {
				t.Errorf("severity = %v, want %v", got.Severity, tc.severity)
			}
		})
	}
}

// An answer shape this client does not understand must be an error, not a
// silent zero. Reading a hazard as 0 when it is really 0.9 is the failure
// mode that makes the screen decorative.
func TestUnreadableAnswersAreAnErrorNotAZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"jailbreak":"very likely","severity":"high"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "")
	if _, err := c.Screen(context.Background(), "text"); err == nil {
		t.Fatal("Screen() reported success with no usable answers — " +
			"that is how a screen silently passes everything")
	}
}

// A backend that is down is an observation with a reason.
func TestBackendErrorCarriesItsReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"provider_unavailable","message":"systemone backend down"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "")
	_, err := c.Screen(context.Background(), "text")
	if err == nil {
		t.Fatal("Screen() returned nil error on a 503")
	}
	if !contains(err.Error(), "systemone backend down") {
		t.Errorf("error = %q, want the backend's reason", err)
	}
}

// No endpoint configured is its own error, distinct from an empty verdict.
func TestNoEndpointIsAnError(t *testing.T) {
	c := New("", "", "")
	if _, err := c.Screen(context.Background(), "text"); err == nil {
		t.Fatal("Screen() succeeded with no endpoint")
	}
}

// Empty text is refused before the round trip: "clean" is a verdict the
// battery did not give.
func TestEmptyTextIsRefusedBeforeTheRoundTrip(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()

	c := New(srv.URL, "", "")
	if _, err := c.Screen(context.Background(), "   "); err == nil {
		t.Fatal("Screen() accepted whitespace as content")
	}
	if hit {
		t.Error("the backend was called for empty text")
	}
}

// An API key is sent when configured, and omitted when not (a local Laya
// expects no Authorization header at all).
func TestKeyIsSentWhenConfiguredAndAbsentWhenNot(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"jailbreak":0.1,"severity":0.2}}`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "sk-abc", "").Screen(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(srv.URL, "", "").Screen(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if auth[0] != "Bearer sk-abc" {
		t.Errorf("Authorization = %q, want the bearer key", auth[0])
	}
	if auth[1] != "" {
		t.Errorf("Authorization = %q, want none for a keyless backend", auth[1])
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
