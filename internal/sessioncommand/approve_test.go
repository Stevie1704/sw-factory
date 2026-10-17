package sessioncommand_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/publication"
	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// unstartablePublisherForTest previews through the real publisher but
// refuses every publication start before any tracker write, as a provider
// preflight that fails after the approval was recorded does.
type unstartablePublisherForTest struct {
	sessioncommand.Publisher
}

// Publish refuses the start with a typed mapping error.
func (unstartablePublisherForTest) Publish(context.Context, publication.Request) (publication.Result, error) {
	return publication.Result{}, tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "the issue type Feature no longer exists")
}

// TestApprovalRecordedBeforeAFailedPublicationStartIsApplied verifies that
// an approval that the coordinator recorded is reported as applied with the
// reason why publication did not start, so the page reloads to the approved
// session, and that the session stays approved without a tracker write.
func TestApprovalRecordedBeforeAFailedPublicationStartIsApplied(t *testing.T) {
	t.Parallel()

	c := newCoordinatorForTest(t)
	c.createSession("session-1")
	revision := c.draftPlan("session-1")
	preview := c.preview("preview-1", "session-1", revision)
	c.submit("approve-1", "session-1", sessioncommand.KindApprove, revision, approvePayload(preview))
	dispatcher := c.dispatcher()
	dispatcher.Env.Publisher = unstartablePublisherForTest{Publisher: dispatcher.Env.Publisher}
	if err := dispatcher.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.Wait(); err != nil {
		t.Fatal(err)
	}

	applied := c.requireOutcome("approve-1", store.IdeationCommandApplied)
	session := c.history("session-1").Session
	if session.State != store.IdeationStateApproved || session.ApprovalID != "approve-1" || applied.SessionRevision != session.Revision {
		t.Fatalf("session = %#v, outcome = %#v; want approved under approve-1 at the outcome revision", session, applied)
	}
	for _, want := range []string{"approved", "publication did not start", "Feature no longer exists", "publish"} {
		if !strings.Contains(applied.Detail, want) {
			t.Fatalf("detail = %q, want it to name %q", applied.Detail, want)
		}
	}
	if c.fake.Writes() != 0 {
		t.Fatalf("Writes() = %d, want no tracker write", c.fake.Writes())
	}
}
