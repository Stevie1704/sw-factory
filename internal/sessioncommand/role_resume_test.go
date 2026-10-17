package sessioncommand_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// storedRolesForTest starts session roles as the coordinator does: it
// records an active invocation and moves the session to role_active in one
// store write. When crash is set, the first start stops the coordinator
// right after that commit, before the command outcome is recorded. When fail
// is set, a start returns it before any store write.
type storedRolesForTest struct {
	store *store.Store
	mu    sync.Mutex
	calls []string
	crash context.CancelFunc
	fail  error
}

// StartIdeation records an ideation invocation for turnID.
func (r *storedRolesForTest) StartIdeation(ctx context.Context, sessionID, turnID string) error {
	return r.start(ctx, workflow.RoleIdeation, sessionID, turnID)
}

// StartPlanning records a planning invocation for requestID.
func (r *storedRolesForTest) StartPlanning(ctx context.Context, sessionID, requestID string) error {
	return r.start(ctx, workflow.RolePlanning, sessionID, requestID)
}

// CancelRole is not used by these tests.
func (r *storedRolesForTest) CancelRole(context.Context, string, string) error { return nil }

// start persists one active invocation of role triggered by triggerID.
func (r *storedRolesForTest) start(ctx context.Context, role, sessionID, triggerID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, role+":"+triggerID)
	if r.fail != nil {
		err := r.fail
		r.fail = nil
		return err
	}
	session, err := r.store.IdeationSession(ctx, sessionID)
	if err != nil {
		return err
	}
	write, err := workflow.DefaultSessionLifecycle().Write(session, workflow.SessionEventRoleStarted, "", dispatchNow)
	if err != nil {
		return err
	}
	if _, _, err := r.store.StartSessionInvocation(ctx, write, store.SessionInvocation{
		ID: "sinv-" + triggerID, Role: role, TriggerID: triggerID, WorkerID: "session-" + sessionID + "-" + role,
		Harness: "codex", PromptVersion: "session-v1", ContextRevision: strings.Repeat("a", 40), PromptSHA256: strings.Repeat("b", 64),
	}); err != nil {
		return err
	}
	if r.crash != nil {
		r.crash()
		r.crash = nil
		return context.Canceled
	}
	return nil
}

// drainWith runs one dispatcher pass with roles under ctx.
func (c *coordinatorForTest) drainWith(ctx context.Context, roles sessioncommand.Roles) error {
	c.t.Helper()
	dispatcher := c.dispatcher()
	dispatcher.Env.Roles = roles
	err := dispatcher.Drain(ctx)
	_ = dispatcher.Wait()
	return err
}

// TestRoleStartCommandsAreResumeSafe verifies that a coordinator that stops
// after a hand-off, a regeneration, or a turn started its role, but before
// the command outcome was recorded, reports the original applied outcome on
// resume instead of a conflict, and starts no second role.
func TestRoleStartCommandsAreResumeSafe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prepare func(c *coordinatorForTest) (requestID string, kind sessioncommand.Kind, revision int64, payload string)
	}{
		{name: "hand_off", prepare: func(c *coordinatorForTest) (string, sessioncommand.Kind, int64, string) {
			revision := c.editBrief("brief-1", "session-1", c.createSession("session-1"), `{"brief":{"problem":"Slow onboarding."}}`)
			return "handoff-1", sessioncommand.KindHandOff, revision, `{"brief_revision":1}`
		}},
		{name: "regenerate_draft", prepare: func(c *coordinatorForTest) (string, sessioncommand.Kind, int64, string) {
			c.createSession("session-1")
			return "regenerate-1", sessioncommand.KindRegenerateDraft, c.draftPlan("session-1"), `{"plan_revision":1}`
		}},
		{name: "submit_turn", prepare: func(c *coordinatorForTest) (string, sessioncommand.Kind, int64, string) {
			return "turn-1", sessioncommand.KindSubmitTurn, c.createSession("session-1"), `{"body":"Who onboards?"}`
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			c := newCoordinatorForTest(t)
			requestID, kind, revision, payload := test.prepare(c)
			c.submit(requestID, "session-1", kind, revision, payload)
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			_ = c.drainWith(ctx, &storedRolesForTest{store: c.store, crash: stop})
			if command := c.command(requestID); command.State != store.IdeationCommandApplying {
				t.Fatalf("command = %#v, want applying after the coordinator stopped", command)
			}
			started := c.history("session-1").Session
			if started.State != store.IdeationStateRoleActive {
				t.Fatalf("session = %#v, want the role started before the stop", started)
			}

			resumed := &storedRolesForTest{store: c.store}
			if err := c.drainWith(t.Context(), resumed); err != nil {
				t.Fatalf("resumed Drain() error = %v", err)
			}
			applied := c.requireOutcome(requestID, store.IdeationCommandApplied)
			if applied.SessionRevision != started.Revision || !strings.Contains(applied.Detail, "role started") {
				t.Fatalf("resumed outcome = %#v, want the original applied outcome at revision %d", applied, started.Revision)
			}
			if len(resumed.calls) != 0 {
				t.Fatalf("resumed role starts = %v, want none for a started role", resumed.calls)
			}
		})
	}
}

// TestTransientStoreFailureAtRoleStartKeepsTheTurnPending verifies that a
// busy store at the ideation role start follows the transient rule: the
// turn command stays pending without an outcome, and the next pass starts
// the role once and applies the command.
func TestTransientStoreFailureAtRoleStartKeepsTheTurnPending(t *testing.T) {
	t.Parallel()

	c := newCoordinatorForTest(t)
	c.submit("turn-1", "session-1", sessioncommand.KindSubmitTurn, c.createSession("session-1"), `{"body":"Who onboards?"}`)
	roles := &storedRolesForTest{store: c.store, fail: busyErrorForTest(t)}
	if err := c.drainWith(t.Context(), roles); !store.IsTransient(err) {
		t.Fatalf("Drain() error = %v, want the transient store failure", err)
	}
	if command := c.command("turn-1"); command.State != store.IdeationCommandApplying || command.Outcome != "" {
		t.Fatalf("command = %#v, want it pending without an outcome", command)
	}

	if err := c.drainWith(t.Context(), roles); err != nil {
		t.Fatalf("second Drain() error = %v", err)
	}
	applied := c.requireOutcome("turn-1", store.IdeationCommandApplied)
	if !strings.Contains(applied.Detail, "role started") || len(roles.calls) != 2 || len(c.history("session-1").Turns) != 1 {
		t.Fatalf("outcome = %#v, role starts = %v; want one turn and the role started on the second pass", applied, roles.calls)
	}
}
