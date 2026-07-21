package scheduler

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/OmniMintX/overmind/internal/aoclient"
	"github.com/OmniMintX/overmind/internal/store"
)

// doneScript is the happy path: one working poll, then idle with an
// "ok" marker (and optionally an open PR, only recorded as pr_url).
func doneScript(pr int) []sessStep {
	step := stepMarker(aoclient.StatusIdle, "ok: finished the work")
	step.pr = pr
	return []sessStep{{status: aoclient.StatusWorking}, step}
}

// newHarness builds an in-memory store with one approved plan and a
// scheduler wired to the fake AO, fake git, and fake clock.
func newHarness(t *testing.T, tasks []store.NewTask, cfg Config) (*store.Store, *fakeAO, *Scheduler) {
	st, ao, git, s := newHarnessGit(t, tasks, cfg)
	_ = git
	return st, ao, s
}

func newHarnessGit(t *testing.T, tasks []store.NewTask, cfg Config) (*store.Store, *fakeAO, *fakeGit, *Scheduler) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.CreatePlan("plan-1", "test goal", "proj-1", tasks); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if err := st.ApprovePlan("plan-1"); err != nil {
		t.Fatalf("approve plan: %v", err)
	}
	ao := newFakeAO()
	git := newFakeGit(ao)
	clock := newFakeClock()
	s := &Scheduler{St: st, AO: ao, Git: git, Cfg: cfg, PID: 42, Now: clock.Now, Sleep: clock.Sleep}
	return st, ao, git, s
}

func taskStatuses(t *testing.T, st *store.Store) map[string]string {
	t.Helper()
	ds, err := st.PlanState("plan-1")
	if err != nil {
		t.Fatalf("plan state: %v", err)
	}
	return ds.TaskStatus
}

func planStatus(t *testing.T, st *store.Store) string {
	t.Helper()
	ds, err := st.PlanState("plan-1")
	if err != nil {
		t.Fatalf("plan state: %v", err)
	}
	return ds.PlanStatus
}

func nt(id string, deps ...string) store.NewTask {
	return store.NewTask{ID: id, Title: "task " + id, Prompt: "do " + id, Harness: "claude-code", DependsOn: deps}
}

// TestRunDAGOrder: chain a -> b -> c must dispatch strictly in dependency
// order, and each parent's session branch must be LOCALLY merged into the
// default branch BEFORE its child's session is created.
func TestRunDAGOrder(t *testing.T) {
	st, ao, s := newHarness(t, []store.NewTask{
		nt("a1234567"), nt("b1234567", "a1234567"), nt("c1234567", "b1234567"),
	}, Config{})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(0)
	ao.scripts[displayNameFor("plan-1", "b1234567")] = doneScript(0)
	ao.scripts[displayNameFor("plan-1", "c1234567")] = doneScript(0)

	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := planStatus(t, st); got != store.PlanDone {
		t.Fatalf("plan status = %s, want done", got)
	}
	for id, status := range taskStatuses(t, st) {
		if status != store.TaskDone {
			t.Errorf("task %s = %s, want done", id, status)
		}
	}
	var order []string
	for _, e := range ao.events() {
		if strings.HasPrefix(e, "create:") || strings.HasPrefix(e, "merge:") {
			order = append(order, e)
		}
	}
	// Sessions are created in dispatch order sess-1..sess-3; each branch
	// merge must land before the next create.
	want := []string{
		"create:" + displayNameFor("plan-1", "a1234567"), "merge:ao/sess-1",
		"create:" + displayNameFor("plan-1", "b1234567"), "merge:ao/sess-2",
		"create:" + displayNameFor("plan-1", "c1234567"), "merge:ao/sess-3",
	}
	if len(order) != len(want) {
		t.Fatalf("event order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("event[%d] = %s, want %s (full: %v)", i, order[i], want[i], order)
		}
	}
	// The merge audit event must be recorded.
	events, err := st.ListEvents("plan-1")
	if err != nil {
		t.Fatal(err)
	}
	merged := 0
	for _, e := range events {
		if e.Type == store.EventTaskBranchMerged {
			merged++
		}
	}
	if merged != 3 {
		t.Fatalf("task_branch_merged events = %d, want 3", merged)
	}
}

// TestRunMaxParallel: 4 independent tasks with MaxParallel=2 must never
// have more than 2 live AO sessions at once.
func TestRunMaxParallel(t *testing.T) {
	tasks := []store.NewTask{nt("a1234567"), nt("b1234567"), nt("c1234567"), nt("d1234567")}
	st, ao, s := newHarness(t, tasks, Config{MaxParallel: 2})
	for _, tk := range tasks {
		ao.scripts[displayNameFor("plan-1", tk.ID)] = doneScript(0)
	}
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := planStatus(t, st); got != store.PlanDone {
		t.Fatalf("plan status = %s, want done", got)
	}
	if ao.maxActive > 2 {
		t.Fatalf("max concurrent sessions = %d, want <= 2", ao.maxActive)
	}
}

// TestRunEmptyPlanIsDone: a plan with zero tasks finishes immediately.
func TestRunEmptyPlanIsDone(t *testing.T) {
	st, _, s := newHarness(t, nil, Config{})
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := planStatus(t, st); got != store.PlanDone {
		t.Fatalf("plan status = %s, want done", got)
	}
}

// TestDisplayNameFor: the marker must be deterministic, plan-scoped (same
// task id in different plans must NOT collide — planner ids are t1..tN),
// and within AO's 20-rune displayName cap.
func TestDisplayNameFor(t *testing.T) {
	a := displayNameFor("p-aaaa1111", "t1")
	b := displayNameFor("p-bbbb2222", "t1")
	if a == b {
		t.Fatalf("same task id across plans collided: %s", a)
	}
	if a != displayNameFor("p-aaaa1111", "t1") {
		t.Fatalf("displayNameFor is not deterministic")
	}
	if c := displayNameFor("p-aaaa1111", "t2"); c == a {
		t.Fatalf("different task ids in one plan collided: %s", c)
	}
	for _, n := range []string{a, b} {
		if !strings.HasPrefix(n, "om-") {
			t.Fatalf("displayName %q must start with om-", n)
		}
		if len(n) != len("om-")+8 {
			t.Fatalf("displayName %q length = %d, want %d", n, len(n), len("om-")+8)
		}
	}
}

// TestMarkerPathPerTask: the marker path must be per-task, share the
// displayName hash, and differ across plans and tasks.
func TestMarkerPathPerTask(t *testing.T) {
	m := markerPathFor("p-aaaa1111", "t1")
	if m != ".om-done."+strings.TrimPrefix(displayNameFor("p-aaaa1111", "t1"), "om-") {
		t.Fatalf("marker %q must reuse the displayName hash", m)
	}
	if m == markerPathFor("p-bbbb2222", "t1") || m == markerPathFor("p-aaaa1111", "t2") {
		t.Fatal("marker paths must be unique per (plan, task)")
	}
}

// TestParseMarker covers the tolerant first-line parser.
func TestParseMarker(t *testing.T) {
	cases := []struct {
		in      string
		verdict markerVerdict
		detail  string
	}{
		{"ok", markerOK, ""},
		{"ok: all done", markerOK, "all done"},
		{"OK: caps", markerOK, "caps"},
		{"Ok done without colon", markerOK, "done without colon"},
		{"fail: broke", markerFail, "broke"},
		{"FAIL", markerFail, ""},
		{"\ufeffok: bom", markerOK, "bom"},
		{"\r\nok: crlf\r\n", markerOK, "crlf"},
		{"\n\n  ok: blank lines first", markerOK, "blank lines first"},
		{"ok: first\nsecond line detail", markerOK, "first"},
		{"", markerEmpty, ""},
		{"   \n \n", markerEmpty, ""},
		{"okay so this looks done", markerMalformed, ""},
		{"failure notes", markerMalformed, ""},
		{"```\nok\n```", markerMalformed, ""},
		{"done!", markerMalformed, ""},
	}
	for _, c := range cases {
		v, d := parseMarker(c.in)
		if v != c.verdict {
			t.Errorf("parseMarker(%q) verdict = %v, want %v", c.in, v, c.verdict)
		}
		if c.verdict == markerOK || c.verdict == markerFail {
			if d != c.detail {
				t.Errorf("parseMarker(%q) detail = %q, want %q", c.in, d, c.detail)
			}
		}
	}
}

// TestDispatchAppendsFooter: the spawn prompt must contain the exact
// per-task marker path, and a max-size planner prompt plus footer must
// stay within AO's 4096-byte limit.
func TestDispatchAppendsFooter(t *testing.T) {
	_, ao, s := newHarness(t, []store.NewTask{nt("a1234567")}, Config{})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(0)
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	prompt := ao.prompts[displayNameFor("plan-1", "a1234567")]
	markerPath := markerPathFor("plan-1", "a1234567")
	if !strings.Contains(prompt, markerPath) {
		t.Fatalf("spawn prompt missing marker path %s", markerPath)
	}
	if !strings.Contains(prompt, "fail:") || !strings.Contains(prompt, "Do NOT commit") {
		t.Fatalf("footer incomplete: %s", prompt)
	}
	full := promptWithFooter(strings.Repeat("x", 3500), markerPath)
	if footer := len(full) - 3500; footer > 550 {
		t.Fatalf("footer is %d bytes, budget 550", footer)
	}
	if len(full) > 4096 {
		t.Fatalf("max prompt + footer = %d bytes, exceeds AO's 4096 cap", len(full))
	}
}

// TestRunMarkerFail: an agent that honestly reports failure must fail the
// task with kind=marker_fail, and its branch must NOT be merged.
func TestRunMarkerFail(t *testing.T) {
	st, ao, git, s := newHarnessGit(t, []store.NewTask{nt("a1234567")}, Config{})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = []sessStep{
		{status: aoclient.StatusWorking},
		stepMarker(aoclient.StatusIdle, "fail: could not find greeting.txt"),
	}
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskFailed {
		t.Fatalf("task = %s, want failed", got)
	}
	if len(git.merged) != 0 {
		t.Fatalf("failed task's branch must not be merged: %v", git.merged)
	}
	events, _ := st.ListEvents("plan-1")
	found := false
	for _, e := range events {
		if e.Type == store.EventTaskFailed &&
			strings.Contains(e.PayloadJSON, `"kind":"marker_fail"`) &&
			strings.Contains(e.PayloadJSON, "greeting.txt") {
			found = true
		}
	}
	if !found {
		t.Fatal("task_failed payload must carry kind=marker_fail and the marker content")
	}
	killed := false
	for _, e := range ao.events() {
		if strings.HasPrefix(e, "kill:") {
			killed = true
		}
	}
	if !killed {
		t.Fatal("marker_fail session must be killed")
	}
}

// TestRunMarkerEmptyWaits: an existing-but-empty marker means "still
// writing" — the task must not fail on that poll and finishes when the
// content lands.
func TestRunMarkerEmptyWaits(t *testing.T) {
	st, ao, s := newHarness(t, []store.NewTask{nt("a1234567")}, Config{})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = []sessStep{
		{status: aoclient.StatusWorking},
		stepMarker(aoclient.StatusIdle, ""),
		stepMarker(aoclient.StatusIdle, "ok: now complete"),
	}
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskDone {
		t.Fatalf("task = %s, want done", got)
	}
}

// TestRunMarkerMalformedGrace: one malformed poll gets grace (agent may be
// mid-write); the second consecutive malformed poll fails the task.
func TestRunMarkerMalformedGrace(t *testing.T) {
	st, ao, s := newHarness(t, []store.NewTask{nt("a1234567")}, Config{})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = []sessStep{
		{status: aoclient.StatusWorking},
		stepMarker(aoclient.StatusIdle, "gibberish first line"),
	}
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskFailed {
		t.Fatalf("task = %s, want failed", got)
	}
	events, _ := st.ListEvents("plan-1")
	for _, e := range events {
		if e.Type == store.EventTaskFailed && !strings.Contains(e.PayloadJSON, "marker_malformed") {
			t.Fatalf("want kind=marker_malformed, got %s", e.PayloadJSON)
		}
	}
}

// TestRunMergeConflict: a real merge conflict fails the task
// deterministically (kind=merge_conflict) and the plan fails.
func TestRunMergeConflict(t *testing.T) {
	st, ao, git, s := newHarnessGit(t, []store.NewTask{nt("a1234567")}, Config{})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(0)
	git.conflicts["ao/sess-1"] = true
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskFailed {
		t.Fatalf("task = %s, want failed", got)
	}
	events, _ := st.ListEvents("plan-1")
	found := false
	for _, e := range events {
		if e.Type == store.EventTaskFailed && strings.Contains(e.PayloadJSON, "merge_conflict") {
			found = true
		}
	}
	if !found {
		t.Fatal("want task_failed kind=merge_conflict")
	}
}

// TestRunMergeBlockedRetries: a transiently blocked merge (dirty user
// checkout) must NOT fail or time out the task; it retries and completes,
// recording exactly one merge_blocked event per streak.
func TestRunMergeBlockedRetries(t *testing.T) {
	st, ao, git, s := newHarnessGit(t, []store.NewTask{nt("a1234567")},
		Config{IdleNoMarkerTimeout: time.Minute, PollInterval: 15 * time.Second})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(0)
	git.blocked = 6 // 6 polls * 15s = 90s > IdleNoMarkerTimeout if clock ran
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskDone {
		t.Fatalf("task = %s, want done (blocked merges must retry)", got)
	}
	events, _ := st.ListEvents("plan-1")
	blockedEvents := 0
	for _, e := range events {
		if e.Type == store.EventMergeBlocked {
			blockedEvents++
		}
		if e.Type == store.EventTaskFailed {
			t.Fatalf("blocked merge must never fail the task: %s", e.PayloadJSON)
		}
	}
	if blockedEvents != 1 {
		t.Fatalf("merge_blocked events = %d, want 1 per streak", blockedEvents)
	}
}

// TestRunPrecheckOriginRefusal: a repo with origin/<default> must be
// refused at startup (Phase 1 supports remoteless repos only).
func TestRunPrecheckOriginRefusal(t *testing.T) {
	st, _, git, s := newHarnessGit(t, []store.NewTask{nt("a1234567")}, Config{})
	git.hasOrigin = true
	err := s.Run(context.Background(), "plan-1")
	if err == nil || !strings.Contains(err.Error(), "origin/main") {
		t.Fatalf("want origin precheck refusal, got %v", err)
	}
	if got := planStatus(t, st); got != store.PlanApproved {
		t.Fatalf("plan status after refusal = %s, want approved (untouched)", got)
	}
}

// TestRunVerifyEmptyDiffFails: tier 0 must fail a task whose branch has no
// diff vs the base AND no uncommitted work — with task_verdict(fail,
// tier=0), kind=verify_budget_exhausted (budget 0), and NO merge.
func TestRunVerifyEmptyDiffFails(t *testing.T) {
	st, ao, git, s := newHarnessGit(t, []store.NewTask{nt("a1234567")}, Config{})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(0)
	git.emptyDiff["ao/sess-1"] = true
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskFailed {
		t.Fatalf("task = %s, want failed", got)
	}
	if len(git.merged) != 0 {
		t.Fatalf("empty-diff branch must not be merged: %v", git.merged)
	}
	events, _ := st.ListEvents("plan-1")
	verdict, failed := "", ""
	for _, e := range events {
		switch e.Type {
		case store.EventTaskVerdict:
			verdict = e.PayloadJSON
		case store.EventTaskFailed:
			failed = e.PayloadJSON
		}
	}
	if !strings.Contains(verdict, `"verdict":"fail"`) || !strings.Contains(verdict, `"tier":0`) {
		t.Fatalf("want task_verdict fail tier 0, got %q", verdict)
	}
	if !strings.Contains(failed, `"kind":"verify_budget_exhausted"`) || !strings.Contains(failed, "empty diff") {
		t.Fatalf("want task_failed kind=verify_budget_exhausted with empty-diff reason, got %q", failed)
	}
}

// TestRunSystemCommitRescuesUncommitted: work the agent forgot to commit
// must pass tier 0 (uncommitted counts as work), be system-committed
// BEFORE the merge, and be audited as task_system_commit.
func TestRunSystemCommitRescuesUncommitted(t *testing.T) {
	st, ao, git, s := newHarnessGit(t, []store.NewTask{nt("a1234567")}, Config{})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(0)
	git.emptyDiff["ao/sess-1"] = true
	git.uncommitted["ao/sess-1"] = true
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskDone {
		t.Fatalf("task = %s, want done", got)
	}
	var order []string
	for _, e := range ao.events() {
		if strings.HasPrefix(e, "syscommit:") || strings.HasPrefix(e, "merge:") {
			order = append(order, e)
		}
	}
	want := []string{"syscommit:ao/sess-1", "merge:ao/sess-1"}
	if len(order) != 2 || order[0] != want[0] || order[1] != want[1] {
		t.Fatalf("system-commit must precede merge, got %v", order)
	}
	events, _ := st.ListEvents("plan-1")
	var sysCommit, verdict string
	for _, e := range events {
		switch e.Type {
		case store.EventTaskSystemCommit:
			sysCommit = e.PayloadJSON
		case store.EventTaskVerdict:
			verdict = e.PayloadJSON
		}
	}
	if !strings.Contains(sysCommit, `"branch":"ao/sess-1"`) || !strings.Contains(sysCommit, `"sha":`) {
		t.Fatalf("want task_system_commit {branch, sha, files}, got %q", sysCommit)
	}
	if !strings.Contains(verdict, `"verdict":"pass"`) {
		t.Fatalf("want task_verdict pass, got %q", verdict)
	}
}

// TestRunCheckCommandPasses: the planner's per-task check command must run
// in the session worktree; exit 0 lets the pipeline continue to done.
func TestRunCheckCommandPasses(t *testing.T) {
	task := nt("a1234567")
	task.Check = "test -s reply.txt"
	st, ao, _, s := newHarnessGit(t, []store.NewTask{task}, Config{})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(0)
	var gotDir, gotCmd string
	s.RunCheck = func(_ context.Context, dir, command string) (string, error) {
		gotDir, gotCmd = dir, command
		return "ok output", nil
	}
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskDone {
		t.Fatalf("task = %s, want done", got)
	}
	if gotCmd != "test -s reply.txt" || gotDir != fakeWorktreeRoot+"ao/sess-1" {
		t.Fatalf("check ran with dir=%q cmd=%q; want worktree dir + planner command", gotDir, gotCmd)
	}
}

// TestRunCheckCommandFails: a failing check command must fail the task at
// tier 0 (budget 0: kind=verify_budget_exhausted) with the check output in
// the payload and no merge.
func TestRunCheckCommandFails(t *testing.T) {
	task := nt("a1234567")
	task.Check = "go test ./..."
	st, ao, git, s := newHarnessGit(t, []store.NewTask{task}, Config{})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(0)
	s.RunCheck = func(_ context.Context, _, _ string) (string, error) {
		return "FAIL: TestX broke", fmt.Errorf("exit status 1")
	}
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskFailed {
		t.Fatalf("task = %s, want failed", got)
	}
	if len(git.merged) != 0 {
		t.Fatalf("check-failed branch must not be merged: %v", git.merged)
	}
	events, _ := st.ListEvents("plan-1")
	found := false
	for _, e := range events {
		if e.Type == store.EventTaskFailed &&
			strings.Contains(e.PayloadJSON, `"kind":"verify_budget_exhausted"`) &&
			strings.Contains(e.PayloadJSON, "TestX broke") {
			found = true
		}
	}
	if !found {
		t.Fatal("task_failed payload must carry kind=verify_budget_exhausted and the check output")
	}
}

// TestRunVerifyOnceAcrossBlockedMerges: a transiently blocked merge must
// NOT re-run tier-0 verify or the system-commit on every retry poll.
func TestRunVerifyOnceAcrossBlockedMerges(t *testing.T) {
	task := nt("a1234567")
	task.Check = "true"
	st, ao, git, s := newHarnessGit(t, []store.NewTask{task}, Config{})
	ao.scripts[displayNameFor("plan-1", "a1234567")] = doneScript(0)
	git.blocked = 3
	checkRuns := 0
	s.RunCheck = func(_ context.Context, _, _ string) (string, error) {
		checkRuns++
		return "", nil
	}
	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := taskStatuses(t, st)["a1234567"]; got != store.TaskDone {
		t.Fatalf("task = %s, want done", got)
	}
	if checkRuns != 1 {
		t.Fatalf("check command ran %d times, want 1 (verify must not repeat per blocked poll)", checkRuns)
	}
	events, _ := st.ListEvents("plan-1")
	verdicts := 0
	for _, e := range events {
		if e.Type == store.EventTaskVerdict {
			verdicts++
		}
	}
	if verdicts != 1 {
		t.Fatalf("task_verdict events = %d, want 1", verdicts)
	}
}

// TestRunResumeRemergesParent: crash after parent done but before its
// branch reached main (simulated by clearing fake git state) — the
// ancestry re-check in ensureParentsMerged must merge the parent before
// dispatching the child.
func TestRunResumeRemergesParent(t *testing.T) {
	st, ao, git, s := newHarnessGit(t, []store.NewTask{
		nt("a1234567"), nt("b1234567", "a1234567"),
	}, Config{})
	// Simulate the pre-crash world: parent already done in the DB with a
	// recorded branch, but its branch never reached main.
	sessID := ao.addSession(displayNameFor("plan-1", "a1234567"), true, nil)
	if err := st.StartRun("plan-1", "run-crashed"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTaskDispatching("plan-1", "a1234567", "run-crashed"); err != nil {
		t.Fatal(err)
	}
	if err := st.DispatchTask("plan-1", "a1234567", "run-crashed", sessID, "ao/"+sessID); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishTask("plan-1", "a1234567", "run-crashed", "", ""); err != nil {
		t.Fatal(err)
	}
	ao.scripts[displayNameFor("plan-1", "b1234567")] = doneScript(0)

	if err := s.Run(context.Background(), "plan-1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := planStatus(t, st); got != store.PlanDone {
		t.Fatalf("plan status = %s, want done", got)
	}
	if !git.merged["ao/"+sessID] {
		t.Fatal("parent branch must be re-merged on resume before child dispatch")
	}
}


