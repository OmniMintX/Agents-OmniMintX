// Package aoclient is a thin HTTP client for the Agent Orchestrator (AO)
// daemon loopback API (http://127.0.0.1:<port>, unauthenticated — AO hard
// rule; the live port is recorded in the run file ~/.ao/running.json).
//
// Wire shapes are hand-mirrored from the AO backend source (read-only copy
// under docs/repo/agent-orchestrator/backend/):
//   - internal/httpd/controllers/dto.go          (request/response DTOs)
//   - internal/httpd/envelope/envelope.go        (APIError envelope)
//   - internal/domain/session.go, status.go      (session read model, status enum)
//   - internal/service/project/types.go, dto.go  (project shapes)
//   - internal/httpd/apispec/openapi.yaml        (generated spec)
package aoclient

import "time"

// Daemon-enforced input limits, mirrored client-side to fail fast.
// Source: backend/internal/httpd/controllers/sessions.go:26-28.
const (
	// MaxPromptLen caps SpawnSessionRequest.Prompt in BYTES (daemon checks
	// len(prompt); sessions.go:125).
	MaxPromptLen = 4096
	// MaxMessageLen caps SendMessage in BYTES (daemon checks len(message);
	// sessions.go:470).
	MaxMessageLen = 4096
	// MaxDisplayNameLen caps DisplayName in RUNES (daemon checks
	// utf8.RuneCountInString; sessions.go:134).
	MaxDisplayNameLen = 20
)

// SessionStatus is AO's DERIVED display status, exposed verbatim.
// Interpreting it is the scheduler's job, not this client's.
// Source: backend/internal/domain/status.go:9-25.
type SessionStatus string

// The 13 derived session statuses.
const (
	StatusWorking          SessionStatus = "working"
	StatusPROpen           SessionStatus = "pr_open"
	StatusDraft            SessionStatus = "draft"
	StatusCIFailed         SessionStatus = "ci_failed"
	StatusReviewPending    SessionStatus = "review_pending"
	StatusChangesRequested SessionStatus = "changes_requested"
	StatusApproved         SessionStatus = "approved"
	StatusMergeable        SessionStatus = "mergeable"
	StatusMerged           SessionStatus = "merged"
	StatusNeedsInput       SessionStatus = "needs_input"
	StatusIdle             SessionStatus = "idle"
	StatusTerminated       SessionStatus = "terminated"
	StatusNoSignal         SessionStatus = "no_signal"
)

// Harness identifies the coding agent driving a session.
// Enum source: backend/internal/httpd/controllers/dto.go:157.
type Harness string

// Supported harnesses.
const (
	HarnessClaudeCode Harness = "claude-code"
	HarnessCodex      Harness = "codex"
	HarnessAider      Harness = "aider"
	HarnessOpencode   Harness = "opencode"
	HarnessGrok       Harness = "grok"
	HarnessDroid      Harness = "droid"
	HarnessAmp        Harness = "amp"
	HarnessAgy        Harness = "agy"
	HarnessCrush      Harness = "crush"
	HarnessCursor     Harness = "cursor"
	HarnessQwen       Harness = "qwen"
	HarnessCopilot    Harness = "copilot"
	HarnessGoose      Harness = "goose"
	HarnessAuggie     Harness = "auggie"
	HarnessContinue   Harness = "continue"
	HarnessDevin      Harness = "devin"
	HarnessCline      Harness = "cline"
	HarnessKimi       Harness = "kimi"
	HarnessKiro       Harness = "kiro"
	HarnessKilocode   Harness = "kilocode"
	HarnessVibe       Harness = "vibe"
	HarnessPi         Harness = "pi"
	HarnessAutohand   Harness = "autohand"
)

// Activity is the persisted agent activity reading.
// Source: backend/internal/domain/activity.go:44-47.
type Activity struct {
	State          string    `json:"state"`
	LastActivityAt time.Time `json:"lastActivityAt"`
}

// SessionPRFacts is the curated per-PR read shape on a session.
// Source: backend/internal/httpd/controllers/dto.go:282-291.
type SessionPRFacts struct {
	URL            string    `json:"url"`
	Number         int       `json:"number"`
	State          string    `json:"state"`
	CI             string    `json:"ci"`
	Review         string    `json:"review"`
	Mergeability   string    `json:"mergeability"`
	ReviewComments bool      `json:"reviewComments"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// Session mirrors the SessionView wire shape (domain.Session flattened).
// Source: backend/internal/httpd/controllers/dto.go:132-145 and
// backend/internal/domain/session.go:46-76.
type Session struct {
	ID               string           `json:"id"`
	ProjectID        string           `json:"projectId"`
	IssueID          string           `json:"issueId,omitempty"`
	Kind             string           `json:"kind"`
	Harness          Harness          `json:"harness,omitempty"`
	DisplayName      string           `json:"displayName,omitempty"`
	Activity         Activity         `json:"activity"`
	IsTerminated     bool             `json:"isTerminated"`
	CreatedAt        time.Time        `json:"createdAt"`
	UpdatedAt        time.Time        `json:"updatedAt"`
	Status           SessionStatus    `json:"status"`
	TerminalHandleID string           `json:"terminalHandleId,omitempty"`
	Branch           string           `json:"branch,omitempty"`
	PreviewURL       string           `json:"previewUrl,omitempty"`
	PreviewRevision  int64            `json:"previewRevision,omitempty"`
	PRs              []SessionPRFacts `json:"prs"`
}

// SpawnSessionRequest is the body of POST /api/v1/sessions.
// Source: backend/internal/httpd/controllers/dto.go:153-164.
type SpawnSessionRequest struct {
	ProjectID string  `json:"projectId"`
	IssueID   string  `json:"issueId,omitempty"`
	Kind      string  `json:"kind,omitempty"` // worker|orchestrator; daemon defaults to worker
	Harness   Harness `json:"harness,omitempty"`
	Branch    string  `json:"branch,omitempty"` // optional base branch
	Prompt    string  `json:"prompt,omitempty"` // ≤ MaxPromptLen bytes
	// DisplayName is the sidebar label, ≤ MaxDisplayNameLen runes. Overmind
	// embeds the task id here as an idempotency marker.
	DisplayName string `json:"displayName,omitempty"`
}

// ProjectSummary is one row of GET /api/v1/projects.
// Source: backend/internal/service/project/types.go:6-14.
type ProjectSummary struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	Path              string  `json:"path"`
	Kind              string  `json:"kind"`
	SessionPrefix     string  `json:"sessionPrefix"`
	OrchestratorAgent Harness `json:"orchestratorAgent,omitempty"`
	ResolveError      string  `json:"resolveError,omitempty"`
}

// Project is the full read model returned by POST /api/v1/projects (201).
// Source: backend/internal/service/project/types.go:17-27 (Config and
// WorkspaceRepos omitted — not needed by Overmind).
type Project struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	Path          string `json:"path"`
	Repo          string `json:"repo"`
	DefaultBranch string `json:"defaultBranch"`
	Agent         string `json:"agent,omitempty"`
}

// AddProjectInput is the body of POST /api/v1/projects. The daemon
// strict-decodes it (unknown fields are a 400), so only mirrored fields exist.
// Source: backend/internal/service/project/dto.go:13-19 (config omitted).
type AddProjectInput struct {
	Path        string  `json:"path"`
	ProjectID   *string `json:"projectId,omitempty"`
	Name        *string `json:"name,omitempty"`
	AsWorkspace bool    `json:"asWorkspace,omitempty"`
}

// AgentInfo is the user-facing identity of one agent adapter.
// Source: backend/internal/service/agent/service.go:34-38.
type AgentInfo struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	AuthStatus string `json:"authStatus,omitempty"`
}

// AgentInventory is the body of GET /api/v1/agents: daemon-supported agents
// plus best-effort local probe results (advisory; spawn is the authoritative
// validation point).
// Source: backend/internal/service/agent/service.go:44-48.
type AgentInventory struct {
	Supported  []AgentInfo `json:"supported"`
	Installed  []AgentInfo `json:"installed"`
	Authorized []AgentInfo `json:"authorized"`
}

// ListSessionsFilter maps to the GET /api/v1/sessions query string.
// Source: backend/internal/httpd/controllers/dto.go:110-115.
type ListSessionsFilter struct {
	// Project filters by project id (?project=).
	Project string
	// Active, when set, selects non-terminated (true) or terminated (false)
	// sessions (?active=).
	Active *bool
}

// KillSessionResult is the body of POST /api/v1/sessions/{sessionId}/kill.
// Source: backend/internal/httpd/controllers/dto.go:237-242.
type KillSessionResult struct {
	OK        bool   `json:"ok"`
	SessionID string `json:"sessionId"`
	Freed     bool   `json:"freed,omitempty"`
}

// SendMessageResult is the body of POST /api/v1/sessions/{sessionId}/send.
// Source: backend/internal/httpd/controllers/dto.go:274-279.
type SendMessageResult struct {
	OK        bool   `json:"ok"`
	SessionID string `json:"sessionId"`
	Message   string `json:"message"`
}

// MergePRResult is the body of POST /api/v1/prs/{id}/merge.
// Source: backend/internal/httpd/controllers/dto.go:580-585.
type MergePRResult struct {
	OK       bool   `json:"ok"`
	PRNumber int    `json:"prNumber"`
	Method   string `json:"method"`
}

// WorkspaceFile is one file row in the session workspace listing.
// Status enum: unmodified, modified, added, deleted, renamed.
// Source: backend/internal/httpd/controllers/dto.go:179-187 (WorkspaceFileSummary).
type WorkspaceFile struct {
	Path      string `json:"path"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Size      int64  `json:"size"`
	Binary    bool   `json:"binary"`
}

// WorkspaceFiles is the body of GET /api/v1/sessions/{sessionId}/workspace/files
// (no wrapper key — fields are top-level, unlike the { session } envelope).
// Source: backend/internal/httpd/controllers/dto.go:171-177 (ListWorkspaceFilesResponse).
type WorkspaceFiles struct {
	SessionID string          `json:"sessionId"`
	Files     []WorkspaceFile `json:"files"`
	Truncated bool            `json:"truncated"`
}
