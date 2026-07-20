package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// CLI implements LLM by shelling out to an installed coding-agent CLI in
// headless mode (default: `claude -p --output-format json <prompt>`), reusing
// its stored auth so no API key is needed. The prompt is appended as the
// last argument.
type CLI struct {
	Command string        // binary name or path (default "claude")
	Args    []string      // args before the prompt
	Timeout time.Duration // subprocess hard timeout (0 = no extra timeout)
}

// NewCLI returns a CLI provider for command with args and timeout.
func NewCLI(command string, args []string, timeout time.Duration) *CLI {
	return &CLI{Command: command, Args: args, Timeout: timeout}
}

// Complete runs the CLI once and returns the first JSON object found in its
// output (tolerating envelopes, JSONL and prose around the JSON).
func (c *CLI) Complete(ctx context.Context, prompt string) (string, error) {
	path, err := exec.LookPath(c.Command)
	if err != nil {
		return "", fmt.Errorf("cli: command %q not found in PATH (install it or change llm.cli_command): %w", c.Command, err)
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, path, append(append([]string{}, c.Args...), prompt)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("cli: %s timed out after %s", c.Command, c.Timeout)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return "", fmt.Errorf("cli: %s failed: %w: %s", c.Command, err, truncate(msg, 500))
	}
	text, err := cliText(stdout.String())
	if err != nil {
		return "", err
	}
	if obj, ok := firstJSONObject(text); ok {
		return obj, nil
	}
	return text, nil
}

// cliText unwraps known CLI output envelopes into plain response text:
// claude single-object envelope ({"type":"result","result":...}), opencode
// JSONL text events, or the raw output as-is.
func cliText(out string) (string, error) {
	trimmed := strings.TrimSpace(out)
	var env struct {
		Type    string `json:"type"`
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
	}
	if err := json.Unmarshal([]byte(trimmed), &env); err == nil && env.Type == "result" {
		if env.IsError {
			return "", fmt.Errorf("cli: agent reported an error: %s", truncate(env.Result, 500))
		}
		return env.Result, nil
	}
	var sb strings.Builder
	for _, line := range strings.Split(trimmed, "\n") {
		var ev struct {
			Type string `json:"type"`
			Part struct {
				Text string `json:"text"`
			} `json:"part"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Type == "text" {
			sb.WriteString(ev.Part.Text)
		}
	}
	if sb.Len() > 0 {
		return sb.String(), nil
	}
	return trimmed, nil
}

// firstJSONObject returns the first balanced {...} block in s, tracking
// strings and escapes so prose or fences around the JSON do not break it.
func firstJSONObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		ch := s[i]
		switch {
		case esc:
			esc = false
		case inStr:
			if ch == '\\' {
				esc = true
			} else if ch == '"' {
				inStr = false
			}
		case ch == '"':
			inStr = true
		case ch == '{':
			depth++
		case ch == '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

// truncate limits msg to n bytes for error messages.
func truncate(msg string, n int) string {
	if len(msg) <= n {
		return msg
	}
	return msg[:n] + "..."
}
