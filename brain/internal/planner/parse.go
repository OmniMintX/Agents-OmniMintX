package planner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// taskJSON / planJSON mirror the fixed LLM output schema:
// {"tasks":[{"title","prompt","harness","check","verify","depends_on":[]}]}.
// depends_on references other tasks by TITLE. check is the optional
// deterministic tier-0 verification command run in the session worktree.
// verify is the optional verify strategy (none|deterministic|llm); empty
// defaults to deterministic.
type taskJSON struct {
	Title     string   `json:"title"`
	Prompt    string   `json:"prompt"`
	Harness   string   `json:"harness"`
	Check     string   `json:"check,omitempty"`
	Verify    string   `json:"verify,omitempty"`
	DependsOn []string `json:"depends_on"`
}

type planJSON struct {
	Tasks []taskJSON `json:"tasks"`
}

// Parse strict-decodes and validates raw plan JSON against the schema and
// the phase-1 rules, then assigns sequential task IDs (t1..tN) and resolves
// depends_on titles into IDs. allowedHarnesses is the AO-reported list.
func Parse(data []byte, allowedHarnesses []string) (*Plan, error) {
	raw := extractJSON(data)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var pj planJSON
	if err := dec.Decode(&pj); err != nil {
		return nil, fmt.Errorf("plan JSON does not match the schema: %w", err)
	}
	if err := validate(pj, allowedHarnesses); err != nil {
		return nil, err
	}

	idByTitle := make(map[string]string, len(pj.Tasks))
	for i, t := range pj.Tasks {
		idByTitle[t.Title] = fmt.Sprintf("t%d", i+1)
	}
	plan := &Plan{schema: pj, Tasks: make([]Task, len(pj.Tasks))}
	for i, t := range pj.Tasks {
		var deps []string
		for _, d := range t.DependsOn {
			deps = append(deps, idByTitle[d])
		}
		verify := strings.TrimSpace(t.Verify)
		if verify == "" {
			verify = "deterministic"
		}
		plan.Tasks[i] = Task{
			ID:        idByTitle[t.Title],
			Title:     t.Title,
			Prompt:    t.Prompt,
			Harness:   t.Harness,
			Check:     strings.TrimSpace(t.Check),
			Verify:    verify,
			DependsOn: deps,
		}
	}
	return plan, nil
}

// validate enforces: >=1 task, unique non-empty titles, harness from the
// allowed list, prompt non-empty / <= MaxPromptChars, depends_on
// resolvable with AT MOST ONE parent (chain/fan-out only, no diamond),
// and no cycles. The completion-marker protocol is appended by the
// scheduler at dispatch, so it is deliberately NOT validated here.
func validate(pj planJSON, allowedHarnesses []string) error {
	if len(pj.Tasks) == 0 {
		return fmt.Errorf("plan must contain at least one task")
	}
	allowed := make(map[string]bool, len(allowedHarnesses))
	for _, h := range allowedHarnesses {
		allowed[h] = true
	}
	titles := make(map[string]bool, len(pj.Tasks))
	for _, t := range pj.Tasks {
		if strings.TrimSpace(t.Title) == "" {
			return fmt.Errorf("every task needs a non-empty title")
		}
		if titles[t.Title] {
			return fmt.Errorf("duplicate task title %q: titles must be unique", t.Title)
		}
		titles[t.Title] = true
	}
	parent := make(map[string]string, len(pj.Tasks)) // title -> single parent title
	for _, t := range pj.Tasks {
		if !allowed[t.Harness] {
			return fmt.Errorf("task %q: harness %q is not available; use one of: %s",
				t.Title, t.Harness, strings.Join(allowedHarnesses, ", "))
		}
		if strings.TrimSpace(t.Prompt) == "" {
			return fmt.Errorf("task %q: prompt must not be empty", t.Title)
		}
		if len(t.Prompt) > MaxPromptChars {
			return fmt.Errorf("task %q: prompt is %d chars, exceeding the %d limit",
				t.Title, len(t.Prompt), MaxPromptChars)
		}
		switch strings.TrimSpace(t.Verify) {
		case "", "none", "deterministic", "llm":
		default:
			return fmt.Errorf("task %q: verify %q is invalid; use \"none\", \"deterministic\" or \"llm\" (empty defaults to deterministic)",
				t.Title, t.Verify)
		}
		if len(t.DependsOn) > 1 {
			return fmt.Errorf("task %q has %d parents; at most 1 is allowed (chain or fan-out only, no diamond dependencies in phase 1)",
				t.Title, len(t.DependsOn))
		}
		for _, d := range t.DependsOn {
			if d == t.Title {
				return fmt.Errorf("task %q depends on itself", t.Title)
			}
			if !titles[d] {
				return fmt.Errorf("task %q depends on unknown task %q", t.Title, d)
			}
			parent[t.Title] = d
		}
	}
	// With at most one parent per task, a cycle is a loop in the parent chain.
	for start := range parent {
		seen := map[string]bool{start: true}
		for cur, ok := parent[start]; ok; cur, ok = parent[cur] {
			if seen[cur] {
				return fmt.Errorf("dependency cycle detected involving task %q", cur)
			}
			seen[cur] = true
		}
	}
	return nil
}

// extractJSON tolerates markdown fences / prose around the JSON object by
// slicing from the first '{' to the last '}'.
func extractJSON(data []byte) []byte {
	start := bytes.IndexByte(data, '{')
	end := bytes.LastIndexByte(data, '}')
	if start >= 0 && end > start {
		return data[start : end+1]
	}
	return data
}
