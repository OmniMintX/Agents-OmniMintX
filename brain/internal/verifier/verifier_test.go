package verifier

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// mockLLM replays canned responses/errors and records prompts.
type mockLLM struct {
	prompts   []string
	responses []string
	errs      []error
}

func (m *mockLLM) Complete(_ context.Context, prompt string) (string, error) {
	m.prompts = append(m.prompts, prompt)
	i := len(m.prompts) - 1
	if i < len(m.errs) && m.errs[i] != nil {
		return "", m.errs[i]
	}
	if i < len(m.responses) {
		return m.responses[i], nil
	}
	return "", fmt.Errorf("mockLLM: no response for call %d", i+1)
}

var testInput = Input{
	TaskTitle:  "add health endpoint",
	TaskPrompt: "Add GET /health returning 200. Done when curl /health returns ok.",
	Diff:       "diff --git a/api/server.go b/api/server.go\n+func health() {}\n",
}

func TestVerifyPromptContents(t *testing.T) {
	llm := &mockLLM{responses: []string{"verdict: ok\nreason: done\n"}}
	v, err := Verify(context.Background(), llm, testInput)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if v.Verdict != VerdictOK {
		t.Fatalf("unexpected verdict: %+v", v)
	}
	if len(llm.prompts) != 1 {
		t.Fatalf("want exactly 1 LLM call, got %d", len(llm.prompts))
	}
	p := llm.prompts[0]
	for _, want := range []string{
		testInput.TaskTitle,
		testInput.TaskPrompt,
		testInput.Diff,
		"ONLY valid YAML",
		"definition of done",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestVerifyRetryWithFeedback(t *testing.T) {
	llm := &mockLLM{responses: []string{"total garbage", failFixture}}
	v, err := Verify(context.Background(), llm, testInput)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if v.Verdict != VerdictFail || len(v.Feedback) != 2 || len(llm.prompts) != 2 {
		t.Fatalf("want retry success with 2 calls, got %+v / %d calls", v, len(llm.prompts))
	}
	retry := llm.prompts[1]
	if !strings.Contains(retry, "total garbage") || !strings.Contains(retry, "Error:") {
		t.Fatalf("retry prompt must contain previous response and error, got: %s", retry[:200])
	}
}

func TestVerifyFailsAfterRetry(t *testing.T) {
	llm := &mockLLM{responses: []string{"garbage one", "garbage two"}}
	_, err := Verify(context.Background(), llm, testInput)
	if err == nil || !strings.Contains(err.Error(), "after retry") {
		t.Fatalf("want after-retry error, got %v", err)
	}
	if len(llm.prompts) != 2 {
		t.Fatalf("want exactly 2 LLM calls, got %d", len(llm.prompts))
	}
}

func TestVerifyInputValidation(t *testing.T) {
	llm := &mockLLM{}
	cases := map[string]Input{
		"empty title":  {TaskPrompt: "p", Diff: "d"},
		"empty prompt": {TaskTitle: "t", Diff: "d"},
		"empty diff":   {TaskTitle: "t", TaskPrompt: "p"},
	}
	for name, in := range cases {
		if _, err := Verify(context.Background(), llm, in); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
	if len(llm.prompts) != 0 {
		t.Fatalf("LLM must not be called on invalid input, got %d calls", len(llm.prompts))
	}
}

func TestVerifyLLMError(t *testing.T) {
	llm := &mockLLM{errs: []error{fmt.Errorf("boom")}}
	if _, err := Verify(context.Background(), llm, testInput); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want wrapped LLM error, got %v", err)
	}
	llm2 := &mockLLM{responses: []string{"garbage"}, errs: []error{nil, fmt.Errorf("boom2")}}
	if _, err := Verify(context.Background(), llm2, testInput); err == nil || !strings.Contains(err.Error(), "boom2") {
		t.Fatalf("want wrapped retry LLM error, got %v", err)
	}
}
