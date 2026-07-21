package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/OmniMintX/overmind/internal/aoclient"
	"github.com/OmniMintX/overmind/internal/config"
	"github.com/OmniMintX/overmind/internal/gitops"
	"github.com/OmniMintX/overmind/internal/scheduler"
	"github.com/OmniMintX/overmind/internal/store"
	"github.com/OmniMintX/overmind/internal/verifier"
)

// openStore opens the Overmind DB, creating its directory when missing
// (so om status works before om plan has ever run).
func openStore(cfg config.Config) (*store.Store, error) {
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o700); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	return store.Open(cfg.DBPath)
}

// runRun is `om run <plan-id>`: execute an approved plan against the AO
// daemon until it is done or failed. Ctrl-C stops cleanly; re-running
// resumes from the event log (dispatch is idempotent).
func runRun(cfg config.Config, planID, autonomyFlag, notifyFlag string) error {
	autonomy, err := resolveAutonomy(cfg, autonomyFlag)
	if err != nil {
		return err
	}
	notify, err := resolveNotify(cfg, notifyFlag)
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	verify, err := verifierFor(cfg, st, planID)
	if err != nil {
		return err
	}
	ao := aoclient.New(cfg.AOBaseURL)
	if autonomy != config.AutonomyOff {
		plan, err := st.GetPlan(planID)
		if err != nil {
			return fmt.Errorf("plan %s not found: %w", planID, err)
		}
		if err := ensureAutonomy(ctx, ao, plan.ProjectID, permissionForAutonomy(autonomy), os.Stdout); err != nil {
			return err
		}
	}
	s := &scheduler.Scheduler{
		St:  st,
		AO:  ao,
		Git: gitops.Merger{},
		Cfg: scheduler.Config{
			MaxParallel:         cfg.MaxParallel,
			PollInterval:        time.Duration(cfg.PollIntervalSec) * time.Second,
			TaskTimeout:         time.Duration(cfg.TaskTimeoutMin) * time.Minute,
			NoSignalTimeout:     time.Duration(cfg.NoSignalTimeoutMin) * time.Minute,
			IdleNoMarkerTimeout: time.Duration(cfg.IdleNoMarkerTimeoutMin) * time.Minute,
			MaxVerifyRounds:     cfg.MaxVerifyRounds,
			Notifier:            notifierFor(notify, os.Stderr),
		},
		PID:    int64(os.Getpid()),
		Log:    os.Stdout,
		Verify: verify,
	}
	if err := s.Run(ctx, planID); err != nil {
		if ctx.Err() != nil {
			fmt.Printf("Interrupted. Resume with: om run %s\n", planID)
			return nil
		}
		return err
	}
	return nil
}

// resolveAutonomy returns the effective autonomy mode for this run: the
// --autonomy flag when set, else the config knob. bypass-permissions
// additionally requires the autonomy_allow_bypass opt-in in the config file
// (there is no CLI flag for it) — workers run without a sandbox.
func resolveAutonomy(cfg config.Config, flag string) (string, error) {
	mode, err := config.NormalizeAutonomy(cfg.Autonomy)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(flag) != "" {
		if mode, err = config.NormalizeAutonomy(flag); err != nil {
			return "", err
		}
	}
	if mode == config.AutonomyBypass && !cfg.AutonomyAllowBypass {
		return "", errors.New("bypass-permissions chạy worker KHÔNG sandbox; chỉ bật khi hiểu rủi ro: set autonomy_allow_bypass: true trong ~/.overmind/config.yaml")
	}
	return mode, nil
}

// permissionForAutonomy maps a non-off autonomy mode onto AO's permission
// vocabulary (the strings coincide, but the mapping is explicit on purpose).
func permissionForAutonomy(mode string) aoclient.PermissionMode {
	switch mode {
	case config.AutonomyAcceptEdits:
		return aoclient.PermissionAcceptEdits
	case config.AutonomyBypass:
		return aoclient.PermissionBypassPermissions
	default:
		return aoclient.PermissionAuto
	}
}

// projectConfigAPI is the slice of *aoclient.Client that ensureAutonomy
// needs, so tests can stub the AO daemon.
type projectConfigAPI interface {
	GetProjectConfig(ctx context.Context, projectID string) (aoclient.ProjectConfig, error)
	UpdateProjectConfig(ctx context.Context, projectID string, cfg aoclient.ProjectConfig) (aoclient.Project, error)
}

// ensureAutonomy sets the project's agentConfig.permissions to want, once,
// before any dispatch. AO's PUT replaces the stored config wholesale, so the
// flow is mandatory: GET the config, mutate only permissions, PUT the whole
// object back (aoclient round-trips unmodeled fields losslessly). Idempotent —
// already at want means no PUT. Any GET/PUT error aborts the run: never
// dispatch with an unknown permission state.
func ensureAutonomy(ctx context.Context, ao projectConfigAPI, projectID string, want aoclient.PermissionMode, logw io.Writer) error {
	pc, err := ao.GetProjectConfig(ctx, projectID)
	if err != nil {
		return fmt.Errorf("autonomy: get project %s config: %w", projectID, err)
	}
	old := pc.AgentConfig.Permissions
	if old == want {
		fmt.Fprintf(logw, "autonomy: đã ở %s\n", want)
		return nil
	}
	pc.AgentConfig.Permissions = want
	if _, err := ao.UpdateProjectConfig(ctx, projectID, pc); err != nil {
		return fmt.Errorf("autonomy: set project %s permissions %s: %w", projectID, want, err)
	}
	oldLabel := string(old)
	if oldLabel == "" {
		oldLabel = "(unset)"
	}
	fmt.Fprintf(logw, "autonomy: project %s permissions %s → %s\n", projectID, oldLabel, want)
	return nil
}

// verifierFor builds the tier-1 LLM verifier when the plan has at least
// one verify=llm task, failing FAST (before any dispatch) when the config
// lacks a usable roles.verifier. Plans without llm-verified tasks run with
// no verifier at all.
func verifierFor(cfg config.Config, st *store.Store, planID string) (scheduler.Verifier, error) {
	tasks, err := st.GetTasks(planID)
	if err != nil {
		return nil, fmt.Errorf("load tasks of plan %s: %w", planID, err)
	}
	needed := false
	for _, t := range tasks {
		if t.Verify == "llm" {
			needed = true
			break
		}
	}
	if !needed {
		return nil, nil
	}
	llm, llmDesc, err := newRoleLLM(cfg, "verifier")
	if err != nil {
		return nil, fmt.Errorf("plan %s has verify=llm tasks but no usable verifier LLM: %w", planID, err)
	}
	fmt.Printf("Tier-1 verification with %s\n", llmDesc)
	return scheduler.VerifyFunc(func(ctx context.Context, in verifier.Input) (verifier.Verdict, error) {
		return verifier.Verify(ctx, llm, in)
	}), nil
}

// runStatus is `om status [plan-id]`: without an id, list all plans; with
// one, show its tasks with event-derived statuses.
func runStatus(cfg config.Config, planID string) error {
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	if planID == "" {
		plans, err := st.ListPlans()
		if err != nil {
			return err
		}
		if len(plans) == 0 {
			fmt.Println("No plans. Create one with: om plan \"<goal>\" --project <id-or-path>")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "PLAN\tSTATUS\tPROJECT\tCREATED\tGOAL")
		for _, p := range plans {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				p.ID, p.Status, p.ProjectID, p.CreatedAt.Local().Format("2006-01-02 15:04"), p.Goal)
		}
		return w.Flush()
	}
	plan, err := st.GetPlan(planID)
	if err != nil {
		return fmt.Errorf("plan %s not found: %w", planID, err)
	}
	ds, err := st.PlanState(planID)
	if err != nil {
		return err
	}
	tasks, err := st.GetTasks(planID)
	if err != nil {
		return err
	}
	fmt.Printf("Plan %s [%s] — %s\n\n", plan.ID, ds.PlanStatus, plan.Goal)
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TASK\tSTATUS\tHARNESS\tDEPENDS ON\tSESSION\tPR")
	var awaiting []string
	for _, t := range tasks {
		deps, sess, pr := "-", "-", "-"
		if len(t.DependsOn) > 0 {
			deps = strings.Join(t.DependsOn, ",")
		}
		if t.AOSessionID != nil {
			sess = *t.AOSessionID
		}
		if t.PRURL != nil {
			pr = *t.PRURL
		}
		if ds.TaskStatus[t.ID] == store.TaskAwaitingApproval {
			awaiting = append(awaiting, t.ID)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, ds.TaskStatus[t.ID], t.Harness, deps, sess, pr)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, id := range awaiting {
		fmt.Printf("\nTask %s awaits approval: om approve-task %s %s  (or reject: om reject-task %s %s --reason \"...\")\n",
			id, planID, id, planID, id)
	}
	return nil
}

// runEvents is `om events <plan-id>`: dump the append-only brain event log.
func runEvents(cfg config.Config, planID string) error {
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	events, err := st.ListEvents(planID)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return fmt.Errorf("no events for plan %s (unknown plan?)", planID)
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tTYPE\tTASK\tRUN\tPAYLOAD")
	for _, e := range events {
		task, payload := "-", "-"
		if e.TaskID != nil {
			task = *e.TaskID
		}
		if e.PayloadJSON != "" && e.PayloadJSON != "{}" {
			payload = e.PayloadJSON
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			e.CreatedAt.Local().Format("15:04:05"), e.Type, task, e.RunID, payload)
	}
	return w.Flush()
}
