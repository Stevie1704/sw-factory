package store_test

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// contextRevisionForTest is the pinned repository commit of test invocations.
const contextRevisionForTest = "0123456789abcdef0123456789abcdef01234567"

// sessionInvocationForTest returns a pinned ideation invocation for trigger.
func sessionInvocationForTest(id, trigger string) store.SessionInvocation {
	return store.SessionInvocation{
		ID: id, Role: "ideation", TriggerID: trigger, ContextRevision: contextRevisionForTest,
		Harness: "claude", Model: "opus", ReasoningEffort: "high",
		PromptVersion: "ideation-v1", PromptSHA256: "b2c88fe7a651a61953d3c9f1d0d5ea219cdfa0cd4a5090e4ef0440e19bbc134d",
		Image: "ghcr.io/example/worker", ImageDigest: "sha256:" + contextRevisionForTest + "0123456789abcdef01234567",
		WorkerID: "session-1-ideation",
	}
}

// startInvocationForTest appends user turn trigger and starts invocation id
// for it, returning the role-active session and the pinned invocation.
func startInvocationForTest(t *testing.T, opened *store.Store, session store.IdeationSession, id, trigger string) (store.IdeationSession, store.SessionInvocation) {
	t.Helper()
	session, _, err := opened.AppendIdeationTurn(t.Context(), ideationWrite(session, store.IdeationStateAwaitingUser), store.IdeationTurn{
		ID: trigger, Author: store.IdeationTurnAuthorUser, Body: "Export run history as JSON.",
	})
	if err != nil {
		t.Fatalf("AppendIdeationTurn() error = %v", err)
	}
	session, invocation, err := opened.StartSessionInvocation(t.Context(), ideationWrite(session, store.IdeationStateRoleActive), sessionInvocationForTest(id, trigger))
	if err != nil {
		t.Fatalf("StartSessionInvocation() error = %v", err)
	}
	return session, invocation
}

// TestSessionInvocationPinsTheSessionRevisionAndSurvivesRestart verifies that
// starting a role records the exact session revision it serves, makes the
// session role-active in the same write, admits only one active invocation,
// and stays readable after a coordinator restart.
func TestSessionInvocationPinsTheSessionRevisionAndSurvivesRestart(t *testing.T) {
	t.Parallel()

	opened, path := openIdeationStoreForTest(t)
	session, invocation := startInvocationForTest(t, opened, createIdeationSessionForTest(t, opened, "session-1"), "sinv-1", "turn-1")
	if session.State != store.IdeationStateRoleActive || invocation.SessionRevision != session.Revision ||
		invocation.Status != store.SessionInvocationActive || invocation.SessionID != "session-1" {
		t.Fatalf("started session/invocation = %#v / %#v, want role-active session pinned at its revision", session, invocation)
	}
	if _, _, err := opened.StartSessionInvocation(t.Context(), ideationWrite(session, store.IdeationStateRoleActive), sessionInvocationForTest("sinv-2", "turn-1")); !errors.Is(err, store.ErrSessionInvocationActive) {
		t.Fatalf("second StartSessionInvocation() error = %v, want ErrSessionInvocationActive", err)
	}
	if _, err := opened.RecordSessionInvocationNativeSession(t.Context(), "sinv-1", "7c9e6679-7425-40de-944b-e07fc1f90ae7"); err != nil {
		t.Fatalf("RecordSessionInvocationNativeSession() error = %v", err)
	}

	reopened := reopenIdeationStoreForTest(t, opened, path)
	active, err := reopened.ActiveSessionInvocations(t.Context())
	if err != nil {
		t.Fatalf("ActiveSessionInvocations() error = %v", err)
	}
	want := sessionInvocationForTest("sinv-1", "turn-1")
	if len(active) != 1 || active[0].NativeSessionID != "7c9e6679-7425-40de-944b-e07fc1f90ae7" ||
		active[0].ContextRevision != want.ContextRevision || active[0].PromptSHA256 != want.PromptSHA256 ||
		active[0].ImageDigest != want.ImageDigest || active[0].Model != "opus" || active[0].SessionRevision != session.Revision {
		t.Fatalf("ActiveSessionInvocations() = %#v, want the pinned invocation", active)
	}
}

// TestSessionRoleResultIsAcceptedOncePerTrigger verifies that an accepted
// ideation result appends the role turn and brief revision atomically, that a
// late repeat of the same report is refused, and that the same user turn
// cannot start a second role result.
func TestSessionRoleResultIsAcceptedOncePerTrigger(t *testing.T) {
	t.Parallel()

	opened, _ := openIdeationStoreForTest(t)
	session, invocation := startInvocationForTest(t, opened, createIdeationSessionForTest(t, opened, "session-1"), "sinv-1", "turn-1")
	result := store.SessionRoleResult{
		InvocationID: invocation.ID,
		Turn:         &store.IdeationTurn{ID: "role-sinv-1", Author: store.IdeationTurnAuthorRole, Role: "ideation", InvocationID: invocation.ID, Body: "Who uses the export?"},
		Brief:        &store.Brief{Problem: "No export.", UnresolvedQuestions: []store.BriefItem{{ID: "who", Text: "Who uses it?"}}},
		Payload:      []byte(`{"message":"Who uses the export?"}`),
	}
	accepted, finished, err := opened.AcceptSessionRoleResult(t.Context(), ideationWrite(session, store.IdeationStateAwaitingUser), result)
	if err != nil {
		t.Fatalf("AcceptSessionRoleResult() error = %v", err)
	}
	if accepted.State != store.IdeationStateAwaitingUser || accepted.BriefRevision != 1 || accepted.Revision != session.Revision+1 ||
		finished.Status != store.SessionInvocationAccepted || string(finished.Payload) != string(result.Payload) {
		t.Fatalf("accepted session/invocation = %#v / %#v", accepted, finished)
	}
	history, err := opened.IdeationHistory(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Turns) != 2 || history.Turns[1].Author != store.IdeationTurnAuthorRole || len(history.Briefs) != 1 {
		t.Fatalf("history = %#v, want the user turn, one role turn, and one brief", history)
	}
	if _, _, err := opened.AcceptSessionRoleResult(t.Context(), ideationWrite(accepted, store.IdeationStateAwaitingUser), result); !errors.Is(err, store.ErrSessionInvocationNotActive) {
		t.Fatalf("repeated AcceptSessionRoleResult() error = %v, want ErrSessionInvocationNotActive", err)
	}
	if _, _, err := opened.StartSessionInvocation(t.Context(), ideationWrite(accepted, store.IdeationStateRoleActive), sessionInvocationForTest("sinv-2", "turn-1")); !errors.Is(err, store.ErrSessionTriggerConsumed) {
		t.Fatalf("StartSessionInvocation(consumed trigger) error = %v, want ErrSessionTriggerConsumed", err)
	}
}

// TestSessionRoleResultFromAnOldRevisionOrCancelledInvocationIsRejected
// verifies that a report arriving after the session moved on, or after its
// invocation was cancelled, changes nothing.
func TestSessionRoleResultFromAnOldRevisionOrCancelledInvocationIsRejected(t *testing.T) {
	t.Parallel()

	opened, _ := openIdeationStoreForTest(t)
	session, invocation := startInvocationForTest(t, opened, createIdeationSessionForTest(t, opened, "session-1"), "sinv-1", "turn-1")
	cancelled, err := opened.TransitionIdeationSession(t.Context(), ideationWrite(session, store.IdeationStateCancelled))
	if err != nil {
		t.Fatal(err)
	}
	result := store.SessionRoleResult{InvocationID: invocation.ID, Payload: []byte(`{}`)}
	if _, _, err := opened.AcceptSessionRoleResult(t.Context(), ideationWrite(session, store.IdeationStateAwaitingUser), result); !errors.Is(err, store.ErrIdeationRevisionConflict) {
		t.Fatalf("AcceptSessionRoleResult(old revision) error = %v, want ErrIdeationRevisionConflict", err)
	}
	if _, _, err := opened.AcceptSessionRoleResult(t.Context(), ideationWrite(cancelled, store.IdeationStateAwaitingUser), result); !errors.Is(err, store.ErrIdeationRevisionConflict) {
		t.Fatalf("AcceptSessionRoleResult(newer revision) error = %v, want a conflict with the pinned revision", err)
	}
	closed, err := opened.CloseSessionInvocation(t.Context(), store.SessionInvocationEnd{InvocationID: invocation.ID, Status: store.SessionInvocationCancelled, Reason: "session cancelled"})
	if err != nil || closed.Status != store.SessionInvocationCancelled {
		t.Fatalf("CloseSessionInvocation() = %#v, %v; want cancelled", closed, err)
	}
	if _, _, err := opened.AcceptSessionRoleResult(t.Context(), ideationWrite(cancelled, store.IdeationStateAwaitingUser), result); !errors.Is(err, store.ErrSessionInvocationNotActive) {
		t.Fatalf("AcceptSessionRoleResult(cancelled) error = %v, want ErrSessionInvocationNotActive", err)
	}
	if active, err := opened.ActiveSessionInvocations(t.Context()); err != nil || len(active) != 0 {
		t.Fatalf("ActiveSessionInvocations() = %#v, %v; want none", active, err)
	}
}

// TestReleasedSessionInvocationRecordsTheTypedFailureAndFreesTheTrigger
// verifies that a failed role releases the session to the user with a reason
// in one write, keeps the failure kind, and lets the same user turn start a
// new attempt.
func TestReleasedSessionInvocationRecordsTheTypedFailureAndFreesTheTrigger(t *testing.T) {
	t.Parallel()

	opened, _ := openIdeationStoreForTest(t)
	session, invocation := startInvocationForTest(t, opened, createIdeationSessionForTest(t, opened, "session-1"), "sinv-1", "turn-1")
	write := ideationWrite(session, store.IdeationStateAwaitingUser)
	write.StateReason = "claude authentication expired"
	released, failed, err := opened.ReleaseSessionInvocation(t.Context(), write, store.SessionInvocationEnd{
		InvocationID: invocation.ID, Status: store.SessionInvocationFailed, FailureKind: "authentication_expired", Reason: "claude authentication expired",
	})
	if err != nil {
		t.Fatalf("ReleaseSessionInvocation() error = %v", err)
	}
	if released.State != store.IdeationStateAwaitingUser || released.StateReason != "claude authentication expired" ||
		failed.Status != store.SessionInvocationFailed || failed.FailureKind != "authentication_expired" {
		t.Fatalf("released session/invocation = %#v / %#v", released, failed)
	}
	if _, retried, err := opened.StartSessionInvocation(t.Context(), ideationWrite(released, store.IdeationStateRoleActive), sessionInvocationForTest("sinv-2", "turn-1")); err != nil || retried.SessionRevision != released.Revision+1 {
		t.Fatalf("StartSessionInvocation(retry) = %#v, %v; want a new attempt for the same turn", retried, err)
	}
}

// TestPlanningResultMustMatchThePinnedBriefAndContext verifies that an
// accepted plan records the brief and context revisions the invocation was
// pinned to, and that a plan naming other revisions is refused.
func TestPlanningResultMustMatchThePinnedBriefAndContext(t *testing.T) {
	t.Parallel()

	opened, _ := openIdeationStoreForTest(t)
	session := createIdeationSessionForTest(t, opened, "session-1")
	session, _, err := opened.SaveBriefRevision(t.Context(), ideationWrite(session, store.IdeationStateAwaitingUser), store.Brief{
		Problem: "No export.", Scope: []store.BriefItem{{ID: "json", Text: "JSON export"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	planning := sessionInvocationForTest("sinv-plan", "handoff-1")
	planning.Role, planning.PromptVersion, planning.BriefRevision = "planning", "planning-v1", session.BriefRevision
	session, planning, err = opened.StartSessionInvocation(t.Context(), ideationWrite(session, store.IdeationStateRoleActive), planning)
	if err != nil {
		t.Fatalf("StartSessionInvocation(planning) error = %v", err)
	}
	plan := store.PlanRevision{SourceBriefRevision: 1, ContextRevision: contextRevisionForTest, Nodes: []store.PlanNode{{ID: "fix", Kind: "task", Title: "Add export", BriefRefs: []string{"json"}}}}
	wrong := plan
	wrong.ContextRevision = "fedcba9876543210fedcba9876543210fedcba98"
	if _, _, err := opened.AcceptSessionRoleResult(t.Context(), ideationWrite(session, store.IdeationStateDraftReview), store.SessionRoleResult{InvocationID: planning.ID, Plan: &wrong, Payload: []byte(`{}`)}); err == nil {
		t.Fatal("AcceptSessionRoleResult() accepted a plan for another context revision")
	}
	accepted, _, err := opened.AcceptSessionRoleResult(t.Context(), ideationWrite(session, store.IdeationStateDraftReview), store.SessionRoleResult{InvocationID: planning.ID, Plan: &plan, Payload: []byte(`{}`)})
	if err != nil || accepted.State != store.IdeationStateDraftReview || accepted.PlanRevision != 1 {
		t.Fatalf("AcceptSessionRoleResult(plan) = %#v, %v; want draft review with plan revision 1", accepted, err)
	}
}

// TestSchema44MigrationAddsSessionInvocations verifies that a schema 43 store
// gains the session invocation table on open.
func TestSchema44MigrationAddsSessionInvocations(t *testing.T) {
	t.Parallel()

	opened, path := openIdeationStoreForTest(t)
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(t.Context(), `DROP TABLE ideation_invocations; UPDATE schema_metadata SET version = 43`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if version := reopened.SchemaVersion(); version != store.CurrentSchemaVersion {
		t.Fatalf("SchemaVersion() = %d, want %d", version, store.CurrentSchemaVersion)
	}
	if active, err := reopened.ActiveSessionInvocations(t.Context()); err != nil || len(active) != 0 {
		t.Fatalf("ActiveSessionInvocations() = %#v, %v; want an empty usable table", active, err)
	}
}

// TestSessionInvocationsListOneSessionInStartOrder verifies that a session
// page can read every invocation of one session, ended or active, in the
// order they started, and no invocation of another session.
func TestSessionInvocationsListOneSessionInStartOrder(t *testing.T) {
	t.Parallel()

	opened, _ := openIdeationStoreForTest(t)
	session, first := startInvocationForTest(t, opened, createIdeationSessionForTest(t, opened, "session-1"), "sinv-1", "turn-1")
	session, _, err := opened.ReleaseSessionInvocation(t.Context(), ideationWrite(session, store.IdeationStateAwaitingUser), store.SessionInvocationEnd{
		InvocationID: first.ID, Status: store.SessionInvocationFailed, FailureKind: "rate_limited", Reason: "capacity", At: ideationNow,
	})
	if err != nil {
		t.Fatalf("ReleaseSessionInvocation() error = %v", err)
	}
	startInvocationForTest(t, opened, session, "sinv-2", "turn-2")
	other := createIdeationSessionForTest(t, opened, "session-2")
	startInvocationForTest(t, opened, other, "sinv-3", "turn-3")

	invocations, err := opened.SessionInvocations(t.Context(), "session-1")
	if err != nil {
		t.Fatalf("SessionInvocations() error = %v", err)
	}
	if len(invocations) != 2 || invocations[0].ID != "sinv-1" || invocations[0].FailureKind != "rate_limited" ||
		invocations[1].ID != "sinv-2" || invocations[1].Status != store.SessionInvocationActive {
		t.Fatalf("SessionInvocations() = %#v, want sinv-1 (failed) then sinv-2 (active)", invocations)
	}
}

// TestSchema47MigrationLeavesEndedInvocationsToRelease verifies that an
// invocation which ended before schema 47 is listed for resource cleanup,
// that only an ended invocation can be marked released, and that a released
// invocation leaves the list.
func TestSchema47MigrationLeavesEndedInvocationsToRelease(t *testing.T) {
	t.Parallel()

	opened, path := openIdeationStoreForTest(t)
	session, first := startInvocationForTest(t, opened, createIdeationSessionForTest(t, opened, "session-1"), "sinv-1", "turn-1")
	session, _, err := opened.ReleaseSessionInvocation(t.Context(), ideationWrite(session, store.IdeationStateAwaitingUser), store.SessionInvocationEnd{
		InvocationID: first.ID, Status: store.SessionInvocationCancelled, FailureKind: "cancelled", At: ideationNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	startInvocationForTest(t, opened, session, "sinv-2", "turn-2")
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(t.Context(), `ALTER TABLE ideation_invocations DROP COLUMN resources_released_at; UPDATE schema_metadata SET version = 46`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = reopened.Close() }()

	unreleased, err := reopened.UnreleasedSessionInvocations(t.Context())
	if err != nil || len(unreleased) != 1 || unreleased[0].ID != "sinv-1" {
		t.Fatalf("UnreleasedSessionInvocations() = %#v, %v; want only the ended sinv-1", unreleased, err)
	}
	if _, err := reopened.MarkSessionInvocationResourcesReleased(t.Context(), "sinv-2", ideationNow); !errors.Is(err, store.ErrSessionInvocationActive) {
		t.Fatalf("Mark(active) error = %v, want ErrSessionInvocationActive", err)
	}
	released, err := reopened.MarkSessionInvocationResourcesReleased(t.Context(), "sinv-1", ideationNow)
	if err != nil || !released.ResourcesReleasedAt.Equal(ideationNow) {
		t.Fatalf("Mark(sinv-1) = %#v, %v; want released at %v", released, err, ideationNow)
	}
	if unreleased, err := reopened.UnreleasedSessionInvocations(t.Context()); err != nil || len(unreleased) != 0 {
		t.Fatalf("UnreleasedSessionInvocations() after release = %#v, %v; want none", unreleased, err)
	}
}
