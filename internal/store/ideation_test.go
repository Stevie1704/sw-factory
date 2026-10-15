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

// appendUserTurnForTest appends one user turn and returns the new session.
func appendUserTurnForTest(t *testing.T, opened *store.Store, session store.IdeationSession, id, body string) (store.IdeationSession, store.IdeationTurn) {
	t.Helper()
	updated, turn, err := opened.AppendIdeationTurn(t.Context(), ideationWrite(session, store.IdeationStateAwaitingUser), store.IdeationTurn{
		ID: id, Author: store.IdeationTurnAuthorUser, Body: body,
	})
	if err != nil {
		t.Fatalf("AppendIdeationTurn(%s) error = %v", id, err)
	}
	return updated, turn
}

// TestIdeationTurnsKeepOrderAndRetriesDoNotAppendTwice verifies that accepted
// turns have stable IDs and sequence numbers and that a retried user turn
// returns the original turn instead of a second one, also after a restart.
func TestIdeationTurnsKeepOrderAndRetriesDoNotAppendTwice(t *testing.T) {
	t.Parallel()

	opened, path := openIdeationStoreForTest(t)
	session := createIdeationSessionForTest(t, opened, "session-1")
	afterFirst, first := appendUserTurnForTest(t, opened, session, "turn-a", "Onboarding takes too long.")
	afterRole, roleTurn, err := opened.AppendIdeationTurn(t.Context(), ideationWrite(afterFirst, store.IdeationStateAwaitingUser), store.IdeationTurn{
		ID: "turn-b", Author: store.IdeationTurnAuthorRole, Role: "ideation", InvocationID: "invocation-1",
		Body: "Who is the first user?",
	})
	if err != nil {
		t.Fatalf("AppendIdeationTurn(role) error = %v", err)
	}
	if first.Sequence != 1 || roleTurn.Sequence != 2 || first.SessionRevision != 2 || roleTurn.SessionRevision != 3 {
		t.Fatalf("turn order = (%d, rev %d), (%d, rev %d); want sequences 1, 2 at revisions 2, 3",
			first.Sequence, first.SessionRevision, roleTurn.Sequence, roleTurn.SessionRevision)
	}
	if afterRole.Revision != 3 {
		t.Fatalf("session revision = %d, want 3", afterRole.Revision)
	}

	reopened := reopenIdeationStoreForTest(t, opened, path)
	replayedSession, replayed, err := reopened.AppendIdeationTurn(t.Context(), ideationWrite(session, store.IdeationStateAwaitingUser), store.IdeationTurn{
		ID: "turn-a", Author: store.IdeationTurnAuthorUser, Body: "Onboarding takes too long.",
	})
	if err != nil {
		t.Fatalf("retried AppendIdeationTurn() error = %v", err)
	}
	if replayed.Sequence != 1 || replayed.SessionRevision != 2 || replayedSession.Revision != 3 {
		t.Fatalf("retry = turn %#v session revision %d; want the original turn and an unchanged session", replayed, replayedSession.Revision)
	}
	if _, _, err := reopened.AppendIdeationTurn(t.Context(), ideationWrite(afterRole, store.IdeationStateAwaitingUser), store.IdeationTurn{
		ID: "turn-a", Author: store.IdeationTurnAuthorUser, Body: "A different answer.",
	}); !errors.Is(err, store.ErrIdeationRequestReused) {
		t.Fatalf("reused turn ID with new content error = %v, want ErrIdeationRequestReused", err)
	}

	history, err := reopened.IdeationHistory(t.Context(), "session-1")
	if err != nil {
		t.Fatalf("IdeationHistory() error = %v", err)
	}
	if len(history.Turns) != 2 || history.Turns[0].ID != "turn-a" || history.Turns[1].ID != "turn-b" ||
		history.Turns[1].Role != "ideation" || history.Turns[1].InvocationID != "invocation-1" ||
		!history.Turns[0].CreatedAt.Equal(ideationNow.Add(time.Minute)) {
		t.Fatalf("history turns = %#v", history.Turns)
	}
	if history.Session.Revision != 3 {
		t.Fatalf("history session revision = %d, want 3", history.Session.Revision)
	}
}

// TestIdeationWriteRejectsAStaleRevision verifies that a competing tab or a
// late role report that read an older revision cannot overwrite newer work and
// receives the typed conflict.
func TestIdeationWriteRejectsAStaleRevision(t *testing.T) {
	t.Parallel()

	opened, _ := openIdeationStoreForTest(t)
	session := createIdeationSessionForTest(t, opened, "session-1")
	appendUserTurnForTest(t, opened, session, "turn-a", "First tab.")

	_, _, err := opened.AppendIdeationTurn(t.Context(), ideationWrite(session, store.IdeationStateAwaitingUser), store.IdeationTurn{
		ID: "turn-late", Author: store.IdeationTurnAuthorRole, Role: "ideation", Body: "Late report.",
	})
	var conflict *store.IdeationRevisionConflictError
	if !errors.As(err, &conflict) || !errors.Is(err, store.ErrIdeationRevisionConflict) {
		t.Fatalf("stale AppendIdeationTurn() error = %v, want IdeationRevisionConflictError", err)
	}
	if conflict.Expected != 1 || conflict.Actual != 2 || conflict.SessionID != "session-1" {
		t.Fatalf("conflict = %#v, want expected 1 actual 2", conflict)
	}
	if _, err := opened.TransitionIdeationSession(t.Context(), ideationWrite(session, store.IdeationStateCancelled)); !errors.Is(err, store.ErrIdeationRevisionConflict) {
		t.Fatalf("stale TransitionIdeationSession() error = %v, want conflict", err)
	}
	history, err := opened.IdeationHistory(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Turns) != 1 || history.Session.State != store.IdeationStateAwaitingUser || history.Session.Revision != 2 {
		t.Fatalf("history after stale writes = %#v", history)
	}
}

// briefForTest returns a complete brief whose items the plan nodes trace to.
func briefForTest(problem string) store.Brief {
	return store.Brief{
		Problem:             problem,
		Audience:            "New maintainers",
		SuccessCriteria:     []store.BriefItem{{ID: "sc-1", Text: "Setup finishes in ten minutes."}},
		Scope:               []store.BriefItem{{ID: "scope-1", Text: "The onboarding command."}},
		NonGoals:            []store.BriefItem{{ID: "ng-1", Text: "Remote installers."}},
		Decisions:           []store.BriefDecision{{ID: "d-1", Decision: "Keep one command.", Rationale: "Fewer steps."}},
		Assumptions:         []store.BriefItem{{ID: "a-1", Text: "Docker is installed."}},
		Evidence:            []store.BriefItem{{ID: "e-1", Text: "Three reports in #12."}},
		UnresolvedQuestions: []store.BriefItem{{ID: "q-1", Text: "Do we support Windows?"}},
	}
}

// planNodesForTest returns a parent with one dependent child.
func planNodesForTest() []store.PlanNode {
	return []store.PlanNode{
		{ID: "n-1", Kind: "feature", Title: "Faster onboarding", Body: "Parent.", AcceptanceCriteria: []string{"Setup is one command."}, BriefRefs: []string{"sc-1"}},
		{ID: "n-2", Kind: "issue", Title: "Cache images", Body: "Child.", AcceptanceCriteria: []string{"Second setup is offline."}, ParentID: "n-1", BriefRefs: []string{"scope-1", "d-1"}},
		{ID: "n-3", Kind: "issue", Title: "Document setup", Body: "Child.", AcceptanceCriteria: []string{"Guide names the command."}, ParentID: "n-1", DependsOn: []string{"n-2"}},
	}
}

// saveBriefAndPlanForTest stores brief revision one and a plan derived from it.
func saveBriefAndPlanForTest(t *testing.T, opened *store.Store, session store.IdeationSession) (store.IdeationSession, store.PlanRevision) {
	t.Helper()
	session, brief, err := opened.SaveBriefRevision(t.Context(), ideationWrite(session, store.IdeationStateAwaitingUser), briefForTest("Onboarding is slow."))
	if err != nil {
		t.Fatalf("SaveBriefRevision() error = %v", err)
	}
	session, plan, err := opened.SavePlanRevision(t.Context(), ideationWrite(session, store.IdeationStateDraftReview), store.PlanRevision{
		SourceBriefRevision: brief.Revision, ContextRevision: "0123456789abcdef0123456789abcdef01234567", Nodes: planNodesForTest(),
	})
	if err != nil {
		t.Fatalf("SavePlanRevision() error = %v", err)
	}
	return session, plan
}

// TestBriefRevisionChangeMarksPriorDraftsStaleWithoutChangingThem verifies that
// briefs and plans are versioned, that a plan records its source brief and
// context revision, and that a newer brief makes the old draft visibly stale
// while its stored content stays the same after a restart.
func TestBriefRevisionChangeMarksPriorDraftsStaleWithoutChangingThem(t *testing.T) {
	t.Parallel()

	opened, path := openIdeationStoreForTest(t)
	session := createIdeationSessionForTest(t, opened, "session-1")
	session, plan := saveBriefAndPlanForTest(t, opened, session)
	if session.BriefRevision != 1 || session.PlanRevision != 1 || plan.Revision != 1 || plan.SourceBriefRevision != 1 || plan.Stale {
		t.Fatalf("session = %#v plan = %#v, want brief 1 and fresh plan 1", session, plan)
	}
	session, second, err := opened.SaveBriefRevision(t.Context(), ideationWrite(session, store.IdeationStateDraftReview), briefForTest("Onboarding is slow and fragile."))
	if err != nil {
		t.Fatalf("second SaveBriefRevision() error = %v", err)
	}
	if second.Revision != 2 || session.BriefRevision != 2 {
		t.Fatalf("second brief revision = %d, session brief = %d; want 2", second.Revision, session.BriefRevision)
	}

	reopened := reopenIdeationStoreForTest(t, opened, path)
	history, err := reopened.IdeationHistory(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Briefs) != 2 || history.Briefs[0].Brief.Problem != "Onboarding is slow." ||
		history.Briefs[1].Brief.Problem != "Onboarding is slow and fragile." || history.Briefs[0].Brief.Decisions[0].Rationale != "Fewer steps." {
		t.Fatalf("history briefs = %#v", history.Briefs)
	}
	if len(history.Plans) != 1 || !history.Plans[0].Stale || history.Plans[0].ContextRevision != plan.ContextRevision ||
		history.Plans[0].Nodes[2].DependsOn[0] != "n-2" || history.Plans[0].Nodes[1].BriefRefs[1] != "d-1" {
		t.Fatalf("history plans = %#v, want the unchanged plan marked stale", history.Plans)
	}

	if _, _, err := reopened.SavePlanRevision(t.Context(), ideationWrite(history.Session, store.IdeationStateDraftReview), store.PlanRevision{
		SourceBriefRevision: 1, ContextRevision: plan.ContextRevision, Nodes: planNodesForTest(),
	}); !errors.Is(err, store.ErrStalePlanSource) {
		t.Fatalf("plan from an old brief error = %v, want ErrStalePlanSource", err)
	}
}

// TestPlanRevisionRejectsBrokenReferences verifies the structural checks the
// store owns: unique node IDs, resolvable parent, dependency, and brief
// references, and no self references.
func TestPlanRevisionRejectsBrokenReferences(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func([]store.PlanNode) []store.PlanNode
	}{
		{"duplicate id", func(nodes []store.PlanNode) []store.PlanNode { nodes[1].ID = "n-1"; return nodes }},
		{"dangling parent", func(nodes []store.PlanNode) []store.PlanNode { nodes[1].ParentID = "n-9"; return nodes }},
		{"self parent", func(nodes []store.PlanNode) []store.PlanNode { nodes[0].ParentID = "n-1"; return nodes }},
		{"dangling dependency", func(nodes []store.PlanNode) []store.PlanNode { nodes[2].DependsOn = []string{"n-9"}; return nodes }},
		{"self dependency", func(nodes []store.PlanNode) []store.PlanNode { nodes[2].DependsOn = []string{"n-3"}; return nodes }},
		{"unknown brief item", func(nodes []store.PlanNode) []store.PlanNode { nodes[0].BriefRefs = []string{"sc-9"}; return nodes }},
		{"missing title", func(nodes []store.PlanNode) []store.PlanNode { nodes[0].Title = " "; return nodes }},
		{"missing kind", func(nodes []store.PlanNode) []store.PlanNode { nodes[0].Kind = ""; return nodes }},
		{"no nodes", func([]store.PlanNode) []store.PlanNode { return nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			opened, _ := openIdeationStoreForTest(t)
			session := createIdeationSessionForTest(t, opened, "session-1")
			session, _, err := opened.SaveBriefRevision(t.Context(), ideationWrite(session, store.IdeationStateAwaitingUser), briefForTest("Problem."))
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = opened.SavePlanRevision(t.Context(), ideationWrite(session, store.IdeationStateDraftReview), store.PlanRevision{
				SourceBriefRevision: 1, ContextRevision: "abc1234", Nodes: test.mutate(planNodesForTest()),
			})
			if err == nil {
				t.Fatal("SavePlanRevision() accepted a broken plan graph")
			}
		})
	}
}
