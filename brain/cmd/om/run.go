package main

import (
	"context"
	"fmt"
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
func runRun(cfg config.Config, planID string) error {
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s := &scheduler.Scheduler{
		St:  st,
		AO:  aoclient.New(cfg.AOBaseURL),
		Git: gitops.Merger{},
		Cfg: scheduler.Config{
			MaxParallel:         cfg.MaxParallel,
			PollInterval:        time.Duration(cfg.PollIntervalSec) * time.Second,
			TaskTimeout:         time.Duration(cfg.TaskTimeoutMin) * time.Minute,
			NoSignalTimeout:     time.Duration(cfg.NoSignalTimeoutMin) * time.Minute,
			IdleNoMarkerTimeout: time.Duration(cfg.IdleNoMarkerTimeoutMin) * time.Minute,
		},
		PID: int64(os.Getpid()),
		Log: os.Stdout,
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
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, ds.TaskStatus[t.ID], t.Harness, deps, sess, pr)
	}
	return w.Flush()
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
