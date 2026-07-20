package store

import (
	"strings"
	"testing"
)

func nt(id string, deps ...string) NewTask {
	return NewTask{ID: id, Title: id, Prompt: "p", Harness: "claude-code", DependsOn: deps}
}

func TestValidateDAG(t *testing.T) {
	cases := []struct {
		name    string
		tasks   []NewTask
		wantErr string
	}{
		{"empty", nil, ""},
		{"linear", []NewTask{nt("a"), nt("b", "a"), nt("c", "b")}, ""},
		{"diamond", []NewTask{nt("a"), nt("b", "a"), nt("c", "a"), nt("d", "b", "c")}, ""},
		{"self dep", []NewTask{nt("a", "a")}, "depends on itself"},
		{"two cycle", []NewTask{nt("a", "b"), nt("b", "a")}, "cycle detected"},
		{"long cycle", []NewTask{nt("a", "c"), nt("b", "a"), nt("c", "b")}, "cycle detected"},
		{"unknown dep", []NewTask{nt("a", "ghost")}, "not in this plan"},
		{"duplicate edge", []NewTask{nt("a"), nt("b", "a", "a")}, "duplicate edge"},
		{"duplicate task id", []NewTask{nt("a"), nt("a")}, "duplicate task id"},
		{"empty id", []NewTask{{Title: "x"}}, "empty id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDAG(tc.tasks)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected ok, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestCreatePlanRejectsCycle(t *testing.T) {
	s := openTestStore(t)
	err := s.CreatePlan("p1", "goal", "proj", []NewTask{nt("a", "b"), nt("b", "a")})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
	// Rejected plan must leave no rows behind.
	if _, err := s.GetPlan("p1"); err == nil {
		t.Fatal("plan should not exist after rejected DAG")
	}
}

func TestSchemaConstraintsOnDependencies(t *testing.T) {
	s := openTestStore(t)
	mustCreatePlan(t, s, "p1", nt("a"), nt("b", "a"))
	mustCreatePlan(t, s, "p2", nt("x"))

	// Self-dependency blocked by CHECK.
	if _, err := s.db.Exec(
		`INSERT INTO task_dependencies (plan_id, task_id, depends_on_task_id) VALUES ('p1','a','a')`); err == nil {
		t.Fatal("self-dependency insert should fail")
	}
	// Duplicate edge blocked by PK.
	if _, err := s.db.Exec(
		`INSERT INTO task_dependencies (plan_id, task_id, depends_on_task_id) VALUES ('p1','b','a')`); err == nil {
		t.Fatal("duplicate edge insert should fail")
	}
	// Cross-plan edge blocked by composite FK.
	if _, err := s.db.Exec(
		`INSERT INTO task_dependencies (plan_id, task_id, depends_on_task_id) VALUES ('p1','a','x')`); err == nil {
		t.Fatal("cross-plan edge insert should fail")
	}
}
