package store_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// ideationNow is the fixed clock used by ideation-session tests.
var ideationNow = time.Date(2026, time.October, 15, 9, 0, 0, 0, time.UTC)

// openIdeationStoreForTest opens a fresh operational store in a private
// directory and returns the store and its path for restart round trips.
func openIdeationStoreForTest(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	return opened, path
}

// reopenIdeationStoreForTest closes opened and opens the same path again, as a
// coordinator restart does.
func reopenIdeationStoreForTest(t *testing.T, opened *store.Store, path string) *store.Store {
	t.Helper()
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	return reopened
}

// createIdeationSessionForTest persists one awaiting-user session.
func createIdeationSessionForTest(t *testing.T, opened *store.Store, id string) store.IdeationSession {
	t.Helper()
	session, err := opened.CreateIdeationSession(t.Context(), store.IdeationSession{
		ID:             id,
		RepositoryPath: "/repos/factory",
		Title:          "Faster onboarding",
		State:          store.IdeationStateAwaitingUser,
		CreatedAt:      ideationNow,
	})
	if err != nil {
		t.Fatalf("CreateIdeationSession() error = %v", err)
	}
	return session
}

// ideationWrite returns a write guard that keeps the session in state at the
// expected revision.
func ideationWrite(session store.IdeationSession, state store.IdeationState) store.IdeationWrite {
	return store.IdeationWrite{
		SessionID:        session.ID,
		ExpectedRevision: session.Revision,
		State:            state,
		At:               ideationNow.Add(time.Duration(session.Revision) * time.Minute),
	}
}

// TestIdeationSessionIsCreatedWithoutARemoteIssueAndSurvivesRestart verifies
// that a repository-scoped session needs no issue or run and can be resumed
// and listed after the store is reopened.
func TestIdeationSessionIsCreatedWithoutARemoteIssueAndSurvivesRestart(t *testing.T) {
	t.Parallel()

	opened, path := openIdeationStoreForTest(t)
	created := createIdeationSessionForTest(t, opened, "session-1")
	if created.Revision != 1 || created.BriefRevision != 0 || created.PlanRevision != 0 || created.ApprovalID != "" {
		t.Fatalf("created session = %#v, want revision 1 and no derived revisions", created)
	}
	if run, err := opened.CurrentRun(t.Context()); err != nil || run != nil {
		t.Fatalf("CurrentRun() = %#v, %v; a session must not create a run", run, err)
	}

	reopened := reopenIdeationStoreForTest(t, opened, path)
	resumed, err := reopened.IdeationSession(t.Context(), "session-1")
	if err != nil {
		t.Fatalf("IdeationSession() error = %v", err)
	}
	if resumed.ID != "session-1" || resumed.RepositoryPath != "/repos/factory" || resumed.Title != "Faster onboarding" ||
		resumed.State != store.IdeationStateAwaitingUser || resumed.Revision != 1 || !resumed.CreatedAt.Equal(ideationNow) {
		t.Fatalf("resumed session = %#v", resumed)
	}
	listed, err := reopened.IdeationSessions(t.Context(), "/repos/factory")
	if err != nil {
		t.Fatalf("IdeationSessions() error = %v", err)
	}
	if len(listed) != 1 || listed[0].ID != "session-1" {
		t.Fatalf("IdeationSessions() = %#v, want session-1", listed)
	}
	other, err := reopened.IdeationSessions(t.Context(), "/repos/other")
	if err != nil || len(other) != 0 {
		t.Fatalf("IdeationSessions(other) = %#v, %v; want none", other, err)
	}
}

// TestIdeationSessionRejectsMissingAndDuplicateIdentities verifies the typed
// errors for an unknown session and a reused session identity.
func TestIdeationSessionRejectsMissingAndDuplicateIdentities(t *testing.T) {
	t.Parallel()

	opened, _ := openIdeationStoreForTest(t)
	if _, err := opened.IdeationSession(t.Context(), "absent"); !errors.Is(err, store.ErrIdeationSessionNotFound) {
		t.Fatalf("IdeationSession(absent) error = %v, want ErrIdeationSessionNotFound", err)
	}
	createIdeationSessionForTest(t, opened, "session-1")
	_, err := opened.CreateIdeationSession(t.Context(), store.IdeationSession{
		ID: "session-1", RepositoryPath: "/repos/factory", State: store.IdeationStateAwaitingUser,
	})
	if !errors.Is(err, store.ErrIdeationSessionExists) {
		t.Fatalf("duplicate CreateIdeationSession() error = %v, want ErrIdeationSessionExists", err)
	}
	_, err = opened.CreateIdeationSession(t.Context(), store.IdeationSession{
		ID: "session-2", RepositoryPath: "/repos/factory", State: "running",
	})
	if err == nil {
		t.Fatal("CreateIdeationSession() accepted an undeclared state")
	}
}

// TestSchema42MigrationAddsIdeationTablesAndKeepsStoredRuns verifies that an
// older store gains the ideation tables while its existing runs stay readable.
func TestSchema42MigrationAddsIdeationTablesAndKeepsStoredRuns(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	run := store.Run{
		ID: "run-1", RepositoryPath: "/repos/factory", IssueNumber: 7,
		Stage: store.StageImplementation, Status: store.StatusActive, Branch: "factory/run-1",
		Worktree: "/worktrees/run-1", Revision: 3,
		CreatedAt: ideationNow, UpdatedAt: ideationNow,
	}
	if err := opened.SaveRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(t.Context(), `
		DROP TABLE ideation_sessions;
		DROP TABLE ideation_turns;
		DROP TABLE ideation_brief_revisions;
		DROP TABLE ideation_plan_revisions;
		DROP TABLE ideation_approvals;
		DROP TABLE ideation_publication_receipts;
		UPDATE schema_metadata SET version = 41 WHERE singleton = 1`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open() migration error = %v", err)
	}
	defer func() { _ = migrated.Close() }()
	if got := migrated.SchemaVersion(); got != store.CurrentSchemaVersion {
		t.Fatalf("SchemaVersion() = %d, want %d", got, store.CurrentSchemaVersion)
	}
	current, err := migrated.CurrentRun(t.Context())
	if err != nil {
		t.Fatalf("CurrentRun() after migration error = %v", err)
	}
	if current == nil || current.ID != "run-1" || current.IssueNumber != 7 || current.Revision != 3 {
		t.Fatalf("CurrentRun() after migration = %#v, want run-1", current)
	}
	sessions, err := migrated.IdeationSessions(t.Context(), "/repos/factory")
	if err != nil || len(sessions) != 0 {
		t.Fatalf("IdeationSessions() after migration = %#v, %v; want empty", sessions, err)
	}
	createIdeationSessionForTest(t, migrated, "session-1")
}
