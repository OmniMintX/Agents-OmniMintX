package planner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// All OpenAI tests run against httptest servers — never a real API.

func newOpenAITest(t *testing.T, model string, handler http.HandlerFunc) *OpenAI {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewOpenAI("test-key", model, srv.URL)
}

func openaiResponse(text string) map[string]any {
	return map[string]any{
		"choices": []map[string]any{
			{"message": map[string]any{"role": "assistant", "content": text}},
		},
	}
}

func TestOpenAIComplete(t *testing.T) {
	o := newOpenAITest(t, "gpt-4o", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("missing bearer auth: %v", r.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["model"] != "gpt-4o" {
			t.Fatalf("unexpected model: %v", body["model"])
		}
		msgs := body["messages"].([]any)
		first := msgs[0].(map[string]any)
		if first["role"] != "user" || first["content"] != "plan it" {
			t.Fatalf("unexpected messages: %v", msgs)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openaiResponse("{\"tasks\":[]}"))
	})
	out, err := o.Complete(context.Background(), "plan it")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "{\"tasks\":[]}" {
		t.Fatalf("got %q", out)
	}
}

// A DeepSeek/Ollama-style base URL (no /v1 suffix, custom host) must be used
// verbatim, with only /chat/completions appended.
func TestOpenAICustomBaseURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(func() http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(openaiResponse("ok"))
		}
	}())
	defer srv.Close()
	o := NewOpenAI("deepseek-key", "deepseek-chat", srv.URL+"/deepseek/")
	out, err := o.Complete(context.Background(), "p")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "ok" {
		t.Fatalf("got %q", out)
	}
	if gotPath != "/deepseek/chat/completions" {
		t.Fatalf("base_url not respected, path = %q", gotPath)
	}
}

func TestOpenAIAPIError(t *testing.T) {
	o := newOpenAITest(t, "m", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"type": "invalid_request_error", "message": "Incorrect API key provided"},
		})
	})
	_, err := o.Complete(context.Background(), "p")
	if err == nil || !strings.Contains(err.Error(), "invalid_request_error") || !strings.Contains(err.Error(), "Incorrect API key provided") {
		t.Fatalf("want API error envelope surfaced, got %v", err)
	}
}

func TestOpenAIEmptyChoices(t *testing.T) {
	o := newOpenAITest(t, "m", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{}})
	})
	if _, err := o.Complete(context.Background(), "p"); err == nil || !strings.Contains(err.Error(), "no text content") {
		t.Fatalf("want empty-choices error, got %v", err)
	}
}

// Keyless local endpoints (Ollama) must work: no Authorization header sent.
func TestOpenAIEmptyKeySkipsAuthHeader(t *testing.T) {
	var gotAuth string
	var hasAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, hasAuth = r.Header["Authorization"]
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openaiResponse("ok"))
	}))
	defer srv.Close()
	o := NewOpenAI("", "llama3.1", srv.URL)
	out, err := o.Complete(context.Background(), "p")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "ok" {
		t.Fatalf("got %q", out)
	}
	if hasAuth {
		t.Fatalf("empty key must not send Authorization header, got %q", gotAuth)
	}
}
