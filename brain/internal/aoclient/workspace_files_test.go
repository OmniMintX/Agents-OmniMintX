package aoclient

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestListWorkspaceFiles(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/sessions/sess-1/workspace/files" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		// Body shape is ListWorkspaceFilesResponse: top-level fields, no wrapper.
		// Source: backend/internal/httpd/controllers/dto.go:171-187.
		writeJSON(t, w, http.StatusOK, map[string]any{
			"sessionId": "sess-1",
			"files": []map[string]any{
				{"path": ".om-done", "status": "added", "additions": 1, "deletions": 0, "size": 5, "binary": false},
				{"path": "main.go", "status": "modified", "additions": 10, "deletions": 2, "size": 2048, "binary": false},
			},
			"truncated": false,
		})
	})
	files, err := c.ListWorkspaceFiles(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("ListWorkspaceFiles: %v", err)
	}
	if files.SessionID != "sess-1" || files.Truncated || len(files.Files) != 2 {
		t.Fatalf("unexpected result: %+v", files)
	}
	if f := files.Files[0]; f.Path != ".om-done" || f.Status != "added" || f.Additions != 1 {
		t.Fatalf("unexpected first file: %+v", f)
	}
	if f := files.Files[1]; f.Path != "main.go" || f.Size != 2048 {
		t.Fatalf("unexpected second file: %+v", f)
	}
}

func TestListWorkspaceFilesEscapesSessionID(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/v1/sessions/se%2Fss/workspace/files" {
			t.Fatalf("session id not escaped: %s", r.URL.EscapedPath())
		}
		writeJSON(t, w, http.StatusOK, map[string]any{"sessionId": "se/ss", "files": []any{}, "truncated": false})
	})
	if _, err := c.ListWorkspaceFiles(context.Background(), "se/ss"); err != nil {
		t.Fatalf("ListWorkspaceFiles: %v", err)
	}
}

func TestListWorkspaceFilesNotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, http.StatusNotFound, "not_found", "SESSION_NOT_FOUND", "Unknown session")
	})
	_, err := c.ListWorkspaceFiles(context.Background(), "nope")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus != http.StatusNotFound || apiErr.Code != "SESSION_NOT_FOUND" {
		t.Fatalf("want SESSION_NOT_FOUND APIError, got %v", err)
	}
}

func TestPreviewFileFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/api/v1/sessions/sess-1/preview/files/.om-done" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
		}
		// AO serves the raw file body as text/plain (no JSON wrapper).
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Created greeting.txt\n"))
	})
	content, found, err := c.PreviewFile(context.Background(), "sess-1", ".om-done")
	if err != nil || !found || content != "Created greeting.txt\n" {
		t.Fatalf("PreviewFile = (%q, %v, %v), want content, found, nil", content, found, err)
	}
}

func TestPreviewFileMissingIs404NotError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// AO 0.10.x answers a plain-text 404 (not the APIError envelope)
		// when the file does not exist in the worktree.
		http.NotFound(w, r)
	})
	content, found, err := c.PreviewFile(context.Background(), "sess-1", ".om-done")
	if err != nil || found || content != "" {
		t.Fatalf("PreviewFile = (%q, %v, %v), want not-found without error", content, found, err)
	}
}

func TestPreviewFileServerError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(t, w, http.StatusInternalServerError, "internal", "INTERNAL", "boom")
	})
	_, _, err := c.PreviewFile(context.Background(), "sess-1", ".om-done")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus != http.StatusInternalServerError {
		t.Fatalf("want 500 APIError, got %v", err)
	}
}
