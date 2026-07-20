package scheduler

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/OmniMintX/overmind/internal/aoclient"
)

// sessStep is one scripted observation of a fake session: on each GetSession
// call the next step is applied; the last step persists.
type sessStep struct {
	status aoclient.SessionStatus
	marker bool // .om-done exists in the workspace
	pr     int  // attach an open PR with this number (0 = none)
}

type fakeSession struct {
	sess   aoclient.Session
	files  []aoclient.WorkspaceFile
	script []sessStep
}

// fakeAO is an in-memory AO daemon. `failGets` makes the next N GetSession
// calls fail with a transport error (daemon down). `onCreate` runs inside
// CreateSession for ordering assertions.
type fakeAO struct {
	mu        sync.Mutex
	nextID    int
	sessions  map[string]*fakeSession
	scripts   map[string][]sessStep // displayName -> script for new sessions
	log       []string              // "create:om-x", "merge:1", "kill:sess-1"
	active    int
	maxActive int
	failGets  int
	onCreate  func(f *fakeAO, in aoclient.SpawnSessionRequest)
	// wsFilesRouteMissing mimics AO 0.10.x: the workspace/files listing
	// route answers 404 ROUTE_NOT_FOUND (per-file preview still works).
	wsFilesRouteMissing bool
}

func newFakeAO() *fakeAO {
	return &fakeAO{sessions: map[string]*fakeSession{}, scripts: map[string][]sessStep{}}
}

func (f *fakeAO) downErr() error {
	return fmt.Errorf("%w: fake daemon down", aoclient.ErrDaemonNotRunning)
}

// addSession pre-seeds a session (crash-resume tests). Returns its id.
func (f *fakeAO) addSession(displayName string, terminated bool, script []sessStep) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := "sess-" + strconv.Itoa(f.nextID)
	f.sessions[id] = &fakeSession{
		sess: aoclient.Session{
			ID: id, ProjectID: "proj-1", Kind: "worker", DisplayName: displayName,
			Branch: "ao/" + id, IsTerminated: terminated, Status: aoclient.StatusWorking,
			CreatedAt: time.Unix(int64(f.nextID), 0),
		},
		script: script,
	}
	if !terminated {
		f.active++
	}
	return id
}

func (f *fakeAO) CreateSession(_ context.Context, in aoclient.SpawnSessionRequest) (aoclient.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onCreate != nil {
		f.onCreate(f, in)
	}
	f.nextID++
	id := "sess-" + strconv.Itoa(f.nextID)
	fs := &fakeSession{
		sess: aoclient.Session{
			ID: id, ProjectID: in.ProjectID, Kind: "worker", DisplayName: in.DisplayName,
			Branch: "ao/" + id, Status: aoclient.StatusWorking,
			CreatedAt: time.Unix(int64(f.nextID), 0),
		},
		script: f.scripts[in.DisplayName],
	}
	f.sessions[id] = fs
	f.log = append(f.log, "create:"+in.DisplayName)
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	return fs.sess, nil
}

func (f *fakeAO) GetSession(_ context.Context, sessionID string) (aoclient.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGets > 0 {
		f.failGets--
		return aoclient.Session{}, f.downErr()
	}
	fs, ok := f.sessions[sessionID]
	if !ok {
		return aoclient.Session{}, &aoclient.APIError{HTTPStatus: 404, Kind: "not_found", Code: "SESSION_NOT_FOUND", Message: sessionID}
	}
	if len(fs.script) > 0 {
		step := fs.script[0]
		if len(fs.script) > 1 {
			fs.script = fs.script[1:]
		}
		fs.sess.Status = step.status
		if step.marker {
			fs.files = []aoclient.WorkspaceFile{{Path: DoneMarker, Status: "added", Size: 1}}
		}
		if step.pr != 0 && !fs.hasPR(step.pr) {
			fs.sess.PRs = append(fs.sess.PRs, aoclient.SessionPRFacts{
				URL: "https://example.test/pr/" + strconv.Itoa(step.pr), Number: step.pr, State: "open",
			})
		}
	}
	return fs.sess, nil
}

func (fs *fakeSession) hasPR(n int) bool {
	for _, pr := range fs.sess.PRs {
		if pr.Number == n {
			return true
		}
	}
	return false
}

func (f *fakeAO) ListSessions(_ context.Context, _ aoclient.ListSessionsFilter) ([]aoclient.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []aoclient.Session
	for _, fs := range f.sessions {
		out = append(out, fs.sess)
	}
	return out, nil
}

func (f *fakeAO) KillSession(_ context.Context, sessionID string) (aoclient.KillSessionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fs, ok := f.sessions[sessionID]
	if !ok {
		return aoclient.KillSessionResult{}, &aoclient.APIError{HTTPStatus: 404, Kind: "not_found", Code: "SESSION_NOT_FOUND", Message: sessionID}
	}
	if !fs.sess.IsTerminated {
		fs.sess.IsTerminated = true
		fs.sess.Status = aoclient.StatusTerminated
		fs.script = nil
		f.active--
	}
	f.log = append(f.log, "kill:"+sessionID)
	return aoclient.KillSessionResult{OK: true, SessionID: sessionID}, nil
}

func (f *fakeAO) MergePR(_ context.Context, prID string) (aoclient.MergePRResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := strconv.Atoi(prID)
	if err != nil {
		return aoclient.MergePRResult{}, err
	}
	for _, fs := range f.sessions {
		for i := range fs.sess.PRs {
			if fs.sess.PRs[i].Number == n {
				fs.sess.PRs[i].State = "merged"
				f.log = append(f.log, "merge:"+prID)
				return aoclient.MergePRResult{OK: true, PRNumber: n, Method: "squash"}, nil
			}
		}
	}
	return aoclient.MergePRResult{}, &aoclient.APIError{HTTPStatus: 404, Kind: "not_found", Code: "PR_NOT_FOUND", Message: prID}
}

func (f *fakeAO) ListWorkspaceFiles(_ context.Context, sessionID string) (aoclient.WorkspaceFiles, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.wsFilesRouteMissing {
		return aoclient.WorkspaceFiles{}, &aoclient.APIError{HTTPStatus: 404, Kind: "not_found", Code: "ROUTE_NOT_FOUND", Message: "GET /api/v1/sessions/" + sessionID + "/workspace/files has no handler"}
	}
	fs, ok := f.sessions[sessionID]
	if !ok {
		return aoclient.WorkspaceFiles{}, &aoclient.APIError{HTTPStatus: 404, Kind: "not_found", Code: "SESSION_NOT_FOUND", Message: sessionID}
	}
	return aoclient.WorkspaceFiles{SessionID: sessionID, Files: fs.files}, nil
}

func (f *fakeAO) PreviewFile(_ context.Context, sessionID, filePath string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fs, ok := f.sessions[sessionID]
	if !ok {
		return "", false, nil
	}
	for _, fl := range fs.files {
		if fl.Path == filePath && fl.Status != "deleted" {
			return "marker", true, nil
		}
	}
	return "", false, nil
}

func (f *fakeAO) events() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

// fakeClock advances instantly on Sleep so timeout tests run in real
// microseconds while covering minutes of scheduler time.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return nil
}
