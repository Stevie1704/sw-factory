package webui_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/localissues"
	"github.com/Stevie1704/sw-factory/internal/publication"
	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/tracker/trackertest"
	"github.com/Stevie1704/sw-factory/internal/webui"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// localID is a stable content identity, independent of tracker and PR numbers.
const localID = "local-11111111111111111111111111111111"

// localViews supplies the retained coordinator views, never effects or authority.
type localViews struct {
	backlog   factory.LocalBacklogView
	execution factory.LocalExecutionOverview
	operator  factory.LocalRunOperatorView
}

// localPreviewTracker renders local-shaped exact content at the planning port.
type localPreviewTracker struct{ *trackertest.PlanTracker }

func (p localPreviewTracker) PreflightPlan(ctx context.Context, repo tracker.Repository, graph tracker.PlanGraph) (tracker.PlanPreview, error) {
	preview, err := p.PlanTracker.PreflightPlan(ctx, repo, graph)
	if err != nil {
		return preview, err
	}
	preview.Target = "local:" + repo.String()
	for i := range preview.Items {
		item := &preview.Items[i]
		id := fmt.Sprintf("local-%032x", i+1)
		item.Type = graph.Items[i].Kind
		item.Fields = map[string]string{"local_id": id, "path": "docs/issues/" + id + ".md", "graph_digest": "sha256:" + strings.Repeat("c", 64), "specification_branch": "factory/specifications/batch", "publication_policy": "local_commit", "target_branch": "main"}
		item.Body = "---\nschema_version: 1\nid: " + id + "\n---\n\nExact approved Markdown.\n"
	}
	return preview, nil
}

// TestLocalPlanSeparatesCommitAcceptanceFromExplicitSelectedExecution keeps
// publication-only approval distinct from task-only execution authorization.
func TestLocalPlanSeparatesCommitAcceptanceFromExplicitSelectedExecution(t *testing.T) {
	ui := newPlanUI(t, planNodesForTest())
	ui.tracker = localPreviewTracker{ui.fake}
	ui.send(t, "preview-local", sessioncommand.KindPreviewPlan, ui.history(t).Session.Revision, `{"plan_revision":1}`)
	ui.drain(t)
	page := ui.page(t)
	assertContains(t, page, "Accept and commit", "Approve and start", `data-session-command="approve_start"`, `name="selected_issue_ids"`, "waiting for specification merge", "Exact approved Markdown.", "factory/specifications/batch")
	if strings.Contains(page, "Mark items ready for the factory in the tracker") {
		t.Fatal("local page suggests remote labels grant authority")
	}
}

// LocalBacklog returns retained source/Run projections only.
func (v localViews) LocalBacklog(context.Context) (factory.LocalBacklogView, error) {
	return v.backlog, nil
}

// LocalExecution returns retained preview and authority evidence only.
func (v localViews) LocalExecution(context.Context) (factory.LocalExecutionOverview, error) {
	return v.execution, nil
}

// LocalRunOperator returns the supported exact displayed action bindings.
func (v localViews) LocalRunOperator(context.Context, string) (factory.LocalRunOperatorView, error) {
	return v.operator, nil
}

// localFixtureViews represents a stopped coordinator with exact source and a Run.
func localFixtureViews(t *testing.T) localViews {
	t.Helper()
	data, err := localissues.Render(localissues.Issue{SchemaVersion: 1, ID: localID, Title: "Export <script>alert(1)</script>", Kind: localissues.KindTask, Correlation: "session-1/1/n-1", Body: "## Problem\n\nOperators need exports.\n\n## Scope and non-goals\n\nJSON only.\n\n## Acceptance criteria\n\n- An export downloads.\n"})
	if err != nil {
		t.Fatal(err)
	}
	issue := store.LocalExecutionIssue{ID: localID, Number: 7, Path: "docs/issues/" + localID + ".md", Bytes: data, ContentDigest: localissues.Digest(data), Kind: localissues.KindTask}
	return localViews{backlog: factory.LocalBacklogView{Supervisor: factory.SupervisorView{ObservedAt: fixtureNow}, Projection: store.LocalBacklogProjection{Revision: baseSHA, GraphDigest: "sha256:" + strings.Repeat("a", 64), ObservedAt: fixtureNow, Items: []store.LocalBacklogItem{{Issue: issue, Title: "Export <script>alert(1)</script>", State: "executing", Reason: "the frozen Run is active", QueueState: "stale_authorization", QueueReason: "source changed; preview again", RunState: "executing", FrozenRevision: baseSHA, FrozenContentDigest: issue.ContentDigest, RunID: "run-1", SessionID: "session-1", AuthorizationID: "approve-start-1", PublicationBranch: "factory/specifications/batch", PublicationCommit: baseSHA, PublicationPolicy: "local_commit"}}}}, execution: factory.LocalExecutionOverview{Supervisor: factory.SupervisorView{ObservedAt: fixtureNow}}, operator: factory.LocalRunOperatorView{RunID: "run-1", Revision: 4, PacketVersion: 2, Questions: []factory.LocalRunQuestion{{ID: "q-1", Prompt: "Which format?", Digest: "sha256:" + strings.Repeat("b", 64)}}, Actions: []string{"answer_run", "cancel_run"}}}
}

// TestLocalBacklogAndIssueReadRetainedSourceWhileStopped shows genuine queue
// blockers independently from an active Run's frozen source, with escaped text.
func TestLocalBacklogAndIssueReadRetainedSourceWhileStopped(t *testing.T) {
	ui := newSessionUI(t, false)
	views := localFixtureViews(t)
	ui.handler = webui.NewHandler(factory.NewWithDependencies(ui.configPath, factory.Dependencies{Now: func() time.Time { return fixtureNow }}), webui.Options{Sessions: factory.NewWithDependencies(ui.configPath, factory.Dependencies{}), Local: views})
	for _, path := range []string{"/issues", "/issues/" + localID} {
		response := get(t, ui.handler, path)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body)
		}
		body := response.Body.String()
		assertContains(t, body, localID, "source changed; preview again", "/runs/run-1", "not live")
		if strings.Contains(body, "<script>alert(1)</script>") {
			t.Fatal("repository title rendered as HTML")
		}
		assertNoExternalResources(t, body)
	}
	page := get(t, ui.handler, "/issues/"+localID).Body.String()
	assertContains(t, page, "## Problem", "factory/specifications/batch", "approve-start-1", "Frozen Run", "stale_authorization")
}

// TestLocalRunFormsUseOnlyCoordinatorOfferedBindings preserves remote pages
// and binds local questions to exact Run, packet and question revisions.
func TestLocalRunFormsUseOnlyCoordinatorOfferedBindings(t *testing.T) {
	ui := newSessionUI(t, false)
	views := localFixtureViews(t)
	views.operator.IssueID = localID
	service := factory.NewWithDependencies(ui.configPath, factory.Dependencies{Now: func() time.Time { return fixtureNow }})
	detail, err := service.RunDetail(t.Context(), waitingRunID)
	if err != nil {
		t.Fatal(err)
	}
	views.operator.RunID = waitingRunID
	views.operator.Revision = detail.Run.Revision
	views.operator.PacketVersion = detail.Inspection.Policy.PacketVersion
	ui.handler = webui.NewHandler(service, webui.Options{Sessions: service, Local: views})
	page := get(t, ui.handler, "/runs/"+waitingRunID).Body.String()
	assertContains(t, page, `data-command-scope="run"`, `data-subject-id="`+waitingRunID+`"`, fmt.Sprintf(`data-expected-revision="%d"`, detail.Run.Revision), fmt.Sprintf(`name="packet_version" data-type="number" value="%d"`, detail.Inspection.Policy.PacketVersion), `name="question_digest" value="sha256:`, "Which format?", `data-session-command="cancel_run"`, `href="/issues/`+localID)
	for _, unsupported := range []string{`data-session-command="retry_run"`, `data-session-command="resume_run"`} {
		if strings.Contains(page, unsupported) {
			t.Fatal("UI invented unsupported recovery action")
		}
	}
	remote := get(t, webui.NewHandler(service, webui.Options{Sessions: service}), "/runs/"+waitingRunID).Body.String()
	if strings.Contains(remote, `data-command-scope="run"`) {
		t.Fatal("remote Run gained local mutable controls")
	}
	views.operator.Revision++
	raced := get(t, webui.NewHandler(service, webui.Options{Sessions: service, Local: views}), "/runs/"+waitingRunID).Body.String()
	assertContains(t, raced, "Run changed while this page was read")
	if strings.Contains(raced, `data-command-scope="run"`) {
		t.Fatal("incoherent Run revision exposed mutable controls")
	}
}

// TestInvestigationPreviewOffersCommitOnly never presents unsupported research
// as an executable task merely because the tracker can publish its content.
func TestInvestigationPreviewOffersCommitOnly(t *testing.T) {
	node := planNodesForTest()[1]
	node.ParentID = ""
	node.Kind = "investigation"
	node.BriefRefs = []string{"scope-1"}
	ui := newPlanUI(t, []store.PlanNode{node})
	ui.tracker = localPreviewTracker{ui.fake}
	ui.send(t, "preview-investigation", sessioncommand.KindPreviewPlan, ui.history(t).Session.Revision, `{"plan_revision":1}`)
	ui.drain(t)
	page := ui.page(t)
	assertContains(t, page, "Accept and commit", "investigations do not start implementation Runs")
	if strings.Contains(page, `data-session-command="approve_start"`) {
		t.Fatal("unsupported investigation offered executable approval")
	}
}

// TestFreshExecutionPreviewShowsExactBindingAndSuppressesStaleApproval renders
// retained preview bytes, selected IDs and sha-prefixed coordinator digests.
func TestFreshExecutionPreviewShowsExactBindingAndSuppressesStaleApproval(t *testing.T) {
	ui := newSessionUI(t, false)
	views := localFixtureViews(t)
	issue := views.backlog.Projection.Items[0].Issue
	views.execution.Preview = &store.LocalExecutionPreview{ID: "preview-1", Revision: 3, Binding: store.LocalExecutionBinding{SourceRevision: baseSHA, GraphDigest: views.backlog.Projection.GraphDigest, PublicationPolicy: "target_visible", OperatorName: "operator", PayloadSHA256: "sha256:" + strings.Repeat("d", 64), RelationshipMappingSHA256: "sha256:" + strings.Repeat("e", 64), Issues: []store.LocalExecutionIssue{issue}, SelectedIssueIDs: []string{localID}}}
	service := factory.NewWithDependencies(ui.configPath, factory.Dependencies{})
	page := get(t, webui.NewHandler(service, webui.Options{Sessions: service, Local: views}), "/issues/"+localID).Body.String()
	assertContains(t, page, `data-execution-preview="preview-1"`, `data-session-command="authorize_execution"`, `data-expected-revision="3"`, `name="selected_issue_ids" data-list value="`+localID+`" checked hidden`, `name="payload_sha256" value="sha256:`, "## Problem", "target_visible")
	views.backlog.Projection.GraphDigest = "sha256:" + strings.Repeat("f", 64)
	page = get(t, webui.NewHandler(service, webui.Options{Sessions: service, Local: views}), "/issues/"+localID).Body.String()
	assertContains(t, page, "changed or unavailable source after this preview")
	if strings.Contains(page, `data-session-command="authorize_execution"`) {
		t.Fatal("stale exact execution preview remained approvable")
	}
}

// TestScopedLocalSubmissionUsesProtectedInboxAndStoppedReplay uses the real
// admission/store seam: accepted once, unavailable when stopped, replay retained.
func TestScopedLocalSubmissionUsesProtectedInboxAndStoppedReplay(t *testing.T) {
	ui := newSessionUI(t, true)
	host, err := config.LoadHost(ui.configPath)
	if err != nil {
		t.Fatal(err)
	}
	host.Repositories[0].Tracker = config.ProviderLocal
	host.Repositories[0].CodeHost = config.ProviderGitHub
	host.Repositories[0].LocalOperator = config.LocalOperatorConfig{InstallationID: "install-11111111111111111111111111111111", Name: "operator"}
	if err := config.SaveHost(ui.configPath, host); err != nil {
		t.Fatal(err)
	}
	service := factory.NewWithDependencies(ui.configPath, factory.Dependencies{Now: func() time.Time { return fixtureNow }})
	ui.handler = webui.NewHandler(service, webui.Options{Sessions: service, Local: service})
	match := csrfMeta.FindStringSubmatch(get(t, ui.handler, "/issues").Body.String())
	if match == nil {
		t.Fatal("missing protected command token")
	}
	ui.token = match[1]
	body := `{"request_id":"local-preview-1","scope":"backlog","subject_id":"repository","kind":"preview_execution","expected_revision":0,"payload":{"selected_issue_ids":["` + localID + `"]}}`
	if response := ui.post(t, body, nil); response.Code != http.StatusAccepted {
		t.Fatalf("scoped inbox: %d %s", response.Code, response.Body)
	}
	if response := ui.post(t, strings.Replace(body, "local-preview-1", "foreign-1", 1), func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }); response.Code != http.StatusForbidden {
		t.Fatalf("foreign scoped mutation=%d", response.Code)
	}
	opened, err := store.Open(t.Context(), ui.storePath)
	if err != nil {
		t.Fatal(err)
	}
	heartbeat, err := opened.ReadSupervisorHeartbeat(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	heartbeat.RenewedAt = fixtureNow.Add(-2 * time.Minute)
	heartbeat.ExpiresAt = fixtureNow.Add(-time.Second)
	if err := opened.SaveSupervisorHeartbeat(t.Context(), *heartbeat); err != nil {
		t.Fatal(err)
	}
	opened.Close()
	if response := ui.post(t, strings.Replace(body, "local-preview-1", "local-preview-2", 1), nil); response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "unavailable") {
		t.Fatalf("stopped=%d %s", response.Code, response.Body)
	}
	if response := ui.post(t, body, nil); response.Code != http.StatusOK {
		t.Fatalf("stopped replay=%d %s", response.Code, response.Body)
	}
	if response := get(t, ui.handler, "/issues"); response.Code != http.StatusOK {
		t.Fatalf("stopped read=%d %s", response.Code, response.Body)
	}
}

// TestBatchProgressHasOwnCountAndReconciliation rejects the old item-only UI
// assumption while retaining the established publication recovery command.
func TestBatchProgressHasOwnCountAndReconciliation(t *testing.T) {
	ui := approvedPlanUI(t)
	opened, err := store.Open(t.Context(), ui.storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	session := ui.history(t).Session
	rows, err := opened.StartPublicationLedger(t.Context(), session.ID, session.ApprovalID, []store.PublicationOperation{{ID: "git/batch", Kind: store.PublicationOperationGitBatch}}, fixtureNow)
	if err != nil {
		t.Fatal(err)
	}
	detail, _ := json.Marshal(publication.BatchOperationDetail{Evidence: tracker.PlanBatchResult{CommitSHA: baseSHA, Branch: "factory/specifications/batch", Complete: true}})
	rows[0].State = store.PublicationOperationSucceeded
	rows[0].ExternalID = baseSHA
	rows[0].Detail = string(detail)
	if _, err := opened.RecordPublicationOperation(t.Context(), rows[0]); err != nil {
		t.Fatal(err)
	}
	assertContains(t, ui.page(t), "1 atomic batch committed · 0 items created · 0 relationships created", "factory/specifications/batch")
	other := approvedPlanUI(t)
	db, err := store.Open(t.Context(), other.storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	session = other.history(t).Session
	life := workflow.DefaultSessionLifecycle()
	for _, event := range []workflow.SessionEvent{workflow.SessionEventPublicationStarted, workflow.SessionEventPublicationIncomplete} {
		write, err := life.Write(session, event, "stopped", fixtureNow)
		if err != nil {
			t.Fatal(err)
		}
		session, err = db.TransitionIdeationSession(t.Context(), write)
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err = db.StartPublicationLedger(t.Context(), session.ID, session.ApprovalID, []store.PublicationOperation{{ID: "git/batch", Kind: store.PublicationOperationGitBatch}}, fixtureNow)
	if err != nil {
		t.Fatal(err)
	}
	rows[0].State = store.PublicationOperationAmbiguous
	if _, err := db.RecordPublicationOperation(t.Context(), rows[0]); err != nil {
		t.Fatal(err)
	}
	assertContains(t, other.page(t), `data-reconcile="git/batch"`, "Complete matching commit SHA", "Confirm pending effect absent after inspection")
}

// TestDraftRevisionShowsDeclinesAndExactChangedCriteria makes revision decisions
// reviewable without inferring anything from the planning role's prose.
func TestDraftRevisionShowsDeclinesAndExactChangedCriteria(t *testing.T) {
	ui := newPlanUI(t, planNodesForTest())
	nodes := []store.PlanNode{planNodesForTest()[1]}
	nodes[0].ParentID = ""
	nodes[0].BriefRefs = []string{"scope-1"}
	nodes[0].AcceptanceCriteria = []string{"Current acceptance criterion."}
	encoded, err := json.Marshal(sessioncommand.PlanNodesFromStore(nodes))
	if err != nil {
		t.Fatal(err)
	}
	ui.send(t, "decline-and-edit", sessioncommand.KindEditDraft, ui.history(t).Session.Revision, `{"nodes":`+string(encoded)+`}`)
	ui.drain(t)
	if command := ui.command(t, "decline-and-edit"); command.Outcome != store.IdeationCommandApplied {
		t.Fatalf("draft change=%s %s", command.Outcome, command.Detail)
	}
	assertContains(t, ui.page(t), "Declined n-3", "Acceptance criteria", "A second onboarding pulls no image.", "Current acceptance criterion.", "plan revision 2")
}
