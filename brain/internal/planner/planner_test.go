package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

var testHarnesses = []string{"claude-code", "codex"}

// planFixture builds schema JSON from taskJSON values.
func planFixture(t *testing.T, tasks ...taskJSON) string {
	t.Helper()
	data, err := json.Marshal(planJSON{Tasks: tasks})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(data)
}

// okPrompt returns a prompt that satisfies the DoneMarker rule.
func okPrompt(body string) string {
	return body + " When finished, commit all changes and create the .om-done marker file at the repo root."
}

func validFixture(t *testing.T) string {
	return planFixture(t,
		taskJSON{Title: "setup", Prompt: okPrompt("Set up scaffolding."), Harness: "claude-code"},
		taskJSON{Title: "feature A", Prompt: okPrompt("Build feature A."), Harness: "codex", DependsOn: []string{"setup"}},
		taskJSON{Title: "feature B", Prompt: okPrompt("Build feature B."), Harness: "claude-code", DependsOn: []string{"setup"}},
	)
}

// mockLLM pops canned responses and records the prompts it received.
type mockLLM struct {
	responses []string
	errs      []error
	prompts   []string
}

func (m *mockLLM) Complete(_ context.Context, prompt string) (string, error) {
	m.prompts = append(m.prompts, prompt)
	i := len(m.prompts) - 1
	if i < len(m.errs) && m.errs[i] != nil {
		return "", m.errs[i]
	}
	if i >= len(m.responses) {
		return "", fmt.Errorf("mockLLM: no response %d", i)
	}
	return m.responses[i], nil
}

func TestParseValid(t *testing.T) {
	plan, err := Parse([]byte(validFixture(t)), testHarnesses)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(plan.Tasks) != 3 {
		t.Fatalf("want 3 tasks, got %d", len(plan.Tasks))
	}
	if plan.Tasks[0].ID != "t1" || plan.Tasks[1].ID != "t2" || plan.Tasks[2].ID != "t3" {
		t.Fatalf("unexpected ids: %+v", plan.Tasks)
	}
	if len(plan.Tasks[1].DependsOn) != 1 || plan.Tasks[1].DependsOn[0] != "t1" {
		t.Fatalf("depends_on titles must resolve to ids: %+v", plan.Tasks[1])
	}
	if len(plan.Tasks[0].DependsOn) != 0 {
		t.Fatalf("root task must have no deps: %+v", plan.Tasks[0])
	}
}

func TestParseCycle(t *testing.T) {
	fx := planFixture(t,
		taskJSON{Title: "a", Prompt: okPrompt("A."), Harness: "codex", DependsOn: []string{"b"}},
		taskJSON{Title: "b", Prompt: okPrompt("B."), Harness: "codex", DependsOn: []string{"a"}},
	)
	if _, err := Parse([]byte(fx), testHarnesses); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want cycle error, got %v", err)
	}
}

func TestParseDiamond(t *testing.T) {
	fx := planFixture(t,
		taskJSON{Title: "a", Prompt: okPrompt("A."), Harness: "codex"},
		taskJSON{Title: "b", Prompt: okPrompt("B."), Harness: "codex"},
		taskJSON{Title: "c", Prompt: okPrompt("C."), Harness: "codex", DependsOn: []string{"a", "b"}},
	)
	if _, err := Parse([]byte(fx), testHarnesses); err == nil || !strings.Contains(err.Error(), "at most 1") {
		t.Fatalf("want multi-parent error, got %v", err)
	}
}

func TestParseUnknownHarness(t *testing.T) {
	fx := planFixture(t,
		taskJSON{Title: "a", Prompt: okPrompt("A."), Harness: "skynet"},
	)
	if _, err := Parse([]byte(fx), testHarnesses); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("want harness error, got %v", err)
	}
}

func TestParsePromptTooLong(t *testing.T) {
	fx := planFixture(t,
		taskJSON{Title: "a", Prompt: okPrompt(strings.Repeat("x", MaxPromptChars)), Harness: "codex"},
	)
	if _, err := Parse([]byte(fx), testHarnesses); err == nil || !strings.Contains(err.Error(), "exceeding") {
		t.Fatalf("want prompt length error, got %v", err)
	}
}

func TestParseMissingDoneMarker(t *testing.T) {
	fx := planFixture(t,
		taskJSON{Title: "a", Prompt: "Do A, no marker mentioned.", Harness: "codex"},
	)
	if _, err := Parse([]byte(fx), testHarnesses); err == nil || !strings.Contains(err.Error(), DoneMarker) {
		t.Fatalf("want done-marker error, got %v", err)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"empty tasks":        `{"tasks":[]}`,
		"unknown field":      `{"tasks":[],"notes":"x"}`,
		"not json":           `hello there`,
		"duplicate title":    planFixture(t, taskJSON{Title: "a", Prompt: okPrompt("A."), Harness: "codex"}, taskJSON{Title: "a", Prompt: okPrompt("A2."), Harness: "codex"}),
		"unknown dependency": planFixture(t, taskJSON{Title: "a", Prompt: okPrompt("A."), Harness: "codex", DependsOn: []string{"ghost"}}),
		"self dependency":    planFixture(t, taskJSON{Title: "a", Prompt: okPrompt("A."), Harness: "codex", DependsOn: []string{"a"}}),
		"empty prompt":       planFixture(t, taskJSON{Title: "a", Prompt: "", Harness: "codex"}),
	}
	for name, fx := range cases {
		if _, err := Parse([]byte(fx), testHarnesses); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
}

func TestParseStripsFences(t *testing.T) {
	fenced := "Here is the plan:\n```json\n" + validFixture(t) + "\n```\n"
	if _, err := Parse([]byte(fenced), testHarnesses); err != nil {
		t.Fatalf("Parse fenced: %v", err)
	}
}

func TestSchemaJSONRoundTrip(t *testing.T) {
	plan, err := Parse([]byte(validFixture(t)), testHarnesses)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	data, err := plan.SchemaJSON()
	if err != nil {
		t.Fatalf("SchemaJSON: %v", err)
	}
	again, err := Parse(data, testHarnesses)
	if err != nil {
		t.Fatalf("Parse round-trip: %v", err)
	}
	if len(again.Tasks) != len(plan.Tasks) || again.Tasks[1].DependsOn[0] != "t1" {
		t.Fatalf("round-trip mismatch: %+v", again.Tasks)
	}
}

func TestGeneratePromptContents(t *testing.T) {
	llm := &mockLLM{responses: []string{validFixture(t)}}
	if _, err := New(llm).Generate(context.Background(), Input{Goal: "build the thing", Harnesses: testHarnesses}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(llm.prompts) != 1 {
		t.Fatalf("want exactly 1 LLM call, got %d", len(llm.prompts))
	}
	p := llm.prompts[0]
	for _, want := range []string{"build the thing", "claude-code", "codex", DoneMarker, "3500", "commit"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestGenerateRetryWithFeedback(t *testing.T) {
	llm := &mockLLM{responses: []string{"total garbage", validFixture(t)}}
	plan, err := New(llm).Generate(context.Background(), Input{Goal: "g", Harnesses: testHarnesses})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(plan.Tasks) != 3 || len(llm.prompts) != 2 {
		t.Fatalf("want retry success with 2 calls, got %d tasks / %d calls", len(plan.Tasks), len(llm.prompts))
	}
	retry := llm.prompts[1]
	if !strings.Contains(retry, "total garbage") || !strings.Contains(retry, "Error:") {
		t.Fatalf("retry prompt must contain previous response and error, got: %s", retry[:200])
	}
}

func TestGenerateFailsAfterRetry(t *testing.T) {
	llm := &mockLLM{responses: []string{"garbage one", "garbage two"}}
	_, err := New(llm).Generate(context.Background(), Input{Goal: "g", Harnesses: testHarnesses})
	if err == nil || !strings.Contains(err.Error(), "after retry") {
		t.Fatalf("want after-retry error, got %v", err)
	}
	if len(llm.prompts) != 2 {
		t.Fatalf("want exactly 2 LLM calls, got %d", len(llm.prompts))
	}
}

func TestGenerateInputValidation(t *testing.T) {
	llm := &mockLLM{}
	if _, err := New(llm).Generate(context.Background(), Input{Goal: " ", Harnesses: testHarnesses}); err == nil {
		t.Fatal("want error for empty goal")
	}
	if _, err := New(llm).Generate(context.Background(), Input{Goal: "g"}); err == nil {
		t.Fatal("want error for no harnesses")
	}
	if len(llm.prompts) != 0 {
		t.Fatalf("LLM must not be called on invalid input, got %d calls", len(llm.prompts))
	}
}

func TestGenerateLLMError(t *testing.T) {
	llm := &mockLLM{errs: []error{fmt.Errorf("boom")}}
	if _, err := New(llm).Generate(context.Background(), Input{Goal: "g", Harnesses: testHarnesses}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want wrapped LLM error, got %v", err)
	}
}
