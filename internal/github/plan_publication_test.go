package github_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/github/githubtest"
	"github.com/Stevie1704/sw-factory/internal/hostcmd"
	"github.com/Stevie1704/sw-factory/internal/publication"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// publicationNow is the fixed clock of the end-to-end tests.
var publicationNow = time.Date(2026, time.October, 15, 10, 0, 0, 0, time.UTC)

// approvedSession stores a session with the given plan nodes, approves the
// GitHub preview, and returns the publisher and the store.
func approvedSession(t *testing.T, gh *githubtest.Scripted, nodes []store.PlanNode) (*publication.Publisher, *store.Store) {
	t.Helper()
	ctx := t.Context()
	opened, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	lifecycle := workflow.DefaultSessionLifecycle()
	write := func(session store.IdeationSession, event workflow.SessionEvent) store.IdeationWrite {
		t.Helper()
		resolved, err := lifecycle.Write(session, event, "", publicationNow)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}
	session, err := opened.CreateIdeationSession(ctx, store.IdeationSession{ID: "session-1", RepositoryPath: "/repos/project", State: lifecycle.InitialState(), CreatedAt: publicationNow})
	if err != nil {
		t.Fatal(err)
	}
	session, _, err = opened.AppendIdeationTurn(ctx, write(session, workflow.SessionEventTurnAccepted), store.IdeationTurn{ID: "turn-1", Author: store.IdeationTurnAuthorUser, Body: "PRIVATE-TURN-TEXT"})
	if err != nil {
		t.Fatal(err)
	}
	session, brief, err := opened.SaveBriefRevision(ctx, write(session, workflow.SessionEventEdited), store.Brief{
		Problem: "Onboarding is slow.", Scope: []store.BriefItem{{ID: "scope-1", Text: "The onboarding command."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if session, err = opened.TransitionIdeationSession(ctx, write(session, workflow.SessionEventRoleStarted)); err != nil {
		t.Fatal(err)
	}
	if session, _, err = opened.SavePlanRevision(ctx, write(session, workflow.SessionEventPlanDrafted), store.PlanRevision{
		SourceBriefRevision: brief.Revision, ContextRevision: "0123456789abcdef0123456789abcdef01234567", Nodes: nodes,
	}); err != nil {
		t.Fatal(err)
	}
	publisher := &publication.Publisher{
		Store: opened, Tracker: &github.GhClient{Runner: gh}, Repository: planRepository,
		Lifecycle: lifecycle, Now: func() time.Time { return publicationNow },
	}
	binding, err := publisher.Preview(ctx, "session-1")
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}
	if _, _, err := opened.ApprovePlan(ctx, write(session, workflow.SessionEventApproved), binding.Approval("approval-1")); err != nil {
		t.Fatalf("ApprovePlan() error = %v", err)
	}
	return publisher, opened
}

// decomposedNodes is a parent specification with two children, where the
// second child depends on the first.
func decomposedNodes() []store.PlanNode {
	return []store.PlanNode{
		{ID: "n-1", Kind: "spec", Title: "Faster onboarding", Body: "Parent.", AcceptanceCriteria: []string{"One command."}, BriefRefs: []string{"scope-1"}},
		{ID: "n-2", Kind: "task", Title: "Cache images", Body: "Child.", ParentID: "n-1"},
		{ID: "n-3", Kind: "task", Title: "Document setup", Body: "Docs.", ParentID: "n-1", DependsOn: []string{"n-2"}},
	}
}

// assertInertIssues verifies decision D6 and the raw-conversation rule on
// the scripted repository, and that publication started no run.
func assertInertIssues(t *testing.T, gh *githubtest.Scripted, opened *store.Store) {
	t.Helper()
	for _, issue := range gh.Issues() {
		if len(issue.Labels) != 0 || strings.Contains(issue.Body, "PRIVATE-TURN-TEXT") {
			t.Fatalf("issue %#v carries a label or conversation text", issue)
		}
	}
	if run, err := opened.CurrentRun(t.Context()); err != nil || run != nil {
		t.Fatalf("CurrentRun() = %#v, %v; publication must not start a run", run, err)
	}
}

// TestPublishCreatesASingleIssue verifies that a single-item plan publishes
// as one unlabeled issue and the receipt returns its URL.
func TestPublishCreatesASingleIssue(t *testing.T) {
	t.Parallel()

	gh := githubtest.New(t, true)
	publisher, opened := approvedSession(t, gh, []store.PlanNode{{ID: "n-1", Kind: "task", Title: "Cache images", Body: "Reuse images."}})

	result, err := publisher.Publish(t.Context(), publication.Request{SessionID: "session-1", AttemptID: "attempt-1"})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if result.Receipt.Outcome != store.PublicationOutcomePublished || len(result.Receipt.Items) != 1 || result.Receipt.Items[0].URL != githubtest.IssueURL(1) {
		t.Fatalf("receipt = %#v, want issue 1 published", result.Receipt)
	}
	if len(gh.Issues()) != 1 || !strings.Contains(gh.Issues()[0].Body, "<!-- factory-plan: session-1/1/n-1 -->") {
		t.Fatalf("issues = %#v, want one marked issue", gh.Issues())
	}
	assertInertIssues(t, gh, opened)
}

// TestPublishCreatesTheParentChildrenGraphWithNativeLinks verifies that a
// decomposed plan publishes the parent and its children, links them as
// sub-issues and blocking dependencies, and returns every URL.
func TestPublishCreatesTheParentChildrenGraphWithNativeLinks(t *testing.T) {
	t.Parallel()

	gh := githubtest.New(t, true)
	publisher, opened := approvedSession(t, gh, decomposedNodes())

	result, err := publisher.Publish(t.Context(), publication.Request{SessionID: "session-1", AttemptID: "attempt-1"})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if result.Receipt.Outcome != store.PublicationOutcomePublished || len(result.Receipt.Items) != 3 || len(result.Receipt.Relationships) != 3 {
		t.Fatalf("receipt = %#v, want three issues and three links", result.Receipt)
	}
	urls := map[string]string{}
	for _, item := range result.Receipt.Items {
		urls[item.NodeID] = item.URL
	}
	if urls["n-1"] != githubtest.IssueURL(1) || urls["n-2"] != githubtest.IssueURL(2) || urls["n-3"] != githubtest.IssueURL(3) {
		t.Fatalf("urls = %#v, want the parent first and both children", urls)
	}
	if fmt.Sprint(gh.SubIssues()[1]) != "[2 3]" || fmt.Sprint(gh.BlockedBy()[3]) != "[2]" || len(gh.Comments()) != 0 {
		t.Fatalf("sub-issues = %v, blocked by = %v, comments = %v; want native links only", gh.SubIssues(), gh.BlockedBy(), gh.Comments())
	}
	assertInertIssues(t, gh, opened)
}

// TestPublishCreatesTheApprovedTextualFallbackLinks verifies that a plan
// approved with the textual fallback publishes it: the bodies carry the
// relationship section and the marked backlink comments link the numbers.
func TestPublishCreatesTheApprovedTextualFallbackLinks(t *testing.T) {
	t.Parallel()

	gh := githubtest.New(t, false)
	publisher, opened := approvedSession(t, gh, decomposedNodes())

	result, err := publisher.Publish(t.Context(), publication.Request{SessionID: "session-1", AttemptID: "attempt-1"})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if result.Receipt.Outcome != store.PublicationOutcomePublished {
		t.Fatalf("receipt = %#v, want published", result.Receipt)
	}
	if !strings.Contains(gh.Issues()[0].Body, "- [ ] Sub-issue: Cache images (`n-2`)") {
		t.Fatalf("parent body = %q, want the approved textual task list", gh.Issues()[0].Body)
	}
	if len(gh.Comments()[2]) != 1 || !strings.HasPrefix(gh.Comments()[2][0], "Part of #1") ||
		len(gh.Comments()[3]) != 2 || !strings.HasPrefix(gh.Comments()[3][1], "Depends on #2") {
		t.Fatalf("comments = %#v, want the backlinks on the sub-issues and the dependent issue", gh.Comments())
	}
	if len(gh.SubIssues()) != 0 || len(gh.BlockedBy()) != 0 {
		t.Fatal("textual fallback created native links after approval")
	}
	assertInertIssues(t, gh, opened)
}

// TestPublishRetriesAfterAnUncertainCreateWithoutADuplicate verifies that a
// create whose response is lost leaves the publication incomplete and that
// the retry adopts the issue by its marker instead of creating it again.
func TestPublishRetriesAfterAnUncertainCreateWithoutADuplicate(t *testing.T) {
	t.Parallel()

	gh := githubtest.New(t, true)
	publisher, opened := approvedSession(t, gh, decomposedNodes())
	gh.FailOnce("create:Cache images", &hostcmd.TimeoutError{Operation: "gh api", Timeout: github.CommandTimeout}, true)

	first, err := publisher.Publish(t.Context(), publication.Request{SessionID: "session-1", AttemptID: "attempt-1"})
	if err != nil {
		t.Fatalf("first Publish() error = %v", err)
	}
	if first.Receipt.Outcome != store.PublicationOutcomeIncomplete || len(gh.Issues()) != 2 {
		t.Fatalf("first receipt = %#v with %d issues, want incomplete after the lost response", first.Receipt, len(gh.Issues()))
	}
	second, err := publisher.Publish(t.Context(), publication.Request{SessionID: "session-1", AttemptID: "attempt-2"})
	if err != nil {
		t.Fatalf("second Publish() error = %v", err)
	}
	if second.Receipt.Outcome != store.PublicationOutcomePublished || len(gh.Issues()) != 3 || gh.Creates() != 3 {
		t.Fatalf("second receipt = %#v, issues = %d, creates = %d; want three issues from three creates", second.Receipt, len(gh.Issues()), gh.Creates())
	}
	assertInertIssues(t, gh, opened)
}

// TestPublishResumesAfterPartialCreationAndLinkFailures verifies that a
// rejected create stops publication with a proven no-change failure, that a
// link with an unknown outcome is verified before the retry, and that the
// retry completes the graph without a duplicate issue or link.
func TestPublishResumesAfterPartialCreationAndLinkFailures(t *testing.T) {
	t.Parallel()

	gh := githubtest.New(t, true)
	publisher, opened := approvedSession(t, gh, decomposedNodes())
	gh.FailOnce("create:Document setup", fmt.Errorf("gh api repos/example/project/issues --method POST: HTTP 403: Resource not accessible"), false)

	first, err := publisher.Publish(t.Context(), publication.Request{SessionID: "session-1", AttemptID: "attempt-1"})
	if err != nil {
		t.Fatalf("first Publish() error = %v", err)
	}
	if first.Receipt.Outcome != store.PublicationOutcomeIncomplete || len(gh.Issues()) != 2 || !strings.Contains(first.Receipt.Detail, "made no change") {
		t.Fatalf("first receipt = %#v, want incomplete after a rejected create", first.Receipt)
	}

	gh.FailOnce("sub_issue:1", fmt.Errorf("gh api repos/example/project/issues/1/sub_issues --method POST: HTTP 502: Bad Gateway"), true)
	second, err := publisher.Publish(t.Context(), publication.Request{SessionID: "session-1", AttemptID: "attempt-2"})
	if err != nil {
		t.Fatalf("second Publish() error = %v", err)
	}
	if second.Receipt.Outcome != store.PublicationOutcomeIncomplete || !strings.Contains(second.Receipt.Detail, "unknown") {
		t.Fatalf("second receipt = %#v, want incomplete after an unknown link outcome", second.Receipt)
	}

	third, err := publisher.Publish(t.Context(), publication.Request{SessionID: "session-1", AttemptID: "attempt-3"})
	if err != nil {
		t.Fatalf("third Publish() error = %v", err)
	}
	if third.Receipt.Outcome != store.PublicationOutcomePublished || len(gh.Issues()) != 3 {
		t.Fatalf("third receipt = %#v, want published with three issues", third.Receipt)
	}
	if fmt.Sprint(gh.SubIssues()[1]) != "[2 3]" || fmt.Sprint(gh.BlockedBy()[3]) != "[2]" {
		t.Fatalf("sub-issues = %v, blocked by = %v; want every link once", gh.SubIssues(), gh.BlockedBy())
	}
	assertInertIssues(t, gh, opened)
}
