package webui_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/plangraph"
	"github.com/Stevie1704/sw-factory/internal/publication"
	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/tracker/trackertest"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// planPath is the review page of the fixture session.
const planPath = "/sessions/session-1/plan"

// planUI is a command-capable UI over a session in draft review, with a
// coordinator that applies inbox commands against a fake planning tracker.
type planUI struct {
	sessionUI
	fake *trackertest.PlanTracker
	// tracker is the planning port the coordinator uses. It is the fake
	// unless a test wraps it.
	tracker tracker.PlanPublisher
}

// planNodesForTest is a valid three-node plan: a parent with two tasks, the
// second of which depends on the first.
func planNodesForTest() []store.PlanNode {
	return []store.PlanNode{
		{ID: "n-1", Kind: plangraph.KindParent, Title: "Faster onboarding", Body: "Group the onboarding work.",
			AcceptanceCriteria: []string{"Onboarding takes under five minutes."}, BriefRefs: []string{"scope-1"}},
		{ID: "n-2", Kind: plangraph.KindTask, Title: "Cache worker images", Body: "Keep pulled images between runs.",
			AcceptanceCriteria: []string{"A second onboarding pulls no image."}, ParentID: "n-1"},
		{ID: "n-3", Kind: plangraph.KindTask, Title: "Document the setup", Body: "Explain the image cache.",
			AcceptanceCriteria: []string{"The guide names the cache."}, ParentID: "n-1", DependsOn: []string{"n-2"}},
	}
}

// newPlanUI returns the session UI with session-1 in draft review of nodes,
// as session roles leave it.
func newPlanUI(t *testing.T, nodes []store.PlanNode) planUI {
	t.Helper()
	ui := planUI{sessionUI: newSessionUI(t, true), fake: trackertest.NewPlanTracker()}
	ui.tracker = ui.fake
	opened, err := store.Open(t.Context(), ui.storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	lifecycle := workflow.DefaultSessionLifecycle()
	session, err := opened.IdeationSession(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	write := func(event workflow.SessionEvent) store.IdeationWrite {
		value, err := lifecycle.Write(session, event, "", fixtureNow)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	if session, _, err = opened.SaveBriefRevision(t.Context(), write(workflow.SessionEventEdited), store.Brief{
		Problem: "Onboarding is slow.", Scope: []store.BriefItem{{ID: "scope-1", Text: "The onboarding command."}},
	}); err != nil {
		t.Fatal(err)
	}
	if session, err = opened.TransitionIdeationSession(t.Context(), write(workflow.SessionEventRoleStarted)); err != nil {
		t.Fatal(err)
	}
	if _, _, err = opened.SavePlanRevision(t.Context(), write(workflow.SessionEventPlanDrafted), store.PlanRevision{
		SourceBriefRevision: session.BriefRevision, ContextRevision: baseSHA, Nodes: nodes,
	}); err != nil {
		t.Fatal(err)
	}
	return ui
}

// drain runs one coordinator pass over the inbox, as `factory start` does,
// and waits for its tracker work.
func (ui planUI) drain(t *testing.T) {
	t.Helper()
	opened, err := store.Open(t.Context(), ui.storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	dispatcher := sessioncommand.NewDispatcher(ui.env(opened))
	if err := dispatcher.Drain(t.Context()); err != nil {
		t.Fatalf("Drain() error = %v", err)
	}
	dispatcher.Wait()
}

// env is the coordinator environment over opened.
func (ui planUI) env(opened *store.Store) sessioncommand.Env {
	lifecycle := workflow.DefaultSessionLifecycle()
	return sessioncommand.Env{
		Store: opened, Lifecycle: lifecycle, ValidatePlan: plangraph.Validate, RepositoryPath: "/repos/project",
		Now: func() time.Time { return fixtureNow },
		Publisher: &publication.Publisher{
			Store: opened, Tracker: ui.tracker, Repository: tracker.Repository{Owner: "example", Name: "project"},
			Lifecycle: lifecycle, Now: func() time.Time { return fixtureNow },
		},
	}
}

// page reads the plan review page and requires 200.
func (ui planUI) page(t *testing.T) string {
	t.Helper()
	response := get(t, ui.handler, planPath)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d; body:\n%s", planPath, response.Code, response.Body)
	}
	body := response.Body.String()
	assertNoExternalResources(t, body)
	return body
}

// send posts one command as a page script does and requires an accepted or
// replayed answer.
func (ui planUI) send(t *testing.T, requestID string, kind sessioncommand.Kind, expected int64, payload string) {
	t.Helper()
	body := `{"request_id":"` + requestID + `","session_id":"session-1","kind":"` + string(kind) + `","expected_revision":` +
		strconv.FormatInt(expected, 10) + `,"payload":` + payload + `}`
	if response := ui.post(t, body, nil); response.Code != http.StatusAccepted && response.Code != http.StatusOK {
		t.Fatalf("POST %s status = %d; body:\n%s", kind, response.Code, response.Body)
	}
}

// history reads the session history directly.
func (ui planUI) history(t *testing.T) store.IdeationHistory {
	t.Helper()
	opened, err := store.OpenReadOnly(t.Context(), ui.storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	history, err := opened.IdeationHistory(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	return history
}

// command reads one command directly.
func (ui planUI) command(t *testing.T, requestID string) store.IdeationCommand {
	t.Helper()
	opened, err := store.OpenReadOnly(t.Context(), ui.storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	command, err := opened.IdeationCommand(t.Context(), requestID)
	if err != nil {
		t.Fatal(err)
	}
	return command
}

// draftData is the editor model a page hands to its script.
type draftData struct {
	SessionID        string                        `json:"session_id"`
	ExpectedRevision int64                         `json:"expected_revision"`
	PlanRevision     int64                         `json:"plan_revision"`
	Nodes            []sessioncommand.PlanNodeJSON `json:"nodes"`
}

// draftBlock matches the editor model block of the review page.
var draftBlock = regexp.MustCompile(`(?s)<script type="application/json" id="plan-draft-data">(.*?)</script>`)

// readDraft decodes the editor model of page.
func readDraft(t *testing.T, page string) draftData {
	t.Helper()
	match := draftBlock.FindStringSubmatch(page)
	if match == nil {
		t.Fatalf("page has no editor model:\n%s", page)
	}
	var data draftData
	if err := json.Unmarshal([]byte(match[1]), &data); err != nil {
		t.Fatalf("decode editor model %s: %v", match[1], err)
	}
	return data
}

// TestPlanReviewShowsEditableItemsAndRelationships verifies that the review
// page shows every item with its content and relationships, hands the
// complete draft to the editor bound to the displayed revisions, offers
// regeneration and preview, and states that publication does not start
// implementation.
func TestPlanReviewShowsEditableItemsAndRelationships(t *testing.T) {
	t.Parallel()

	ui := newPlanUI(t, planNodesForTest())
	page := ui.page(t)
	assertContains(t, page,
		"Faster onboarding", "Cache worker images", "Keep pulled images between runs.", "A second onboarding pulls no image.",
		"Document the setup", `data-node-id="n-3"`, "depends on", `href="#node-n-2"`, "scope-1",
		`data-session-command="regenerate_draft"`, `data-session-command="preview_plan"`, `<script src="/assets/plan.js" defer></script>`,
		"does not start implementation",
	)
	draft := readDraft(t, page)
	history := ui.history(t)
	if draft.SessionID != "session-1" || draft.ExpectedRevision != history.Session.Revision || draft.PlanRevision != 1 || len(draft.Nodes) != 3 ||
		draft.Nodes[2].DependsOn[0] != "n-2" || draft.Nodes[1].ParentID != "n-1" {
		t.Fatalf("editor model = %#v, want the current draft at revision %d", draft, history.Session.Revision)
	}
	if strings.Contains(page, `data-session-command="approve"`) {
		t.Fatal("page offers approval before a preview")
	}
}

// TestInvalidPlanShowsProblemsPerItemAndCannotBePreviewed verifies that the
// page shows each validation problem at its item and offers no preview or
// approval for an invalid graph.
func TestInvalidPlanShowsProblemsPerItemAndCannotBePreviewed(t *testing.T) {
	t.Parallel()

	nodes := planNodesForTest()[:2]
	ui := newPlanUI(t, nodes)
	page := ui.page(t)
	assertContains(t, page, "data-plan-invalid", `data-node-id="n-1" data-invalid`, `parent &#34;n-1&#34; has 1 children`)
	for _, unwanted := range []string{`data-session-command="preview_plan"`, `data-session-command="approve"`} {
		if strings.Contains(page, unwanted) {
			t.Errorf("invalid plan page contains %s", unwanted)
		}
	}
	if !strings.Contains(page, `id="plan-draft-data"`) {
		t.Error("invalid plan page offers no editor to repair it")
	}
}

// TestStaleDraftOffersOnlyRegeneration verifies that a draft whose brief
// changed offers no editor, preview, or approval: only regeneration from the
// current brief.
func TestStaleDraftOffersOnlyRegeneration(t *testing.T) {
	t.Parallel()

	ui := newPlanUI(t, planNodesForTest())
	revision := ui.history(t).Session.Revision
	ui.send(t, "brief-2", sessioncommand.KindEditBrief, revision,
		`{"brief":{"problem":"Onboarding is slow for new teams.","scope":[{"id":"scope-1","text":"The onboarding command."}]}}`)
	ui.drain(t)
	page := ui.page(t)
	assertContains(t, page, "but the brief changed", `data-session-command="regenerate_draft"`)
	for _, unwanted := range []string{`id="plan-draft-data"`, `data-session-command="preview_plan"`, `data-session-command="approve"`} {
		if strings.Contains(page, unwanted) {
			t.Errorf("stale draft page contains %s", unwanted)
		}
	}
}

// intermediateTracker wraps the fake planning tracker with a provider that
// needs an intermediate hierarchy level between a parent and its children,
// as an Azure DevOps process with a Feature level does.
type intermediateTracker struct {
	*trackertest.PlanTracker
}

// PreflightPlan adds the Feature item n-1--feature below n-1 and moves the
// children of n-1 below it.
func (i intermediateTracker) PreflightPlan(ctx context.Context, repository tracker.Repository, graph tracker.PlanGraph) (tracker.PlanPreview, error) {
	preview, err := i.PlanTracker.PreflightPlan(ctx, repository, graph)
	if err != nil {
		return preview, err
	}
	feature := tracker.RenderedPlanItem{
		NodeID: "n-1--feature", Type: "Feature", Title: "Faster onboarding", Body: "Feature level of n-1.",
		Fields: map[string]string{"Microsoft.VSTS.Common.ValueArea": "Business"}, Correlation: tracker.PlanCorrelation(graph.SessionID, graph.PlanRevision, "n-1") + "--feature",
	}
	preview.Items = append(preview.Items[:1], append([]tracker.RenderedPlanItem{feature}, preview.Items[1:]...)...)
	for index, relationship := range preview.Relationships {
		if relationship.Kind == tracker.PlanRelationshipParent && relationship.ToNodeID == "n-1" {
			preview.Relationships[index].ToNodeID = feature.NodeID
		}
	}
	preview.Relationships = append(preview.Relationships, tracker.PlanRelationshipMapping{
		Kind: tracker.PlanRelationshipParent, FromNodeID: feature.NodeID, ToNodeID: "n-1", Mode: tracker.PlanRelationshipNative,
	})
	return preview, nil
}

// previewOf reads the decoded result of a completed preview command.
func (ui planUI) previewOf(t *testing.T, requestID string) sessioncommand.PreviewResult {
	t.Helper()
	command := ui.command(t, requestID)
	if command.Outcome != store.IdeationCommandApplied {
		t.Fatalf("preview %s = %q (%s), want applied", requestID, command.Outcome, command.Detail)
	}
	var result sessioncommand.PreviewResult
	if err := json.Unmarshal(command.Result, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// TestPreviewShowsTheRenderedPublicationAndBindsApproval verifies that a
// preview through the coordinator shows the target, the resolved types, the
// full rendered content with its traceability metadata, every relationship
// with its representation, and the intermediate items the provider needs,
// without a tracker write, and that the approval form binds its digests.
func TestPreviewShowsTheRenderedPublicationAndBindsApproval(t *testing.T) {
	t.Parallel()

	ui := newPlanUI(t, planNodesForTest())
	ui.fake.Types = map[string]string{plangraph.KindParent: "Epic", plangraph.KindTask: "User Story"}
	ui.fake.Labels = []string{"planned"}
	ui.fake.Mode = tracker.PlanRelationshipTextual
	ui.tracker = intermediateTracker{PlanTracker: ui.fake}
	revision := ui.history(t).Session.Revision
	ui.send(t, "preview-1", sessioncommand.KindPreviewPlan, revision, `{"plan_revision":1}`)
	ui.drain(t)
	result := ui.previewOf(t, "preview-1")
	if ui.fake.Writes() != 0 {
		t.Fatalf("Writes() = %d after a preview, want 0", ui.fake.Writes())
	}

	page := ui.page(t)
	assertContains(t, page,
		"<code>fake:example/project</code>", `<span class="kind">Epic</span>`, `<span class="kind">User Story</span>`,
		`data-rendered-node="n-1--feature" data-intermediate`, "Intermediate item", "Microsoft.VSTS.Common.ValueArea",
		"## Acceptance criteria\n- [ ] A second onboarding pulls no image.", "&lt;!-- factory-plan: session-1/1/n-2 --&gt;",
		"<code>session-1/1/n-3</code>", "<code>planned</code>",
		`data-relationship="parent:n-2:n-1--feature"`, `data-relationship="depends_on:n-3:n-2"`, `data-relationship="parent:n-1--feature:n-1"`,
		"data-textual-fallback",
		`data-session-command="approve"`, `value="`+result.PayloadSHA256+`"`, `value="`+result.RelationshipMappingSHA256+`"`,
		`data-expected-revision="`+strconv.FormatInt(revision, 10)+`"`,
	)
}

// approveBody returns the approve payload that binds result, as the approval
// form sends it.
func approveBody(result sessioncommand.PreviewResult) string {
	return `{"plan_revision":` + strconv.FormatInt(result.PlanRevision, 10) + `,"payload_sha256":"` + result.PayloadSHA256 +
		`","relationship_mapping_sha256":"` + result.RelationshipMappingSHA256 + `"}`
}

// previewed returns the UI after a preview of plan revision 1 and the
// preview result.
func previewedPlanUI(t *testing.T) (planUI, sessioncommand.PreviewResult) {
	t.Helper()
	ui := newPlanUI(t, planNodesForTest())
	ui.send(t, "preview-1", sessioncommand.KindPreviewPlan, ui.history(t).Session.Revision, `{"plan_revision":1}`)
	ui.drain(t)
	return ui, ui.previewOf(t, "preview-1")
}

// TestDoubleClickedApprovalPublishesOnce verifies that a repeated approval
// request and a second approval sent before the coordinator applied the
// first record one approval and publish each item once, and that the page
// hides the approval while the request is pending.
func TestDoubleClickedApprovalPublishesOnce(t *testing.T) {
	t.Parallel()

	ui, result := previewedPlanUI(t)
	revision := ui.history(t).Session.Revision
	ui.send(t, "approve-1", sessioncommand.KindApprove, revision, approveBody(result))
	ui.send(t, "approve-1", sessioncommand.KindApprove, revision, approveBody(result))
	ui.send(t, "approve-2", sessioncommand.KindApprove, revision, approveBody(result))
	pending := ui.page(t)
	assertContains(t, pending, "data-pending-commands", "request <code>approve-1</code> is accepted", "An accepted request is not a completed one")
	if strings.Contains(pending, `data-session-command="approve"`) {
		t.Fatal("page offers approval again while an approval is pending")
	}

	// One pass runs one tracker command per session; the next pass applies
	// the second approval.
	ui.drain(t)
	ui.drain(t)
	if command := ui.command(t, "approve-1"); command.Outcome != store.IdeationCommandApplied {
		t.Fatalf("approve-1 = %q (%s), want applied", command.Outcome, command.Detail)
	}
	if command := ui.command(t, "approve-2"); command.Outcome != store.IdeationCommandConflict {
		t.Fatalf("approve-2 = %q (%s), want a conflict", command.Outcome, command.Detail)
	}
	history := ui.history(t)
	if len(history.Approvals) != 1 || len(history.Receipts) != 1 || len(ui.fake.Items()) != 3 || history.Session.State != store.IdeationStatePublished {
		t.Fatalf("approvals = %d, receipts = %d, items = %d, state = %q; want one published approval with three items",
			len(history.Approvals), len(history.Receipts), len(ui.fake.Items()), history.Session.State)
	}
	published := ui.page(t)
	assertContains(t, published, "3 items created", `<a href="https://tracker.invalid/items/1" rel="noreferrer noopener">1</a>`,
		"Publication is complete. No implementation started.", "Approved publication")
	for _, unwanted := range []string{`data-session-command="approve"`, `data-session-command="publish"`, `id="plan-draft-data"`} {
		if strings.Contains(published, unwanted) {
			t.Errorf("published page contains %s", unwanted)
		}
	}
}

// TestStaleTabsCannotPublishAnOldPreview verifies that an edit in one tab,
// or a provider mapping change, stops another tab from approving the preview
// it still shows.
func TestStaleTabsCannotPublishAnOldPreview(t *testing.T) {
	t.Parallel()

	ui, result := previewedPlanUI(t)
	revision := ui.history(t).Session.Revision
	edited := sessioncommand.PlanNodesFromStore(planNodesForTest())
	edited[1].Title = "Cache worker images locally"
	nodes, err := json.Marshal(edited)
	if err != nil {
		t.Fatal(err)
	}
	ui.send(t, "edit-tab-b", sessioncommand.KindEditDraft, revision, `{"nodes":`+string(nodes)+`}`)
	ui.send(t, "approve-tab-a", sessioncommand.KindApprove, revision, approveBody(result))
	ui.drain(t)
	if command := ui.command(t, "approve-tab-a"); command.Outcome != store.IdeationCommandConflict {
		t.Fatalf("approval from the stale tab = %q (%s), want a conflict", command.Outcome, command.Detail)
	}
	history := ui.history(t)
	if history.Session.PlanRevision != 2 || history.Session.ApprovalID != "" || ui.fake.Writes() != 0 {
		t.Fatalf("session = %#v, writes = %d; want plan revision 2, no approval, no tracker write", history.Session, ui.fake.Writes())
	}
	if response := ui.post(t, `{"request_id":"approve-late","session_id":"session-1","kind":"approve","expected_revision":`+
		strconv.FormatInt(revision, 10)+`,"payload":`+approveBody(result)+`}`, nil); response.Code != http.StatusConflict {
		t.Fatalf("late approval from the stale tab status = %d, want 409", response.Code)
	}
	page := ui.page(t)
	assertContains(t, page, "Cache worker images locally", "No preview of plan revision 2 yet")
	if strings.Contains(page, `data-session-command="approve"`) || strings.Contains(page, result.PayloadSHA256) {
		t.Fatal("page offers the preview of plan revision 1 for approval of plan revision 2")
	}

	current := history.Session.Revision
	ui.send(t, "preview-2", sessioncommand.KindPreviewPlan, current, `{"plan_revision":2}`)
	ui.drain(t)
	second := ui.previewOf(t, "preview-2")
	ui.fake.Types = map[string]string{plangraph.KindParent: "Epic", plangraph.KindTask: "Bug"}
	ui.send(t, "approve-drift", sessioncommand.KindApprove, current, approveBody(second))
	ui.drain(t)
	refused := ui.command(t, "approve-drift")
	if refused.Outcome != store.IdeationCommandRefused || !strings.Contains(refused.Detail, "preview the plan again") || ui.fake.Writes() != 0 {
		t.Fatalf("approval after a mapping change = %q (%s), writes = %d; want a refusal before any write", refused.Outcome, refused.Detail, ui.fake.Writes())
	}
	assertContains(t, ui.page(t), "rendered publication changed since the preview")
}

// stoppingPublisher stops the coordinator after the approval is recorded and
// before publication writes anything, as a process stop at that point does.
type stoppingPublisher struct {
	sessioncommand.Publisher
}

// Publish waits until the coordinator stops.
func (stoppingPublisher) Publish(ctx context.Context, _ publication.Request) (publication.Result, error) {
	<-ctx.Done()
	return publication.Result{}, ctx.Err()
}

// TestApprovalSurvivesACoordinatorRestart verifies that an approval recorded
// before a coordinator stop stays bound and is published once by the next
// coordinator, and that the page shows the approved content and the pending
// request meanwhile.
func TestApprovalSurvivesACoordinatorRestart(t *testing.T) {
	t.Parallel()

	ui, result := previewedPlanUI(t)
	ui.send(t, "approve-1", sessioncommand.KindApprove, ui.history(t).Session.Revision, approveBody(result))

	opened, err := store.Open(t.Context(), ui.storePath)
	if err != nil {
		t.Fatal(err)
	}
	env := ui.env(opened)
	env.Publisher = stoppingPublisher{Publisher: env.Publisher}
	stopped := sessioncommand.NewDispatcher(env)
	ctx, stop := context.WithCancel(t.Context())
	if err := stopped.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	for ui.history(t).Session.ApprovalID == "" {
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	stopped.Wait()
	_ = opened.Close()

	interrupted := ui.page(t)
	assertContains(t, interrupted, "Approved publication", "request <code>approve-1</code> is applying", result.PayloadSHA256)
	if strings.Contains(interrupted, `data-session-command="approve"`) || strings.Contains(interrupted, `data-session-command="publish"`) {
		t.Fatal("page offers a second approval or publication while the first is pending")
	}

	ui.drain(t)
	if command := ui.command(t, "approve-1"); command.Outcome != store.IdeationCommandApplied || command.Attempts != 2 {
		t.Fatalf("approve-1 = %#v, want applied on the second attempt", command)
	}
	history := ui.history(t)
	if history.Session.State != store.IdeationStatePublished || len(history.Approvals) != 1 || len(ui.fake.Items()) != 3 {
		t.Fatalf("state = %q, approvals = %d, items = %d; want one published approval", history.Session.State, len(history.Approvals), len(ui.fake.Items()))
	}
	assertContains(t, ui.page(t), "3 items created", "https://tracker.invalid/items/3")
}

// TestStoppedPublicationOffersRetryAndCancel verifies that a session left
// publishing without a pending command, as a coordinator that stopped
// before it recorded the attempt leaves it, is never a dead end: the page
// says that publication stopped and offers a retry and cancellation, and the
// retry publishes the same approval.
func TestStoppedPublicationOffersRetryAndCancel(t *testing.T) {
	t.Parallel()

	ui, _ := previewedPlanUI(t)
	opened, err := store.Open(t.Context(), ui.storePath)
	if err != nil {
		t.Fatal(err)
	}
	env := ui.env(opened)
	binding, err := env.Publisher.Preview(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	session := ui.history(t).Session
	write, err := env.Lifecycle.Write(session, workflow.SessionEventApproved, "", fixtureNow)
	if err == nil {
		session, _, err = opened.ApprovePlan(t.Context(), write, binding.Approval("approval-1"))
	}
	if err == nil {
		write, err = env.Lifecycle.Write(session, workflow.SessionEventPublicationStarted, "", fixtureNow)
	}
	if err == nil {
		session, err = opened.TransitionIdeationSession(t.Context(), write)
	}
	_ = opened.Close()
	if err != nil {
		t.Fatal(err)
	}

	page := ui.page(t)
	assertContains(t, page, "stopped without a recorded result", "Retry publication", `data-session-command="publish"`, `data-session-command="cancel"`)
	if strings.Contains(page, "The coordinator is publishing.") {
		t.Fatal("page says the coordinator is publishing although no command is pending")
	}
	ui.send(t, "publish-1", sessioncommand.KindPublish, session.Revision, `{}`)
	ui.drain(t)
	if state := ui.history(t).Session.State; state != store.IdeationStatePublished || len(ui.fake.Items()) != 3 {
		t.Fatalf("state = %q, items = %d; want the retry to publish three items", state, len(ui.fake.Items()))
	}
}

// TestPartialPublicationShowsLinksFailuresAndRecoveryActions verifies the
// progress of an incomplete publication: created links, pending operations,
// the operation with an unknown outcome, and the reconcile, retry, and
// cancel actions, which then complete the publication.
func TestPartialPublicationShowsLinksFailuresAndRecoveryActions(t *testing.T) {
	t.Parallel()

	ui, result := previewedPlanUI(t)
	ui.fake.Inject(trackertest.Fault{Op: trackertest.OpCreateItem, Key: "n-3", Err: errors.New("connection reset")})
	ui.send(t, "approve-1", sessioncommand.KindApprove, ui.history(t).Session.Revision, approveBody(result))
	ui.drain(t)
	history := ui.history(t)
	if history.Session.State != store.IdeationStatePublicationIncomplete {
		t.Fatalf("state = %q, want publication_incomplete", history.Session.State)
	}

	page := ui.page(t)
	assertContains(t, page,
		"2 items created · 0 relationships created · 3 pending · 0 failed · 1 unknown outcome",
		`<a href="https://tracker.invalid/items/1" rel="noreferrer noopener">1</a>`, `<a href="https://tracker.invalid/items/2" rel="noreferrer noopener">2</a>`,
		`data-operation="item/n-3" data-state="ambiguous"`, "connection reset", `data-operation="link/depends_on/n-3/n-2" data-state="pending"`,
		`data-receipt="approve-1"`, "incomplete",
		`data-reconcile="item/n-3"`, "<code>session-1/1/n-3</code>", `data-session-command="reconcile"`,
		"Retry publication", `data-session-command="publish"`,
		`data-session-command="cancel"`, "nothing is deleted",
		"does not start implementation",
	)

	revision := history.Session.Revision
	ui.send(t, "reconcile-1", sessioncommand.KindReconcile, revision, `{"operation_id":"item/n-3","external_id":""}`)
	ui.drain(t)
	ui.send(t, "publish-1", sessioncommand.KindPublish, revision, `{}`)
	ui.drain(t)
	if state := ui.history(t).Session.State; state != store.IdeationStatePublished || len(ui.fake.Items()) != 3 {
		t.Fatalf("state = %q, items = %d; want published with three items", state, len(ui.fake.Items()))
	}
	assertContains(t, ui.page(t), "3 items created · 3 relationships created · 0 pending")
}

// TestCancelledPublicationOffersReconciliationOnly verifies that a session
// cancelled after an ambiguous create offers the reconcile action for that
// create and no publish or retry, and that a reconciliation keeps the
// session cancelled and shows its receipt.
func TestCancelledPublicationOffersReconciliationOnly(t *testing.T) {
	t.Parallel()

	ui, result := previewedPlanUI(t)
	ui.fake.Inject(trackertest.Fault{Op: trackertest.OpCreateItem, Key: "n-3", Err: errors.New("connection reset")})
	ui.send(t, "approve-1", sessioncommand.KindApprove, ui.history(t).Session.Revision, approveBody(result))
	ui.drain(t)
	ui.send(t, "cancel-1", sessioncommand.KindCancel, ui.history(t).Session.Revision, `{"reason":"Stop."}`)
	ui.drain(t)
	if state := ui.history(t).Session.State; state != store.IdeationStateCancelled {
		t.Fatalf("state = %q, want cancelled", state)
	}

	page := ui.page(t)
	assertContains(t, page, `data-reconcile="item/n-3"`, `data-session-command="reconcile"`, "The session stays cancelled",
		"2 items created · 0 relationships created · 0 pending · 0 failed · 1 unknown outcome · 3 not run", "<td>not run</td>")
	for _, unwanted := range []string{`data-session-command="publish"`, `data-session-command="cancel"`} {
		if strings.Contains(page, unwanted) {
			t.Fatalf("cancelled page offers %s", unwanted)
		}
	}

	ui.send(t, "reconcile-1", sessioncommand.KindReconcile, ui.history(t).Session.Revision, `{"operation_id":"item/n-3","external_id":""}`)
	ui.drain(t)
	if command := ui.command(t, "reconcile-1"); command.Outcome != store.IdeationCommandApplied {
		t.Fatalf("reconcile-1 = %#v, want applied", command)
	}
	if state := ui.history(t).Session.State; state != store.IdeationStateCancelled {
		t.Fatalf("state = %q, want cancelled", state)
	}
	page = ui.page(t)
	assertContains(t, page, `data-receipt="reconcile-1"`, `data-operation="item/n-3" data-state="failed"`)
	if strings.Contains(page, `data-reconcile="item/n-3"`) {
		t.Fatal("a settled operation is still offered for reconciliation")
	}
}

// TestEditedAndSplitDraftIsSavedAndBrokenReferencesAreRefused verifies the
// draft edits the editor sends: a split with repaired references becomes
// the next plan revision, and a removal that leaves a dangling reference is
// refused with the problem on the page and no new revision.
func TestEditedAndSplitDraftIsSavedAndBrokenReferencesAreRefused(t *testing.T) {
	t.Parallel()

	ui := newPlanUI(t, planNodesForTest())
	revision := ui.history(t).Session.Revision
	removed := sessioncommand.PlanNodesFromStore(planNodesForTest())
	removed = append(removed[:1], removed[2:]...)
	encoded, err := json.Marshal(removed)
	if err != nil {
		t.Fatal(err)
	}
	ui.send(t, "remove-unrepaired", sessioncommand.KindEditDraft, revision, `{"nodes":`+string(encoded)+`}`)
	ui.drain(t)
	refused := ui.command(t, "remove-unrepaired")
	if refused.Outcome != store.IdeationCommandRefused || !strings.Contains(refused.Detail, "n-2") || ui.history(t).Session.PlanRevision != 1 {
		t.Fatalf("unrepaired removal = %q (%s), want a refusal that names n-2 and no new revision", refused.Outcome, refused.Detail)
	}
	assertContains(t, ui.page(t), `<li data-outcome="refused"><code>edit_draft</code> refused`)

	split := sessioncommand.PlanNodesFromStore(planNodesForTest())
	split[2].Title, split[2].AcceptanceCriteria = "Document the cache", []string{"The guide names the cache."}
	split = append(split, sessioncommand.PlanNodeJSON{
		ID: "n-3-2", Kind: plangraph.KindTask, Title: "Document cache cleanup", Body: "Explain how to clear the cache.",
		AcceptanceCriteria: []string{"The guide explains cache cleanup."}, ParentID: "n-1", DependsOn: []string{"n-2"},
	})
	if encoded, err = json.Marshal(split); err != nil {
		t.Fatal(err)
	}
	ui.send(t, "split-1", sessioncommand.KindEditDraft, revision, `{"nodes":`+string(encoded)+`}`)
	ui.drain(t)
	if command := ui.command(t, "split-1"); command.Outcome != store.IdeationCommandApplied {
		t.Fatalf("split = %q (%s), want applied", command.Outcome, command.Detail)
	}
	page := ui.page(t)
	assertContains(t, page, "plan revision 2", `data-node-id="n-3-2"`, "Document cache cleanup", "data-plan-valid")
	if draft := readDraft(t, page); draft.PlanRevision != 2 || len(draft.Nodes) != 4 {
		t.Fatalf("editor model = %#v, want the four-node revision 2", draft)
	}
}
