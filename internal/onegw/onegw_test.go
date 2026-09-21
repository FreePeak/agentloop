package onegw

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChat_SendsComboAsModelAndParsesUsage(t *testing.T) {
	var gotModel, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		var body struct {
			Model    string    `json:"model"`
			Messages []Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		gotModel = body.Model
		gotAuth = r.Header.Get("Authorization")
		if len(body.Messages) != 1 || body.Messages[0].Role != "user" {
			t.Errorf("messages = %+v, want one user message", body.Messages)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"deepseek-v4.1-flash","choices":[{"message":{"content":"ok"}}],` +
			`"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", "dev")
	got, err := c.Chat(context.Background(), Message{Role: "user", Content: "hi"})
	if err != nil {
		t.Fatalf("Chat() error: %v", err)
	}
	if gotModel != "dev" {
		t.Errorf("wire model = %q, want the combo name %q", gotModel, "dev")
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want Bearer sk-test", gotAuth)
	}
	if got.Content != "ok" {
		t.Errorf("Content = %q, want ok", got.Content)
	}
	// The leg that answered is reported separately from the combo asked for.
	if got.Model != "deepseek-v4.1-flash" {
		t.Errorf("Model = %q, want the answering leg", got.Model)
	}
	if got.Usage.Prompt != 11 || got.Usage.Completion != 2 || got.Usage.Total != 13 {
		t.Errorf("Usage = %+v, want 11/2/13", got.Usage)
	}
}

// A keyless gateway (local onegw with no [auth] keys) must receive no
// Authorization header at all, not an empty "Bearer ".
func TestChat_NoKeySendsNoAuthHeader(t *testing.T) {
	var sawAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawAuth = r.Header["Authorization"]
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "", "dev").Chat(context.Background(), Message{Role: "user", Content: "hi"}); err != nil {
		t.Fatalf("Chat() error: %v", err)
	}
	if sawAuth {
		t.Error("Authorization header sent for a keyless client")
	}
}

func TestChat_SurfacesGatewayErrorMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid api key"}}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "bad", "dev").Chat(context.Background(), Message{Role: "user", Content: "hi"})
	if err == nil {
		t.Fatal("Chat() error = nil, want failure on 401")
	}
	if !strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("error = %v, want it to carry the gateway message", err)
	}
}

func TestChat_EmptyComboFailsClosed(t *testing.T) {
	if _, err := New("http://127.0.0.1:1", "k", "").Chat(context.Background(), Message{Role: "user"}); err == nil {
		t.Fatal("Chat() error = nil, want refusal when no combo is configured")
	}
}

func TestChat_NoChoicesIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "k", "dev").Chat(context.Background(), Message{Role: "user"}); err == nil {
		t.Fatal("Chat() error = nil, want failure when the response carries no choices")
	}
}
