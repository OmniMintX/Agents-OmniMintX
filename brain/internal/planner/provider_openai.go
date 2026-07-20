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

// OpenAI-compatible /chat/completions client. One implementation covers
// OpenAI, DeepSeek, Ollama and any other endpoint speaking the same schema;
// only BaseURL differs (e.g. https://api.deepseek.com, http://localhost:11434/v1).
const openaiDefaultBaseURL = "https://api.openai.com/v1"

// OpenAI implements LLM against any OpenAI-compatible chat completions API.
type OpenAI struct {
	APIKey  string // from env (config.LLM.APIKeyEnv, default OPENAI_API_KEY)
	Model   string // from config
	BaseURL string // from config; openaiDefaultBaseURL when empty
	HTTPC   *http.Client
}

// NewOpenAI returns a client with a generous timeout (plans are one
// long completion).
func NewOpenAI(apiKey, model, baseURL string) *OpenAI {
	return &OpenAI{
		APIKey:  apiKey,
		Model:   model,
		BaseURL: baseURL,
		HTTPC:   &http.Client{Timeout: 3 * time.Minute},
	}
}

// Complete sends one user message and returns choices[0].message.content.
func (o *OpenAI) Complete(ctx context.Context, prompt string) (string, error) {
	if o.APIKey == "" {
		return "", fmt.Errorf("openai: API key is empty")
	}
	body, err := json.Marshal(map[string]any{
		"model": o.Model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	})
	if err != nil {
		return "", fmt.Errorf("openai: encode request: %w", err)
	}
	base := o.BaseURL
	if base == "" {
		base = openaiDefaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(base, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("openai: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.APIKey)

	httpc := o.HTTPC
	if httpc == nil {
		httpc = &http.Client{Timeout: 3 * time.Minute}
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("openai: request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("openai: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var env struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &env) == nil && env.Error.Message != "" {
			return "", fmt.Errorf("openai: %d %s: %s", resp.StatusCode, env.Error.Type, env.Error.Message)
		}
		return "", fmt.Errorf("openai: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var msg struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return "", fmt.Errorf("openai: decode response: %w", err)
	}
	if len(msg.Choices) == 0 || msg.Choices[0].Message.Content == "" {
		return "", fmt.Errorf("openai: response contained no text content")
	}
	return msg.Choices[0].Message.Content, nil
}
