// Package planner turns one user goal into a validated Overmind plan:
// it renders prompt.tmpl (with the AO-reported harness list), calls the
// LLM ONCE, and retries the parse once with error feedback. The plan is
// data; execution belongs to the scheduler.
package planner

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
)

//go:embed prompt.tmpl
var promptTmpl string

// MaxPromptChars caps each task prompt, leaving headroom for the
// scheduler to append its completion-protocol footer under AO's
// 4096-byte spawn limit.
const MaxPromptChars = 3500

// LLM is the single-shot completion interface (mocked in tests).
type LLM interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// Task is one validated plan task, ready for store.NewTask.
type Task struct {
	ID               string
	Title            string
	Prompt           string
	Harness          string
	Check            string   // tier-0 verify command run in the worktree (may be empty)
	Verify           string   // verify strategy: none|deterministic|llm (empty input defaults to deterministic)
	RequiresApproval bool     // dispatch gated on human approval (OM-12); absent defaults to false
	DependsOn        []string // task IDs (at most one in phase 1)
}

// Plan is the validated result of a Generate or Parse call.
type Plan struct {
	Tasks  []Task
	schema planJSON // original title-keyed schema, for SchemaJSON / --edit
}

// SchemaJSON renders the plan back to indented schema JSON (title-keyed
// depends_on), the format `om plan --edit` opens in $EDITOR.
func (p *Plan) SchemaJSON() ([]byte, error) {
	return json.MarshalIndent(p.schema, "", "  ")
}

// Input is what Generate needs besides the LLM.
type Input struct {
	Goal      string
	Harnesses []string // available harness IDs from AO (GET /api/v1/agents)
}

// Planner generates plans through an LLM.
type Planner struct {
	llm LLM
}

// New returns a Planner backed by llm.
func New(llm LLM) *Planner {
	return &Planner{llm: llm}
}

// Generate renders the prompt template, calls the LLM, and parses the
// response. On a parse/validation failure it retries exactly once, feeding
// the previous response and error back; a second failure is fatal.
func (p *Planner) Generate(ctx context.Context, in Input) (*Plan, error) {
	if strings.TrimSpace(in.Goal) == "" {
		return nil, fmt.Errorf("planner: goal must not be empty")
	}
	if len(in.Harnesses) == 0 {
		return nil, fmt.Errorf("planner: no available harnesses (is the AO daemon running with agents installed?)")
	}
	prompt, err := renderPrompt(in)
	if err != nil {
		return nil, err
	}
	out, err := p.llm.Complete(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("planner: LLM call failed: %w", err)
	}
	plan, perr := Parse([]byte(out), in.Harnesses)
	if perr == nil {
		return plan, nil
	}
	retry := fmt.Sprintf("%s\n\nYour previous response was rejected.\nPrevious response:\n%s\n\nError: %s\n\nReturn ONLY the corrected JSON object, nothing else.",
		prompt, out, perr)
	out2, err := p.llm.Complete(ctx, retry)
	if err != nil {
		return nil, fmt.Errorf("planner: LLM retry call failed: %w", err)
	}
	plan, perr2 := Parse([]byte(out2), in.Harnesses)
	if perr2 != nil {
		return nil, fmt.Errorf("planner: plan invalid after retry: %w (first attempt: %v)", perr2, perr)
	}
	return plan, nil
}

// renderPrompt executes prompt.tmpl with the goal + harness list.
func renderPrompt(in Input) (string, error) {
	t, err := template.New("prompt").Parse(promptTmpl)
	if err != nil {
		return "", fmt.Errorf("planner: parse prompt.tmpl: %w", err)
	}
	var sb strings.Builder
	err = t.Execute(&sb, struct {
		Goal           string
		Harnesses      []string
		MaxPromptChars int
	}{in.Goal, in.Harnesses, MaxPromptChars})
	if err != nil {
		return "", fmt.Errorf("planner: render prompt.tmpl: %w", err)
	}
	return sb.String(), nil
}
