package factory_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/plangraph"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// sessionServiceForTest composes a coordinator service over the fixture's
// repository, store, worker, and snapshots, as the production composition
// root does.
func sessionServiceForTest(fixture *sessionFixture) *factory.Service {
	host := config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{{
		Path: fixture.repo, OperationalDataPath: fixture.path, RepositoryConfigPath: filepath.Join(fixture.repo, "factory.yaml"),
	}}}
	ids := 0
	return factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config: &fakeConfig{value: host},
		OpenStore: func(ctx context.Context, path string) (factory.OperationalStore, error) {
			return store.Open(ctx, path)
		},
		LoadRepository:    func(string) (config.RepositoryConfig, error) { return fixture.policy, nil },
		Worker:            fixture.worker,
		HeadlessHarnesses: harness.NewHeadlessAdapters(fixture.worker),
		Worktree:          snapshotWorktreeForTest{fixture.snapshots},
		Now:               func() time.Time { return time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC) },
		NewRunID: func() (string, error) {
			ids++
			return "plan-" + string(rune('0'+ids)), nil
		},
	})
}

// briefForTest saves a brief revision with the scope items "json" and
// "format-unknown" while the session allows an edit.
func (f *sessionFixture) briefForTest(problem string) {
	f.t.Helper()
	write, err := workflow.DefaultSessionLifecycle().Write(f.session(), workflow.SessionEventEdited, "", time.Time{})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, _, err := f.store.SaveBriefRevision(f.t.Context(), write, store.Brief{
		Problem:             problem,
		Scope:               []store.BriefItem{{ID: "json", Text: "JSON export of run history"}},
		UnresolvedQuestions: []store.BriefItem{{ID: "format-unknown", Text: "Which consumers read the export?"}},
	}); err != nil {
		f.t.Fatal(err)
	}
}

// planNodeForTest returns a complete report task node with id.
func planNodeForTest(id string) report.PlanNode {
	return report.PlanNode{
		ID: id, Kind: "task", Title: "Item " + id, Body: "Problem, outcome, and context of " + id + ".",
		AcceptanceCriteria: []string{id + " is observable"}, BriefRefs: []string{"json"},
	}
}

// planReportForTest is a completed planning report with nodes.
func planReportForTest(nodes ...report.PlanNode) report.Report {
	return report.Report{Outcome: report.OutcomeCompleted, PlanDraft: &report.PlanDraft{Nodes: nodes}}
}

// TestServiceRefusesAnInvalidPlanGraphWithActionableReason verifies that the
// composed coordinator validates a planning draft with the plan graph rules:
// a dependency cycle is refused as plan_rejected, no plan revision is saved,
// and the session reason names the cycle.
func TestServiceRefusesAnInvalidPlanGraphWithActionableReason(t *testing.T) {
	fixture := newSessionFixture(t, config.HarnessCodex)
	fixture.briefForTest("Operators cannot export run history.")
	service := sessionServiceForTest(fixture)
	var planning store.SessionInvocation
	if err := service.WithSessionRoles(t.Context(), func(roles *factory.SessionRoles) error {
		var err error
		planning, err = roles.StartPlanning(t.Context(), "session-1", "handoff-1")
		return err
	}); err != nil {
		t.Fatalf("WithSessionRoles(start planning) error = %v", err)
	}
	first, second := planNodeForTest("writer"), planNodeForTest("command")
	first.DependsOn, second.DependsOn = []string{"command"}, []string{"writer"}
	fixture.report(planning, planReportForTest(first, second))
	if err := service.ReconcileSessionRoles(t.Context()); err != nil {
		t.Fatalf("ReconcileSessionRoles() error = %v", err)
	}
	got, session := fixture.invocation(planning.ID), fixture.session()
	if got.Status != store.SessionInvocationRejected || got.FailureKind != factory.SessionFailurePlanRejected ||
		session.PlanRevision != 0 || session.State != store.IdeationStateAwaitingUser ||
		!strings.Contains(session.StateReason, "dependency cycle writer -> command -> writer") {
		t.Fatalf("invocation = %#v, session = %#v; want a refused draft with the cycle in the reason", got, session)
	}
}

// transitionForTest applies one lifecycle event with no content change.
func (f *sessionFixture) transitionForTest(event workflow.SessionEvent) {
	f.t.Helper()
	write, err := workflow.DefaultSessionLifecycle().Write(f.session(), event, "", time.Time{})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.store.TransitionIdeationSession(f.t.Context(), write); err != nil {
		f.t.Fatal(err)
	}
}

// planForTest runs one planning invocation to an accepted report and returns
// the invocation.
func (f *sessionFixture) planForTest(requestID string, nodes ...report.PlanNode) store.SessionInvocation {
	f.t.Helper()
	planning, err := f.roles().StartPlanning(f.t.Context(), "session-1", requestID)
	if err != nil {
		f.t.Fatalf("StartPlanning(%s) error = %v", requestID, err)
	}
	f.report(planning, planReportForTest(nodes...))
	if err := f.roles().Reconcile(f.t.Context()); err != nil {
		f.t.Fatalf("Reconcile(%s) error = %v", requestID, err)
	}
	return planning
}

// TestPlanningAcceptsValidDraftShapes verifies that a single fix, a
// multi-item feature with dependencies, and an investigation-first draft are
// accepted as one plan revision each, pinned to the exact brief and context
// revision, with stable local IDs and typed relationships.
func TestPlanningAcceptsValidDraftShapes(t *testing.T) {
	parent := planNodeForTest("export")
	parent.Kind = "parent"
	writer, command := planNodeForTest("writer"), planNodeForTest("command")
	writer.ParentID, command.ParentID, command.DependsOn = "export", "export", []string{"writer"}
	spike := planNodeForTest("consumers")
	spike.Kind, spike.BriefRefs = "investigation", []string{"format-unknown"}
	afterSpike := planNodeForTest("schema")
	afterSpike.DependsOn = []string{"consumers"}
	tests := []struct {
		name  string
		nodes []report.PlanNode
	}{
		{"single fix", []report.PlanNode{planNodeForTest("fix")}},
		{"feature with dependencies", []report.PlanNode{parent, writer, command}},
		{"investigation first", []report.PlanNode{spike, afterSpike}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSessionFixture(t, config.HarnessClaude)
			fixture.validator = plangraph.Validate
			fixture.briefForTest("Operators cannot export run history.")
			planning := fixture.planForTest("handoff-1", test.nodes...)
			history, err := fixture.store.IdeationHistory(t.Context(), "session-1")
			if err != nil {
				t.Fatal(err)
			}
			if got := fixture.invocation(planning.ID); got.Status != store.SessionInvocationAccepted {
				t.Fatalf("planning invocation = %#v, want accepted", got)
			}
			if history.Session.State != store.IdeationStateDraftReview || len(history.Plans) != 1 {
				t.Fatalf("history = %#v, want one draft under review", history)
			}
			plan := history.Plans[0]
			if plan.SourceBriefRevision != 1 || plan.ContextRevision != sessionContextRevisionForTest || plan.Stale || len(plan.Nodes) != len(test.nodes) {
				t.Fatalf("plan = %#v, want brief revision 1 and context %s with every node", plan, sessionContextRevisionForTest)
			}
			for position, node := range test.nodes {
				stored := plan.Nodes[position]
				if stored.ID != node.ID || stored.Kind != node.Kind || stored.ParentID != node.ParentID || strings.Join(stored.DependsOn, ",") != strings.Join(node.DependsOn, ",") {
					t.Fatalf("stored node %d = %#v, want %#v", position, stored, node)
				}
			}
		})
	}
}

// TestPlanningDraftBecomesStaleWhenTheBriefChanges verifies that a later
// brief revision marks an accepted draft stale without changing it, and that
// the next draft records the new brief revision.
func TestPlanningDraftBecomesStaleWhenTheBriefChanges(t *testing.T) {
	fixture := newSessionFixture(t, config.HarnessCodex)
	fixture.validator = plangraph.Validate
	fixture.briefForTest("Operators cannot export run history.")
	fixture.planForTest("handoff-1", planNodeForTest("fix"))
	fixture.briefForTest("Operators and auditors cannot export run history.")
	history, err := fixture.store.IdeationHistory(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Plans) != 1 || !history.Plans[0].Stale || history.Plans[0].SourceBriefRevision != 1 {
		t.Fatalf("plans after brief change = %#v, want the first draft stale at brief revision 1", history.Plans)
	}
	fixture.planForTest("regenerate-1", planNodeForTest("fix"))
	history, err = fixture.store.IdeationHistory(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Plans) != 2 || history.Plans[1].SourceBriefRevision != 2 || history.Plans[1].Stale {
		t.Fatalf("plans after regeneration = %#v, want a fresh draft at brief revision 2", history.Plans)
	}
}

// TestPlanningRegenerationKeepsApprovedAndPublishingRevisions verifies that
// planning cannot start while a revision is approved or publishing, and that
// regeneration after an edit creates a new revision while the approved
// revision and its approval stay unchanged.
func TestPlanningRegenerationKeepsApprovedAndPublishingRevisions(t *testing.T) {
	fixture := newSessionFixture(t, config.HarnessCodex)
	fixture.validator = plangraph.Validate
	fixture.briefForTest("Operators cannot export run history.")
	fixture.planForTest("handoff-1", planNodeForTest("fix"))
	approve := func(id string, revision int64) {
		t.Helper()
		write, err := workflow.DefaultSessionLifecycle().Write(fixture.session(), workflow.SessionEventApproved, "", time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := fixture.store.ApprovePlan(t.Context(), write, store.PlanApproval{
			ID: id, PlanRevision: revision, Target: "fake:repo", Payload: []byte(`{"items":1}`), RelationshipMapping: []byte(`[]`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	approve("approve-1", 1)
	if _, err := fixture.roles().StartPlanning(t.Context(), "session-1", "regenerate-approved"); !errors.Is(err, factory.ErrSessionRoleNotStartable) {
		t.Fatalf("StartPlanning(approved) error = %v, want ErrSessionRoleNotStartable", err)
	}

	fixture.transitionForTest(workflow.SessionEventEdited)
	regenerated := planNodeForTest("fix")
	regenerated.Title = "Regenerated fix"
	fixture.planForTest("regenerate-1", regenerated)
	history, err := fixture.store.IdeationHistory(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Plans) != 2 || history.Plans[0].Nodes[0].Title != "Item fix" || history.Plans[1].Nodes[0].Title != "Regenerated fix" ||
		len(history.Approvals) != 1 || history.Approvals[0].PlanRevision != 1 || history.Session.ApprovalID != "" || history.Session.PlanRevision != 2 {
		t.Fatalf("history after regeneration = %#v, want revision 2 beside the unchanged approved revision 1", history)
	}

	approve("approve-2", 2)
	fixture.transitionForTest(workflow.SessionEventPublicationStarted)
	if _, err := fixture.roles().StartPlanning(t.Context(), "session-1", "regenerate-publishing"); !errors.Is(err, factory.ErrSessionRoleNotStartable) {
		t.Fatalf("StartPlanning(publishing) error = %v, want ErrSessionRoleNotStartable", err)
	}
	if session := fixture.session(); session.PlanRevision != 2 || session.ApprovalID != "approve-2" || session.State != store.IdeationStatePublishing {
		t.Fatalf("session = %#v, want the publishing revision unchanged", session)
	}
}

// TestPlanningRejectsLateResultsFromStaleInvocations verifies that a draft
// from a planning invocation that was cancelled and replaced, or whose
// session moved on while it ran, never becomes a plan revision, while the
// current invocation's draft is accepted against the current brief.
func TestPlanningRejectsLateResultsFromStaleInvocations(t *testing.T) {
	t.Run("cancelled and replaced", func(t *testing.T) {
		fixture := newSessionFixture(t, config.HarnessCodex)
		fixture.validator = plangraph.Validate
		fixture.briefForTest("Operators cannot export run history.")
		stale, err := fixture.roles().StartPlanning(t.Context(), "session-1", "handoff-1")
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.roles().CancelRole(t.Context(), "session-1", "brief changed"); err != nil {
			t.Fatal(err)
		}
		fixture.briefForTest("Operators and auditors cannot export run history.")
		current, err := fixture.roles().StartPlanning(t.Context(), "session-1", "handoff-2")
		if err != nil {
			t.Fatal(err)
		}
		fixture.report(stale, planReportForTest(planNodeForTest("stale")))
		fixture.report(current, planReportForTest(planNodeForTest("current")))
		if err := fixture.roles().Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		history, err := fixture.store.IdeationHistory(t.Context(), "session-1")
		if err != nil {
			t.Fatal(err)
		}
		if got := fixture.invocation(stale.ID); got.Status != store.SessionInvocationCancelled {
			t.Fatalf("stale invocation = %#v, want it to stay cancelled", got)
		}
		if len(history.Plans) != 1 || history.Plans[0].Nodes[0].ID != "current" || history.Plans[0].SourceBriefRevision != 2 {
			t.Fatalf("plans = %#v, want only the current draft at brief revision 2", history.Plans)
		}
		write, err := workflow.DefaultSessionLifecycle().Write(history.Session, workflow.SessionEventPlanDrafted, "", time.Time{})
		if err == nil {
			_, _, err = fixture.store.AcceptSessionRoleResult(t.Context(), write, store.SessionRoleResult{InvocationID: stale.ID})
		}
		if err == nil {
			t.Fatal("AcceptSessionRoleResult(stale invocation) error = nil, want a refusal")
		}
	})
	t.Run("session moved while running", func(t *testing.T) {
		fixture := newSessionFixture(t, config.HarnessClaude)
		fixture.validator = plangraph.Validate
		fixture.briefForTest("Operators cannot export run history.")
		stale, err := fixture.roles().StartPlanning(t.Context(), "session-1", "handoff-1")
		if err != nil {
			t.Fatal(err)
		}
		fixture.transitionForTest(workflow.SessionEventCancelled)
		fixture.report(stale, planReportForTest(planNodeForTest("stale")))
		if err := fixture.roles().Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		got, session := fixture.invocation(stale.ID), fixture.session()
		if got.Status != store.SessionInvocationCancelled || got.FailureKind != factory.SessionFailureSessionMoved || session.PlanRevision != 0 {
			t.Fatalf("stale invocation = %#v, session = %#v; want session_moved and no plan revision", got, session)
		}
	})
}
