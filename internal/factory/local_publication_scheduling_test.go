package factory_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"

	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/localissues"
	"github.com/Stevie1704/sw-factory/internal/localpublication"
	"github.com/Stevie1704/sw-factory/internal/publication"
	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// prepareSchedulingPlan reuses the ordinary Brief/plan and exact approval path;
// role inference is a fixture, while Git publication and SQLite are real.
func prepareSchedulingPlan(t *testing.T, f *scheduleFixture, start bool, open ...factory.StoreOpener) (string, string) {
	t.Helper()
	f.planner = localpublication.NewPublisher(f.registration, "main", f.host)
	if len(open) > 0 {
		f.recompose(open[0])
	} else {
		f.recompose(nil)
	}
	contextRevision := scheduleGit(t, f.registration.Path, "rev-parse", "main")
	f.withStore(func(db *store.Store) {
		session, err := db.IdeationSession(t.Context(), "session-1")
		if err != nil {
			t.Fatal(err)
		}
		write := func(event workflow.SessionEvent) store.IdeationWrite {
			w, err := workflow.DefaultSessionLifecycle().Write(session, event, "", sessionCommandNow)
			if err != nil {
				t.Fatal(err)
			}
			return w
		}
		session, brief, err := db.SaveBriefRevision(t.Context(), write(workflow.SessionEventEdited), store.Brief{Problem: "Add local output."})
		if err != nil {
			t.Fatal(err)
		}
		session, err = db.TransitionIdeationSession(t.Context(), write(workflow.SessionEventRoleStarted))
		if err != nil {
			t.Fatal(err)
		}
		session, _, err = db.SavePlanRevision(t.Context(), write(workflow.SessionEventPlanDrafted), store.PlanRevision{SourceBriefRevision: brief.Revision, ContextRevision: contextRevision, Nodes: []store.PlanNode{{ID: "new-output", Kind: "task", Title: "New output", Body: "Add output.", AcceptanceCriteria: []string{"Output is visible."}}}})
		if err != nil {
			t.Fatal(err)
		}
	})
	var binding publication.Binding
	var revision int64
	f.withStore(func(db *store.Store) {
		p := publication.Publisher{Store: db, Tracker: localpublication.Wrap(f.source, f.planner), Repository: tracker.Repository{Owner: "example", Name: "project"}, Lifecycle: workflow.DefaultSessionLifecycle()}
		if err := p.Prepare(t.Context(), "session-1"); err != nil {
			t.Fatal(err)
		}
		var err error
		binding, err = p.Preview(t.Context(), "session-1")
		if err != nil {
			t.Fatal(err)
		}
		session, err := db.IdeationSession(t.Context(), "session-1")
		if err != nil {
			t.Fatal(err)
		}
		revision = session.Revision
	})
	id := binding.Preview.Items[0].Fields["local_id"]
	payload := sessioncommand.ApproveStartPayload{ApprovePayload: sessioncommand.ApprovePayload{PlanRevision: binding.PlanRevision, PayloadSHA256: strings.TrimPrefix(localissues.Digest(binding.Payload), "sha256:"), RelationshipMappingSHA256: strings.TrimPrefix(localissues.Digest(binding.RelationshipMapping), "sha256:")}, SelectedIssueIDs: []string{id}}
	kind := sessioncommand.KindApproveStart
	var body any = payload
	if !start {
		kind = sessioncommand.KindApprove
		body = payload.ApprovePayload
	}
	data, _ := json.Marshal(map[string]any{"request_id": "publish-approval", "session_id": "session-1", "kind": kind, "expected_revision": revision, "payload": body})
	command, err := sessioncommand.ParseRequest(data)
	if err != nil {
		t.Fatal(err)
	}
	applied := submitExecutionCommand(t, f.sessionCommandFixture, command)
	if applied.Outcome != store.IdeationCommandApplied && len(open) == 0 {
		t.Fatalf("local plan approval=%#v", applied)
	}
	return id, binding.Preview.Items[0].Fields["specification_branch"]
}

// squashSchedulingSpecifications represents the human publishing path and uses
// a different commit SHA with byte-identical accepted files.
func squashSchedulingSpecifications(t *testing.T, f *scheduleFixture, branch string) string {
	t.Helper()
	scheduleGit(t, f.registration.Path, "merge", "--squash", branch)
	scheduleGit(t, f.registration.Path, "-c", "commit.gpgsign=false", "commit", "-m", "human specification squash")
	sha := scheduleGit(t, f.registration.Path, "rev-parse", "HEAD")
	scheduleGit(t, f.registration.Path, "push", "origin", "main")
	return sha
}

// TestLocalPublicationWaitsForExactTargetAndAcceptsHumanSquash covers complete
// publication/authority separation and the manual protected-target path.
func TestLocalPublicationWaitsForExactTargetAndAcceptsHumanSquash(t *testing.T) {
	baseline := schedulingTask("local-00112233445566778899aabbccddeeff", "Existing content")
	f := newScheduleFixture(t, baseline)
	id, branch := prepareSchedulingPlan(t, f, true)
	pass, err := f.service.PollOnce(t.Context())
	if err != nil || pass.Outcome != factory.PollNoWork {
		t.Fatalf("spec branch prematurely started implementation: %#v,%v", pass, err)
	}
	backlog, err := f.service.LocalBacklog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range backlog.Projection.Items {
		if item.Issue.ID == id {
			found = true
			if item.State != "waiting_for_specification_merge" || item.PublicationCommit == "" || item.SessionID != "session-1" {
				t.Fatalf("merge wait evidence=%#v", item)
			}
		}
	}
	if !found {
		t.Fatal("accepted issue absent from backlog")
	}
	published := scheduleGit(t, f.registration.Path, "rev-parse", branch)
	squash := squashSchedulingSpecifications(t, f, branch)
	if published == squash {
		t.Fatal("fixture did not create a different SHA")
	}
	pass, err = f.service.PollOnce(t.Context())
	if err != nil || pass.Outcome != factory.PollClaimed || pass.Run.BaseCheckpointSHA != squash {
		t.Fatalf("squash target claim=%#v,%v", pass, err)
	}
}

// TestPublicationOnlyItemsRemainVisibleAndNeverStart proves publication evidence
// is available for local issue pages even without any execution authority.
func TestPublicationOnlyItemsRemainVisibleAndNeverStart(t *testing.T) {
	baseline := schedulingTask("local-00112233445566778899aabbccddeeff", "Existing content")
	f := newScheduleFixture(t, baseline)
	id, branch := prepareSchedulingPlan(t, f, false)
	squashSchedulingSpecifications(t, f, branch)
	pass, err := f.service.PollOnce(t.Context())
	if err != nil || pass.Outcome != factory.PollNoWork {
		t.Fatalf("publication only ran: %#v,%v", pass, err)
	}
	view, err := f.service.LocalBacklog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range view.Projection.Items {
		if item.Issue.ID == id {
			if item.AuthorizationID != "" || item.State != "missing_execution_approval" || item.PublicationCommit == "" || item.SessionID == "" {
				t.Fatalf("publication-only evidence=%#v", item)
			}
			return
		}
	}
	t.Fatal("published issue is absent")
}

// cancelAtFinalPublicationBoundary cancels only when every durable batch/item
// operation succeeded, immediately before the publisher records its receipt.
type cancelAtFinalPublicationBoundary struct {
	*store.Store
	cancelled *bool
}

// IdeationSession injects that ordinary lifecycle cancellation at final read.
func (s cancelAtFinalPublicationBoundary) IdeationSession(ctx context.Context, id string) (store.IdeationSession, error) {
	session, err := s.Store.IdeationSession(ctx, id)
	if err != nil || *s.cancelled || session.State != store.IdeationStatePublishing {
		return session, err
	}
	ledger, err := s.Store.PublicationLedger(ctx, id, session.ApprovalID)
	if err != nil {
		return session, err
	}
	if len(ledger) == 0 {
		return session, nil
	}
	for _, op := range ledger {
		if op.State != store.PublicationOperationSucceeded {
			return session, nil
		}
	}
	*s.cancelled = true
	write, err := workflow.DefaultSessionLifecycle().Write(session, workflow.SessionEventCancelled, "cancelled at final receipt boundary", sessionCommandNow)
	if err != nil {
		return session, err
	}
	return s.Store.TransitionIdeationSession(ctx, write)
}

// TestCancelledPublicationCannotReleaseItsOriginalAuthorizationEvenWithAllEffects
// distinguishes a final incomplete receipt from complete item/commit ledger rows.
func TestCancelledPublicationCannotReleaseItsOriginalAuthorizationEvenWithAllEffects(t *testing.T) {
	baseline := schedulingTask("local-00112233445566778899aabbccddeeff", "Baseline")
	f := newScheduleFixture(t, baseline)
	cancelled := false
	id, branch := prepareSchedulingPlan(t, f, true, func(ctx context.Context, path string) (factory.OperationalStore, error) {
		db, err := store.Open(ctx, path)
		if err != nil {
			return nil, err
		}
		return cancelAtFinalPublicationBoundary{Store: db, cancelled: &cancelled}, nil
	})
	f.withStore(func(db *store.Store) {
		history, err := db.IdeationHistory(t.Context(), "session-1")
		if err != nil || history.Session.State != store.IdeationStateCancelled || len(history.Receipts) == 0 || history.Receipts[len(history.Receipts)-1].Outcome != store.PublicationOutcomeIncomplete {
			t.Fatalf("cancelled publication=%#v,%v", history, err)
		}
		for _, op := range history.Operations {
			if op.State != store.PublicationOperationSucceeded {
				t.Fatalf("fixture did not reach final boundary: %#v", op)
			}
		}
	})
	squashSchedulingSpecifications(t, f, branch)
	f.recompose(nil)
	pass, err := f.service.PollOnce(t.Context())
	if err != nil || pass.Outcome != factory.PollNoWork {
		t.Fatalf("cancelled receipt released old authority: %#v,%v", pass, err)
	}
	f.authorize(t, "new-target-decision", id)
	pass, err = f.service.PollOnce(t.Context())
	if err != nil || pass.Outcome != factory.PollClaimed {
		t.Fatalf("fresh independent target approval failed: %#v,%v", pass, err)
	}
	view, err := f.service.LocalBacklog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range view.Projection.Items {
		if item.Issue.ID == id && item.SessionID != "session-1" {
			t.Fatal("fresh target approval lost genuine publication provenance")
		}
	}
}

// TestDeletedBaselineIsStaleRatherThanAProtectedTargetMergeWait rejects a lost
// existing item even when every newly approved specification is still pending.
func TestDeletedBaselineIsStaleRatherThanAProtectedTargetMergeWait(t *testing.T) {
	baseline := schedulingTask("local-00112233445566778899aabbccddeeff", "Baseline")
	f := newScheduleFixture(t, baseline)
	id, _ := prepareSchedulingPlan(t, f, true)
	name, _ := localissues.DefaultPath(baseline.ID)
	scheduleGit(t, f.registration.Path, "rm", name)
	scheduleGit(t, f.registration.Path, "-c", "commit.gpgsign=false", "commit", "-m", "delete baseline")
	scheduleGit(t, f.registration.Path, "push", "origin", "main")
	pass, err := f.service.PollOnce(t.Context())
	if err != nil || pass.Outcome != factory.PollNoWork {
		t.Fatalf("deleted baseline admitted=%#v,%v", pass, err)
	}
	view, err := f.service.LocalBacklog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range view.Projection.Items {
		if item.Issue.ID == id {
			if item.State != "stale_authorization" {
				t.Fatalf("deleted baseline treated as merge wait: %#v", item)
			}
			return
		}
	}
	t.Fatal("pending approved issue disappeared")
}

// TestCompletedPublicationAllowsHistoricalSpecRefRemovalAfterExactTargetVisibility
// reflects normal source-branch deletion following a human specification merge.
func TestCompletedPublicationAllowsHistoricalSpecRefRemovalAfterExactTargetVisibility(t *testing.T) {
	baseline := schedulingTask("local-00112233445566778899aabbccddeeff", "Baseline")
	f := newScheduleFixture(t, baseline)
	_, branch := prepareSchedulingPlan(t, f, true)
	squashSchedulingSpecifications(t, f, branch)
	scheduleGit(t, f.registration.Path, "update-ref", "-d", "refs/heads/"+branch)
	pass, err := f.service.PollOnce(t.Context())
	if err != nil || pass.Outcome != factory.PollClaimed {
		t.Fatalf("final publication proof lost after historical ref cleanup: %#v,%v", pass, err)
	}
}

// batchInspectionFault fails publication inspection, as an unreadable
// repository or an unreachable code host would.
type batchInspectionFault struct{ *localpublication.Adapter }

// FindPlanBatch returns an inspection error instead of an observation.
func (batchInspectionFault) FindPlanBatch(context.Context, tracker.Repository, tracker.PlanPreview) (tracker.PlanBatchResult, bool, error) {
	return tracker.PlanBatchResult{}, false, errors.New("injected inspection failure")
}

// TestPublicationInspectionFailureKeepsTheItemOutOfAdmission separates a
// failed inspection from a removed historical ref: the item must wait.
func TestPublicationInspectionFailureKeepsTheItemOutOfAdmission(t *testing.T) {
	baseline := schedulingTask("local-00112233445566778899aabbccddeeff", "Baseline")
	f := newScheduleFixture(t, baseline)
	id, branch := prepareSchedulingPlan(t, f, true)
	squashSchedulingSpecifications(t, f, branch)
	f.service = factory.NewWithDependencies(f.configPath, factory.Dependencies{Tracker: batchInspectionFault{localpublication.Wrap(f.source, f.planner)}, Worktree: f.manager, GitWorkspace: f.manager, Worker: &agentWorker{}, HeadlessHarnesses: testHeadlessHarnesses(&agentHarness{}), PullRequests: f.host, CommitStatuses: f.host, PullRequestReviews: f.host, PullRequestComments: f.host, PullRequestCommentPublisher: f.host, CodeHostReadiness: f.host, LoadRepository: func(string) (config.RepositoryConfig, error) { return *f.policy, nil }, Now: func() time.Time { return sessionCommandNow }})
	pass, err := f.service.PollOnce(t.Context())
	if err != nil || pass.Outcome != factory.PollNoWork {
		t.Fatalf("uninspected publication admitted: %#v,%v", pass.Outcome, err)
	}
	view, err := f.service.LocalBacklog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range view.Projection.Items {
		if item.Issue.ID == id {
			if item.State != "publication_incomplete" || !strings.Contains(item.Reason, "could not be inspected") || strings.Contains(item.Reason, "removed") {
				t.Fatalf("inspection failure projection=%s: %s", item.State, item.Reason)
			}
			return
		}
	}
	t.Fatal("published item missing from the backlog")
}
