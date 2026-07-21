package scheduler

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/OmniMintX/overmind/internal/store"
	"github.com/OmniMintX/overmind/internal/verifier"
)

// fakeVerifier scripts tier-1 verdicts: each Verify call consumes the next
// step; the last step persists. A step with err != nil simulates an LLM
// transport/call failure.
type fakeVerifier struct {
	mu    sync.Mutex
	steps []verifyStep
	calls int
	ins   []verifier.Input
}

type verifyStep struct {
	v   verifier.Verdict
	err error
}

func vOK() verifyStep { return verifyStep{v: verifier.Verdict{Verdict: verifier.VerdictOK}} }

func vFail(reason string, items ...verifier.Item) verifyStep {
	return verifyStep{v: verifier.Verdict{Verdict: verifier.VerdictFail, Reason: reason, Feedback: items}}
}

func vErr(msg string) verifyStep { return verifyStep{err: fmt.Errorf("%s", msg)} }

func (f *fakeVerifier) Verify(_ context.Context, in verifier.Input) (verifier.Verdict, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.ins = append(f.ins, in)
	step := f.steps[0]
	if len(f.steps) > 1 {
		f.steps = f.steps[1:]
	}
	return step.v, step.err
}

// ntLLM is nt() with verify=llm.
func ntLLM(id string, deps ...string) store.NewTask {
	t := nt(id, deps...)
	t.Verify = "llm"
	return t
}

// eventPayloads returns payloads of all events of one type, in order.
func eventPayloads(t *testing.T, st *store.Store, typ string) []string {
	t.Helper()
	events, err := st.ListEvents("plan-1")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		if e.Type == typ {
			out = append(out, e.PayloadJSON)
		}
	}
	return out
}

// TestDisplayNameForRound: round 0 must equal the legacy hash (adopting
// pre-retry sessions on resume); rounds must not collide with each other
// or across tasks, and stay within AO's displayName constraints.
func TestDisplayNameForRound(t *testing.T) {
	if displayNameForRound("p-1", "t1", 0) != displayNameFor("p-1", "t1") {
		t.Fatal("round 0 must keep the legacy displayName")
	}
	seen := map[string]bool{}
	for round := 0; round <= 3; round++ {
		n := displayNameForRound("p-1", "t1", round)
		if seen[n] {
			t.Fatalf("round %d displayName %q collides", round, n)
		}
		seen[n] = true
		if !strings.HasPrefix(n, "om-") || len(n) != len("om-")+8 {
			t.Fatalf("displayName %q must be om-<hex8>", n)
		}
	}
	if displayNameForRound("p-1", "t1", 1) == displayNameForRound("p-1", "t2", 1) {
		t.Fatal("same round across tasks must not collide")
	}
	if displayNameForRound("p-1", "t1", 1) != displayNameForRound("p-1", "t1", 1) {
		t.Fatal("displayNameForRound must be deterministic")
	}
}

// TestRunTier1PassMerges: verify=llm + ok verdict -> tier-1 verdict pass
// recorded, branch merged, task done. The verifier must be called exactly
// once and AFTER the system-commit (its diff includes rescued work).
func TestRunTier1PassMerges(t *testing.T) {
	st, ao, git, s := newHarnessGit(t, []store.NewTask{ntLLM("a1234567")}, Config{MaxVerifyRounds: 2})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(0)
	git.emptyDiff["ao/sess-1"] = true // force the system-commit path
	git.uncommitted["ao/sess-1"] = true
	fv := &fakeVerifier{steps: []verifyStep{vOK()}}
	s.Verify = fv
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskDone {
		t.Fatalf("task = %s, want done", got)
	}
	if fv.calls != 1 {
		t.Fatalf("verifier calls = %d, want 1", fv.calls)
	}
	if !git.merged["ao/sess-1"] {
		t.Fatal("tier-1-passed branch must be merged")
	}
	verdicts := eventPayloads(t, st, store.EventTaskVerdict)
	if len(verdicts) != 2 {
		t.Fatalf("task_verdict events = %d, want 2 (tier 0 + tier 1): %v", len(verdicts), verdicts)
	}
	if !strings.Contains(verdicts[1], `"tier":1`) || !strings.Contains(verdicts[1], `"verdict":"pass"`) {
		t.Fatalf("want tier-1 pass verdict, got %q", verdicts[1])
	}
	// syscommit must precede the verify call: its diff was requested after.
	var order []string
	for _, e := range ao.events() {
		if strings.HasPrefix(e, "syscommit:") || strings.HasPrefix(e, "merge:") {
			order = append(order, e)
		}
	}
	if len(order) != 2 || order[0] != "syscommit:ao/sess-1" || order[1] != "merge:ao/sess-1" {
		t.Fatalf("want syscommit before merge, got %v", order)
	}
	if fv.ins[0].TaskTitle != "task a1234567" || fv.ins[0].Diff == "" {
		t.Fatalf("verifier input incomplete: %+v", fv.ins[0])
	}
}

// TestRunTier1FailRetriesWithFeedback: a tier-1 fail within budget must
// kill the session, record task_retry {round, tier, reason, feedback}, and
// re-dispatch a NEW session under the round-1 displayName whose prompt
// carries the verifier feedback; a pass on round 1 finishes the task.
func TestRunTier1FailRetriesWithFeedback(t *testing.T) {
	st, ao, git, s := newHarnessGit(t, []store.NewTask{ntLLM("a1234567")}, Config{MaxVerifyRounds: 2})
	name0 := displayNameFor("plan-1", "a1234567")
	name1 := displayNameForRound("plan-1", "a1234567", 1)
	ao.scripts[name0] = doneScript(0)
	ao.scripts[name1] = doneScript(0)
	fv := &fakeVerifier{steps: []verifyStep{
		vFail("greeting file missing", verifier.Item{File: "hello.txt", Issue: "not created", Suggestion: "create it"}),
		vOK(),
	}}
	s.Verify = fv
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskDone {
		t.Fatalf("task = %s, want done", got)
	}
	retries := eventPayloads(t, st, store.EventTaskRetry)
	if len(retries) != 1 {
		t.Fatalf("task_retry events = %d, want 1: %v", len(retries), retries)
	}
	for _, want := range []string{`"round":1`, `"tier":1`, "greeting file missing", "hello.txt"} {
		if !strings.Contains(retries[0], want) {
			t.Fatalf("task_retry payload missing %q: %s", want, retries[0])
		}
	}
	if _, ok := ao.prompts[name1]; !ok {
		t.Fatalf("no round-1 session was created: %v", ao.events())
	}
	p := ao.prompts[name1]
	for _, want := range []string{"VERIFIER FEEDBACK", "greeting file missing", "hello.txt: not created -> create it", "OVERMIND PROTOCOL"} {
		if !strings.Contains(p, want) {
			t.Fatalf("round-1 prompt missing %q:\n%s", want, p)
		}
	}
	if len(p) > maxSpawnPrompt {
		t.Fatalf("round-1 prompt is %d bytes, exceeds AO's %d cap", len(p), maxSpawnPrompt)
	}
	// The rejected round-0 session must be killed before the re-dispatch.
	killedBeforeCreate := false
	for _, e := range ao.events() {
		if e == "kill:sess-1" {
			killedBeforeCreate = true
		}
		if e == "create:"+name1 && !killedBeforeCreate {
			t.Fatalf("round-1 session created before round-0 was killed: %v", ao.events())
		}
	}
	if !killedBeforeCreate {
		t.Fatalf("round-0 session was never killed: %v", ao.events())
	}
	if !git.merged["ao/sess-2"] {
		t.Fatal("round-1 branch must be merged after the tier-1 pass")
	}
}

// TestRunTier1BudgetExhausted: with MaxVerifyRounds=1, two tier-1 fails
// must consume the single retry and then fail the task with
// kind=verify_budget_exhausted — the rounds counted from the event log.
func TestRunTier1BudgetExhausted(t *testing.T) {
	st, ao, git, s := newHarnessGit(t, []store.NewTask{ntLLM("a1234567")}, Config{MaxVerifyRounds: 1})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(0)
	ao.scripts[displayNameForRound("plan-1", "a1234567", 1)] = doneScript(0)
	fv := &fakeVerifier{steps: []verifyStep{vFail("still wrong")}}
	s.Verify = fv
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskFailed {
		t.Fatalf("task = %s, want failed", got)
	}
	if got := planStatus(t, st); got != store.PlanFailed {
		t.Fatalf("plan = %s, want failed", got)
	}
	if len(eventPayloads(t, st, store.EventTaskRetry)) != 1 {
		t.Fatal("want exactly 1 task_retry (the budget)")
	}
	failed := eventPayloads(t, st, store.EventTaskFailed)
	if len(failed) != 1 || !strings.Contains(failed[0], `"kind":"verify_budget_exhausted"`) ||
		!strings.Contains(failed[0], `"rounds_used":1`) {
		t.Fatalf("want task_failed kind=verify_budget_exhausted rounds_used=1, got %v", failed)
	}
	if len(git.merged) != 0 {
		t.Fatalf("rejected branches must not be merged: %v", git.merged)
	}
}

// TestRunTier0FailRetries: tier-0 fails (failing check command) must also
// consume the retry budget and re-dispatch with the check output as
// feedback — retry is not tier-1-only.
func TestRunTier0FailRetries(t *testing.T) {
	task := ntLLM("a1234567")
	task.Check = "go test ./..."
	st, ao, _, s := newHarnessGit(t, []store.NewTask{task}, Config{MaxVerifyRounds: 2})
	name1 := displayNameForRound("plan-1", "a1234567", 1)
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(0)
	ao.scripts[name1] = doneScript(0)
	checkRuns := 0
	s.RunCheck = func(_ context.Context, _, _ string) (string, error) {
		checkRuns++
		if checkRuns == 1 {
			return "FAIL: TestX broke", fmt.Errorf("exit status 1")
		}
		return "ok", nil
	}
	s.Verify = &fakeVerifier{steps: []verifyStep{vOK()}}
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskDone {
		t.Fatalf("task = %s, want done", got)
	}
	retries := eventPayloads(t, st, store.EventTaskRetry)
	if len(retries) != 1 || !strings.Contains(retries[0], `"tier":0`) {
		t.Fatalf("want 1 tier-0 task_retry, got %v", retries)
	}
	if p := ao.prompts[name1]; !strings.Contains(p, "TestX broke") {
		t.Fatalf("round-1 prompt must carry the check output as feedback:\n%s", p)
	}
}

// TestRunTier1LLMErrorToleratesTwo: transient LLM failures must NOT fail
// the task or burn the retry budget — the poll retries, and a later
// success proceeds to done.
func TestRunTier1LLMErrorToleratesTwo(t *testing.T) {
	st, ao, _, s := newHarnessGit(t, []store.NewTask{ntLLM("a1234567")}, Config{MaxVerifyRounds: 2})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(0)
	fv := &fakeVerifier{steps: []verifyStep{vErr("429 rate limited"), vErr("timeout"), vOK()}}
	s.Verify = fv
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskDone {
		t.Fatalf("task = %s, want done", got)
	}
	if fv.calls != 3 {
		t.Fatalf("verifier calls = %d, want 3", fv.calls)
	}
	if n := len(eventPayloads(t, st, store.EventTaskRetry)); n != 0 {
		t.Fatalf("LLM errors must not burn the retry budget: %d task_retry events", n)
	}
}

// TestRunTier1LLMErrorThriceFails: three CONSECUTIVE LLM failures fail the
// task with kind=verify_error (not verify_budget_exhausted — the budget is
// for worker fixes, not provider outages).
func TestRunTier1LLMErrorThriceFails(t *testing.T) {
	st, ao, git, s := newHarnessGit(t, []store.NewTask{ntLLM("a1234567")}, Config{MaxVerifyRounds: 2})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(0)
	s.Verify = &fakeVerifier{steps: []verifyStep{vErr("provider down")}}
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskFailed {
		t.Fatalf("task = %s, want failed", got)
	}
	failed := eventPayloads(t, st, store.EventTaskFailed)
	if len(failed) != 1 || !strings.Contains(failed[0], `"kind":"verify_error"`) {
		t.Fatalf("want task_failed kind=verify_error, got %v", failed)
	}
	if len(git.merged) != 0 {
		t.Fatalf("unverified branch must not be merged: %v", git.merged)
	}
}

// TestRunVerifyNotLLMSkipsTier1: a task without verify=llm must never call
// the verifier, even when one is configured.
func TestRunVerifyNotLLMSkipsTier1(t *testing.T) {
	st, ao, _, s := newHarnessGit(t, []store.NewTask{nt("a1234567")}, Config{MaxVerifyRounds: 2})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(0)
	fv := &fakeVerifier{steps: []verifyStep{vFail("must never be consulted")}}
	s.Verify = fv
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskDone {
		t.Fatalf("task = %s, want done", got)
	}
	if fv.calls != 0 {
		t.Fatalf("verifier calls = %d, want 0 for verify!=llm", fv.calls)
	}
}

// TestResumeRetryRoundAdoptsCurrentSession: crash between the round-1
// task_dispatching intent and task_dispatched, with BOTH the old round-0
// session (terminated) and the round-1 session live in AO. Resume must
// adopt the round-1 session — never the round-0 one — and finish.
func TestResumeRetryRoundAdoptsCurrentSession(t *testing.T) {
	st, ao, _, s := newHarnessGit(t, []store.NewTask{ntLLM("a1234567")}, Config{MaxVerifyRounds: 2})
	if err := st.StartRun("plan-1", "run-crashed"); err != nil {
		t.Fatal(err)
	}
	// Round 0 lifecycle: dispatched, tier-1 failed, retried.
	if err := st.MarkTaskDispatching("plan-1", "a1234567", "run-crashed"); err != nil {
		t.Fatal(err)
	}
	old := ao.addSession(displayNameFor("plan-1", "a1234567"), true, nil)
	if err := st.DispatchTask("plan-1", "a1234567", "run-crashed", old, "ao/"+old); err != nil {
		t.Fatal(err)
	}
	if err := st.RetryTask("plan-1", "a1234567", "run-crashed", `{"round":1,"tier":1,"reason":"r","feedback":"f"}`); err != nil {
		t.Fatal(err)
	}
	// Round 1: intent recorded, CreateSession landed, then CRASH.
	if err := st.MarkTaskDispatching("plan-1", "a1234567", "run-crashed"); err != nil {
		t.Fatal(err)
	}
	cur := ao.addSession(displayNameForRound("plan-1", "a1234567", 1), false, doneScript(0))
	s.Verify = &fakeVerifier{steps: []verifyStep{vOK()}}

	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := planStatus(t, st); got != store.PlanDone {
		t.Fatalf("plan = %s, want done", got)
	}
	for _, e := range ao.events() {
		if strings.HasPrefix(e, "create:") {
			t.Fatalf("resume must adopt the round-1 session, not create: %v", ao.events())
		}
	}
	tasks, err := st.GetTasks("plan-1")
	if err != nil {
		t.Fatal(err)
	}
	if tasks[0].AOSessionID == nil || *tasks[0].AOSessionID != cur {
		t.Fatalf("task adopted %v, want the round-1 session %s", tasks[0].AOSessionID, cur)
	}
}

// TestPromptWithFeedbackBudget: a max-size planner prompt plus huge
// feedback must never exceed AO's 4096-byte cap, and the protocol footer
// must survive untruncated at the end.
func TestPromptWithFeedbackBudget(t *testing.T) {
	marker := markerPathFor("plan-1", "a1234567")
	p := promptWithFeedbackAndFooter(strings.Repeat("x", 3500), strings.Repeat("f", 10000), marker)
	if len(p) > maxSpawnPrompt {
		t.Fatalf("prompt = %d bytes, exceeds %d", len(p), maxSpawnPrompt)
	}
	if !strings.HasSuffix(p, fmt.Sprintf(promptFooterFmt, marker)) {
		t.Fatal("footer must be intact at the end of the prompt")
	}
	// No feedback: byte-identical to the plain footer path.
	if promptWithFeedbackAndFooter("do it", "", marker) != promptWithFooter("do it", marker) {
		t.Fatal("empty feedback must not alter the prompt")
	}
}
