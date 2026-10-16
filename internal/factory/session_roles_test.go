package factory_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

const (
	// sessionContextRevisionForTest is the commit of every test snapshot.
	sessionContextRevisionForTest = "0123456789abcdef0123456789abcdef01234567"
	// sessionImageDigestForTest is the pinned worker image digest.
	sessionImageDigestForTest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	// freshNativeSessionForTest is the identity a fresh Codex launch reports.
	freshNativeSessionForTest = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	// forkedNativeSessionForTest is an identity no launch asked for.
	forkedNativeSessionForTest = "9b2f6c1e-3d4a-4f5b-8c6d-7e8f9a0b1c2d"
)

// sessionWorkerForTest is a worker runtime with the detached process
// extension. It records every worker and process request and reports the
// native session that each launch command names, so the real Codex and
// Claude adapters run unchanged against it.
type sessionWorkerForTest struct {
	mu        sync.Mutex
	starts    []worker.StartRequest
	stops     []string
	launches  []worker.HeadlessRequest
	cancelled []string
	finished  []string
	processes map[string]worker.HeadlessInspection
	// onLaunch optionally replaces the process state a launch produces.
	onLaunch func(worker.HeadlessRequest) (worker.HeadlessInspection, bool)
}

// Start records one worker start.
func (w *sessionWorkerForTest) Start(_ context.Context, request worker.StartRequest) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.starts = append(w.starts, request)
	return nil
}

// Resume implements the base worker seam.
func (*sessionWorkerForTest) Resume(context.Context, worker.ResumeRequest) error { return nil }

// RunCommand implements the base worker seam.
func (*sessionWorkerForTest) RunCommand(context.Context, worker.CommandRequest) (worker.CommandResult, error) {
	return worker.CommandResult{}, nil
}

// Stop records one worker stop.
func (w *sessionWorkerForTest) Stop(_ context.Context, workerID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stops = append(w.stops, workerID)
	return nil
}

// Inspect implements the base worker seam.
func (*sessionWorkerForTest) Inspect(context.Context, string) (worker.Inspection, error) {
	return worker.Inspection{Exists: true, Running: true}, nil
}

// StartHeadless records one launch and creates its process state.
func (w *sessionWorkerForTest) StartHeadless(_ context.Context, request worker.HeadlessRequest) (worker.HeadlessExecution, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.launches = append(w.launches, request)
	if w.processes == nil {
		w.processes = map[string]worker.HeadlessInspection{}
	}
	inspection := worker.HeadlessInspection{Status: worker.HeadlessStatusRunning, Stdout: nativeEventsForTest(nativeSessionFromCommand(request.Command))}
	if w.onLaunch != nil {
		if replaced, ok := w.onLaunch(request); ok {
			inspection = replaced
		}
	}
	w.processes[request.InvocationID] = inspection
	return worker.HeadlessExecution{RunID: request.RunID, WorkerID: request.WorkerID, InvocationID: request.InvocationID}, nil
}

// InspectHeadless returns the recorded process state or a missing process.
func (w *sessionWorkerForTest) InspectHeadless(_ context.Context, request worker.HeadlessRequest) (worker.HeadlessInspection, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if inspection, ok := w.processes[request.InvocationID]; ok {
		return inspection, nil
	}
	return worker.HeadlessInspection{Status: worker.HeadlessStatusMissing}, nil
}

// CancelHeadless records one cancellation.
func (w *sessionWorkerForTest) CancelHeadless(_ context.Context, request worker.HeadlessRequest) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cancelled = append(w.cancelled, request.InvocationID)
	if inspection, ok := w.processes[request.InvocationID]; ok {
		inspection.Status = worker.HeadlessStatusCancelled
		w.processes[request.InvocationID] = inspection
	}
	return nil
}

// FinishHeadless records one accepted-completion shutdown.
func (w *sessionWorkerForTest) FinishHeadless(_ context.Context, request worker.HeadlessRequest) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.finished = append(w.finished, request.InvocationID)
	return nil
}

// exit replaces the process state of one invocation, as a process that ends
// after launch does.
func (w *sessionWorkerForTest) exit(invocationID string, stdout string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	inspection := w.processes[invocationID]
	inspection.Status = worker.HeadlessStatusExited
	inspection.ExitCode = 1
	inspection.Stdout += stdout
	w.processes[invocationID] = inspection
}

// nativeSessionFromCommand returns the native session a launch command names,
// or the fixed fresh identity of a Codex launch.
func nativeSessionFromCommand(command []string) string {
	for index, argument := range command {
		if (argument == "--session-id" || argument == "--resume" || argument == "resume") && index+1 < len(command) {
			return command[index+1]
		}
	}
	return freshNativeSessionForTest
}

// nativeEventsForTest returns the machine events both adapters read.
func nativeEventsForTest(nativeSessionID string) string {
	return `{"type":"thread.started","thread_id":"` + nativeSessionID + `"}` + "\n" +
		`{"type":"system","subtype":"init","session_id":"` + nativeSessionID + `"}` + "\n"
}

// authenticationFailureEventsForTest returns the machine events both adapters
// classify as expired authentication.
func authenticationFailureEventsForTest() string {
	return `{"type":"error","code":"unauthorized"}` + "\n" +
		`{"type":"system","subtype":"api_retry","error":"authentication_failed"}` + "\n" +
		`{"type":"result","subtype":"error_during_execution","is_error":true}` + "\n"
}

// snapshotsForTest creates plain directories as context snapshots.
type snapshotsForTest struct {
	root    string
	mu      sync.Mutex
	removed []string
}

// CreateContextSnapshot creates one snapshot directory at the fixed revision.
func (s *snapshotsForTest) CreateContextSnapshot(_ context.Context, _, targetBranch, snapshotID string) (gitadapter.ContextSnapshot, error) {
	if targetBranch != "main" {
		return gitadapter.ContextSnapshot{}, errors.New("unexpected target branch")
	}
	path := filepath.Join(s.root, snapshotID)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return gitadapter.ContextSnapshot{}, err
	}
	return gitadapter.ContextSnapshot{Revision: sessionContextRevisionForTest, Worktree: path}, nil
}

// RemoveContextSnapshot records and removes one snapshot directory.
func (s *snapshotsForTest) RemoveContextSnapshot(_ context.Context, _ string, snapshot gitadapter.ContextSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removed = append(s.removed, snapshot.Worktree)
	return os.RemoveAll(snapshot.Worktree)
}

// sessionFixture is one repository, store, worker, and session-role
// coordinator for a harness.
type sessionFixture struct {
	t         *testing.T
	harness   config.Harness
	path      string
	store     *store.Store
	worker    *sessionWorkerForTest
	snapshots *snapshotsForTest
	repo      string
	policy    config.RepositoryConfig
	ids       int
	validator func([]store.PlanNode) error
}

// newSessionFixture creates a session "session-1" with one user turn
// "turn-1" in a fresh store.
func newSessionFixture(t *testing.T, harnessName config.Harness) *sessionFixture {
	t.Helper()
	root := t.TempDir()
	repository := filepath.Join(root, "project")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(t.Context(), "git", "init", "-q", repository).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
	path := filepath.Join(root, "data", "factory.db")
	opened, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &sessionFixture{
		t: t, harness: harnessName, path: path, store: opened, repo: repository,
		worker: &sessionWorkerForTest{}, snapshots: &snapshotsForTest{root: filepath.Join(root, "snapshots")},
		policy: config.RepositoryConfig{
			TargetBranch: "main",
			RoleHarnessDefaults: map[string]config.Harness{
				workflow.RoleImplementation: harnessName, workflow.RoleIdeation: harnessName, workflow.RolePlanning: harnessName,
			},
			ModelOptions: map[string][]string{
				workflow.RoleImplementation: {"model-a"}, workflow.RoleIdeation: {"model-i"}, workflow.RolePlanning: {"model-p"},
			},
			WorkerBuild: config.WorkerBuildConfig{Image: "ghcr.io/example/worker", Digest: sessionImageDigestForTest},
		},
	}
	t.Cleanup(func() { _ = fixture.store.Close() })
	session, err := opened.CreateIdeationSession(t.Context(), store.IdeationSession{
		ID: "session-1", RepositoryPath: repository, State: store.IdeationStateAwaitingUser, CreatedAt: time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.userTurn(session, "turn-1", "Operators need to export run history.")
	return fixture
}

// roles returns a session-role coordinator over the fixture's current store.
func (f *sessionFixture) roles() *factory.SessionRoles {
	return factory.NewSessionRoles(factory.SessionRoleDependencies{
		Store:        f.store,
		Harnesses:    harness.NewHeadlessAdapters(f.worker),
		Workers:      f.worker,
		Snapshots:    f.snapshots,
		Registration: config.RepositoryRegistration{Path: f.repo},
		Policy:       f.policy,
		Now:          func() time.Time { return time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC) },
		NewInvocationID: func() (string, error) {
			f.ids++
			return "sinv-" + string(rune('0'+f.ids)), nil
		},
		ValidatePlan: f.validator,
	})
}

// restart closes and reopens the store, as a coordinator restart does.
func (f *sessionFixture) restart() {
	f.t.Helper()
	if err := f.store.Close(); err != nil {
		f.t.Fatal(err)
	}
	reopened, err := store.Open(f.t.Context(), f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	f.store = reopened
}

// session returns the current session.
func (f *sessionFixture) session() store.IdeationSession {
	f.t.Helper()
	session, err := f.store.IdeationSession(f.t.Context(), "session-1")
	if err != nil {
		f.t.Fatal(err)
	}
	return session
}

// invocation returns the current state of one invocation.
func (f *sessionFixture) invocation(id string) store.SessionInvocation {
	f.t.Helper()
	invocation, err := f.store.SessionInvocation(f.t.Context(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return invocation
}

// userTurn appends one user turn while the session waits for the user.
func (f *sessionFixture) userTurn(session store.IdeationSession, id, body string) {
	f.t.Helper()
	write, err := workflow.DefaultSessionLifecycle().Write(session, workflow.SessionEventTurnAccepted, "", time.Time{})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, _, err := f.store.AppendIdeationTurn(f.t.Context(), write, store.IdeationTurn{ID: id, Author: store.IdeationTurnAuthorUser, Body: body}); err != nil {
		f.t.Fatal(err)
	}
}

// report writes value for invocation into its result directory, as the
// worker's factory-report does.
func (f *sessionFixture) report(invocation store.SessionInvocation, value report.Report) {
	f.t.Helper()
	value.SchemaVersion = report.SchemaVersion
	value.InvocationID = invocation.ID
	value.SessionID = invocation.SessionID
	value.SessionRevision = invocation.SessionRevision
	value.Harness = invocation.Harness
	value.Role = invocation.Role
	value.Stage = invocation.Role
	if value.Summary == "" {
		value.Summary = "role turn done"
	}
	value.ReportedAt = time.Date(2026, 10, 15, 10, 5, 0, 0, time.UTC)
	if _, err := report.WriteAtomicForInvocation(invocation.ResultDirectory, invocation.ID, value); err != nil {
		f.t.Fatalf("write report: %v", err)
	}
}

// ideationResultForTest is a completed ideation report with one question and
// a brief update.
func ideationResultForTest() report.Report {
	return report.Report{Outcome: report.OutcomeCompleted, Ideation: &report.IdeationHandoff{
		Message:   "The CLI has no export command. Who reads the export?",
		Questions: []report.Question{{ID: "audience", Prompt: "Who reads the export?"}},
		Brief:     &report.Brief{Problem: "Operators cannot export run history.", Scope: []report.BriefItem{{ID: "json", Text: "JSON export"}}},
		Readiness: &report.ReadinessProposal{Proposal: report.ReadinessInvestigationNeeded, Rationale: "The audience is unknown."},
	}}
}

// sessionHarnesses lists both supported harness adapters.
var sessionHarnesses = []config.Harness{config.HarnessCodex, config.HarnessClaude}

// TestSessionRolesLaunchAcceptAndResumeThroughBothHarnesses verifies that an
// ideation turn launches through HeadlessRuntime on a read-only snapshot with
// a pinned identity, that its report is applied by the coordinator, that the
// session then waits for the user with no active work, and that the next
// user turn resumes the recorded native session without any browser.
func TestSessionRolesLaunchAcceptAndResumeThroughBothHarnesses(t *testing.T) {
	for _, harnessName := range sessionHarnesses {
		t.Run(string(harnessName), func(t *testing.T) {
			fixture := newSessionFixture(t, harnessName)
			roles := fixture.roles()
			first, err := roles.StartIdeationTurn(t.Context(), "session-1", "turn-1")
			if err != nil {
				t.Fatalf("StartIdeationTurn() error = %v", err)
			}
			session := fixture.session()
			identity, _ := prompt.SessionContentIdentity(workflow.RoleIdeation)
			if session.State != store.IdeationStateRoleActive || first.SessionRevision != session.Revision ||
				first.ContextRevision != sessionContextRevisionForTest || first.Harness != string(harnessName) || first.Model != "model-i" ||
				first.PromptVersion != identity.Version || first.PromptSHA256 != identity.SHA256 || first.ImageDigest != sessionImageDigestForTest ||
				first.NativeSessionID == "" || first.TriggerID != "turn-1" || first.ResumedFrom != "" {
				t.Fatalf("started invocation = %#v in session %#v, want a pinned active invocation", first, session)
			}
			start := fixture.worker.starts[0]
			if !start.WorktreeReadOnly || start.WorktreePath == fixture.repo || start.WorktreePath != first.SnapshotPath ||
				start.ImageDigest != sessionImageDigestForTest || start.RunID == "" || start.Role != workflow.RoleIdeation {
				t.Fatalf("worker start = %#v, want a read-only snapshot mount and the pinned image", start)
			}
			launch := fixture.worker.launches[0]
			if launch.Environment["FACTORY_SESSION_ID"] != "session-1" || launch.Environment["FACTORY_SESSION_REVISION"] == "" || launch.Mode != worker.HeadlessLaunchFresh {
				t.Fatalf("headless launch = %#v, want session identity and a fresh native session", launch)
			}
			var packet struct {
				SessionID       string `json:"session_id"`
				SessionRevision int64  `json:"session_revision"`
				ContextRevision string `json:"context_revision"`
				Turns           []struct {
					Author string `json:"author"`
					Body   string `json:"body"`
				} `json:"turns"`
			}
			data, err := os.ReadFile(filepath.Join(first.InvocationDirectory, "session.json"))
			if err != nil || json.Unmarshal(data, &packet) != nil || packet.SessionID != "session-1" || packet.SessionRevision != first.SessionRevision ||
				packet.ContextRevision != sessionContextRevisionForTest || len(packet.Turns) != 1 || packet.Turns[0].Body != "Operators need to export run history." {
				t.Fatalf("session packet = %s, %v; want the pinned identity and the user's turn", data, err)
			}

			fixture.report(first, ideationResultForTest())
			if err := roles.Reconcile(t.Context()); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			session = fixture.session()
			accepted := fixture.invocation(first.ID)
			if session.State != store.IdeationStateAwaitingUser || session.BriefRevision != 1 || accepted.Status != store.SessionInvocationAccepted {
				t.Fatalf("after acceptance session/invocation = %#v / %#v", session, accepted)
			}
			if len(fixture.worker.finished) != 1 || len(fixture.worker.stops) != 1 || len(fixture.snapshots.removed) != 1 {
				t.Fatalf("finished %v, stopped %v, removed %v; want the active work released", fixture.worker.finished, fixture.worker.stops, fixture.snapshots.removed)
			}
			if active, _ := fixture.store.ActiveSessionInvocations(t.Context()); len(active) != 0 {
				t.Fatalf("active invocations = %#v, want none while waiting for the user", active)
			}

			fixture.userTurn(session, "turn-2", "Support engineers read it.")
			second, err := fixture.roles().StartIdeationTurn(t.Context(), "session-1", "turn-2")
			if err != nil {
				t.Fatalf("StartIdeationTurn(turn-2) error = %v", err)
			}
			resume := fixture.worker.launches[1]
			if second.ResumedFrom != first.ID || second.NativeSessionID != first.NativeSessionID || resume.Mode != worker.HeadlessLaunchResume {
				t.Fatalf("second invocation = %#v, launch = %#v; want the recorded native session resumed", second, resume)
			}
			if fixture.worker.starts[1].WorkerID != start.WorkerID {
				t.Fatalf("second worker = %q, want the session role worker %q that keeps the native session", fixture.worker.starts[1].WorkerID, start.WorkerID)
			}
		})
	}
}

// TestSessionRolesAcceptAtMostOneResultPerUserTurn verifies that a second
// launch for an answered turn is refused, and that a role result from a
// cancelled invocation or an old revision changes nothing.
func TestSessionRolesAcceptAtMostOneResultPerUserTurn(t *testing.T) {
	fixture := newSessionFixture(t, config.HarnessCodex)
	roles := fixture.roles()
	cancelled, err := roles.StartIdeationTurn(t.Context(), "session-1", "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := roles.CancelRole(t.Context(), "session-1", "user stopped the turn"); err != nil {
		t.Fatalf("CancelRole() error = %v", err)
	}
	session := fixture.session()
	if session.State != store.IdeationStateAwaitingUser || !strings.Contains(session.StateReason, "user stopped the turn") ||
		fixture.invocation(cancelled.ID).Status != store.SessionInvocationCancelled || len(fixture.worker.cancelled) != 1 {
		t.Fatalf("after cancel session = %#v, invocation = %#v", session, fixture.invocation(cancelled.ID))
	}
	fixture.report(cancelled, ideationResultForTest())
	if err := roles.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if fixture.session().BriefRevision != 0 {
		t.Fatal("a report from a cancelled invocation changed the brief")
	}

	retried, err := roles.StartIdeationTurn(t.Context(), "session-1", "turn-1")
	if err != nil {
		t.Fatalf("StartIdeationTurn(retry) error = %v", err)
	}
	fixture.report(retried, ideationResultForTest())
	if err := roles.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := roles.StartIdeationTurn(t.Context(), "session-1", "turn-1"); !errors.Is(err, factory.ErrSessionRoleNotStartable) && !errors.Is(err, store.ErrSessionTriggerConsumed) {
		t.Fatalf("StartIdeationTurn(answered turn) error = %v, want a refusal for an answered turn", err)
	}
	if got := len(fixture.worker.launches); got != 2 {
		t.Fatalf("launches = %d, want no launch for an answered turn", got)
	}

	fixture.userTurn(fixture.session(), "turn-2", "Support engineers.")
	stale, err := roles.StartIdeationTurn(t.Context(), "session-1", "turn-2")
	if err != nil {
		t.Fatal(err)
	}
	write, err := workflow.DefaultSessionLifecycle().Write(fixture.session(), workflow.SessionEventCancelled, "user cancelled the session", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.TransitionIdeationSession(t.Context(), write); err != nil {
		t.Fatal(err)
	}
	fixture.report(stale, ideationResultForTest())
	if err := roles.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := fixture.invocation(stale.ID); got.Status != store.SessionInvocationCancelled || fixture.session().BriefRevision != 1 {
		t.Fatalf("stale invocation = %#v, brief revision %d; want cancelled and no new brief", got, fixture.session().BriefRevision)
	}
}

// TestSessionRolesSurfaceTypedFailuresAndReleaseTheSession verifies that an
// expired credential, a process that exits without a report, and a resume
// that reveals another native session become visible typed failures that
// return the session to the user, with both harness adapters.
func TestSessionRolesSurfaceTypedFailuresAndReleaseTheSession(t *testing.T) {
	for _, harnessName := range sessionHarnesses {
		t.Run(string(harnessName)+"/credentials", func(t *testing.T) {
			fixture := newSessionFixture(t, harnessName)
			fixture.worker.onLaunch = func(worker.HeadlessRequest) (worker.HeadlessInspection, bool) {
				return worker.HeadlessInspection{Status: worker.HeadlessStatusExited, ExitCode: 1, Stdout: authenticationFailureEventsForTest()}, true
			}
			invocation, err := fixture.roles().StartIdeationTurn(t.Context(), "session-1", "turn-1")
			if !harness.IsAuthenticationExpired(err) {
				t.Fatalf("StartIdeationTurn() error = %v, want expired authentication", err)
			}
			assertReleased(t, fixture, invocation.ID, "authentication_expired")
		})
		t.Run(string(harnessName)+"/process exit", func(t *testing.T) {
			fixture := newSessionFixture(t, harnessName)
			roles := fixture.roles()
			invocation, err := roles.StartIdeationTurn(t.Context(), "session-1", "turn-1")
			if err != nil {
				t.Fatal(err)
			}
			fixture.worker.exit(invocation.ID, "")
			if err := roles.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			assertReleased(t, fixture, invocation.ID, "unexpected_exit")
		})
		t.Run(string(harnessName)+"/native identity mismatch", func(t *testing.T) {
			fixture := newSessionFixture(t, harnessName)
			roles := fixture.roles()
			first, err := roles.StartIdeationTurn(t.Context(), "session-1", "turn-1")
			if err != nil {
				t.Fatal(err)
			}
			fixture.report(first, ideationResultForTest())
			if err := roles.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			fixture.userTurn(fixture.session(), "turn-2", "Support engineers.")
			fixture.worker.onLaunch = func(request worker.HeadlessRequest) (worker.HeadlessInspection, bool) {
				return worker.HeadlessInspection{Status: worker.HeadlessStatusRunning, Stdout: nativeEventsForTest(forkedNativeSessionForTest)}, true
			}
			second, err := roles.StartIdeationTurn(t.Context(), "session-1", "turn-2")
			if !harness.IsNativeSessionMismatch(err) {
				t.Fatalf("StartIdeationTurn(resume) error = %v, want a native-session mismatch", err)
			}
			assertReleased(t, fixture, second.ID, "native_session_mismatch")
		})
	}
}

// assertReleased checks that invocationID failed with kind and that the
// session waits for the user with a visible reason and no active work.
func assertReleased(t *testing.T, fixture *sessionFixture, invocationID, kind string) {
	t.Helper()
	invocation := fixture.invocation(invocationID)
	session := fixture.session()
	if invocation.Status != store.SessionInvocationFailed || invocation.FailureKind != kind ||
		session.State != store.IdeationStateAwaitingUser || session.StateReason == "" {
		t.Fatalf("invocation = %#v, session = %#v; want failure %q released to the user", invocation, session, kind)
	}
	if active, _ := fixture.store.ActiveSessionInvocations(t.Context()); len(active) != 0 {
		t.Fatalf("active invocations = %#v, want none", active)
	}
}

// TestSessionRolesRecoverAcrossCoordinatorRestart verifies that a report
// written while the coordinator was stopped is accepted by a new coordinator,
// and that a launch interrupted before its native session was recorded is
// adopted when the process exists and released when it does not.
func TestSessionRolesRecoverAcrossCoordinatorRestart(t *testing.T) {
	for _, harnessName := range sessionHarnesses {
		t.Run(string(harnessName), func(t *testing.T) {
			fixture := newSessionFixture(t, harnessName)
			invocation, err := fixture.roles().StartIdeationTurn(t.Context(), "session-1", "turn-1")
			if err != nil {
				t.Fatal(err)
			}
			fixture.restart()
			fixture.report(invocation, ideationResultForTest())
			if err := fixture.roles().Reconcile(t.Context()); err != nil {
				t.Fatalf("Reconcile() after restart error = %v", err)
			}
			if got := fixture.invocation(invocation.ID); got.Status != store.SessionInvocationAccepted {
				t.Fatalf("invocation after restart = %#v, want accepted", got)
			}

			fixture.userTurn(fixture.session(), "turn-2", "Support engineers.")
			adopted := persistInterruptedLaunch(t, fixture, "sinv-adopt", "turn-2")
			fixture.worker.processes["sinv-adopt"] = worker.HeadlessInspection{Status: worker.HeadlessStatusRunning, Stdout: nativeEventsForTest(freshNativeSessionForTest)}
			if err := fixture.roles().Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := fixture.invocation(adopted.ID); got.Status != store.SessionInvocationActive || got.NativeSessionID != freshNativeSessionForTest {
				t.Fatalf("interrupted launch = %#v, want the running process adopted", got)
			}
			if err := fixture.roles().CancelRole(t.Context(), "session-1", "test"); err != nil {
				t.Fatal(err)
			}

			lost := persistInterruptedLaunch(t, fixture, "sinv-lost", "turn-2")
			if err := fixture.roles().Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			assertReleased(t, fixture, lost.ID, "launch_interrupted")
		})
	}
}

// persistInterruptedLaunch records an active invocation as a coordinator does
// just before it crashes, without a native session.
func persistInterruptedLaunch(t *testing.T, fixture *sessionFixture, id, trigger string) store.SessionInvocation {
	t.Helper()
	identity, _ := prompt.SessionContentIdentity(workflow.RoleIdeation)
	write, err := workflow.DefaultSessionLifecycle().Write(fixture.session(), workflow.SessionEventRoleStarted, "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	resultDirectory := filepath.Join(t.TempDir(), id, "results")
	if err := os.MkdirAll(resultDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	_, invocation, err := fixture.store.StartSessionInvocation(t.Context(), write, store.SessionInvocation{
		ID: id, Role: workflow.RoleIdeation, TriggerID: trigger, ContextRevision: sessionContextRevisionForTest,
		Harness: string(fixture.harness), Model: "model-i", PromptVersion: identity.Version, PromptSHA256: identity.SHA256,
		ImageDigest: sessionImageDigestForTest, WorkerID: "session-1-ideation", ResultDirectory: resultDirectory,
	})
	if err != nil {
		t.Fatal(err)
	}
	return invocation
}

// TestSessionRolesPlanFromTheFrozenBrief verifies that planning receives the
// frozen brief without the conversation, that an accepted plan records the
// pinned brief and context revisions and moves the session to draft review,
// and that the plan validation seam can reject a draft.
func TestSessionRolesPlanFromTheFrozenBrief(t *testing.T) {
	fixture := newSessionFixture(t, config.HarnessClaude)
	write, err := workflow.DefaultSessionLifecycle().Write(fixture.session(), workflow.SessionEventEdited, "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.store.SaveBriefRevision(t.Context(), write, store.Brief{Problem: "No export.", Scope: []store.BriefItem{{ID: "json", Text: "JSON export"}}}); err != nil {
		t.Fatal(err)
	}
	roles := fixture.roles()
	planning, err := roles.StartPlanning(t.Context(), "session-1", "handoff-1")
	if err != nil {
		t.Fatalf("StartPlanning() error = %v", err)
	}
	var packet struct {
		BriefRevision int64           `json:"brief_revision"`
		Brief         *report.Brief   `json:"brief"`
		Turns         json.RawMessage `json:"turns"`
	}
	data, err := os.ReadFile(filepath.Join(planning.InvocationDirectory, "session.json"))
	if err != nil || json.Unmarshal(data, &packet) != nil || packet.BriefRevision != 1 || packet.Brief == nil || packet.Brief.Problem != "No export." || len(packet.Turns) != 0 {
		t.Fatalf("planning packet = %s, %v; want the frozen brief and no conversation", data, err)
	}
	fixture.report(planning, report.Report{Outcome: report.OutcomeCompleted, PlanDraft: &report.PlanDraft{Nodes: []report.PlanNode{
		{ID: "export", Kind: "task", Title: "Add the export command", AcceptanceCriteria: []string{"factory export writes JSON"}, BriefRefs: []string{"json"}},
	}}})
	if err := roles.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	history, err := fixture.store.IdeationHistory(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if history.Session.State != store.IdeationStateDraftReview || len(history.Plans) != 1 ||
		history.Plans[0].SourceBriefRevision != 1 || history.Plans[0].ContextRevision != sessionContextRevisionForTest {
		t.Fatalf("history after planning = %#v, want one pinned draft in review", history)
	}

	fixture.validator = func([]store.PlanNode) error { return errors.New("plan has a dependency cycle") }
	rejected, err := fixture.roles().StartPlanning(t.Context(), "session-1", "regenerate-1")
	if err != nil {
		t.Fatalf("StartPlanning(regenerate) error = %v", err)
	}
	fixture.report(rejected, report.Report{Outcome: report.OutcomeCompleted, PlanDraft: &report.PlanDraft{Nodes: []report.PlanNode{{ID: "a", Kind: "task", Title: "A"}}}})
	if err := fixture.roles().Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := fixture.invocation(rejected.ID)
	if got.Status != store.SessionInvocationRejected || got.FailureKind != "plan_rejected" || fixture.session().PlanRevision != 1 ||
		fixture.session().State != store.IdeationStateDraftReview {
		t.Fatalf("rejected plan invocation = %#v, session = %#v; want the draft kept in review", got, fixture.session())
	}
}

// TestSessionRolesFailClosedWithoutRolePolicy verifies that a repository that
// declares no harness for a session role gets a typed policy rejection and an
// unchanged session.
func TestSessionRolesFailClosedWithoutRolePolicy(t *testing.T) {
	fixture := newSessionFixture(t, config.HarnessCodex)
	delete(fixture.policy.RoleHarnessDefaults, workflow.RoleIdeation)
	delete(fixture.policy.ModelOptions, workflow.RoleIdeation)
	before := fixture.session()
	_, err := fixture.roles().StartIdeationTurn(t.Context(), "session-1", "turn-1")
	var rejection *factory.PolicyRejection
	if !errors.As(err, &rejection) {
		t.Fatalf("StartIdeationTurn() error = %v, want a PolicyRejection", err)
	}
	if after := fixture.session(); after.Revision != before.Revision || len(fixture.worker.starts) != 0 {
		t.Fatalf("session changed to %#v or a worker started; want no effect", after)
	}
}

// snapshotWorktreeForTest adds the run worktree seam to snapshotsForTest, as
// the production Git adapter provides both.
type snapshotWorktreeForTest struct {
	*snapshotsForTest
}

// Create is unused by session roles.
func (snapshotWorktreeForTest) Create(context.Context, string, string, string) (gitadapter.Workspace, error) {
	return gitadapter.Workspace{}, errors.New("unused")
}

// Remove is unused by session roles.
func (snapshotWorktreeForTest) Remove(context.Context, string, gitadapter.Workspace) error {
	return nil
}

// TestServiceDrivesSessionRolesFromTheSupervisor verifies that the composed
// coordinator starts a session role through the registered repository policy
// and that its supervisor reconciliation applies the report, so neither a
// browser nor a terminal is needed to continue a session.
func TestServiceDrivesSessionRolesFromTheSupervisor(t *testing.T) {
	fixture := newSessionFixture(t, config.HarnessCodex)
	host := config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{{
		Path: fixture.repo, OperationalDataPath: fixture.path, RepositoryConfigPath: filepath.Join(fixture.repo, "factory.yaml"),
	}}}
	ids := 0
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:            &fakeConfig{value: host},
		OpenStore:         func(ctx context.Context, path string) (factory.OperationalStore, error) { return store.Open(ctx, path) },
		LoadRepository:    func(string) (config.RepositoryConfig, error) { return fixture.policy, nil },
		Worker:            fixture.worker,
		HeadlessHarnesses: harness.NewHeadlessAdapters(fixture.worker),
		Worktree:          snapshotWorktreeForTest{fixture.snapshots},
		Now:               func() time.Time { return time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC) },
		NewRunID: func() (string, error) {
			ids++
			return "svc-" + string(rune('0'+ids)), nil
		},
	})
	var invocation store.SessionInvocation
	if err := service.WithSessionRoles(t.Context(), func(roles *factory.SessionRoles) error {
		var err error
		invocation, err = roles.StartIdeationTurn(t.Context(), "session-1", "turn-1")
		return err
	}); err != nil {
		t.Fatalf("WithSessionRoles(start) error = %v", err)
	}
	fixture.report(invocation, ideationResultForTest())
	if err := service.ReconcileSessionRoles(t.Context()); err != nil {
		t.Fatalf("ReconcileSessionRoles() error = %v", err)
	}
	if session := fixture.session(); session.State != store.IdeationStateAwaitingUser || session.BriefRevision != 1 {
		t.Fatalf("session after supervisor reconciliation = %#v, want the result applied", session)
	}
}

// TestIdeationPacketMarksACorrectedAnswer verifies that the frozen session
// packet names each turn and the earlier answer that a correction turn
// changes, so the ideation role can revisit the decisions that depend on it.
func TestIdeationPacketMarksACorrectedAnswer(t *testing.T) {
	fixture := newSessionFixture(t, config.HarnessCodex)
	roles := fixture.roles()
	first, err := roles.StartIdeationTurn(t.Context(), "session-1", "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	fixture.report(first, ideationResultForTest())
	if err := roles.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	write, err := workflow.DefaultSessionLifecycle().Write(fixture.session(), workflow.SessionEventTurnAccepted, "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.store.AppendIdeationTurn(t.Context(), write, store.IdeationTurn{
		ID: "turn-2", Author: store.IdeationTurnAuthorUser, Body: "Support engineers export it, not operators.", Corrects: "turn-1",
	}); err != nil {
		t.Fatal(err)
	}
	second, err := fixture.roles().StartIdeationTurn(t.Context(), "session-1", "turn-2")
	if err != nil {
		t.Fatalf("StartIdeationTurn(turn-2) error = %v", err)
	}
	var packet struct {
		Turns []struct {
			ID       string `json:"id"`
			Author   string `json:"author"`
			Corrects string `json:"corrects"`
		} `json:"turns"`
	}
	data, err := os.ReadFile(filepath.Join(second.InvocationDirectory, "session.json"))
	if err != nil || json.Unmarshal(data, &packet) != nil {
		t.Fatalf("read packet: %v", err)
	}
	if len(packet.Turns) != 3 || packet.Turns[0].ID != "turn-1" || packet.Turns[2].ID != "turn-2" ||
		packet.Turns[2].Corrects != "turn-1" || packet.Turns[0].Corrects != "" {
		t.Fatalf("packet turns = %s, want turn-2 marked as the correction of turn-1", data)
	}
}
