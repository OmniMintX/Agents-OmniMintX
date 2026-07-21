// Package verifier grades one finished task: it renders prompt.tmpl with
// the task title/prompt and the (caller-truncated) diff, calls the LLM
// ONCE, and retries the parse once with error feedback — the same pattern
// as planner.Generate. The verdict is binary ok|fail plus re-dispatch
// feedback; acting on it belongs to the scheduler.
package verifier

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"text/template"

	"github.com/OmniMintX/overmind/internal/planner"
)

//go:embed prompt.tmpl
var promptTmpl string

// Verdict values. The verdict is deliberately binary: either the diff
// meets the task's definition of done or it does not.
const (
	VerdictOK   = "ok"
	VerdictFail = "fail"
)

// maxFeedbackItems caps feedback; the prompt asks for at most 5 and the
// parser silently truncates any excess instead of rejecting the response.
const maxFeedbackItems = 5

// Item is one concrete, actionable feedback entry. On fail these items
// are the payload sent verbatim to the re-dispatched worker.
type Item struct {
	File       string `yaml:"file,omitempty"` // empty when not file-specific
	Issue      string `yaml:"issue"`
	Suggestion string `yaml:"suggestion"`
}

// Verdict is the validated result of a Verify call.
type Verdict struct {
	Verdict  string `yaml:"verdict"` // VerdictOK or VerdictFail
	Reason   string `yaml:"reason"`
	Feedback []Item `yaml:"feedback,omitempty"`
}

// Input is what Verify needs besides the LLM. Diff must already be
// truncated by the caller to fit the LLM context.
type Input struct {
	TaskTitle  string
	TaskPrompt string
	Diff       string
}

// Verify renders the prompt template, calls the LLM, and parses the
// response. On a parse/validation failure it retries exactly once, feeding
// the previous response and error back; a second failure is fatal.
func Verify(ctx context.Context, llm planner.LLM, in Input) (Verdict, error) {
	if strings.TrimSpace(in.TaskTitle) == "" {
		return Verdict{}, fmt.Errorf("verifier: task title must not be empty")
	}
	if strings.TrimSpace(in.TaskPrompt) == "" {
		return Verdict{}, fmt.Errorf("verifier: task prompt must not be empty")
	}
	if strings.TrimSpace(in.Diff) == "" {
		return Verdict{}, fmt.Errorf("verifier: diff must not be empty")
	}
	prompt, err := renderPrompt(in)
	if err != nil {
		return Verdict{}, err
	}
	out, err := llm.Complete(ctx, prompt)
	if err != nil {
		return Verdict{}, fmt.Errorf("verifier: LLM call failed: %w", err)
	}
	v, perr := Parse([]byte(out))
	if perr == nil {
		return v, nil
	}
	retry := fmt.Sprintf("%s\n\nYour previous response was rejected.\nPrevious response:\n%s\n\nError: %s\n\nReturn ONLY the corrected YAML, nothing else.",
		prompt, out, perr)
	out2, err := llm.Complete(ctx, retry)
	if err != nil {
		return Verdict{}, fmt.Errorf("verifier: LLM retry call failed: %w", err)
	}
	v, perr2 := Parse([]byte(out2))
	if perr2 != nil {
		return Verdict{}, fmt.Errorf("verifier: verdict invalid after retry: %w (first attempt: %v)", perr2, perr)
	}
	return v, nil
}

// renderPrompt executes prompt.tmpl with the task title, prompt and diff.
func renderPrompt(in Input) (string, error) {
	t, err := template.New("prompt").Parse(promptTmpl)
	if err != nil {
		return "", fmt.Errorf("verifier: parse prompt.tmpl: %w", err)
	}
	var sb strings.Builder
	err = t.Execute(&sb, struct {
		TaskTitle        string
		TaskPrompt       string
		Diff             string
		MaxFeedbackItems int
	}{in.TaskTitle, in.TaskPrompt, in.Diff, maxFeedbackItems})
	if err != nil {
		return "", fmt.Errorf("verifier: render prompt.tmpl: %w", err)
	}
	return sb.String(), nil
}
