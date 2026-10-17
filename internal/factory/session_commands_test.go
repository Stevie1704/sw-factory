package factory_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// sessionCommandNow is the fixed UI clock of the session command tests.
var sessionCommandNow = time.Date(2026, time.October, 15, 12, 0, 0, 0, time.UTC)

// sessionCommandFixture is one registered repository with a real store that
// holds one awaiting-user session, and a UI-side service over it.
type sessionCommandFixture struct {
	t          *testing.T
	configPath string
	storePath  string
	service    *factory.Service
}

// newSessionCommandFixture writes a host configuration and a store with
// session-1 at revision 1.
func newSessionCommandFixture(t *testing.T) sessionCommandFixture {
	t.Helper()
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repo")
	if err := os.MkdirAll(repositoryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(root, "state", "factory.db")
	configPath := filepath.Join(root, "config", "host.yaml")
	if err := config.SaveHost(configPath, config.HostConfig{
		SchemaVersion: config.CurrentHostSchemaVersion,
		Repositories: []config.RepositoryRegistration{{
			Path: repositoryPath, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
			AuthorizedUsers: []string{"alice"}, Polling: config.PollingConfig{Interval: "30s", Backoff: "5m"},
			OperationalDataPath: storePath, RepositoryConfigPath: filepath.Join(repositoryPath, config.RepositoryConfigFileName),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	opened, err := store.Open(t.Context(), storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	if _, err := opened.CreateIdeationSession(t.Context(), store.IdeationSession{
		ID: "session-1", RepositoryPath: repositoryPath, Title: "Faster onboarding", State: store.IdeationStateAwaitingUser, CreatedAt: sessionCommandNow,
	}); err != nil {
		t.Fatal(err)
	}
	service := factory.NewWithDependencies(configPath, factory.Dependencies{Now: func() time.Time { return sessionCommandNow }})
	return sessionCommandFixture{t: t, configPath: configPath, storePath: storePath, service: service}
}

// withStore opens the store read-write for one setup or check step.
func (f sessionCommandFixture) withStore(use func(*store.Store)) {
	f.t.Helper()
	opened, err := store.Open(f.t.Context(), f.storePath)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	use(opened)
}

// heartbeat records a coordinator heartbeat that expires at expiresAt.
func (f sessionCommandFixture) heartbeat(expiresAt time.Time) {
	f.t.Helper()
	f.withStore(func(opened *store.Store) {
		if err := opened.SaveSupervisorHeartbeat(f.t.Context(), store.SupervisorHeartbeat{
			Coordinator: "coordinator-test", PID: 4242, StartedAt: expiresAt.Add(-time.Hour), RenewedAt: expiresAt.Add(-time.Minute), ExpiresAt: expiresAt,
		}); err != nil {
			f.t.Fatal(err)
		}
	})
}

// turnCommand returns a parsed submit_turn command for session-1.
func turnCommand(t *testing.T, requestID string, expected int64) store.IdeationCommand {
	t.Helper()
	command, err := sessioncommand.ParseRequest([]byte(`{"request_id":"` + requestID + `","session_id":"session-1","kind":"submit_turn","expected_revision":` +
		strconv.FormatInt(expected, 10) + `,"payload":{"body":"Operators wait for images."}}`))
	if err != nil {
		t.Fatal(err)
	}
	return command
}

// TestSessionCommandIsUnavailableWithoutALiveSupervisor verifies that a
// stopped or expired coordinator yields an explicit unavailable result and
// that nothing is appended for a second writer to find.
func TestSessionCommandIsUnavailableWithoutALiveSupervisor(t *testing.T) {
	t.Parallel()

	f := newSessionCommandFixture(t)
	_, err := f.service.SubmitSessionCommand(t.Context(), turnCommand(t, "request-1", 1))
	var unavailable *factory.SupervisorUnavailableError
	if !errors.As(err, &unavailable) || !errors.Is(err, factory.ErrSupervisorUnavailable) {
		t.Fatalf("SubmitSessionCommand() without heartbeat error = %v, want SupervisorUnavailableError", err)
	}

	f.heartbeat(sessionCommandNow.Add(-time.Second))
	if _, err := f.service.SubmitSessionCommand(t.Context(), turnCommand(t, "request-1", 1)); !errors.Is(err, factory.ErrSupervisorUnavailable) {
		t.Fatalf("SubmitSessionCommand() with expired heartbeat error = %v, want ErrSupervisorUnavailable", err)
	}
	f.withStore(func(opened *store.Store) {
		if pending, err := opened.PendingIdeationCommands(t.Context()); err != nil || len(pending) != 0 {
			t.Fatalf("pending = %#v, %v; want an empty inbox", pending, err)
		}
	})
}

// TestSessionCommandIsAcceptedDurablyAndReplayedWithItsOutcome verifies that
// acceptance is not completion, that the UI never changes the session, that a
// repeated request returns the stored outcome even after the coordinator
// stopped, and that a stale tab gets a conflict.
func TestSessionCommandIsAcceptedDurablyAndReplayedWithItsOutcome(t *testing.T) {
	t.Parallel()

	f := newSessionCommandFixture(t)
	f.heartbeat(sessionCommandNow.Add(time.Minute))
	ack, err := f.service.SubmitSessionCommand(t.Context(), turnCommand(t, "request-1", 1))
	if err != nil {
		t.Fatalf("SubmitSessionCommand() error = %v", err)
	}
	if ack.Replayed || ack.Command.State != store.IdeationCommandAccepted || ack.Command.Outcome != "" {
		t.Fatalf("ack = %#v, want a new accepted command without an outcome", ack)
	}
	f.withStore(func(opened *store.Store) {
		session, err := opened.IdeationSession(t.Context(), "session-1")
		if err != nil || session.Revision != 1 {
			t.Fatalf("session = %#v, %v; the UI must not change it", session, err)
		}
		if _, err := opened.CompleteIdeationCommand(t.Context(), store.IdeationCommandResult{
			RequestID: "request-1", Outcome: store.IdeationCommandApplied, SessionRevision: 2, At: sessionCommandNow,
		}); err != nil {
			t.Fatal(err)
		}
	})

	f.heartbeat(sessionCommandNow.Add(-time.Second))
	replay, err := f.service.SubmitSessionCommand(t.Context(), turnCommand(t, "request-1", 1))
	if err != nil {
		t.Fatalf("repeated SubmitSessionCommand() error = %v", err)
	}
	if !replay.Replayed || replay.Command.Outcome != store.IdeationCommandApplied || replay.Command.SessionRevision != 2 {
		t.Fatalf("replay = %#v, want the stored applied outcome", replay)
	}
	changed := turnCommand(t, "request-1", 2)
	if _, err := f.service.SubmitSessionCommand(t.Context(), changed); !errors.Is(err, store.ErrIdeationRequestReused) {
		t.Fatalf("reused request error = %v, want ErrIdeationRequestReused", err)
	}

	f.heartbeat(sessionCommandNow.Add(time.Minute))
	f.withStore(func(opened *store.Store) {
		session, _ := opened.IdeationSession(t.Context(), "session-1")
		if _, err := opened.TransitionIdeationSession(t.Context(), store.IdeationWrite{
			SessionID: session.ID, ExpectedRevision: session.Revision, State: store.IdeationStateDeferred, At: sessionCommandNow,
		}); err != nil {
			t.Fatal(err)
		}
	})
	_, err = f.service.SubmitSessionCommand(t.Context(), turnCommand(t, "request-stale", 1))
	var conflict *store.IdeationRevisionConflictError
	if !errors.As(err, &conflict) || conflict.Actual != 2 || conflict.Expected != 1 {
		t.Fatalf("stale SubmitSessionCommand() error = %v, want a conflict at revision 2", err)
	}
	unknown := turnCommand(t, "request-unknown", 1)
	unknown.SessionID = "session-missing"
	if _, err := f.service.SubmitSessionCommand(t.Context(), unknown); !errors.Is(err, store.ErrIdeationSessionNotFound) {
		t.Fatalf("unknown session error = %v, want ErrIdeationSessionNotFound", err)
	}
}

// TestSessionActivityReloadsAcceptedResultsWhileTheSupervisorIsStopped
// verifies the read model a reconnecting browser uses: it reads the session,
// its history, and its command outcomes without a live coordinator.
func TestSessionActivityReloadsAcceptedResultsWhileTheSupervisorIsStopped(t *testing.T) {
	t.Parallel()

	f := newSessionCommandFixture(t)
	f.heartbeat(sessionCommandNow.Add(time.Minute))
	if _, err := f.service.SubmitSessionCommand(t.Context(), turnCommand(t, "request-1", 1)); err != nil {
		t.Fatal(err)
	}
	f.heartbeat(sessionCommandNow.Add(-time.Second))

	activity, err := f.service.SessionActivity(t.Context(), "session-1")
	if err != nil {
		t.Fatalf("SessionActivity() error = %v", err)
	}
	if activity.Supervisor.Live || activity.History.Session.ID != "session-1" || len(activity.Commands) != 1 ||
		activity.Commands[0].RequestID != "request-1" || activity.Commands[0].State != store.IdeationCommandAccepted {
		t.Fatalf("activity = %#v, want the session and its accepted command", activity)
	}
	view, err := f.service.SessionCommand(t.Context(), "request-1")
	if err != nil || view.Command.RequestID != "request-1" {
		t.Fatalf("SessionCommand() = %#v, %v", view, err)
	}
	overview, err := f.service.SessionOverview(t.Context())
	if err != nil || len(overview.Sessions) != 1 || overview.Sessions[0].ID != "session-1" {
		t.Fatalf("SessionOverview() = %#v, %v", overview, err)
	}
	if _, err := f.service.SessionActivity(t.Context(), "session-missing"); !errors.Is(err, store.ErrIdeationSessionNotFound) {
		t.Fatalf("SessionActivity(missing) error = %v, want ErrIdeationSessionNotFound", err)
	}
}

// sharedStore lets the polling fixture hand one real store to every opener
// call without letting a caller close it.
type sharedStore struct {
	*store.Store
}

// Close keeps the shared store open.
func (sharedStore) Close() error { return nil }

// TestStartAppliesSessionCommandsAcceptedWhileItWasStopped verifies that the
// coordinator drains the inbox on start, so commands accepted before a
// restart complete without a browser connection.
func TestStartAppliesSessionCommandsAcceptedWhileItWasStopped(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	opened, err := store.Open(t.Context(), filepath.Join(root, "state", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	if _, err := opened.CreateIdeationSession(t.Context(), store.IdeationSession{
		ID: "session-1", RepositoryPath: filepath.Join(root, "repository"), State: store.IdeationStateAwaitingUser, CreatedAt: sessionCommandNow,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := opened.AcceptIdeationCommand(t.Context(), turnCommand(t, "request-1", 1)); err != nil {
		t.Fatal(err)
	}

	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	lease := &pollingLease{onRenew: func(tracker.Lease) {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if command, err := opened.IdeationCommand(t.Context(), "request-1"); err == nil && command.State == store.IdeationCommandCompleted {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		stop()
	}}
	service := newPollingService(root, &fakeGitHub{}, &pollingIssueReader{}, lease, sharedStore{opened}, &fakeWorktree{}, nil)
	if err := service.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	command, err := opened.IdeationCommand(t.Context(), "request-1")
	if err != nil || command.Outcome != store.IdeationCommandApplied {
		t.Fatalf("command = %#v, %v; want applied by the coordinator", command, err)
	}
	history, err := opened.IdeationHistory(t.Context(), "session-1")
	if err != nil || len(history.Turns) != 1 {
		t.Fatalf("turns = %#v, %v; want one accepted user turn", history.Turns, err)
	}
}
