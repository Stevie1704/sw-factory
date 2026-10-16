package workflow

import (
	"fmt"
	"sort"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// SessionEvent identifies a coordinator-observed boundary that moves an
// ideation session through its lifecycle. Session events are separate from
// run stages and report outcomes: a session is not a Run (ADR 0021).
type SessionEvent string

const (
	// SessionEventTurnAccepted records an accepted user turn while the session
	// waits for the user.
	SessionEventTurnAccepted SessionEvent = "turn_accepted"
	// SessionEventRoleStarted records the start of an ideation or planning role
	// invocation.
	SessionEventRoleStarted SessionEvent = "role_started"
	// SessionEventRoleAwaitsUser records a role result that needs the user, or
	// a stopped role invocation that releases the session to the user.
	SessionEventRoleAwaitsUser SessionEvent = "role_awaits_user"
	// SessionEventPlanDrafted records an accepted plan draft.
	SessionEventPlanDrafted SessionEvent = "plan_drafted"
	// SessionEventDraftKept records a planning role that stopped, failed, or
	// produced no draft while an earlier draft stays under review.
	SessionEventDraftKept SessionEvent = "draft_kept"
	// SessionEventEdited records a user edit of the brief or the draft. An edit
	// of an approved draft returns it to review.
	SessionEventEdited SessionEvent = "edited"
	// SessionEventApproved records an approval of the current draft.
	SessionEventApproved SessionEvent = "approved"
	// SessionEventPublicationStarted records the start or retry of publication
	// of the current approval.
	SessionEventPublicationStarted SessionEvent = "publication_started"
	// SessionEventPublicationIncomplete records a publication attempt that
	// stopped before every operation succeeded.
	SessionEventPublicationIncomplete SessionEvent = "publication_incomplete"
	// SessionEventPublicationCompleted records a complete publication.
	SessionEventPublicationCompleted SessionEvent = "publication_completed"
	// SessionEventDeferred records that the user parked the session.
	SessionEventDeferred SessionEvent = "deferred"
	// SessionEventInvestigationNeeded records the user's decision that
	// material unknowns need investigation before planning.
	SessionEventInvestigationNeeded SessionEvent = "investigation_needed"
	// SessionEventResumed records that the user resumed a parked session.
	SessionEventResumed SessionEvent = "resumed"
	// SessionEventNoBuild records the user's decision not to build.
	SessionEventNoBuild SessionEvent = "no_build"
	// SessionEventCancelled records cancellation. Cancellation during
	// publication stops future work; it never deletes tracker items.
	SessionEventCancelled SessionEvent = "cancelled"
)

// InvalidSessionTransitionError reports an event that the session lifecycle
// does not declare for the current state.
type InvalidSessionTransitionError struct {
	// From is the current session state.
	From store.IdeationState
	// Event is the refused event.
	Event SessionEvent
}

// Error describes the refused transition.
func (e *InvalidSessionTransitionError) Error() string {
	return fmt.Sprintf("ideation session event %q is not allowed in state %q", e.Event, e.From)
}

// SessionLifecycle is the factory-owned declaration of ideation-session
// transitions. Repositories cannot change it (ADR 0003). The operational store
// persists the resulting state; this declaration decides which state follows.
type SessionLifecycle struct {
	transitions map[store.IdeationState]map[SessionEvent]store.IdeationState
}

// DefaultSessionLifecycle returns the factory-owned session lifecycle.
func DefaultSessionLifecycle() SessionLifecycle {
	const (
		awaitingUser = store.IdeationStateAwaitingUser
		roleActive   = store.IdeationStateRoleActive
		draftReview  = store.IdeationStateDraftReview
		approved     = store.IdeationStateApproved
		publishing   = store.IdeationStatePublishing
		incomplete   = store.IdeationStatePublicationIncomplete
		published    = store.IdeationStatePublished
		deferred     = store.IdeationStateDeferred
		investigate  = store.IdeationStateInvestigationNeeded
		noBuild      = store.IdeationStateNoBuild
		cancelled    = store.IdeationStateCancelled
	)
	return SessionLifecycle{transitions: map[store.IdeationState]map[SessionEvent]store.IdeationState{
		awaitingUser: {
			SessionEventTurnAccepted:        awaitingUser,
			SessionEventEdited:              awaitingUser,
			SessionEventRoleStarted:         roleActive,
			SessionEventDeferred:            deferred,
			SessionEventInvestigationNeeded: investigate,
			SessionEventNoBuild:             noBuild,
			SessionEventCancelled:           cancelled,
		},
		roleActive: {
			SessionEventRoleAwaitsUser: awaitingUser,
			SessionEventPlanDrafted:    draftReview,
			SessionEventDraftKept:      draftReview,
			SessionEventNoBuild:        noBuild,
			SessionEventCancelled:      cancelled,
		},
		draftReview: {
			SessionEventEdited:              draftReview,
			SessionEventRoleStarted:         roleActive,
			SessionEventApproved:            approved,
			SessionEventDeferred:            deferred,
			SessionEventInvestigationNeeded: investigate,
			SessionEventNoBuild:             noBuild,
			SessionEventCancelled:           cancelled,
		},
		approved: {
			SessionEventEdited:             draftReview,
			SessionEventPublicationStarted: publishing,
			SessionEventDeferred:           deferred,
			SessionEventNoBuild:            noBuild,
			SessionEventCancelled:          cancelled,
		},
		publishing: {
			SessionEventPublicationIncomplete: incomplete,
			SessionEventPublicationCompleted:  published,
			SessionEventCancelled:             cancelled,
		},
		incomplete: {
			SessionEventPublicationStarted: publishing,
			SessionEventCancelled:          cancelled,
		},
		deferred: {
			SessionEventResumed:   awaitingUser,
			SessionEventNoBuild:   noBuild,
			SessionEventCancelled: cancelled,
		},
		investigate: {
			SessionEventResumed:   awaitingUser,
			SessionEventNoBuild:   noBuild,
			SessionEventCancelled: cancelled,
		},
		published: {},
		noBuild:   {},
		cancelled: {},
	}}
}

// InitialState returns the state of a newly created session.
func (SessionLifecycle) InitialState() store.IdeationState {
	return store.IdeationStateAwaitingUser
}

// Next returns the state that follows event in state from, or an
// InvalidSessionTransitionError when the lifecycle does not declare it.
func (l SessionLifecycle) Next(from store.IdeationState, event SessionEvent) (store.IdeationState, error) {
	to, ok := l.transitions[from][event]
	if !ok {
		return "", &InvalidSessionTransitionError{From: from, Event: event}
	}
	return to, nil
}

// Events returns the events declared for state in a stable order.
func (l SessionLifecycle) Events(state store.IdeationState) []SessionEvent {
	events := make([]SessionEvent, 0, len(l.transitions[state]))
	for event := range l.transitions[state] {
		events = append(events, event)
	}
	sort.Slice(events, func(i, j int) bool { return events[i] < events[j] })
	return events
}

// Write resolves event for session and returns the store write guard that
// commits the next state at the session's current revision. The store then
// refuses the write if another writer changed the session first.
func (l SessionLifecycle) Write(session store.IdeationSession, event SessionEvent, reason string, at time.Time) (store.IdeationWrite, error) {
	next, err := l.Next(session.State, event)
	if err != nil {
		return store.IdeationWrite{}, err
	}
	return store.IdeationWrite{
		SessionID:        session.ID,
		ExpectedRevision: session.Revision,
		State:            next,
		StateReason:      reason,
		At:               at,
	}, nil
}
