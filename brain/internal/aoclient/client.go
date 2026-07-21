package aoclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// DefaultBaseURL is the AO daemon's default loopback address. The live port is
// recorded in the run file ~/.ao/running.json (AO_RUN_FILE overrides).
const DefaultBaseURL = "http://127.0.0.1:3001"

// Sentinel errors mapped from AO's send guards, surfaced as 409 conflict
// envelopes. Source: backend/internal/service/session/service.go toAPIError()
// and backend/internal/session_manager/manager.go:29,56.
var (
	// ErrTerminated maps envelope code SESSION_TERMINATED.
	ErrTerminated = errors.New("aoclient: session is terminated")
	// ErrAwaitingDecision maps envelope code SESSION_AWAITING_DECISION: the
	// session is paused on a permission decision (blocked) — automated senders
	// must not inject input.
	ErrAwaitingDecision = errors.New("aoclient: session is awaiting a user decision")
	// ErrDaemonNotRunning wraps transport failures reaching the daemon.
	ErrDaemonNotRunning = errors.New("aoclient: AO daemon is not reachable; check it is running (the run file ~/.ao/running.json records the live port) or start it with `ao start`")
)

// APIError is AO's locked non-2xx envelope.
// Source: backend/internal/httpd/envelope/envelope.go:39-45.
type APIError struct {
	HTTPStatus int            `json:"-"`
	Kind       string         `json:"error"`
	Code       string         `json:"code"`
	Message    string         `json:"message"`
	RequestID  string         `json:"requestId,omitempty"`
	Details    map[string]any `json:"details,omitempty"`
}

// Error implements the error interface.
func (e *APIError) Error() string {
	return fmt.Sprintf("aoclient: %d %s (%s): %s", e.HTTPStatus, e.Kind, e.Code, e.Message)
}

// Unwrap maps guard codes onto sentinels so errors.Is works on API errors.
func (e *APIError) Unwrap() error {
	switch e.Code {
	case "SESSION_TERMINATED":
		return ErrTerminated
	case "SESSION_AWAITING_DECISION":
		return ErrAwaitingDecision
	}
	return nil
}

// Client is a thin client for the AO daemon HTTP API.
type Client struct {
	baseURL string
	httpc   *http.Client
}

// New returns a client for the daemon at baseURL (DefaultBaseURL when empty).
func New(baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpc:   &http.Client{Timeout: 30 * time.Second},
	}
}

// SetHTTPClient overrides the underlying HTTP client (custom timeouts, tests).
func (c *Client) SetHTTPClient(h *http.Client) {
	if h != nil {
		c.httpc = h
	}
}

// ListProjects calls GET /api/v1/projects.
// Route: backend/internal/httpd/controllers/projects.go:27.
func (c *Client) ListProjects(ctx context.Context) ([]ProjectSummary, error) {
	var out struct {
		Projects []ProjectSummary `json:"projects"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/projects", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Projects, nil
}

// AddProject calls POST /api/v1/projects (201). The daemon strict-decodes the
// body, so only known fields are sent.
// Route: backend/internal/httpd/controllers/projects.go:28.
func (c *Client) AddProject(ctx context.Context, in AddProjectInput) (Project, error) {
	if strings.TrimSpace(in.Path) == "" {
		return Project{}, errors.New("aoclient: project path is required")
	}
	var out struct {
		Project Project `json:"project"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/projects", nil, in, &out); err != nil {
		return Project{}, err
	}
	return out.Project, nil
}

// GetProjectConfig returns a project's stored config. AO has no
// GET /projects/{id}/config route — the route table only registers PUT for
// config (backend/internal/httpd/controllers/projects.go:26-33) — so this
// reads GET /api/v1/projects/{id}, whose Project read model carries the config
// (backend/internal/service/project/types.go:17-27, `config,omitempty`).
// The response wraps oneOf Project|Degraded under "project"
// (controllers/dto.go:46-101); the Degraded variant (resolveError set,
// service/project/types.go:29-36) has no config and surfaces as an error. A
// project with no stored config decodes as the zero ProjectConfig.
func (c *Client) GetProjectConfig(ctx context.Context, projectID string) (ProjectConfig, error) {
	if strings.TrimSpace(projectID) == "" {
		return ProjectConfig{}, errors.New("aoclient: project id is required")
	}
	var out struct {
		Project struct {
			Config       *ProjectConfig `json:"config"`
			ResolveError string         `json:"resolveError"`
		} `json:"project"`
	}
	path := "/api/v1/projects/" + url.PathEscape(projectID)
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return ProjectConfig{}, err
	}
	if out.Project.ResolveError != "" {
		return ProjectConfig{}, fmt.Errorf("aoclient: project %s is degraded (config unavailable): %s", projectID, out.Project.ResolveError)
	}
	if out.Project.Config == nil {
		return ProjectConfig{}, nil
	}
	return *out.Project.Config, nil
}

// UpdateProjectConfig calls PUT /api/v1/projects/{id}/config with body
// {"config": cfg}. The daemon REPLACES the stored config wholesale — NOT a
// merge: "Config replaces the project's stored config wholesale; a zero-value
// config clears it" (backend/internal/service/project/dto.go:31-35), and
// Service.SetConfig assigns `row.Config = in.Config`
// (service/project/service.go:513-533). Callers MUST therefore
// GetProjectConfig first, mutate the returned value, and send it back whole;
// ProjectConfig round-trips fields this client does not model so none are
// erased. The daemon strict-decodes the body (unknown top-level keys are a
// 400; controllers/projects.go:104-120,143-150) and answers 200 with the
// updated {"project": ...} read model (projects.go:119).
func (c *Client) UpdateProjectConfig(ctx context.Context, projectID string, cfg ProjectConfig) (Project, error) {
	if strings.TrimSpace(projectID) == "" {
		return Project{}, errors.New("aoclient: project id is required")
	}
	body := struct {
		Config ProjectConfig `json:"config"`
	}{Config: cfg}
	var out struct {
		Project Project `json:"project"`
	}
	path := "/api/v1/projects/" + url.PathEscape(projectID) + "/config"
	if err := c.do(ctx, http.MethodPut, path, nil, body, &out); err != nil {
		return Project{}, err
	}
	return out.Project, nil
}

// ListAgents calls GET /api/v1/agents. The body is the inventory itself
// (no wrapper key; envelope.WriteJSON writes it directly).
// Route: backend/internal/httpd/controllers/agents.go:29.
func (c *Client) ListAgents(ctx context.Context) (AgentInventory, error) {
	var out AgentInventory
	if err := c.do(ctx, http.MethodGet, "/api/v1/agents", nil, nil, &out); err != nil {
		return AgentInventory{}, err
	}
	return out, nil
}

// CreateSession calls POST /api/v1/sessions (201). Client-side it mirrors the
// daemon's spawn validation (backend/internal/httpd/controllers/sessions.go:121-137):
// projectId required, prompt ≤ MaxPromptLen bytes, displayName ≤
// MaxDisplayNameLen runes.
func (c *Client) CreateSession(ctx context.Context, in SpawnSessionRequest) (Session, error) {
	if in.ProjectID == "" {
		return Session{}, errors.New("aoclient: projectId is required")
	}
	if len(in.Prompt) > MaxPromptLen {
		return Session{}, fmt.Errorf("aoclient: prompt exceeds %d bytes", MaxPromptLen)
	}
	if utf8.RuneCountInString(strings.TrimSpace(in.DisplayName)) > MaxDisplayNameLen {
		return Session{}, fmt.Errorf("aoclient: displayName exceeds %d characters", MaxDisplayNameLen)
	}
	var out struct {
		Session Session `json:"session"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/sessions", nil, in, &out); err != nil {
		return Session{}, err
	}
	return out.Session, nil
}

// GetSession calls GET /api/v1/sessions/{sessionId}.
// Route: backend/internal/httpd/controllers/sessions.go get().
func (c *Client) GetSession(ctx context.Context, sessionID string) (Session, error) {
	var out struct {
		Session Session `json:"session"`
	}
	path := "/api/v1/sessions/" + url.PathEscape(sessionID)
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return Session{}, err
	}
	return out.Session, nil
}

// ListSessions calls GET /api/v1/sessions with optional ?project=&active=.
// Route: backend/internal/httpd/controllers/sessions.go list().
func (c *Client) ListSessions(ctx context.Context, filter ListSessionsFilter) ([]Session, error) {
	q := url.Values{}
	if filter.Project != "" {
		q.Set("project", filter.Project)
	}
	if filter.Active != nil {
		q.Set("active", fmt.Sprintf("%t", *filter.Active))
	}
	var out struct {
		Sessions []Session `json:"sessions"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/sessions", q, nil, &out); err != nil {
		return nil, err
	}
	return out.Sessions, nil
}

// SendMessage calls POST /api/v1/sessions/{sessionId}/send. It mirrors the
// daemon's validation (backend/internal/httpd/controllers/sessions.go:466-473):
// message required, ≤ MaxMessageLen bytes. Guard failures surface as
// ErrTerminated / ErrAwaitingDecision via the 409 envelope.
func (c *Client) SendMessage(ctx context.Context, sessionID, message string) (SendMessageResult, error) {
	if message == "" {
		return SendMessageResult{}, errors.New("aoclient: message is required")
	}
	if len(message) > MaxMessageLen {
		return SendMessageResult{}, fmt.Errorf("aoclient: message exceeds %d bytes", MaxMessageLen)
	}
	body := struct {
		Message string `json:"message"`
	}{Message: message}
	var out SendMessageResult
	path := "/api/v1/sessions/" + url.PathEscape(sessionID) + "/send"
	if err := c.do(ctx, http.MethodPost, path, nil, body, &out); err != nil {
		return SendMessageResult{}, err
	}
	return out, nil
}

// KillSession calls POST /api/v1/sessions/{sessionId}/kill.
// Route: backend/internal/httpd/controllers/sessions.go kill().
func (c *Client) KillSession(ctx context.Context, sessionID string) (KillSessionResult, error) {
	var out KillSessionResult
	path := "/api/v1/sessions/" + url.PathEscape(sessionID) + "/kill"
	if err := c.do(ctx, http.MethodPost, path, nil, nil, &out); err != nil {
		return KillSessionResult{}, err
	}
	return out, nil
}

// ListWorkspaceFiles calls GET /api/v1/sessions/{sessionId}/workspace/files.
// Overmind uses it to look for the .om-done completion marker.
// Route: backend/internal/httpd/controllers/sessions.go:78,222.
func (c *Client) ListWorkspaceFiles(ctx context.Context, sessionID string) (WorkspaceFiles, error) {
	var out WorkspaceFiles
	path := "/api/v1/sessions/" + url.PathEscape(sessionID) + "/workspace/files"
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return WorkspaceFiles{}, err
	}
	return out, nil
}

// PreviewFile calls GET /api/v1/sessions/{sessionId}/preview/files/{path} and
// returns the raw file content. AO 0.10.x daemons lack the workspace/files
// listing route but serve individual worktree files here (the route behind
// session.previewUrl). A 404 means the file does not exist: found=false, no
// error. Callers must have already confirmed the session exists (GetSession),
// since a missing session also answers 404 on this route.
func (c *Client) PreviewFile(ctx context.Context, sessionID, filePath string) (content string, found bool, err error) {
	u := c.baseURL + "/api/v1/sessions/" + url.PathEscape(sessionID) + "/preview/files/" + url.PathEscape(filePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", false, fmt.Errorf("aoclient: build request: %w", err)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", false, err
		}
		return "", false, fmt.Errorf("%w (GET %s: %v)", ErrDaemonNotRunning, u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", false, decodeAPIError(resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", false, fmt.Errorf("aoclient: read response: %w", err)
	}
	return string(b), true, nil
}

// MergePR calls POST /api/v1/prs/{id}/merge, where id is the PR number.
// Route: backend/internal/httpd/controllers/prs.go:22.
func (c *Client) MergePR(ctx context.Context, prID string) (MergePRResult, error) {
	if strings.TrimSpace(prID) == "" {
		return MergePRResult{}, errors.New("aoclient: pr id is required")
	}
	var out MergePRResult
	path := "/api/v1/prs/" + url.PathEscape(prID) + "/merge"
	if err := c.do(ctx, http.MethodPost, path, nil, nil, &out); err != nil {
		return MergePRResult{}, err
	}
	return out, nil
}

// do performs one JSON round-trip. Any 2xx decodes into out (when non-nil);
// any other status decodes the APIError envelope. Transport failures wrap
// ErrDaemonNotRunning unless the context ended first.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("aoclient: encode request: %w", err)
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return fmt.Errorf("aoclient: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return fmt.Errorf("%w (%s %s: %v)", ErrDaemonNotRunning, method, u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeAPIError(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("aoclient: decode response: %w", err)
	}
	return nil
}

// decodeAPIError parses AO's locked APIError envelope from a non-2xx response,
// falling back to a synthetic envelope when the body is not one.
func decodeAPIError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	apiErr := &APIError{HTTPStatus: resp.StatusCode}
	if err := json.Unmarshal(body, apiErr); err != nil || apiErr.Code == "" {
		apiErr.Kind = "unknown"
		apiErr.Code = "NON_ENVELOPE_RESPONSE"
		apiErr.Message = strings.TrimSpace(string(body))
	}
	return apiErr
}
