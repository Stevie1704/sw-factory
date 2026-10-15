package workflow_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// TestSessionLifecycleResolvesDeclaredTransitions verifies the main path from
// conversation to publication and the parking and closing outcomes.
func TestSessionLifecycleResolvesDeclaredTransitions(t *testing.T) {
	t.Parallel()

	lifecycle := workflow.DefaultSessionLifecycle()
	tests := []struct {
		from  store.IdeationState
		event workflow.SessionEvent
		want  store.IdeationState
	}{
		{store.IdeationStateAwaitingUser, workflow.SessionEventTurnAccepted, store.IdeationStateAwaitingUser},
		{store.IdeationStateAwaitingUser, workflow.SessionEventRoleStarted, store.IdeationStateRoleActive},
		{store.IdeationStateRoleActive, workflow.SessionEventRoleAwaitsUser, store.IdeationStateAwaitingUser},
		{store.IdeationStateRoleActive, workflow.SessionEventPlanDrafted, store.IdeationStateDraftReview},
		{store.IdeationStateDraftReview, workflow.SessionEventEdited, store.IdeationStateDraftReview},
		{store.IdeationStateDraftReview, workflow.SessionEventRoleStarted, store.IdeationStateRoleActive},
		{store.IdeationStateDraftReview, workflow.SessionEventApproved, store.IdeationStateApproved},
		{store.IdeationStateApproved, workflow.SessionEventEdited, store.IdeationStateDraftReview},
		{store.IdeationStateApproved, workflow.SessionEventPublicationStarted, store.IdeationStatePublishing},
		{store.IdeationStatePublishing, workflow.SessionEventPublicationIncomplete, store.IdeationStatePublicationIncomplete},
		{store.IdeationStatePublicationIncomplete, workflow.SessionEventPublicationStarted, store.IdeationStatePublishing},
		{store.IdeationStatePublishing, workflow.SessionEventPublicationCompleted, store.IdeationStatePublished},
		{store.IdeationStateDraftReview, workflow.SessionEventDeferred, store.IdeationStateDeferred},
		{store.IdeationStateDeferred, workflow.SessionEventResumed, store.IdeationStateAwaitingUser},
		{store.IdeationStateDeferred, workflow.SessionEventNoBuild, store.IdeationStateNoBuild},
		{store.IdeationStatePublicationIncomplete, workflow.SessionEventCancelled, store.IdeationStateCancelled},
	}
	for _, test := range tests {
		got, err := lifecycle.Next(test.from, test.event)
		if err != nil || got != test.want {
			t.Errorf("Next(%s, %s) = %s, %v; want %s", test.from, test.event, got, err, test.want)
		}
	}
}

// TestSessionLifecycleRejectsInvalidTransitions verifies the typed error for
// transitions the lifecycle does not declare, including any transition out of
// a terminal state and publication of an unapproved draft.
func TestSessionLifecycleRejectsInvalidTransitions(t *testing.T) {
	t.Parallel()

	lifecycle := workflow.DefaultSessionLifecycle()
	tests := []struct {
		from  store.IdeationState
		event workflow.SessionEvent
	}{
		{store.IdeationStateDraftReview, workflow.SessionEventPublicationStarted},
		{store.IdeationStateAwaitingUser, workflow.SessionEventApproved},
		{store.IdeationStateRoleActive, workflow.SessionEventEdited},
		{store.IdeationStatePublishing, workflow.SessionEventEdited},
		{store.IdeationStatePublishing, workflow.SessionEventDeferred},
		{store.IdeationStatePublished, workflow.SessionEventCancelled},
		{store.IdeationStateCancelled, workflow.SessionEventResumed},
		{store.IdeationStateNoBuild, workflow.SessionEventTurnAccepted},
		{"unknown", workflow.SessionEventCancelled},
	}
	for _, test := range tests {
		_, err := lifecycle.Next(test.from, test.event)
		var invalid *workflow.InvalidSessionTransitionError
		if !errors.As(err, &invalid) || invalid.From != test.from || invalid.Event != test.event {
			t.Errorf("Next(%s, %s) error = %v, want InvalidSessionTransitionError", test.from, test.event, err)
		}
	}
}

// TestSessionLifecycleDeclarationIsComplete verifies that every stored state is
// covered, terminal states have no exit, and every open state can be cancelled.
func TestSessionLifecycleDeclarationIsComplete(t *testing.T) {
	t.Parallel()

	lifecycle := workflow.DefaultSessionLifecycle()
	for _, state := range store.IdeationStates() {
		events := lifecycle.Events(state)
		if store.IsTerminalIdeationState(state) {
			if len(events) != 0 {
				t.Errorf("terminal state %s declares events %v", state, events)
			}
			continue
		}
		if got, err := lifecycle.Next(state, workflow.SessionEventCancelled); err != nil || got != store.IdeationStateCancelled {
			t.Errorf("open state %s cannot be cancelled: %s, %v", state, got, err)
		}
	}
	if lifecycle.InitialState() != store.IdeationStateAwaitingUser {
		t.Fatalf("InitialState() = %s, want awaiting_user", lifecycle.InitialState())
	}
}

// TestSessionLifecycleWriteGuardsTheStoredRevision verifies that the lifecycle
// builds a store write at the session's revision, that the store applies it,
// and that a write built from an old snapshot fails as a stale writer.
func TestSessionLifecycleWriteGuardsTheStoredRevision(t *testing.T) {
	t.Parallel()

	opened, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	lifecycle := workflow.DefaultSessionLifecycle()
	now := time.Date(2026, time.October, 15, 9, 0, 0, 0, time.UTC)
	session, err := opened.CreateIdeationSession(t.Context(), store.IdeationSession{
		ID: "session-1", RepositoryPath: "/repos/factory", State: lifecycle.InitialState(), CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.Write(session, workflow.SessionEventApproved, "", now); err == nil {
		t.Fatal("Write() built an undeclared transition")
	}
	start, err := lifecycle.Write(session, workflow.SessionEventRoleStarted, "ideation role started", now)
	if err != nil {
		t.Fatal(err)
	}
	if start.SessionID != "session-1" || start.ExpectedRevision != 1 || start.State != store.IdeationStateRoleActive || start.StateReason != "ideation role started" {
		t.Fatalf("Write() = %#v", start)
	}
	started, err := opened.TransitionIdeationSession(t.Context(), start)
	if err != nil {
		t.Fatalf("TransitionIdeationSession() error = %v", err)
	}
	if started.State != store.IdeationStateRoleActive || started.Revision != 2 {
		t.Fatalf("started session = %#v", started)
	}
	cancel, err := lifecycle.Write(session, workflow.SessionEventCancelled, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.TransitionIdeationSession(t.Context(), cancel); !errors.Is(err, store.ErrIdeationRevisionConflict) {
		t.Fatalf("write from an old snapshot error = %v, want ErrIdeationRevisionConflict", err)
	}
}
