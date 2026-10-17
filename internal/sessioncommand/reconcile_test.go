package sessioncommand_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker/trackertest"
)

// TestResumedReconcileReturnsItsOriginalOutcome verifies that a reconcile
// command whose coordinator stopped after the reconciliation was recorded,
// but before the command completed, completes as applied on resume. The
// resumed attempt neither refuses the operation it settled nor reports a
// conflict for the revision its own receipt bumped.
func TestResumedReconcileReturnsItsOriginalOutcome(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		cancel bool
		want   string
	}{
		{name: "incomplete publication", want: "publish again"},
		{name: "cancelled session", cancel: true, want: "stays cancelled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			c := newCoordinatorForTest(t)
			c.createSession("session-1")
			revision := c.draftPlan("session-1")
			preview := c.preview("preview-1", "session-1", revision)
			c.fake.Inject(trackertest.Fault{Op: trackertest.OpCreateItem, Key: "n-2", Err: errors.New("connection reset")})
			c.submit("approve-1", "session-1", sessioncommand.KindApprove, revision, approvePayload(preview))
			c.drain()
			revision = c.requireOutcome("approve-1", store.IdeationCommandApplied).SessionRevision
			if test.cancel {
				c.submit("cancel-1", "session-1", sessioncommand.KindCancel, revision, `{"reason":"Stop."}`)
				c.drain()
				revision = c.requireOutcome("cancel-1", store.IdeationCommandApplied).SessionRevision
			}

			c.submit("reconcile-1", "session-1", sessioncommand.KindReconcile, revision, `{"operation_id":"item/n-2","external_id":""}`)
			c.reconcileAndStop()
			if command := c.command("reconcile-1"); command.State != store.IdeationCommandApplying {
				t.Fatalf("interrupted command state = %q, want applying", command.State)
			}
			c.restart()
			c.drain()
			resumed := c.requireOutcome("reconcile-1", store.IdeationCommandApplied)
			history := c.history("session-1")
			if !strings.Contains(resumed.Detail, test.want) || resumed.SessionRevision != history.Session.Revision {
				t.Fatalf("resumed = %#v, session revision %d; want %q at the current revision", resumed, history.Session.Revision, test.want)
			}
			receipts := 0
			for _, receipt := range history.Receipts {
				if receipt.ID == "reconcile-1" {
					receipts++
				}
			}
			if test.cancel && receipts != 1 {
				t.Fatalf("receipts = %#v, want one reconciliation receipt", history.Receipts)
			}
		})
	}
}

// reconcileAndStop runs one dispatcher pass whose reconcile handler records
// the reconciliation and then stops the coordinator before the command
// completes.
func (c *coordinatorForTest) reconcileAndStop() {
	c.t.Helper()
	ctx, stop := context.WithCancel(c.t.Context())
	defer stop()
	interrupted := c.dispatcher()
	reconcile := sessioncommand.DefaultHandlers()[sessioncommand.KindReconcile]
	interrupted.Handlers[sessioncommand.KindReconcile] = func(ctx context.Context, env sessioncommand.Env, command store.IdeationCommand) (sessioncommand.Outcome, error) {
		if _, err := reconcile(ctx, env, command); err != nil {
			c.t.Errorf("reconcile handler error = %v", err)
		}
		stop()
		return sessioncommand.Outcome{}, ctx.Err()
	}
	if err := interrupted.Drain(ctx); err != nil && !errors.Is(err, context.Canceled) {
		c.t.Fatalf("Drain() error = %v", err)
	}
	if err := interrupted.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		c.t.Fatalf("Wait() error = %v", err)
	}
}
