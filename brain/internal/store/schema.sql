-- Overmind brain store schema (v1).
--
-- Event sourcing discipline:
--   * brain_events is APPEND-ONLY (never UPDATE/DELETE).
--   * Every state change = ONE transaction: INSERT brain_event + UPDATE cache column.
--   * status columns on plans/tasks are display caches; the source of truth is
--     derive(brain_events) — see PlanState().
-- Phase 1: a failed plan is TERMINAL. To retry, create a new plan.

CREATE TABLE IF NOT EXISTS plans (
    id                    TEXT PRIMARY KEY,
    goal                  TEXT NOT NULL,
    project_id            TEXT NOT NULL,
    status                TEXT NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft','approved','running','done','failed','cancelled')),
    run_lock_pid          INTEGER,
    run_lock_heartbeat_at TEXT,
    created_at            TEXT NOT NULL,
    approved_at           TEXT
);

-- Task ids are PLAN-SCOPED (the planner reuses t1..tN in every plan), so the
-- primary key must be composite. Databases created when `id` alone was the
-- PK are rebuilt by migrateTasksPK (store.go).
-- check_cmd is the planner-generated tier-0 verification command (may be
-- empty). Older databases get the column via migrateTasksCheck (store.go).
CREATE TABLE IF NOT EXISTS tasks (
    id            TEXT NOT NULL,
    plan_id       TEXT NOT NULL REFERENCES plans(id),
    title         TEXT NOT NULL,
    prompt        TEXT NOT NULL,
    harness       TEXT NOT NULL,
    check_cmd     TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','ready','dispatching','dispatched','running','needs_human','done','failed')),
    ao_session_id TEXT,
    branch        TEXT,
    pr_url        TEXT,
    created_at    TEXT NOT NULL,
    PRIMARY KEY (id, plan_id)
);

CREATE INDEX IF NOT EXISTS idx_tasks_plan ON tasks(plan_id);

-- Both endpoints must belong to the same plan (composite FKs forbid
-- cross-plan edges); plan-scoped PK forbids duplicate edges without
-- colliding across plans; CHECK forbids self-dependency. Global acyclicity
-- is validated by topo-sort in Go when the plan is saved (CreatePlan).
CREATE TABLE IF NOT EXISTS task_dependencies (
    plan_id            TEXT NOT NULL,
    task_id            TEXT NOT NULL,
    depends_on_task_id TEXT NOT NULL,
    PRIMARY KEY (plan_id, task_id, depends_on_task_id),
    CHECK (task_id <> depends_on_task_id),
    FOREIGN KEY (task_id, plan_id) REFERENCES tasks(id, plan_id),
    FOREIGN KEY (depends_on_task_id, plan_id) REFERENCES tasks(id, plan_id)
);

CREATE INDEX IF NOT EXISTS idx_deps_plan ON task_dependencies(plan_id);

CREATE TABLE IF NOT EXISTS brain_events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    plan_id      TEXT NOT NULL REFERENCES plans(id),
    task_id      TEXT,
    run_id       TEXT NOT NULL DEFAULT '',
    type         TEXT NOT NULL,
    payload_json TEXT NOT NULL DEFAULT '{}',
    created_at   TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_plan ON brain_events(plan_id, id);

CREATE TRIGGER IF NOT EXISTS brain_events_no_update
BEFORE UPDATE ON brain_events
BEGIN SELECT RAISE(ABORT, 'brain_events is append-only'); END;

CREATE TRIGGER IF NOT EXISTS brain_events_no_delete
BEFORE DELETE ON brain_events
BEGIN SELECT RAISE(ABORT, 'brain_events is append-only'); END;
