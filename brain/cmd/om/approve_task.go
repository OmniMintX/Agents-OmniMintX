package main

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/OmniMintX/overmind/internal/config"
	"github.com/OmniMintX/overmind/internal/store"
)

// runApproveTask is `om approve-task <plan-id> [task-id]` (task-id XOR
// --all): flip awaiting_approval task(s) back to pending so the running
// scheduler dispatches them on its next tick. Events are attributed to the
// plan's most recent run (the om run process holding the gate).
func runApproveTask(cfg config.Config, planID, taskID string, all bool) error {
	if all && taskID != "" {
		return fmt.Errorf("pass either a task id or --all, not both")
	}
	if !all && taskID == "" {
		return fmt.Errorf("task id required (or --all to approve every awaiting task)")
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	ds, err := st.PlanState(planID)
	if err != nil {
		return fmt.Errorf("plan %s not found: %w", planID, err)
	}
	if !all {
		if err := st.ApproveTask(planID, taskID, ds.LastRunID); err != nil {
			return err
		}
		fmt.Printf("Task %s approved — the scheduler dispatches it on its next tick.\n", taskID)
		return nil
	}
	// --all: approve exactly the tasks awaiting approval AT THIS MOMENT
	// (one task_approved each); tasks gated later need their own approve.
	var waiting []string
	for id, status := range ds.TaskStatus {
		if status == store.TaskAwaitingApproval {
			waiting = append(waiting, id)
		}
	}
	if len(waiting) == 0 {
		fmt.Printf("Plan %s has no tasks awaiting approval.\n", planID)
		return nil
	}
	sort.Strings(waiting)
	for _, id := range waiting {
		if err := st.ApproveTask(planID, id, ds.LastRunID); err != nil {
			return err
		}
		fmt.Printf("Task %s approved.\n", id)
	}
	fmt.Printf("%d task(s) approved — the scheduler dispatches them on its next tick.\n", len(waiting))
	return nil
}

// runRejectTask is `om reject-task <plan-id> <task-id> [--reason]`: fail an
// awaiting_approval task terminally (task_failed kind=rejected). The
// scheduler then cascades dependency_failed onto its dependents.
func runRejectTask(cfg config.Config, planID, taskID, reason string) error {
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	ds, err := st.PlanState(planID)
	if err != nil {
		return fmt.Errorf("plan %s not found: %w", planID, err)
	}
	if got := ds.TaskStatus[taskID]; got != store.TaskAwaitingApproval {
		if got == "" {
			return fmt.Errorf("task %s: not found in plan %s", taskID, planID)
		}
		return fmt.Errorf("task %s: cannot reject from status %q (only awaiting_approval tasks can be rejected)", taskID, got)
	}
	payload := map[string]any{"kind": "rejected"}
	if reason != "" {
		payload["reason"] = reason
	}
	enc, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := st.FailTask(planID, taskID, ds.LastRunID, string(enc)); err != nil {
		return err
	}
	fmt.Printf("Task %s rejected (terminal) — dependents will fail with dependency_failed.\n", taskID)
	return nil
}
