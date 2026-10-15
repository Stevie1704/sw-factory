package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/hostcmd"
	"github.com/Stevie1704/sw-factory/internal/publication"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// scriptedGitHub is a stateful scripted gh for end-to-end publication tests.
// It answers the gh api calls of the planning adapter from an in-memory
// repository and fails a selected write once, optionally after applying it,
// as a lost response does. An unexpected call fails the test.
type scriptedGitHub struct {
	t         *testing.T
	mu        sync.Mutex
	native    bool
	issues    []fakeIssue
	subIssues map[int][]int
	blockedBy map[int][]int
	comments  map[int][]string
	faults    map[string]fakeFault
	writes    []string
}

// fakeIssue is one issue of the scripted repository.
type fakeIssue struct {
	Number int
	Title  string
	Body   string
	Labels []string
}

// fakeFault fails the next write with a key once.
type fakeFault struct {
	err        error
	afterWrite bool
}

// newScriptedGitHub returns an empty repository; native selects whether the
// host offers sub-issues and blocking dependencies.
func newScriptedGitHub(t *testing.T, native bool) *scriptedGitHub {
	return &scriptedGitHub{
		t: t, native: native, subIssues: map[int][]int{}, blockedBy: map[int][]int{},
		comments: map[int][]string{}, faults: map[string]fakeFault{},
	}
}

// failOnce fails the next write with key: "create:<title>",
// "sub_issue:<parent>", "blocked_by:<dependent>", or "comment:<issue>".
func (g *scriptedGitHub) failOnce(key string, err error, afterWrite bool) {
	g.faults[key] = fakeFault{err: err, afterWrite: afterWrite}
}

// creates returns the number of issue create requests.
func (g *scriptedGitHub) creates() int {
	count := 0
	for _, write := range g.writes {
		if strings.HasPrefix(write, "create:") {
			count++
		}
	}
	return count
}

// Run answers one gh invocation.
func (g *scriptedGitHub) Run(_ context.Context, args []string, input []byte) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(args) < 2 || args[0] != "api" {
		g.t.Fatalf("unexpected gh call %#v", args)
	}
	method := "GET"
	if index := slices.Index(args, "--method"); index >= 0 {
		method = args[index+1]
	}
	path := strings.TrimPrefix(args[1], "repos/example/project")
	switch {
	case args[1] == "user":
		return []byte(`{"login":"factory-bot"}`), nil
	case args[1] == "graphql":
		if g.native {
			return []byte(nativeCapabilities), nil
		}
		return []byte(legacyCapabilities), nil
	case path == "":
		return []byte(writableRepository), nil
	case path == "/issues" && method == "GET":
		return g.listIssues(args)
	case path == "/issues" && method == "POST":
		return g.write(input, g.createIssue)
	}
	parts := strings.Split(strings.TrimPrefix(path, "/issues/"), "/")
	number, err := strconv.Atoi(parts[0])
	if err != nil || number < 1 || number > len(g.issues) {
		g.t.Fatalf("unexpected gh path %q", args[1])
	}
	switch rest := strings.Join(parts[1:], "/"); {
	case rest == "" && method == "GET":
		return json.Marshal(map[string]int{"id": 5000 + number, "number": number})
	case rest == "sub_issues" && method == "GET":
		return issueNumbers(g.subIssues[number]), nil
	case rest == "dependencies/blocked_by" && method == "GET":
		return issueNumbers(g.blockedBy[number]), nil
	case rest == "comments" && method == "GET":
		return g.listComments(number), nil
	case rest == "sub_issues" && method == "POST":
		return g.write(input, func(body map[string]any) (string, []byte) {
			g.subIssues[number] = append(g.subIssues[number], int(body["sub_issue_id"].(float64))-5000)
			return "sub_issue:" + strconv.Itoa(number), []byte(`{}`)
		})
	case rest == "dependencies/blocked_by" && method == "POST":
		return g.write(input, func(body map[string]any) (string, []byte) {
			g.blockedBy[number] = append(g.blockedBy[number], int(body["issue_id"].(float64))-5000)
			return "blocked_by:" + strconv.Itoa(number), []byte(`{}`)
		})
	case rest == "comments" && method == "POST":
		return g.write(input, func(body map[string]any) (string, []byte) {
			g.comments[number] = append(g.comments[number], body["body"].(string))
			return "comment:" + strconv.Itoa(number), []byte(`{"id":1}`)
		})
	}
	g.t.Fatalf("unexpected gh call %#v", args)
	return nil, nil
}

// write decodes one request, applies it, and fails it when a fault matches
// its key. A fault without afterWrite fails before the write is applied.
func (g *scriptedGitHub) write(input []byte, apply func(map[string]any) (string, []byte)) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(input, &body); err != nil {
		g.t.Fatalf("decode request body: %v", err)
	}
	snapshot := g.snapshot()
	key, output := apply(body)
	g.writes = append(g.writes, key)
	if fault, ok := g.faults[key]; ok {
		delete(g.faults, key)
		if !fault.afterWrite {
			g.restore(snapshot)
		}
		return nil, fault.err
	}
	return output, nil
}

// createIssue adds one issue from a create request.
func (g *scriptedGitHub) createIssue(body map[string]any) (string, []byte) {
	if _, ok := body["labels"]; ok {
		g.t.Fatalf("create request %#v carries labels", body)
	}
	issue := fakeIssue{Number: len(g.issues) + 1, Title: body["title"].(string), Body: body["body"].(string)}
	g.issues = append(g.issues, issue)
	output, _ := json.Marshal(map[string]any{
		"id": 5000 + issue.Number, "number": issue.Number, "html_url": issueURL(issue.Number),
	})
	return "create:" + issue.Title, output
}

// listIssues answers the account's issue listing, newest first.
func (g *scriptedGitHub) listIssues(args []string) ([]byte, error) {
	if !slices.Contains(args, "creator=factory-bot") || !slices.Contains(args, "page=1") {
		return []byte(`[]`), nil
	}
	listed := []map[string]any{}
	for index := len(g.issues) - 1; index >= 0; index-- {
		issue := g.issues[index]
		listed = append(listed, map[string]any{
			"number": issue.Number, "html_url": issueURL(issue.Number), "body": issue.Body,
			"user": map[string]string{"login": "factory-bot"},
		})
	}
	return json.Marshal(listed)
}

// listComments answers one issue's comment listing.
func (g *scriptedGitHub) listComments(number int) []byte {
	comments := []map[string]any{}
	for index, body := range g.comments[number] {
		comments = append(comments, map[string]any{"id": index + 1, "body": body, "user": map[string]string{"login": "factory-bot"}})
	}
	output, _ := json.Marshal([][]map[string]any{comments})
	return output
}

// fakeState is a copy of the scripted repository state.
type fakeState struct {
	issues    []fakeIssue
	subIssues map[int][]int
	blockedBy map[int][]int
	comments  map[int][]string
}

// snapshot copies the repository state.
func (g *scriptedGitHub) snapshot() fakeState {
	clone := func(source map[int][]int) map[int][]int {
		copied := map[int][]int{}
		for key, value := range source {
			copied[key] = slices.Clone(value)
		}
		return copied
	}
	comments := map[int][]string{}
	for key, value := range g.comments {
		comments[key] = slices.Clone(value)
	}
	return fakeState{issues: slices.Clone(g.issues), subIssues: clone(g.subIssues), blockedBy: clone(g.blockedBy), comments: comments}
}

// restore resets the repository state.
func (g *scriptedGitHub) restore(state fakeState) {
	g.issues, g.subIssues, g.blockedBy, g.comments = state.issues, state.subIssues, state.blockedBy, state.comments
}

// issueNumbers renders a slurped issue listing of numbers.
func issueNumbers(numbers []int) []byte {
	issues := []map[string]int{}
	for _, number := range numbers {
		issues = append(issues, map[string]int{"id": 5000 + number, "number": number})
	}
	output, _ := json.Marshal([][]map[string]int{issues})
	return output
}

// issueURL returns the address of one scripted issue.
func issueURL(number int) string {
	return "https://github.com/example/project/issues/" + strconv.Itoa(number)
}

// publicationNow is the fixed clock of the end-to-end tests.
var publicationNow = time.Date(2026, time.October, 15, 10, 0, 0, 0, time.UTC)

// approvedSession stores a session with the given plan nodes, approves the
// GitHub preview, and returns the publisher and the store.
func approvedSession(t *testing.T, gh *scriptedGitHub, nodes []store.PlanNode) (*publication.Publisher, *store.Store) {
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
func assertInertIssues(t *testing.T, gh *scriptedGitHub, opened *store.Store) {
	t.Helper()
	for _, issue := range gh.issues {
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

	gh := newScriptedGitHub(t, true)
	publisher, opened := approvedSession(t, gh, []store.PlanNode{{ID: "n-1", Kind: "task", Title: "Cache images", Body: "Reuse images."}})

	result, err := publisher.Publish(t.Context(), publication.Request{SessionID: "session-1", AttemptID: "attempt-1"})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if result.Receipt.Outcome != store.PublicationOutcomePublished || len(result.Receipt.Items) != 1 || result.Receipt.Items[0].URL != issueURL(1) {
		t.Fatalf("receipt = %#v, want issue 1 published", result.Receipt)
	}
	if len(gh.issues) != 1 || !strings.Contains(gh.issues[0].Body, "<!-- factory-plan: session-1/1/n-1 -->") {
		t.Fatalf("issues = %#v, want one marked issue", gh.issues)
	}
	assertInertIssues(t, gh, opened)
}

// TestPublishCreatesTheParentChildrenGraphWithNativeLinks verifies that a
// decomposed plan publishes the parent and its children, links them as
// sub-issues and blocking dependencies, and returns every URL.
func TestPublishCreatesTheParentChildrenGraphWithNativeLinks(t *testing.T) {
	t.Parallel()

	gh := newScriptedGitHub(t, true)
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
	if urls["n-1"] != issueURL(1) || urls["n-2"] != issueURL(2) || urls["n-3"] != issueURL(3) {
		t.Fatalf("urls = %#v, want the parent first and both children", urls)
	}
	if fmt.Sprint(gh.subIssues[1]) != "[2 3]" || fmt.Sprint(gh.blockedBy[3]) != "[2]" || len(gh.comments) != 0 {
		t.Fatalf("sub-issues = %v, blocked by = %v, comments = %v; want native links only", gh.subIssues, gh.blockedBy, gh.comments)
	}
	assertInertIssues(t, gh, opened)
}

// TestPublishCreatesTheApprovedTextualFallbackLinks verifies that a plan
// approved with the textual fallback publishes it: the bodies carry the
// relationship section and the marked backlink comments link the numbers.
func TestPublishCreatesTheApprovedTextualFallbackLinks(t *testing.T) {
	t.Parallel()

	gh := newScriptedGitHub(t, false)
	publisher, opened := approvedSession(t, gh, decomposedNodes())

	result, err := publisher.Publish(t.Context(), publication.Request{SessionID: "session-1", AttemptID: "attempt-1"})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if result.Receipt.Outcome != store.PublicationOutcomePublished {
		t.Fatalf("receipt = %#v, want published", result.Receipt)
	}
	if !strings.Contains(gh.issues[0].Body, "- [ ] Sub-issue: Cache images (`n-2`)") {
		t.Fatalf("parent body = %q, want the approved textual task list", gh.issues[0].Body)
	}
	if len(gh.comments[2]) != 1 || !strings.HasPrefix(gh.comments[2][0], "Part of #1") ||
		len(gh.comments[3]) != 2 || !strings.HasPrefix(gh.comments[3][1], "Depends on #2") {
		t.Fatalf("comments = %#v, want the backlinks on the sub-issues and the dependent issue", gh.comments)
	}
	if len(gh.subIssues) != 0 || len(gh.blockedBy) != 0 {
		t.Fatal("textual fallback created native links after approval")
	}
	assertInertIssues(t, gh, opened)
}

// TestPublishRetriesAfterAnUncertainCreateWithoutADuplicate verifies that a
// create whose response is lost leaves the publication incomplete and that
// the retry adopts the issue by its marker instead of creating it again.
func TestPublishRetriesAfterAnUncertainCreateWithoutADuplicate(t *testing.T) {
	t.Parallel()

	gh := newScriptedGitHub(t, true)
	publisher, opened := approvedSession(t, gh, decomposedNodes())
	gh.failOnce("create:Cache images", &hostcmd.TimeoutError{Operation: "gh api", Timeout: github.CommandTimeout}, true)

	first, err := publisher.Publish(t.Context(), publication.Request{SessionID: "session-1", AttemptID: "attempt-1"})
	if err != nil {
		t.Fatalf("first Publish() error = %v", err)
	}
	if first.Receipt.Outcome != store.PublicationOutcomeIncomplete || len(gh.issues) != 2 {
		t.Fatalf("first receipt = %#v with %d issues, want incomplete after the lost response", first.Receipt, len(gh.issues))
	}
	second, err := publisher.Publish(t.Context(), publication.Request{SessionID: "session-1", AttemptID: "attempt-2"})
	if err != nil {
		t.Fatalf("second Publish() error = %v", err)
	}
	if second.Receipt.Outcome != store.PublicationOutcomePublished || len(gh.issues) != 3 || gh.creates() != 3 {
		t.Fatalf("second receipt = %#v, issues = %d, creates = %d; want three issues from three creates", second.Receipt, len(gh.issues), gh.creates())
	}
	assertInertIssues(t, gh, opened)
}

// TestPublishResumesAfterPartialCreationAndLinkFailures verifies that a
// rejected create stops publication with a proven no-change failure, that a
// link with an unknown outcome is verified before the retry, and that the
// retry completes the graph without a duplicate issue or link.
func TestPublishResumesAfterPartialCreationAndLinkFailures(t *testing.T) {
	t.Parallel()

	gh := newScriptedGitHub(t, true)
	publisher, opened := approvedSession(t, gh, decomposedNodes())
	gh.failOnce("create:Document setup", fmt.Errorf("gh api repos/example/project/issues --method POST: HTTP 403: Resource not accessible"), false)

	first, err := publisher.Publish(t.Context(), publication.Request{SessionID: "session-1", AttemptID: "attempt-1"})
	if err != nil {
		t.Fatalf("first Publish() error = %v", err)
	}
	if first.Receipt.Outcome != store.PublicationOutcomeIncomplete || len(gh.issues) != 2 || !strings.Contains(first.Receipt.Detail, "made no change") {
		t.Fatalf("first receipt = %#v, want incomplete after a rejected create", first.Receipt)
	}

	gh.failOnce("sub_issue:1", fmt.Errorf("gh api repos/example/project/issues/1/sub_issues --method POST: HTTP 502: Bad Gateway"), true)
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
	if third.Receipt.Outcome != store.PublicationOutcomePublished || len(gh.issues) != 3 {
		t.Fatalf("third receipt = %#v, want published with three issues", third.Receipt)
	}
	if fmt.Sprint(gh.subIssues[1]) != "[2 3]" || fmt.Sprint(gh.blockedBy[3]) != "[2]" {
		t.Fatalf("sub-issues = %v, blocked by = %v; want every link once", gh.subIssues, gh.blockedBy)
	}
	assertInertIssues(t, gh, opened)
}
