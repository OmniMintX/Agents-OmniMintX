package planner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// All Anthropic tests run against httptest servers — never the real API.

func newAnthropicTest(t *testing.T, handler http.HandlerFunc) *Anthropic {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	a := NewAnthropic("test-key", "claude-sonnet-4-5")
	a.BaseURL = srv.URL
	return a
}

func TestAnthropicComplete(t *testing.T) {
	a := newAnthropicTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "test-key" || r.Header.Get("anthropic-version") != anthropicVersion {
			t.Fatalf("missing auth headers: %v", r.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["model"] != "claude-sonnet-4-5" {
			t.Fatalf("unexpected model: %v", body["model"])
		}
		msgs := body["messages"].([]any)
		first := msgs[0].(map[string]any)
		if first["role"] != "user" || first["content"] != "plan it" {
			t.Fatalf("unexpected messages: %v", msgs)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "{\"tasks\":"},
				{"type": "text", "text": "[]}"},
			},
		})
	})
	out, err := a.Complete(context.Background(), "plan it")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "{\"tasks\":[]}" {
		t.Fatalf("text blocks must be concatenated, got %q", out)
	}
}

func TestAnthropicAPIError(t *testing.T) {
	a := newAnthropicTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "authentication_error", "message": "invalid x-api-key"},
		})
	})
	_, err := a.Complete(context.Background(), "p")
	if err == nil || !strings.Contains(err.Error(), "authentication_error") || !strings.Contains(err.Error(), "invalid x-api-key") {
		t.Fatalf("want API error envelope surfaced, got %v", err)
	}
}

func TestAnthropicEmptyContent(t *testing.T) {
	a := newAnthropicTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"content": []any{}})
	})
	if _, err := a.Complete(context.Background(), "p"); err == nil || !strings.Contains(err.Error(), "no text content") {
		t.Fatalf("want empty-content error, got %v", err)
	}
}

func TestAnthropicRequiresKey(t *testing.T) {
	a := NewAnthropic("", "m")
	if _, err := a.Complete(context.Background(), "p"); err == nil {
		t.Fatal("want error for empty API key")
	}
}
