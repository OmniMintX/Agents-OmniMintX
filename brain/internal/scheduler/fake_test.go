package scheduler

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OmniMintX/overmind/internal/aoclient"
)

// sessStep is one scripted observation of a fake session: on each GetSession
// call the next step is applied; the last step persists.
type sessStep struct {
	status    aoclient.SessionStatus
	marker    string // content of the per-task .om-done.<hex8> file
	hasMarker bool   // marker file exists in the worktree
	pr        int    // attach an open PR with this number (0 = none)
}

// stepMarker builds a step where the marker file exists with content.
func stepMarker(status aoclient.SessionStatus, content string) sessStep {
	return sessStep{status: status, marker: content, hasMarker: true}
}

type fakeSession struct {
	sess      aoclient.Session
	marker    string // current marker content
	hasMarker bool
	script    []sessStep
}

// fakeAO is an in-memory AO daemon. `failGets` makes the next N GetSession
// calls fail with a transport error (daemon down). `onCreate` runs inside
// CreateSession for ordering assertions.
type fakeAO struct {
	mu        sync.Mutex
	nextID    int
	sessions  map[string]*fakeSession
	scripts   map[string][]sessStep // displayName -> script for new sessions
	log       []string              // "create:om-x", "merge:<branch>", "kill:sess-1"
	active    int
	maxActive int
	failGets  int
	onCreate  func(f *fakeAO, in aoclient.SpawnSessionRequest)
	prompts   map[string]string // displayName -> prompt sent to CreateSession
}

func newFakeAO() *fakeAO {
	return &fakeAO{sessions: map[string]*fakeSession{}, scripts: map[string][]sessStep{}, prompts: map[string]string{}}
}

func (f *fakeAO) downErr() error {
	return fmt.Errorf("%w: fake daemon down", aoclient.ErrDaemonNotRunning)
}

func (f *fakeAO) appendLog(e string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, e)
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
	f.prompts[in.DisplayName] = in.Prompt
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
		if step.hasMarker {
			fs.marker, fs.hasMarker = step.marker, true
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

func (f *fakeAO) ListProjects(_ context.Context) ([]aoclient.ProjectSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGets > 0 {
		f.failGets--
		return nil, f.downErr()
	}
	return []aoclient.ProjectSummary{{ID: "proj-1", Name: "proj-1", Path: "/fake/repo"}}, nil
}

// PreviewFile serves ONLY the session's per-task marker file, mirroring the
// real AO preview route the scheduler probes.
func (f *fakeAO) PreviewFile(_ context.Context, sessionID, filePath string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fs, ok := f.sessions[sessionID]
	if !ok {
		return "", false, nil
	}
	if fs.hasMarker && strings.HasPrefix(filePath, DoneMarkerPrefix) {
		return fs.marker, true, nil
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
