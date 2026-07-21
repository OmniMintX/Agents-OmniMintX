package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Anthropic Messages API via plain stdlib HTTP (no SDK).
// Docs: https://docs.anthropic.com/en/api/messages
const (
	anthropicBaseURL   = "https://api.anthropic.com"
	anthropicVersion   = "2023-06-01"
	anthropicMaxTokens = 8192
)

// Anthropic implements LLM against the Anthropic Messages API.
type Anthropic struct {
	APIKey  string // from env (provider api_key_env, default ANTHROPIC_API_KEY)
	Model   string // from config (default claude-sonnet-4-5)
	BaseURL string // override for tests; anthropicBaseURL when empty
	HTTPC   *http.Client
}

// NewAnthropic returns a client with a generous timeout (plans are one
// long completion).
func NewAnthropic(apiKey, model string) *Anthropic {
	return &Anthropic{
		APIKey: apiKey,
		Model:  model,
		HTTPC:  &http.Client{Timeout: 3 * time.Minute},
	}
}

// Complete sends one user message and returns the concatenated text blocks.
func (a *Anthropic) Complete(ctx context.Context, prompt string) (string, error) {
	if a.APIKey == "" {
		return "", fmt.Errorf("anthropic: API key is empty")
	}
	body, err := json.Marshal(map[string]any{
		"model":      a.Model,
		"max_tokens": anthropicMaxTokens,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	})
	if err != nil {
		return "", fmt.Errorf("anthropic: encode request: %w", err)
	}
	base := a.BaseURL
	if base == "" {
		base = anthropicBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(base, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("anthropic: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", anthropicVersion)

	httpc := a.HTTPC
	if httpc == nil {
		httpc = &http.Client{Timeout: 3 * time.Minute}
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("anthropic: request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("anthropic: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var env struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &env) == nil && env.Error.Message != "" {
			return "", fmt.Errorf("anthropic: %d %s: %s", resp.StatusCode, env.Error.Type, env.Error.Message)
		}
		return "", fmt.Errorf("anthropic: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return "", fmt.Errorf("anthropic: decode response: %w", err)
	}
	var sb strings.Builder
	for _, c := range msg.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	if sb.Len() == 0 {
		return "", fmt.Errorf("anthropic: response contained no text content")
	}
	return sb.String(), nil
}
