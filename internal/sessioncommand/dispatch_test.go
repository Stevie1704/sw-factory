package sessioncommand_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/publication"
	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/tracker/trackertest"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// dispatchNow is the fixed coordinator clock of the dispatch tests.
var dispatchNow = time.Date(2026, time.October, 15, 11, 0, 0, 0, time.UTC)

// coordinatorForTest is one coordinator over a real SQLite store. It can be
// restarted, as a stopped and started `factory start` is.
type coordinatorForTest struct {
	t       *testing.T
	path    string
	store   *store.Store
	tracker tracker.PlanPublisher
	fake    *trackertest.PlanTracker
}

// newCoordinatorForTest opens a fresh store with a fake planning tracker.
func newCoordinatorForTest(t *testing.T) *coordinatorForTest {
	t.Helper()
	fake := trackertest.NewPlanTracker()
	c := &coordinatorForTest{t: t, path: filepath.Join(t.TempDir(), "data", "factory.db"), fake: fake, tracker: fake}
	c.open()
	t.Cleanup(func() { _ = c.store.Close() })
	return c
}

// open opens the store at the coordinator's path.
func (c *coordinatorForTest) open() {
	c.t.Helper()
	opened, err := store.Open(c.t.Context(), c.path)
	if err != nil {
		c.t.Fatal(err)
	}
	c.store = opened
}

// restart closes and reopens the store, as a coordinator restart does.
func (c *coordinatorForTest) restart() {
	c.t.Helper()
	if err := c.store.Close(); err != nil {
		c.t.Fatal(err)
	}
	c.open()
}

// dispatcher returns a dispatcher with the default handlers. A nil tracker
// models a tracker adapter without planning capability.
func (c *coordinatorForTest) dispatcher() *sessioncommand.Dispatcher {
	env := sessioncommand.Env{
		Store: c.store, Lifecycle: workflow.DefaultSessionLifecycle(), RepositoryPath: "/repos/factory",
		Now: func() time.Time { return dispatchNow },
	}
	if c.tracker != nil {
		env.Publisher = &publication.Publisher{
			Store: c.store, Tracker: c.tracker, Repository: tracker.Repository{Owner: "owner", Name: "repo"},
			Lifecycle: workflow.DefaultSessionLifecycle(), Now: func() time.Time { return dispatchNow },
		}
	}
	return sessioncommand.NewDispatcher(env)
}

// drain runs one dispatcher pass and waits for its tracker work.
func (c *coordinatorForTest) drain() {
	c.t.Helper()
	dispatcher := c.dispatcher()
	if err := dispatcher.Drain(c.t.Context()); err != nil {
		c.t.Fatalf("Drain() error = %v", err)
	}
	dispatcher.Wait()
}

// submit parses a browser request and appends it to the inbox.
func (c *coordinatorForTest) submit(requestID, sessionID string, kind sessioncommand.Kind, expected int64, payload string) store.IdeationCommand {
	c.t.Helper()
	body := fmt.Sprintf(`{"request_id":%q,"session_id":%q,"kind":%q,"expected_revision":%d,"payload":%s}`, requestID, sessionID, kind, expected, payload)
	command, err := sessioncommand.ParseRequest([]byte(body))
	if err != nil {
		c.t.Fatalf("ParseRequest(%s) error = %v", body, err)
	}
	accepted, _, err := c.store.AcceptIdeationCommand(c.t.Context(), command)
	if err != nil {
		c.t.Fatalf("AcceptIdeationCommand() error = %v", err)
	}
	return accepted
}

// command reads one command.
func (c *coordinatorForTest) command(requestID string) store.IdeationCommand {
	c.t.Helper()
	command, err := c.store.IdeationCommand(c.t.Context(), requestID)
	if err != nil {
		c.t.Fatal(err)
	}
	return command
}

// history reads one session history.
func (c *coordinatorForTest) history(sessionID string) store.IdeationHistory {
	c.t.Helper()
	history, err := c.store.IdeationHistory(c.t.Context(), sessionID)
	if err != nil {
		c.t.Fatal(err)
	}
	return history
}

// requireOutcome fails unless the command completed with outcome.
func (c *coordinatorForTest) requireOutcome(requestID string, outcome store.IdeationCommandOutcome) store.IdeationCommand {
	c.t.Helper()
	command := c.command(requestID)
	if command.State != store.IdeationCommandCompleted || command.Outcome != outcome {
		c.t.Fatalf("command %s = state %q outcome %q (%s), want completed %q", requestID, command.State, command.Outcome, command.Detail, outcome)
	}
	return command
}

// createSession creates session through the inbox and returns its revision.
func (c *coordinatorForTest) createSession(sessionID string) int64 {
	c.t.Helper()
	c.submit("create-"+sessionID, sessionID, sessioncommand.KindCreateSession, 0, `{"title":"Faster onboarding"}`)
	c.drain()
	return c.requireOutcome("create-"+sessionID, store.IdeationCommandApplied).SessionRevision
}

// draftPlan gives session a brief and a three-node plan in draft review, as
// session roles would, and returns the session revision.
func (c *coordinatorForTest) draftPlan(sessionID string) int64 {
	c.t.Helper()
	lifecycle := workflow.DefaultSessionLifecycle()
	session := c.history(sessionID).Session
	write := func(event workflow.SessionEvent) store.IdeationWrite {
		value, err := lifecycle.Write(session, event, "", dispatchNow)
		if err != nil {
			c.t.Fatal(err)
		}
		return value
	}
	var err error
	session, _, err = c.store.SaveBriefRevision(c.t.Context(), write(workflow.SessionEventEdited), store.Brief{
		Problem: "Onboarding is slow.", Scope: []store.BriefItem{{ID: "scope-1", Text: "The onboarding command."}},
	})
	if err != nil {
		c.t.Fatal(err)
	}
	if session, err = c.store.TransitionIdeationSession(c.t.Context(), write(workflow.SessionEventRoleStarted)); err != nil {
		c.t.Fatal(err)
	}
	session, _, err = c.store.SavePlanRevision(c.t.Context(), write(workflow.SessionEventPlanDrafted), store.PlanRevision{
		SourceBriefRevision: session.BriefRevision, ContextRevision: "0123456789abcdef0123456789abcdef01234567",
		Nodes: []store.PlanNode{
			{ID: "n-1", Kind: "spec", Title: "Faster onboarding", Body: "Parent.", BriefRefs: []string{"scope-1"}},
			{ID: "n-2", Kind: "task", Title: "Cache images", Body: "Child.", ParentID: "n-1"},
			{ID: "n-3", Kind: "task", Title: "Document setup", Body: "Docs.", ParentID: "n-1", DependsOn: []string{"n-2"}},
		},
	})
	if err != nil {
		c.t.Fatal(err)
	}
	return session.Revision
}

// previewResult is the part of a preview command result the tests read.
type previewResult struct {
	PlanRevision              int64           `json:"plan_revision"`
	Target                    string          `json:"target"`
	Payload                   json.RawMessage `json:"payload"`
	PayloadSHA256             string          `json:"payload_sha256"`
	RelationshipMappingSHA256 string          `json:"relationship_mapping_sha256"`
}

// preview runs a preview command and returns its decoded result.
func (c *coordinatorForTest) preview(requestID, sessionID string, revision int64) previewResult {
	c.t.Helper()
	c.submit(requestID, sessionID, sessioncommand.KindPreviewPlan, revision, `{"plan_revision":1}`)
	c.drain()
	command := c.requireOutcome(requestID, store.IdeationCommandApplied)
	var result previewResult
	if err := json.Unmarshal(command.Result, &result); err != nil {
		c.t.Fatalf("decode preview result %s: %v", command.Result, err)
	}
	return result
}

// approvePayload returns an approve payload for the previewed result. An
// approval always requests publication, so the payload has no publish flag.
func approvePayload(result previewResult) string {
	return fmt.Sprintf(`{"plan_revision":%d,"payload_sha256":%q,"relationship_mapping_sha256":%q}`,
		result.PlanRevision, result.PayloadSHA256, result.RelationshipMappingSHA256)
}

// TestDuplicateSubmissionAppendsOneTurnAndKeepsTheFirstOutcome verifies that
// a repeated request ID is applied once and later passes do not reapply it.
func TestDuplicateSubmissionAppendsOneTurnAndKeepsTheFirstOutcome(t *testing.T) {
	t.Parallel()

	c := newCoordinatorForTest(t)
	revision := c.createSession("session-1")
	c.submit("turn-1", "session-1", sessioncommand.KindSubmitTurn, revision, `{"body":"Who uses onboarding?"}`)
	c.submit("turn-1", "session-1", sessioncommand.KindSubmitTurn, revision, `{"body":"Who uses onboarding?"}`)
	c.drain()
	c.drain()

	applied := c.requireOutcome("turn-1", store.IdeationCommandApplied)
	history := c.history("session-1")
	if len(history.Turns) != 1 || history.Turns[0].ID != "turn-1" || history.Turns[0].Author != store.IdeationTurnAuthorUser {
		t.Fatalf("turns = %#v, want one user turn", history.Turns)
	}
	if applied.SessionRevision != history.Session.Revision || history.Session.Revision != revision+1 {
		t.Fatalf("command revision = %d, session revision = %d, want %d", applied.SessionRevision, history.Session.Revision, revision+1)
	}
}

// TestStaleTabSubmissionIsAConflictWithTheCurrentRevision verifies that two
// tabs that showed the same revision cannot both change the session.
func TestStaleTabSubmissionIsAConflictWithTheCurrentRevision(t *testing.T) {
	t.Parallel()

	c := newCoordinatorForTest(t)
	revision := c.createSession("session-1")
	c.submit("tab-a", "session-1", sessioncommand.KindSubmitTurn, revision, `{"body":"Tab A answer."}`)
	c.submit("tab-b", "session-1", sessioncommand.KindDefer, revision, `{"reason":"Tab B defers."}`)
	c.drain()

	c.requireOutcome("tab-a", store.IdeationCommandApplied)
	conflict := c.requireOutcome("tab-b", store.IdeationCommandConflict)
	if conflict.SessionRevision != revision+1 || !strings.Contains(conflict.Detail, fmt.Sprint(revision+1)) {
		t.Fatalf("conflict = %#v, want the current revision %d", conflict, revision+1)
	}
	if state := c.history("session-1").Session.State; state != store.IdeationStateAwaitingUser {
		t.Fatalf("state = %q, want the stale defer ignored", state)
	}
}

// TestAcceptedCommandsAreAppliedAfterACoordinatorRestart verifies that an
// accepted command and a command interrupted while applying both complete
// exactly once after a restart.
func TestAcceptedCommandsAreAppliedAfterACoordinatorRestart(t *testing.T) {
	t.Parallel()

	c := newCoordinatorForTest(t)
	revision := c.createSession("session-1")
	c.submit("turn-1", "session-1", sessioncommand.KindSubmitTurn, revision, `{"body":"First."}`)
	if _, err := c.store.BeginIdeationCommand(t.Context(), "turn-1"); err != nil {
		t.Fatal(err)
	}
	c.submit("turn-2", "session-1", sessioncommand.KindSubmitTurn, revision+1, `{"body":"Second."}`)

	c.restart()
	c.drain()

	c.requireOutcome("turn-1", store.IdeationCommandApplied)
	c.requireOutcome("turn-2", store.IdeationCommandApplied)
	if turns := c.history("session-1").Turns; len(turns) != 2 || turns[0].Body != "First." || turns[1].Body != "Second." {
		t.Fatalf("turns = %#v, want both turns once in order", turns)
	}
}

// TestInterruptedCommandStaysPendingUntilTheNextCoordinator verifies that a
// coordinator stopping during a handler leaves the command recoverable, and
// that a replaced handler is the dispatch seam for dependent behavior.
func TestInterruptedCommandStaysPendingUntilTheNextCoordinator(t *testing.T) {
	t.Parallel()

	c := newCoordinatorForTest(t)
	revision := c.createSession("session-1")
	c.submit("turn-1", "session-1", sessioncommand.KindSubmitTurn, revision, `{"body":"Answer."}`)

	ctx, stop := context.WithCancel(t.Context())
	interrupted := c.dispatcher()
	interrupted.Handlers[sessioncommand.KindSubmitTurn] = func(ctx context.Context, _ sessioncommand.Env, _ store.IdeationCommand) (sessioncommand.Outcome, error) {
		stop()
		<-ctx.Done()
		return sessioncommand.Outcome{}, ctx.Err()
	}
	if err := interrupted.Drain(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Drain() error = %v", err)
	}
	interrupted.Wait()
	if command := c.command("turn-1"); command.State != store.IdeationCommandApplying {
		t.Fatalf("interrupted command state = %q, want applying", command.State)
	}

	c.restart()
	c.drain()
	resumed := c.requireOutcome("turn-1", store.IdeationCommandApplied)
	if resumed.Attempts != 2 || len(c.history("session-1").Turns) != 1 {
		t.Fatalf("resumed = %#v, turns = %d; want one turn after the second attempt", resumed, len(c.history("session-1").Turns))
	}
}

// TestMinimalHandlersApplyBriefDraftAndLifecycleCommands verifies the
// coordinator-side handlers that change the session through the lifecycle.
func TestMinimalHandlersApplyBriefDraftAndLifecycleCommands(t *testing.T) {
	t.Parallel()

	c := newCoordinatorForTest(t)
	revision := c.createSession("session-1")
	c.submit("brief-1", "session-1", sessioncommand.KindEditBrief, revision,
		`{"brief":{"problem":"Slow onboarding.","scope":[{"id":"scope-1","text":"The command."}]}}`)
	c.drain()
	revision = c.requireOutcome("brief-1", store.IdeationCommandApplied).SessionRevision

	c.submit("handoff-1", "session-1", sessioncommand.KindHandOff, revision, `{"brief_revision":1}`)
	c.drain()
	c.requireOutcome("handoff-1", store.IdeationCommandUnsupported)

	revision = c.draftPlan("session-1")
	c.submit("draft-1", "session-1", sessioncommand.KindEditDraft, revision,
		`{"nodes":[{"id":"n-1","kind":"task","title":"Only task","body":"Edited.","brief_refs":["scope-1"]}]}`)
	c.drain()
	revision = c.requireOutcome("draft-1", store.IdeationCommandApplied).SessionRevision
	history := c.history("session-1")
	if history.Session.PlanRevision != 2 || len(history.Plans[1].Nodes) != 1 || history.Plans[1].ContextRevision != history.Plans[0].ContextRevision {
		t.Fatalf("plans = %#v, want an edited second revision with the same context", history.Plans)
	}

	for index, kind := range []sessioncommand.Kind{sessioncommand.KindDefer, sessioncommand.KindResume, sessioncommand.KindCancel} {
		requestID := fmt.Sprintf("lifecycle-%d", index)
		c.submit(requestID, "session-1", kind, revision, `{"reason":"Operator decision."}`)
		c.drain()
		command := c.command(requestID)
		if command.Outcome == store.IdeationCommandApplied {
			revision = command.SessionRevision
		}
	}
	c.requireOutcome("lifecycle-0", store.IdeationCommandApplied)
	c.requireOutcome("lifecycle-1", store.IdeationCommandApplied)
	c.requireOutcome("lifecycle-2", store.IdeationCommandApplied)
	if state := c.history("session-1").Session.State; state != store.IdeationStateCancelled {
		t.Fatalf("state = %q, want cancelled", state)
	}
	c.submit("turn-late", "session-1", sessioncommand.KindSubmitTurn, revision, `{"body":"Too late."}`)
	c.drain()
	c.requireOutcome("turn-late", store.IdeationCommandRefused)
}

// TestApprovalBindsThePreviewAndPublishesWithTheRequestID verifies preview,
// approve, and publish through the inbox: the approval binds the previewed
// digests, the request ID becomes the receipt, and a changed preview is
// refused before any tracker write.
func TestApprovalBindsThePreviewAndPublishesWithTheRequestID(t *testing.T) {
	t.Parallel()

	c := newCoordinatorForTest(t)
	c.createSession("session-1")
	revision := c.draftPlan("session-1")
	preview := c.preview("preview-1", "session-1", revision)
	if preview.Target != "fake:owner/repo" || !strings.Contains(string(preview.Payload), "Cache images") {
		t.Fatalf("preview = %#v, want the rendered payload", preview)
	}
	if c.fake.Writes() != 0 {
		t.Fatalf("Writes() = %d after preview, want 0", c.fake.Writes())
	}

	wrong := preview
	wrong.PayloadSHA256 = strings.Repeat("0", 64)
	c.submit("approve-wrong", "session-1", sessioncommand.KindApprove, revision, approvePayload(wrong))
	c.drain()
	c.requireOutcome("approve-wrong", store.IdeationCommandRefused)
	if c.fake.Writes() != 0 || c.history("session-1").Session.ApprovalID != "" {
		t.Fatal("a refused approval wrote to the tracker or recorded an approval")
	}

	c.submit("approve-1", "session-1", sessioncommand.KindApprove, revision, approvePayload(preview))
	c.submit("approve-1", "session-1", sessioncommand.KindApprove, revision, approvePayload(preview))
	c.drain()
	c.requireOutcome("approve-1", store.IdeationCommandApplied)
	history := c.history("session-1")
	if history.Session.State != store.IdeationStatePublished || history.Session.ApprovalID != "approve-1" {
		t.Fatalf("session = %#v, want published under approve-1", history.Session)
	}
	if len(history.Receipts) != 1 || history.Receipts[0].ID != "approve-1" || len(c.fake.Items()) != 3 {
		t.Fatalf("receipts = %#v, items = %d; want one receipt and three items", history.Receipts, len(c.fake.Items()))
	}
	if run, err := c.store.CurrentRun(t.Context()); err != nil || run != nil {
		t.Fatalf("CurrentRun() = %#v, %v; publication must not start a run", run, err)
	}
}

// TestIncompletePublicationIsReconciledAndRetriedThroughCommands verifies the
// retry and reconcile commands after an ambiguous create.
func TestIncompletePublicationIsReconciledAndRetriedThroughCommands(t *testing.T) {
	t.Parallel()

	c := newCoordinatorForTest(t)
	c.createSession("session-1")
	revision := c.draftPlan("session-1")
	preview := c.preview("preview-1", "session-1", revision)
	c.fake.Inject(trackertest.Fault{Op: trackertest.OpCreateItem, Key: "n-2", Err: errors.New("connection reset")})
	c.submit("approve-1", "session-1", sessioncommand.KindApprove, revision, approvePayload(preview))
	c.drain()
	approved := c.requireOutcome("approve-1", store.IdeationCommandApplied)
	if !strings.Contains(approved.Detail, "incomplete") {
		t.Fatalf("approve detail = %q, want an incomplete publication", approved.Detail)
	}
	if state := c.history("session-1").Session.State; state != store.IdeationStatePublicationIncomplete {
		t.Fatalf("state = %q, want publication_incomplete", state)
	}

	revision = approved.SessionRevision
	c.submit("reconcile-1", "session-1", sessioncommand.KindReconcile, revision, `{"operation_id":"item/n-2","external_id":""}`)
	c.drain()
	c.requireOutcome("reconcile-1", store.IdeationCommandApplied)
	c.submit("publish-1", "session-1", sessioncommand.KindPublish, revision, `{}`)
	c.drain()
	c.requireOutcome("publish-1", store.IdeationCommandApplied)
	if state := c.history("session-1").Session.State; state != store.IdeationStatePublished || len(c.fake.Items()) != 3 {
		t.Fatalf("state = %q, items = %d; want published with three items", state, len(c.fake.Items()))
	}
}

// blockingTracker blocks the first item create until release is closed.
type blockingTracker struct {
	*trackertest.PlanTracker
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

// CreatePlanItem signals started and waits for release once.
func (b *blockingTracker) CreatePlanItem(ctx context.Context, repository tracker.Repository, item tracker.RenderedPlanItem) (tracker.PlanItemRef, error) {
	b.once.Do(func() {
		close(b.started)
		<-b.release
	})
	return b.PlanTracker.CreatePlanItem(ctx, repository, item)
}

// TestCancelIsAppliedWhilePublicationRuns verifies that publication runs
// beside other commands, so a cancel stops future tracker writes and keeps
// created items.
func TestCancelIsAppliedWhilePublicationRuns(t *testing.T) {
	t.Parallel()

	c := newCoordinatorForTest(t)
	blocking := &blockingTracker{PlanTracker: c.fake, started: make(chan struct{}), release: make(chan struct{})}
	c.tracker = blocking
	c.createSession("session-1")
	revision := c.draftPlan("session-1")
	preview := c.preview("preview-1", "session-1", revision)
	c.submit("approve-1", "session-1", sessioncommand.KindApprove, revision, approvePayload(preview))

	dispatcher := c.dispatcher()
	if err := dispatcher.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-blocking.started
	publishing := c.history("session-1").Session
	c.submit("cancel-1", "session-1", sessioncommand.KindCancel, publishing.Revision, `{"reason":"Stop."}`)
	if err := dispatcher.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	c.requireOutcome("cancel-1", store.IdeationCommandApplied)
	close(blocking.release)
	dispatcher.Wait()

	c.requireOutcome("approve-1", store.IdeationCommandRefused)
	if state := c.history("session-1").Session.State; state != store.IdeationStateCancelled {
		t.Fatalf("state = %q, want cancelled", state)
	}
	if items := len(c.fake.Items()); items != 1 {
		t.Fatalf("items = %d, want the one create that was in flight and no later create", items)
	}
}

// TestPlanningCommandsAreUnsupportedWithoutAPlanningTracker verifies the
// explicit result when the tracker adapter cannot publish plans.
func TestPlanningCommandsAreUnsupportedWithoutAPlanningTracker(t *testing.T) {
	t.Parallel()

	c := newCoordinatorForTest(t)
	c.tracker = nil
	c.createSession("session-1")
	revision := c.draftPlan("session-1")
	c.submit("preview-1", "session-1", sessioncommand.KindPreviewPlan, revision, `{"plan_revision":1}`)
	c.drain()
	c.requireOutcome("preview-1", store.IdeationCommandUnsupported)
}

// rolesForTest records role starts. When interrupt is set, the first
// ideation start stops the coordinator instead.
type rolesForTest struct {
	mu        sync.Mutex
	calls     []string
	interrupt context.CancelFunc
}

// StartIdeation records the start, or stops the coordinator once.
func (r *rolesForTest) StartIdeation(_ context.Context, sessionID, turnID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.interrupt != nil {
		r.interrupt()
		r.interrupt = nil
		return context.Canceled
	}
	r.calls = append(r.calls, "ideation:"+sessionID+":"+turnID)
	return nil
}

// StartPlanning records the start.
func (r *rolesForTest) StartPlanning(_ context.Context, sessionID, requestID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "planning:"+sessionID+":"+requestID)
	return nil
}

// TestAcceptedTurnAndHandOffStartSessionRoles verifies that an accepted turn
// starts the ideation role and a handoff starts planning, and that a
// coordinator stop between the turn and the role start still starts the
// role once on resume.
func TestAcceptedTurnAndHandOffStartSessionRoles(t *testing.T) {
	t.Parallel()

	c := newCoordinatorForTest(t)
	revision := c.createSession("session-1")
	ctx, stop := context.WithCancel(t.Context())
	roles := &rolesForTest{interrupt: stop}
	c.submit("turn-1", "session-1", sessioncommand.KindSubmitTurn, revision, `{"body":"Who uses this?"}`)

	interrupted := c.dispatcher()
	interrupted.Env.Roles = roles
	_ = interrupted.Drain(ctx)
	if command := c.command("turn-1"); command.State != store.IdeationCommandApplying {
		t.Fatalf("command = %#v, want applying after the interrupted role start", command)
	}

	resumed := c.dispatcher()
	resumed.Env.Roles = roles
	if err := resumed.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	applied := c.requireOutcome("turn-1", store.IdeationCommandApplied)
	if len(c.history("session-1").Turns) != 1 || len(roles.calls) != 1 || roles.calls[0] != "ideation:session-1:turn-1" {
		t.Fatalf("turns = %d, role starts = %#v; want one turn and one ideation start", len(c.history("session-1").Turns), roles.calls)
	}

	c.submit("brief-1", "session-1", sessioncommand.KindEditBrief, applied.SessionRevision, `{"brief":{"problem":"Slow."}}`)
	c.drain()
	revision = c.requireOutcome("brief-1", store.IdeationCommandApplied).SessionRevision
	c.submit("handoff-1", "session-1", sessioncommand.KindHandOff, revision, `{"brief_revision":1}`)
	planner := c.dispatcher()
	planner.Env.Roles = roles
	if err := planner.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	c.requireOutcome("handoff-1", store.IdeationCommandApplied)
	if roles.calls[len(roles.calls)-1] != "planning:session-1:handoff-1" {
		t.Fatalf("role starts = %#v, want a planning start for the handoff", roles.calls)
	}
}

// TestEditedDraftIsValidatedBeforeItIsSaved verifies that a user edit of the
// draft goes through plan validation and an invalid graph is refused.
func TestEditedDraftIsValidatedBeforeItIsSaved(t *testing.T) {
	t.Parallel()

	c := newCoordinatorForTest(t)
	c.createSession("session-1")
	revision := c.draftPlan("session-1")
	c.submit("draft-1", "session-1", sessioncommand.KindEditDraft, revision,
		`{"nodes":[{"id":"n-1","kind":"task","title":"Only task","body":"Edited."}]}`)
	dispatcher := c.dispatcher()
	dispatcher.Env.ValidatePlan = func(nodes []store.PlanNode) error {
		return errors.New("node n-1 needs at least one acceptance criterion")
	}
	if err := dispatcher.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	refused := c.requireOutcome("draft-1", store.IdeationCommandRefused)
	if !strings.Contains(refused.Detail, "acceptance criterion") || c.history("session-1").Session.PlanRevision != 1 {
		t.Fatalf("refused = %#v, want the validation problem and no new plan revision", refused)
	}
}

// TestApprovalRefusesAnInvalidPlanGraph verifies that the coordinator checks
// plan validation again at approval, so an invalid graph cannot be approved
// even with a matching preview.
func TestApprovalRefusesAnInvalidPlanGraph(t *testing.T) {
	t.Parallel()

	c := newCoordinatorForTest(t)
	c.createSession("session-1")
	revision := c.draftPlan("session-1")
	preview := c.preview("preview-1", "session-1", revision)
	c.submit("approve-1", "session-1", sessioncommand.KindApprove, revision, approvePayload(preview))
	dispatcher := c.dispatcher()
	dispatcher.Env.ValidatePlan = func(nodes []store.PlanNode) error {
		return errors.New(`node "n-1" has kind "spec"`)
	}
	if err := dispatcher.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	dispatcher.Wait()
	refused := c.requireOutcome("approve-1", store.IdeationCommandRefused)
	if !strings.Contains(refused.Detail, `kind "spec"`) || c.history("session-1").Session.ApprovalID != "" || c.fake.Writes() != 0 {
		t.Fatalf("refused = %#v, want the validation problem, no approval, and no tracker write", refused)
	}
}

// TestStaleDraftIsNotEditable verifies that an edit of a draft whose brief
// changed is refused: the draft stays stale and needs regeneration from the
// current brief, so an edit cannot make it approvable.
func TestStaleDraftIsNotEditable(t *testing.T) {
	t.Parallel()

	c := newCoordinatorForTest(t)
	c.createSession("session-1")
	revision := c.draftPlan("session-1")
	c.submit("brief-2", "session-1", sessioncommand.KindEditBrief, revision,
		`{"brief":{"problem":"Onboarding is slow for new teams.","scope":[{"id":"scope-1","text":"The onboarding command."}]}}`)
	c.drain()
	revision = c.requireOutcome("brief-2", store.IdeationCommandApplied).SessionRevision

	c.submit("draft-stale", "session-1", sessioncommand.KindEditDraft, revision,
		`{"nodes":[{"id":"n-1","kind":"task","title":"Only task","body":"Edited.","brief_refs":["scope-1"]}]}`)
	c.drain()
	refused := c.requireOutcome("draft-stale", store.IdeationCommandRefused)
	if !strings.Contains(refused.Detail, "regenerate") {
		t.Fatalf("refusal = %q, want it to name regeneration", refused.Detail)
	}
	history := c.history("session-1")
	if history.Session.PlanRevision != 1 || !history.Plans[0].Stale {
		t.Fatalf("plans = %#v, want the one stale draft unchanged", history.Plans)
	}
}

// deferAndResume parks session-1 and resumes it through the inbox with a
// dispatcher that records role starts, and returns the resumed session.
func (c *coordinatorForTest) deferAndResume(revision int64, roles *rolesForTest) store.IdeationSession {
	c.t.Helper()
	for _, step := range []struct {
		requestID string
		kind      sessioncommand.Kind
	}{{"defer-1", sessioncommand.KindDefer}, {"resume-1", sessioncommand.KindResume}} {
		c.submit(step.requestID, "session-1", step.kind, revision, `{"reason":"Back next week."}`)
		dispatcher := c.dispatcher()
		dispatcher.Env.Roles = roles
		if err := dispatcher.Drain(c.t.Context()); err != nil {
			c.t.Fatal(err)
		}
		revision = c.requireOutcome(step.requestID, store.IdeationCommandApplied).SessionRevision
	}
	return c.history("session-1").Session
}

// TestResumedSessionKeepsItsPlanUnderReview verifies that a session parked
// during plan review resumes in review with the same plan revision, and that
// a session parked after approval resumes with its approval, so neither
// needs a new planning run.
func TestResumedSessionKeepsItsPlanUnderReview(t *testing.T) {
	t.Parallel()

	t.Run("draft review", func(t *testing.T) {
		t.Parallel()
		c := newCoordinatorForTest(t)
		c.createSession("session-1")
		roles := &rolesForTest{}
		resumed := c.deferAndResume(c.draftPlan("session-1"), roles)
		if resumed.State != store.IdeationStateDraftReview || resumed.PlanRevision != 1 || len(roles.calls) != 0 {
			t.Fatalf("resumed = %#v, role starts = %v; want draft review of plan 1 without a role start", resumed, roles.calls)
		}
	})

	t.Run("approved", func(t *testing.T) {
		t.Parallel()
		c := newCoordinatorForTest(t)
		c.createSession("session-1")
		c.draftPlan("session-1")
		session := c.history("session-1").Session
		binding, err := c.dispatcher().Env.Publisher.Preview(t.Context(), "session-1")
		if err != nil {
			t.Fatal(err)
		}
		write, err := workflow.DefaultSessionLifecycle().Write(session, workflow.SessionEventApproved, "", dispatchNow)
		if err != nil {
			t.Fatal(err)
		}
		if session, _, err = c.store.ApprovePlan(t.Context(), write, binding.Approval("approval-1")); err != nil {
			t.Fatal(err)
		}
		roles := &rolesForTest{}
		resumed := c.deferAndResume(session.Revision, roles)
		if resumed.State != store.IdeationStateApproved || resumed.ApprovalID != "approval-1" || len(roles.calls) != 0 {
			t.Fatalf("resumed = %#v, role starts = %v; want the approval kept without a role start", resumed, roles.calls)
		}
		c.submit("publish-1", "session-1", sessioncommand.KindPublish, resumed.Revision, `{}`)
		c.drain()
		c.requireOutcome("publish-1", store.IdeationCommandApplied)
		if state := c.history("session-1").Session.State; state != store.IdeationStatePublished {
			t.Fatalf("state = %q, want the resumed approval published", state)
		}
	})

	t.Run("no plan yet", func(t *testing.T) {
		t.Parallel()
		c := newCoordinatorForTest(t)
		resumed := c.deferAndResume(c.createSession("session-1"), &rolesForTest{})
		if resumed.State != store.IdeationStateAwaitingUser {
			t.Fatalf("state = %q, want the conversation resumed", resumed.State)
		}
	})
}

// crashDuringPublication approves session-1 through an approve command and
// leaves the state a coordinator crash leaves: the command applying, the
// approval recorded, and the session publishing without a receipt. It then
// restarts the coordinator store.
func (c *coordinatorForTest) crashDuringPublication() {
	c.t.Helper()
	c.createSession("session-1")
	revision := c.draftPlan("session-1")
	c.submit("approve-1", "session-1", sessioncommand.KindApprove, revision, approvePayload(c.preview("preview-1", "session-1", revision)))
	if _, err := c.store.BeginIdeationCommand(c.t.Context(), "approve-1"); err != nil {
		c.t.Fatal(err)
	}
	lifecycle := workflow.DefaultSessionLifecycle()
	session := c.history("session-1").Session
	binding, err := c.dispatcher().Env.Publisher.Preview(c.t.Context(), "session-1")
	if err != nil {
		c.t.Fatal(err)
	}
	write, err := lifecycle.Write(session, workflow.SessionEventApproved, "", dispatchNow)
	if err != nil {
		c.t.Fatal(err)
	}
	if session, _, err = c.store.ApprovePlan(c.t.Context(), write, binding.Approval("approve-1")); err != nil {
		c.t.Fatal(err)
	}
	if write, err = lifecycle.Write(session, workflow.SessionEventPublicationStarted, "", dispatchNow); err != nil {
		c.t.Fatal(err)
	}
	if _, err = c.store.TransitionIdeationSession(c.t.Context(), write); err != nil {
		c.t.Fatal(err)
	}
	c.restart()
}

// requireRecoverable fails unless session-1 left publishing for
// publication_incomplete with a reason that contains detail, and a retry
// then publishes the same approval.
func (c *coordinatorForTest) requireRecoverable(detail string) {
	c.t.Helper()
	session := c.history("session-1").Session
	if session.State != store.IdeationStatePublicationIncomplete || !strings.Contains(session.StateReason, detail) {
		c.t.Fatalf("session = %#v, want publication_incomplete naming %q", session, detail)
	}
	c.submit("publish-retry", "session-1", sessioncommand.KindPublish, session.Revision, `{}`)
	c.drain()
	c.requireOutcome("publish-retry", store.IdeationCommandApplied)
	if state := c.history("session-1").Session.State; state != store.IdeationStatePublished || len(c.fake.Items()) != 3 {
		c.t.Fatalf("state = %q, items = %d; want the retry to publish three items", state, len(c.fake.Items()))
	}
}

// TestPublicationInterruptedByACrashIsNeverStuck verifies the crash-resume
// path of an approve-and-publish command: the resumed attempt publishes, and
// an attempt that cannot finish leaves publication_incomplete with its
// reason, where a retry continues the same approval.
func TestPublicationInterruptedByACrashIsNeverStuck(t *testing.T) {
	t.Parallel()

	t.Run("resumed attempt publishes", func(t *testing.T) {
		t.Parallel()
		c := newCoordinatorForTest(t)
		c.crashDuringPublication()
		c.drain()
		if resumed := c.requireOutcome("approve-1", store.IdeationCommandApplied); resumed.Attempts != 2 {
			t.Fatalf("attempts = %d, want the second attempt to finish", resumed.Attempts)
		}
		if state := c.history("session-1").Session.State; state != store.IdeationStatePublished {
			t.Fatalf("state = %q, want published", state)
		}
	})

	t.Run("resumed preflight fails", func(t *testing.T) {
		t.Parallel()
		c := newCoordinatorForTest(t)
		c.crashDuringPublication()
		c.fake.Inject(trackertest.Fault{Op: trackertest.OpPreflight, Err: tracker.NewPlanError(tracker.PlanErrorPermission, "token expired")})
		c.drain()
		c.requireOutcome("approve-1", store.IdeationCommandRefused)
		c.requireRecoverable("token expired")
	})

	t.Run("attempt bound reached", func(t *testing.T) {
		t.Parallel()
		c := newCoordinatorForTest(t)
		c.crashDuringPublication()
		for range 2 {
			if _, err := c.store.BeginIdeationCommand(t.Context(), "approve-1"); err != nil {
				t.Fatal(err)
			}
		}
		c.drain()
		c.requireOutcome("approve-1", store.IdeationCommandFailed)
		c.requireRecoverable("without an outcome")
	})
}
