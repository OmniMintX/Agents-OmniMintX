package scheduler

import (
	"context"
	"fmt"
	"strings"

	"github.com/OmniMintX/overmind/internal/aoclient"
	"github.com/OmniMintX/overmind/internal/store"
	"github.com/OmniMintX/overmind/internal/verifier"
)

// maxVerifyDiffBytes caps the diff sent to the tier-1 LLM verifier (the
// gitops.DiffText truncation notice marks anything cut).
const maxVerifyDiffBytes = 64 * 1024

// maxLLMErrs is how many CONSECUTIVE tier-1 LLM call failures are
// tolerated (retried next poll) before the task fails with
// kind=verify_error. Transient provider hiccups must not burn the retry
// budget or fail the task outright.
const maxLLMErrs = 3

// verifyTier1 runs the LLM verify gate AFTER tier 0 and the system-commit
// (so the graded diff includes rescued work). Only tasks with verify=llm
// reach here. proceed=false with nil error means the task was retried,
// failed, or the LLM call will be re-attempted next poll.
func (r *runner) verifyTier1(ctx context.Context, t store.Task, sess aoclient.Session, branch string, ck *taskClock) (proceed bool, err error) {
	diff, err := r.Git.DiffText(ctx, r.repo, r.defaultBranch, branch, maxVerifyDiffBytes)
	if err != nil {
		return false, r.killAndFailKind(ctx, t, sess, "verify_error",
			fmt.Sprintf("tier-1 verify: diff %s vs %s: %v", branch, r.defaultBranch, err), nil)
	}
	v, err := r.Verify.Verify(ctx, verifier.Input{TaskTitle: t.Title, TaskPrompt: t.Prompt, Diff: diff})
	if err != nil {
		// LLM/transport trouble is not the worker's fault: re-attempt on the
		// next poll, and only fail (verify_error, budget untouched) after
		// maxLLMErrs consecutive failures.
		ck.llmErrs++
		if ck.llmErrs >= maxLLMErrs {
			return false, r.killAndFailKind(ctx, t, sess, "verify_error",
				fmt.Sprintf("tier-1 verify: LLM failed %d times in a row: %v", ck.llmErrs, err), nil)
		}
		r.logf("task %s: tier-1 LLM call failed (%d/%d, retrying next poll): %v", t.ID, ck.llmErrs, maxLLMErrs, err)
		return false, nil
	}
	ck.llmErrs = 0
	if v.Verdict == verifier.VerdictOK {
		payload := jsonPayload(map[string]any{"verdict": "pass", "tier": 1})
		if err := r.St.RecordTaskVerdict(r.plan.ID, t.ID, r.runID, payload); err != nil {
			return false, err
		}
		r.logf("task %s: tier-1 verify passed", t.ID)
		return true, nil
	}
	payload := jsonPayload(map[string]any{"verdict": "fail", "tier": 1, "reason": v.Reason})
	if err := r.St.RecordTaskVerdict(r.plan.ID, t.ID, r.runID, payload); err != nil {
		return false, err
	}
	return r.retryOrFail(ctx, t, sess, 1, v.Reason, feedbackText(v), nil)
}

// retryOrFail is the shared verify-fail outcome for tier 0 and tier 1:
// while the budget allows, kill the session and record task_retry (back to
// pending; the next dispatch carries the feedback and a round-scoped
// displayName); once VerifyRounds — replayed from the event log, never
// in-memory — reaches Cfg.MaxVerifyRounds, the task fails with
// kind=verify_budget_exhausted (extra lands in that failure payload).
func (r *runner) retryOrFail(ctx context.Context, t store.Task, sess aoclient.Session, tier int, reason, feedback string, extra map[string]any) (bool, error) {
	st, err := r.St.PlanState(r.plan.ID)
	if err != nil {
		return false, err
	}
	rounds := st.VerifyRounds[t.ID]
	if rounds >= r.Cfg.MaxVerifyRounds {
		failExtra := map[string]any{"rounds_used": rounds}
		for k, v := range extra {
			failExtra[k] = v
		}
		return false, r.killAndFailKind(ctx, t, sess, "verify_budget_exhausted",
			fmt.Sprintf("tier-%d verify failed and the retry budget (%d) is exhausted: %s", tier, r.Cfg.MaxVerifyRounds, reason),
			failExtra)
	}
	if !sess.IsTerminated {
		if _, err := r.AO.KillSession(ctx, sess.ID); err != nil {
			if isTransport(err) {
				return false, err
			}
			r.logf("task %s: kill session %s before retry: %v (ignored)", t.ID, sess.ID, err)
		}
	}
	payload := jsonPayload(map[string]any{
		"round": rounds + 1, "tier": tier,
		"reason":   reason,
		"feedback": truncate(feedback, maxSpawnPrompt),
	})
	if err := r.St.RetryTask(r.plan.ID, t.ID, r.runID, payload); err != nil {
		return false, err
	}
	delete(r.clocks, t.ID)
	r.logf("task %s: verify tier %d failed — RETRY round %d/%d: %s", t.ID, tier, rounds+1, r.Cfg.MaxVerifyRounds, reason)
	return false, nil
}

// feedbackText flattens a fail verdict into the re-dispatch feedback block.
func feedbackText(v verifier.Verdict) string {
	var sb strings.Builder
	sb.WriteString(v.Reason)
	for _, it := range v.Feedback {
		sb.WriteString("\n- ")
		if it.File != "" {
			sb.WriteString(it.File + ": ")
		}
		sb.WriteString(it.Issue)
		if it.Suggestion != "" {
			sb.WriteString(" -> " + it.Suggestion)
		}
	}
	return sb.String()
}
