package sessioncommand_test

import (
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// briefWithUnknown is an edit_brief payload with one decision and one
// unresolved question.
const briefWithUnknown = `{"brief":{"problem":"Slow onboarding.","decisions":[{"id":"d-1","decision":"Cache images.","rationale":"Pulls dominate."}],` +
	`"unresolved_questions":[{"id":"uq-1","text":"Which registry hosts the images?"}]}}`

// editBrief saves payload as the next brief revision and returns the session
// revision.
func (c *coordinatorForTest) editBrief(requestID, sessionID string, revision int64, payload string) int64 {
	c.t.Helper()
	c.submit(requestID, sessionID, sessioncommand.KindEditBrief, revision, payload)
	c.drain()
	return c.requireOutcome(requestID, store.IdeationCommandApplied).SessionRevision
}

// TestHandOffDefersEveryDisplayedUnknownExplicitly verifies that a handoff
// is refused while an unresolved question of the displayed brief is not
// explicitly deferred, and that a handoff which defers every unknown starts
// planning.
func TestHandOffDefersEveryDisplayedUnknownExplicitly(t *testing.T) {
	t.Parallel()

	c := newCoordinatorForTest(t)
	revision := c.editBrief("brief-1", "session-1", c.createSession("session-1"), briefWithUnknown)
	roles := &rolesForTest{}
	drainWithRoles := func() {
		dispatcher := c.dispatcher()
		dispatcher.Env.Roles = roles
		if err := dispatcher.Drain(t.Context()); err != nil {
			t.Fatal(err)
		}
	}

	c.submit("handoff-1", "session-1", sessioncommand.KindHandOff, revision, `{"brief_revision":1}`)
	drainWithRoles()
	refused := c.requireOutcome("handoff-1", store.IdeationCommandRefused)
	if !strings.Contains(refused.Detail, "uq-1") || len(roles.calls) != 0 {
		t.Fatalf("refused = %q, role starts = %v; want a refusal that names uq-1 and no planning start", refused.Detail, roles.calls)
	}

	c.submit("handoff-2", "session-1", sessioncommand.KindHandOff, revision, `{"brief_revision":1,"deferred_questions":["uq-2"]}`)
	drainWithRoles()
	c.requireOutcome("handoff-2", store.IdeationCommandRefused)

	c.submit("handoff-3", "session-1", sessioncommand.KindHandOff, revision, `{"brief_revision":1,"deferred_questions":["uq-1"]}`)
	drainWithRoles()
	c.requireOutcome("handoff-3", store.IdeationCommandApplied)
	if len(roles.calls) != 1 || roles.calls[0] != "planning:session-1:handoff-3" {
		t.Fatalf("role starts = %v, want one planning start for handoff-3", roles.calls)
	}
}

// TestInvestigationNeededRecordsTheUnknownsWithoutBacklog verifies the
// investigation-needed outcome: it needs at least one unresolved question
// in the brief, parks the session without any tracker write, and can resume.
func TestInvestigationNeededRecordsTheUnknownsWithoutBacklog(t *testing.T) {
	t.Parallel()

	c := newCoordinatorForTest(t)
	revision := c.editBrief("brief-1", "session-1", c.createSession("session-1"), `{"brief":{"problem":"Slow onboarding."}}`)
	c.submit("investigate-1", "session-1", sessioncommand.KindInvestigationNeeded, revision, `{"reason":"Registry unknown."}`)
	c.drain()
	refused := c.requireOutcome("investigate-1", store.IdeationCommandRefused)
	if !strings.Contains(refused.Detail, "unresolved question") {
		t.Fatalf("refused detail = %q, want it to ask for an unresolved question", refused.Detail)
	}

	revision = c.editBrief("brief-2", "session-1", revision, briefWithUnknown)
	c.submit("investigate-2", "session-1", sessioncommand.KindInvestigationNeeded, revision, `{"reason":"Registry unknown."}`)
	c.drain()
	revision = c.requireOutcome("investigate-2", store.IdeationCommandApplied).SessionRevision
	session := c.history("session-1").Session
	if session.State != store.IdeationStateInvestigationNeeded || session.StateReason != "Registry unknown." {
		t.Fatalf("session = %#v, want investigation_needed with the reason", session)
	}
	if writes := c.fake.Writes(); writes != 0 {
		t.Fatalf("tracker writes = %v, want none", writes)
	}

	c.submit("resume-1", "session-1", sessioncommand.KindResume, revision, `{}`)
	c.drain()
	c.requireOutcome("resume-1", store.IdeationCommandApplied)
	if state := c.history("session-1").Session.State; state != store.IdeationStateAwaitingUser {
		t.Fatalf("state = %q, want awaiting_user after resume", state)
	}
}

// TestCorrectionTurnNamesAnEarlierUserAnswer verifies that a correction is
// stored as a new turn linked to the earlier user answer it changes, and
// that a correction of a missing or role turn is refused.
func TestCorrectionTurnNamesAnEarlierUserAnswer(t *testing.T) {
	t.Parallel()

	c := newCoordinatorForTest(t)
	revision := c.createSession("session-1")
	c.submit("turn-1", "session-1", sessioncommand.KindSubmitTurn, revision, `{"body":"Only admins onboard."}`)
	c.drain()
	revision = c.requireOutcome("turn-1", store.IdeationCommandApplied).SessionRevision

	c.submit("turn-2", "session-1", sessioncommand.KindSubmitTurn, revision, `{"body":"Every developer onboards.","corrects":"turn-missing"}`)
	c.drain()
	c.requireOutcome("turn-2", store.IdeationCommandRefused)

	c.submit("turn-3", "session-1", sessioncommand.KindSubmitTurn, revision, `{"body":"Every developer onboards.","corrects":"turn-1"}`)
	c.drain()
	c.requireOutcome("turn-3", store.IdeationCommandApplied)
	turns := c.history("session-1").Turns
	if len(turns) != 2 || turns[1].ID != "turn-3" || turns[1].Corrects != "turn-1" || turns[0].Corrects != "" {
		t.Fatalf("turns = %#v, want turn-3 linked to turn-1", turns)
	}
}
