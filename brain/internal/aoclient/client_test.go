package aoclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient wires a Client to an httptest server handler.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(srv.URL)
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func writeEnvelope(t *testing.T, w http.ResponseWriter, status int, kind, code, message string) {
	t.Helper()
	writeJSON(t, w, status, map[string]any{
		"error":     kind,
		"code":      code,
		"message":   message,
		"requestId": "req-1",
	})
}

func TestListProjects(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/projects" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"projects": []map[string]any{{
				"id": "omni", "name": "OmniMintX", "path": "/repo",
				"kind": "repo", "sessionPrefix": "omni",
			}},
		})
	})
	projects, err := c.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 || projects[0].ID != "omni" || projects[0].Path != "/repo" {
		t.Fatalf("unexpected projects: %+v", projects)
	}
}

func TestAddProject(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/projects" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["path"] != "/repo" {
			t.Fatalf("unexpected body: %v", body)
		}
		if _, ok := body["projectId"]; ok {
			t.Fatalf("empty optional field must be omitted: %v", body)
		}
		writeJSON(t, w, http.StatusCreated, map[string]any{
			"project": map[string]any{
				"id": "omni", "name": "OmniMintX", "kind": "repo",
				"path": "/repo", "repo": "org/omni", "defaultBranch": "main",
			},
		})
	})
	p, err := c.AddProject(context.Background(), AddProjectInput{Path: "/repo"})
	if err != nil {
		t.Fatalf("AddProject: %v", err)
	}
	if p.ID != "omni" || p.DefaultBranch != "main" {
		t.Fatalf("unexpected project: %+v", p)
	}
}

func TestAddProjectRequiresPath(t *testing.T) {
	c := New("")
	if _, err := c.AddProject(context.Background(), AddProjectInput{}); err == nil {
		t.Fatal("want error for missing path")
	}
}

func TestListAgents(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/agents" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		// The inventory is the response body itself — no wrapper key.
		writeJSON(t, w, http.StatusOK, map[string]any{
			"supported": []map[string]any{
				{"id": "claude-code", "label": "Claude Code"},
				{"id": "codex", "label": "Codex"},
			},
			"installed": []map[string]any{
				{"id": "claude-code", "label": "Claude Code", "authStatus": "authorized"},
			},
			"authorized": []map[string]any{
				{"id": "claude-code", "label": "Claude Code", "authStatus": "authorized"},
			},
		})
	})
	inv, err := c.ListAgents(context.Background())
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(inv.Supported) != 2 || len(inv.Installed) != 1 || len(inv.Authorized) != 1 {
		t.Fatalf("unexpected inventory: %+v", inv)
	}
	if inv.Installed[0].ID != "claude-code" || inv.Installed[0].AuthStatus != "authorized" {
		t.Fatalf("unexpected installed agent: %+v", inv.Installed[0])
	}
}

func TestCreateSession(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sessions" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["projectId"] != "omni" || body["harness"] != "claude-code" ||
			body["prompt"] != "do the thing" || body["displayName"] != "task-42" {
			t.Fatalf("unexpected body: %v", body)
		}
		writeJSON(t, w, http.StatusCreated, map[string]any{
			"session": map[string]any{
				"id": "omni-1", "projectId": "omni", "kind": "worker",
				"harness": "claude-code", "displayName": "task-42",
				"status": "no_signal", "isTerminated": false,
				"activity": map[string]any{"state": "active"},
				"prs":      []any{},
			},
		})
	})
	s, err := c.CreateSession(context.Background(), SpawnSessionRequest{
		ProjectID:   "omni",
		Harness:     HarnessClaudeCode,
		Prompt:      "do the thing",
		DisplayName: "task-42",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if s.ID != "omni-1" || s.Status != StatusNoSignal || s.Harness != HarnessClaudeCode {
		t.Fatalf("unexpected session: %+v", s)
	}
}

func TestCreateSessionValidation(t *testing.T) {
	c := New("")
	ctx := context.Background()
	if _, err := c.CreateSession(ctx, SpawnSessionRequest{}); err == nil {
		t.Fatal("want error for missing projectId")
	}
	long := strings.Repeat("a", MaxPromptLen+1)
	if _, err := c.CreateSession(ctx, SpawnSessionRequest{ProjectID: "p", Prompt: long}); err == nil {
		t.Fatal("want error for long prompt")
	}
	name := strings.Repeat("á", MaxDisplayNameLen+1) // rune count, not bytes
	if _, err := c.CreateSession(ctx, SpawnSessionRequest{ProjectID: "p", DisplayName: name}); err == nil {
		t.Fatal("want error for long displayName")
	}
	// Exactly MaxDisplayNameLen multibyte runes must pass client validation
	// even though the byte length exceeds the cap (rune-count check).
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	closed := New(srv.URL)
	okName := strings.Repeat("á", MaxDisplayNameLen)
	_, err := closed.CreateSession(ctx, SpawnSessionRequest{ProjectID: "p", DisplayName: okName})
	if !errors.Is(err, ErrDaemonNotRunning) {
		t.Fatalf("want transport error past validation, got %v", err)
	}
}

func TestGetSession(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/sessions/omni-1" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"session": map[string]any{
				"id": "omni-1", "projectId": "omni", "kind": "worker",
				"status": "needs_input", "isTerminated": false,
				"activity": map[string]any{"state": "waiting_input"},
				"branch":   "ao/omni-1", "prs": []any{},
			},
		})
	})
	s, err := c.GetSession(context.Background(), "omni-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if s.Status != StatusNeedsInput || s.Branch != "ao/omni-1" || s.Activity.State != "waiting_input" {
		t.Fatalf("unexpected session: %+v", s)
	}
}

func TestGetSessionNotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, http.StatusNotFound, "not_found", "SESSION_NOT_FOUND", "Unknown session")
	})
	_, err := c.GetSession(context.Background(), "nope")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if apiErr.HTTPStatus != http.StatusNotFound || apiErr.Code != "SESSION_NOT_FOUND" || apiErr.RequestID != "req-1" {
		t.Fatalf("unexpected APIError: %+v", apiErr)
	}
}

func TestListSessionsFilter(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("project") != "omni" || q.Get("active") != "true" {
			t.Fatalf("unexpected query: %s", r.URL.RawQuery)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"sessions": []map[string]any{
				{"id": "omni-1", "projectId": "omni", "kind": "worker", "status": "working", "prs": []any{}},
				{"id": "omni-2", "projectId": "omni", "kind": "worker", "status": "mergeable", "prs": []any{}},
			},
		})
	})
	active := true
	sessions, err := c.ListSessions(context.Background(), ListSessionsFilter{Project: "omni", Active: &active})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 || sessions[0].Status != StatusWorking || sessions[1].Status != StatusMergeable {
		t.Fatalf("unexpected sessions: %+v", sessions)
	}
}

func TestSendMessage(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sessions/omni-1/send" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["message"] != "continue please" {
			t.Fatalf("unexpected body: %v", body)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"ok": true, "sessionId": "omni-1", "message": "continue please",
		})
	})
	res, err := c.SendMessage(context.Background(), "omni-1", "continue please")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if !res.OK || res.SessionID != "omni-1" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestSendMessageValidation(t *testing.T) {
	c := New("")
	ctx := context.Background()
	if _, err := c.SendMessage(ctx, "omni-1", ""); err == nil {
		t.Fatal("want error for empty message")
	}
	if _, err := c.SendMessage(ctx, "omni-1", strings.Repeat("a", MaxMessageLen+1)); err == nil {
		t.Fatal("want error for long message")
	}
}

func TestSendMessageGuardErrors(t *testing.T) {
	cases := []struct {
		code     string
		sentinel error
	}{
		{"SESSION_TERMINATED", ErrTerminated},
		{"SESSION_AWAITING_DECISION", ErrAwaitingDecision},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				writeEnvelope(t, w, http.StatusConflict, "conflict", tc.code, "guard")
			})
			_, err := c.SendMessage(context.Background(), "omni-1", "hi")
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("want errors.Is(err, %v), got %v", tc.sentinel, err)
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Code != tc.code {
				t.Fatalf("want *APIError with code %s, got %v", tc.code, err)
			}
		})
	}
}

func TestKillSession(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sessions/omni-1/kill" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{"ok": true, "sessionId": "omni-1", "freed": true})
	})
	res, err := c.KillSession(context.Background(), "omni-1")
	if err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	if !res.OK || !res.Freed {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestMergePR(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/prs/123/merge" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{"ok": true, "prNumber": 123, "method": "squash"})
	})
	res, err := c.MergePR(context.Background(), "123")
	if err != nil {
		t.Fatalf("MergePR: %v", err)
	}
	if !res.OK || res.PRNumber != 123 || res.Method != "squash" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestMergePRNotMergeable(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, http.StatusConflict, "conflict", "PR_NOT_MERGEABLE", "PR is not mergeable")
	})
	_, err := c.MergePR(context.Background(), "123")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "PR_NOT_MERGEABLE" {
		t.Fatalf("want PR_NOT_MERGEABLE APIError, got %v", err)
	}
}

func TestDaemonNotRunning(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // connection refused from here on
	c := New(srv.URL)
	_, err := c.ListProjects(context.Background())
	if !errors.Is(err, ErrDaemonNotRunning) {
		t.Fatalf("want ErrDaemonNotRunning, got %v", err)
	}
	if !strings.Contains(err.Error(), "running.json") {
		t.Fatalf("error must mention the run file hint, got %v", err)
	}
}

// fullProjectConfigJSON is a stored config exercising fields this client does
// not model (they must survive a GET → mutate → PUT round-trip because the
// daemon replaces the config wholesale).
const fullProjectConfigJSON = `{
	"defaultBranch": "develop",
	"sessionPrefix": "omni",
	"env": {"FOO": "bar"},
	"postCreate": ["make setup"],
	"agentConfig": {"model": "claude-opus-4-5", "permissions": "default", "futureField": 7},
	"worker": {"agent": "codex", "agentConfig": {"model": "gpt-5"}},
	"trackerIntake": {"enabled": true}
}`

func TestGetProjectConfig(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// Config reads use GET /projects/{id}: AO registers no GET config route
		// (backend/internal/httpd/controllers/projects.go:26-33).
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/projects/omni" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var cfg map[string]any
		if err := json.Unmarshal([]byte(fullProjectConfigJSON), &cfg); err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"project": map[string]any{
				"id": "omni", "name": "OmniMintX", "kind": "repo",
				"path": "/repo", "repo": "org/omni", "defaultBranch": "develop",
				"config": cfg,
			},
		})
	})
	cfg, err := c.GetProjectConfig(context.Background(), "omni")
	if err != nil {
		t.Fatalf("GetProjectConfig: %v", err)
	}
	if cfg.AgentConfig.Model != "claude-opus-4-5" || cfg.AgentConfig.Permissions != PermissionDefault {
		t.Fatalf("unexpected agentConfig: %+v", cfg.AgentConfig)
	}
	if cfg.IsZero() {
		t.Fatal("config with settings must not be zero")
	}
}

func TestGetProjectConfigEmpty(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// Project.Config is `config,omitempty` (service/project/types.go:25):
		// a project with no stored config omits the key entirely.
		writeJSON(t, w, http.StatusOK, map[string]any{
			"project": map[string]any{
				"id": "omni", "name": "OmniMintX", "kind": "repo",
				"path": "/repo", "repo": "org/omni", "defaultBranch": "main",
			},
		})
	})
	cfg, err := c.GetProjectConfig(context.Background(), "omni")
	if err != nil {
		t.Fatalf("GetProjectConfig: %v", err)
	}
	if !cfg.IsZero() {
		t.Fatalf("want zero config, got %+v", cfg)
	}
}

func TestGetProjectConfigDegraded(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// Degraded variant (service/project/types.go:29-36) has no config.
		writeJSON(t, w, http.StatusOK, map[string]any{
			"project": map[string]any{
				"id": "omni", "name": "OmniMintX", "kind": "repo",
				"path": "/repo", "resolveError": "config blob corrupt",
			},
		})
	})
	_, err := c.GetProjectConfig(context.Background(), "omni")
	if err == nil || !strings.Contains(err.Error(), "degraded") {
		t.Fatalf("want degraded error, got %v", err)
	}
}

func TestGetProjectConfigNotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, http.StatusNotFound, "not_found", "PROJECT_NOT_FOUND", "Unknown project")
	})
	_, err := c.GetProjectConfig(context.Background(), "ghost")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "PROJECT_NOT_FOUND" || apiErr.HTTPStatus != http.StatusNotFound {
		t.Fatalf("want PROJECT_NOT_FOUND APIError, got %v", err)
	}
}

func TestUpdateProjectConfig(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/projects/omni/config" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Config map[string]json.RawMessage `json:"config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		// PUT replaces the stored config wholesale (service/project/dto.go:31-35),
		// so every unmodeled field from the earlier GET must be echoed back.
		for _, key := range []string{"defaultBranch", "sessionPrefix", "env", "postCreate", "worker", "trackerIntake"} {
			if _, ok := body.Config[key]; !ok {
				t.Fatalf("round-trip dropped %q: %v", key, body.Config)
			}
		}
		var ac map[string]json.RawMessage
		if err := json.Unmarshal(body.Config["agentConfig"], &ac); err != nil {
			t.Fatalf("decode agentConfig: %v", err)
		}
		if string(ac["permissions"]) != `"accept-edits"` {
			t.Fatalf("want mutated permissions, got %s", ac["permissions"])
		}
		if string(ac["model"]) != `"claude-opus-4-5"` || string(ac["futureField"]) != "7" {
			t.Fatalf("agentConfig round-trip dropped fields: %v", ac)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"project": map[string]any{
				"id": "omni", "name": "OmniMintX", "kind": "repo",
				"path": "/repo", "repo": "org/omni", "defaultBranch": "develop",
			},
		})
	})
	var cfg ProjectConfig
	if err := json.Unmarshal([]byte(fullProjectConfigJSON), &cfg); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	cfg.AgentConfig.Permissions = PermissionAcceptEdits
	p, err := c.UpdateProjectConfig(context.Background(), "omni", cfg)
	if err != nil {
		t.Fatalf("UpdateProjectConfig: %v", err)
	}
	if p.ID != "omni" {
		t.Fatalf("unexpected project: %+v", p)
	}
}

func TestUpdateProjectConfigNotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, http.StatusNotFound, "not_found", "PROJECT_NOT_FOUND", "Unknown project")
	})
	_, err := c.UpdateProjectConfig(context.Background(), "ghost", ProjectConfig{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "PROJECT_NOT_FOUND" {
		t.Fatalf("want PROJECT_NOT_FOUND APIError, got %v", err)
	}
}

func TestProjectConfigValidation(t *testing.T) {
	c := New("")
	ctx := context.Background()
	if _, err := c.GetProjectConfig(ctx, ""); err == nil {
		t.Fatal("want error for empty project id on get")
	}
	if _, err := c.UpdateProjectConfig(ctx, "  ", ProjectConfig{}); err == nil {
		t.Fatal("want error for blank project id on update")
	}
}

func TestNonEnvelopeError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("gateway exploded"))
	})
	_, err := c.ListProjects(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if apiErr.Code != "NON_ENVELOPE_RESPONSE" || apiErr.HTTPStatus != http.StatusBadGateway ||
		!strings.Contains(apiErr.Message, "gateway exploded") {
		t.Fatalf("unexpected APIError: %+v", apiErr)
	}
}
