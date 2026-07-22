package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/OmniMintX/overmind/internal/aoclient"
	"github.com/OmniMintX/overmind/internal/config"
	"github.com/OmniMintX/overmind/internal/planner"
	"github.com/OmniMintX/overmind/internal/store"
)

// runPlan is the full `om plan` flow: resolve project + harnesses from AO,
// generate the plan via the LLM, optionally $EDITOR it, save as draft.
func runPlan(cfg config.Config, goal, project string, edit bool) error {
	if strings.TrimSpace(goal) == "" {
		return fmt.Errorf("goal must not be empty")
	}
	if strings.TrimSpace(project) == "" {
		return fmt.Errorf("--project is required (AO project id or repository path)")
	}
	llm, llmDesc, err := newLLM(cfg)
	if err != nil {
		return err
	}
	ctx := context.Background()
	ao := aoclient.New(cfg.AOBaseURL)
	projectID, err := resolveProject(ctx, ao, project)
	if err != nil {
		return err
	}
	harnesses, err := availableHarnesses(ctx, ao)
	if err != nil {
		return err
	}

	fmt.Printf("Planning with %s (project %s, harnesses: %s)...\n",
		llmDesc, projectID, strings.Join(harnesses, ", "))
	plan, err := planner.New(llm).Generate(ctx, planner.Input{Goal: goal, Harnesses: harnesses})
	if err != nil {
		return err
	}
	if edit {
		if plan, err = editPlan(plan, harnesses); err != nil {
			return err
		}
	}

	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o700); err != nil {
		return fmt.Errorf("create db dir: %w", err)
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	planID, err := newPlanID()
	if err != nil {
		return err
	}
	if err := st.CreatePlan(planID, goal, projectID, storeTasks(plan.Tasks)); err != nil {
		return err
	}
	printPlanTable(planID, goal, plan.Tasks)
	fmt.Printf("\nPlan %s saved as draft. Approve with: om approve %s\n", planID, planID)
	return nil
}

// storeTasks maps validated planner tasks to store rows.
func storeTasks(tasks []planner.Task) []store.NewTask {
	out := make([]store.NewTask, len(tasks))
	for i, t := range tasks {
		out[i] = store.NewTask{ID: t.ID, Title: t.Title, Prompt: t.Prompt, Harness: t.Harness, Check: t.Check, Verify: t.Verify, RequiresApproval: t.RequiresApproval, DependsOn: t.DependsOn}
	}
	return out
}

// newLLM builds the planner LLM from the provider assigned to roles.planner.
func newLLM(cfg config.Config) (planner.LLM, string, error) {
	return newRoleLLM(cfg, "planner")
}

// newRoleLLM resolves role → named provider and builds the LLM client.
// Type anthropic|openai-compatible|cli; when unset it auto-detects: cli if
// the CLI binary is installed, else anthropic if its API key env var is set.
// A local base_url (e.g. Ollama) needs no API key. Key VALUES never printed.
func newRoleLLM(cfg config.Config, role string) (planner.LLM, string, error) {
	r, p, err := cfg.LLMForRole(role)
	if err != nil {
		return nil, "", err
	}
	name := r.Provider
	cliCmd := p.Command
	if cliCmd == "" {
		cliCmd = "claude"
	}
	typ := strings.ToLower(strings.TrimSpace(p.Type))
	if typ == "" || typ == "auto" {
		switch {
		case commandExists(cliCmd):
			typ = "cli"
		case os.Getenv(keyEnvName(p.APIKeyEnv, "ANTHROPIC_API_KEY")) != "":
			typ = "anthropic"
		default:
			return nil, "", fmt.Errorf("no LLM available for providers.%s; either: install the %q CLI (type: cli), set the %s environment variable (type: anthropic), or configure an OpenAI-compatible endpoint (type: openai-compatible with base_url, api_key_env)",
				name, cliCmd, keyEnvName(p.APIKeyEnv, "ANTHROPIC_API_KEY"))
		}
	}
	switch typ {
	case "anthropic":
		envName := keyEnvName(p.APIKeyEnv, "ANTHROPIC_API_KEY")
		key := os.Getenv(envName)
		if key == "" {
			return nil, "", fmt.Errorf("providers.%s: API key missing: set the %s environment variable", name, envName)
		}
		return planner.NewAnthropic(key, r.Model), "anthropic/" + r.Model, nil
	case "openai", "openai-compatible":
		envName := keyEnvName(p.APIKeyEnv, "OPENAI_API_KEY")
		key := os.Getenv(envName)
		if key == "" && !config.IsLocalBaseURL(p.BaseURL) {
			return nil, "", fmt.Errorf("providers.%s: API key missing: set the %s environment variable (only local base_url endpoints like Ollama need no key)", name, envName)
		}
		return planner.NewOpenAI(key, r.Model, p.BaseURL), "openai-compatible/" + r.Model, nil
	case "cli":
		if !commandExists(cliCmd) {
			return nil, "", fmt.Errorf("providers.%s: type is cli but command %q was not found in PATH", name, cliCmd)
		}
		timeout := time.Duration(p.TimeoutSec) * time.Second
		if timeout <= 0 {
			timeout = 3 * time.Minute
		}
		return planner.NewCLI(cliCmd, p.Args, timeout), "cli/" + cliCmd, nil
	default:
		return nil, "", fmt.Errorf("providers.%s: unknown type %q (expected openai-compatible, anthropic or cli)", name, p.Type)
	}
}

// keyEnvName returns the configured API key env var name, or fallback.
func keyEnvName(configured, fallback string) string {
	if configured != "" {
		return configured
	}
	return fallback
}

// commandExists reports whether cmd resolves in PATH.
func commandExists(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

// resolveProject maps --project (AO project id or path) to a project id,
// registering the path with AO when it is an existing directory not yet known.
func resolveProject(ctx context.Context, ao *aoclient.Client, arg string) (string, error) {
	projects, err := ao.ListProjects(ctx)
	if err != nil {
		return "", err
	}
	for _, p := range projects {
		if p.ID == arg {
			return p.ID, nil
		}
	}
	abs, absErr := filepath.Abs(arg)
	if absErr == nil {
		for _, p := range projects {
			if p.Path == abs || p.Path == arg {
				return p.ID, nil
			}
		}
		if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
			p, err := ao.AddProject(ctx, aoclient.AddProjectInput{Path: abs})
			if err != nil {
				return "", fmt.Errorf("register project %s with AO: %w", abs, err)
			}
			fmt.Printf("Registered new AO project %s (%s)\n", p.ID, p.Path)
			return p.ID, nil
		}
	}
	return "", fmt.Errorf("project %q is neither a known AO project id/path nor an existing directory", arg)
}

// availableHarnesses returns installed harness IDs from AO, falling back to
// the daemon-supported list when the local probe found none installed.
func availableHarnesses(ctx context.Context, ao *aoclient.Client) ([]string, error) {
	inv, err := ao.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	list := inv.Installed
	if len(list) == 0 {
		list = inv.Supported
	}
	ids := make([]string, 0, len(list))
	for _, a := range list {
		ids = append(ids, a.ID)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("AO daemon reports no available harnesses")
	}
	return ids, nil
}

// editPlan writes the plan's schema JSON to a temp file, opens $EDITOR
// (fallback vi), then re-parses and re-validates the edited content.
func editPlan(plan *planner.Plan, harnesses []string) (*planner.Plan, error) {
	data, err := plan.SchemaJSON()
	if err != nil {
		return nil, fmt.Errorf("render plan JSON: %w", err)
	}
	f, err := os.CreateTemp("", "om-plan-*.json")
	if err != nil {
		return nil, fmt.Errorf("create temp plan file: %w", err)
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return nil, fmt.Errorf("write temp plan file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	parts := strings.Fields(editor)
	cmd := exec.Command(parts[0], append(parts[1:], path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("editor %q failed: %w", editor, err)
	}
	edited, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read edited plan: %w", err)
	}
	out, err := planner.Parse(edited, harnesses)
	if err != nil {
		return nil, fmt.Errorf("edited plan is invalid, nothing saved: %w", err)
	}
	return out, nil
}

// printPlanTable prints the saved plan as a terminal table.
func printPlanTable(planID, goal string, tasks []planner.Task) {
	fmt.Printf("\nPlan %s — %s\n\n", planID, goal)
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTITLE\tHARNESS\tDEPENDS ON\tAPPROVAL\tPROMPT")
	for _, t := range tasks {
		deps := "-"
		if len(t.DependsOn) > 0 {
			deps = strings.Join(t.DependsOn, ",")
		}
		approval := "-"
		if t.RequiresApproval {
			approval = "yes"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d chars\n", t.ID, t.Title, t.Harness, deps, approval, len(t.Prompt))
	}
	w.Flush()
}

// runApprove flips a draft plan to approved (draft -> approved, one event).
func runApprove(cfg config.Config, planID string) error {
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.ApprovePlan(planID); err != nil {
		return fmt.Errorf("approve plan %s: %w (only draft plans can be approved)", planID, err)
	}
	fmt.Printf("Plan %s approved. Run with: om run %s\n", planID, planID)
	return nil
}

// newPlanID returns a short random plan id like p-3fa9c2d1.
func newPlanID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate plan id: %w", err)
	}
	return "p-" + hex.EncodeToString(b[:]), nil
}
