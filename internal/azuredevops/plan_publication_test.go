package azuredevops_test

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/publication"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// publicationNow is the fixed clock of the end-to-end tests.
var publicationNow = time.Date(2026, time.October, 15, 10, 0, 0, 0, time.UTC)

// conversationSecret is user turn text that must never reach the tracker.
const conversationSecret = "PRIVATE-TURN-TEXT-a51c"

// boardsSession is one approved session in a real SQLite store, published
// to a scripted Azure Boards project.
type boardsSession struct {
	t       *testing.T
	path    string
	store   *store.Store
	boards  *scriptedBoards
	mapping *config.AzurePlanMapping
}

// approvedBoardsSession stores a session with a parent and two tasks (n-3
// depends on n-2, and the plan lists n-3 before n-2), approves the Azure
// DevOps preview under mapping, and returns the session.
func approvedBoardsSession(t *testing.T, boards *scriptedBoards, mapping *config.AzurePlanMapping) *boardsSession {
	t.Helper()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	s := &boardsSession{t: t, path: path, store: opened, boards: boards, mapping: mapping}
	t.Cleanup(func() { _ = s.store.Close() })
	session, err := opened.CreateIdeationSession(ctx, store.IdeationSession{ID: "session-1", RepositoryPath: "/repos/service", State: workflow.DefaultSessionLifecycle().InitialState(), CreatedAt: publicationNow})
	if err != nil {
		t.Fatal(err)
	}
	session, _, err = opened.AppendIdeationTurn(ctx, s.write(session, workflow.SessionEventTurnAccepted), store.IdeationTurn{ID: "turn-1", Author: store.IdeationTurnAuthorUser, Body: conversationSecret})
	if err != nil {
		t.Fatal(err)
	}
	session, brief, err := opened.SaveBriefRevision(ctx, s.write(session, workflow.SessionEventEdited), store.Brief{
		Problem: "Onboarding is slow.", Scope: []store.BriefItem{{ID: "scope-1", Text: "The onboarding command."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if session, err = opened.TransitionIdeationSession(ctx, s.write(session, workflow.SessionEventRoleStarted)); err != nil {
		t.Fatal(err)
	}
	if session, _, err = opened.SavePlanRevision(ctx, s.write(session, workflow.SessionEventPlanDrafted), store.PlanRevision{
		SourceBriefRevision: brief.Revision, ContextRevision: planContext,
		Nodes: []store.PlanNode{
			{ID: "n-1", Kind: "parent", Title: "Faster onboarding", Body: "Parent.", AcceptanceCriteria: []string{"One command."}, BriefRefs: []string{"scope-1"}},
			{ID: "n-3", Kind: "task", Title: "Document setup", Body: "Docs.", AcceptanceCriteria: []string{"Documented."}, ParentID: "n-1", DependsOn: []string{"n-2"}},
			{ID: "n-2", Kind: "investigation", Title: "Cache images", Body: "Child.", AcceptanceCriteria: []string{"Cached."}, ParentID: "n-1"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	binding, err := s.publisher().Preview(ctx, "session-1")
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}
	if _, _, err := opened.ApprovePlan(ctx, s.write(session, workflow.SessionEventApproved), binding.Approval("approval-1")); err != nil {
		t.Fatalf("ApprovePlan() error = %v", err)
	}
	return s
}

// write resolves event for session through the session lifecycle.
func (s *boardsSession) write(session store.IdeationSession, event workflow.SessionEvent) store.IdeationWrite {
	s.t.Helper()
	write, err := workflow.DefaultSessionLifecycle().Write(session, event, "", publicationNow)
	if err != nil {
		s.t.Fatal(err)
	}
	return write
}

// publisher returns a publisher over the store and the scripted project.
func (s *boardsSession) publisher() *publication.Publisher {
	return &publication.Publisher{
		Store: s.store, Tracker: planClient(s.boards, s.mapping), Repository: repository,
		Lifecycle: workflow.DefaultSessionLifecycle(), Now: func() time.Time { return publicationNow },
	}
}

// publish runs one publication attempt.
func (s *boardsSession) publish(attempt string) publication.Result {
	s.t.Helper()
	result, err := s.publisher().Publish(s.t.Context(), publication.Request{SessionID: "session-1", AttemptID: attempt})
	if err != nil {
		s.t.Fatalf("Publish(%s) error = %v", attempt, err)
	}
	return result
}

// reopen closes the store and opens it again, as a coordinator restart does.
func (s *boardsSession) reopen() {
	s.t.Helper()
	_ = s.store.Close()
	reopened, err := store.Open(s.t.Context(), s.path)
	if err != nil {
		s.t.Fatalf("reopen store: %v", err)
	}
	s.store = reopened
}

// operation returns the ledger operation with id.
func operation(t *testing.T, result publication.Result, id string) store.PublicationOperation {
	t.Helper()
	for _, operation := range result.Operations {
		if operation.ID == id {
			return operation
		}
	}
	t.Fatalf("ledger has no operation %q: %#v", id, result.Operations)
	return store.PublicationOperation{}
}

// itemsByTitle returns the scripted work items keyed by title and fails on a
// duplicate title.
func (s *boardsSession) itemsByTitle() map[string]boardsItem {
	s.t.Helper()
	items := map[string]boardsItem{}
	for _, item := range s.boards.snapshot() {
		title := item.Fields["System.Title"].(string)
		if _, ok := items[title+"/"+item.Type]; ok {
			s.t.Fatalf("%s %q was created more than once", item.Type, title)
		}
		items[title+"/"+item.Type] = item
	}
	return items
}

// assertInert verifies decision D6 and the raw-conversation rule: every
// created item carries only its correlation tag, no factory state tag, no
// execution state, and no conversation text; every update targets an item
// the plan created; and publication started no run.
func (s *boardsSession) assertInert() {
	s.t.Helper()
	created := map[string]bool{}
	for _, item := range s.boards.snapshot() {
		created[strconv.Itoa(item.ID)] = true
		for _, tag := range item.tags() {
			if tracker.IsFactoryStateLabel(tag) || !strings.HasPrefix(tag, "factory-plan:session-1/") {
				s.t.Fatalf("work item %d carries tag %q", item.ID, tag)
			}
		}
		if _, ok := item.Fields["System.State"]; ok {
			s.t.Fatalf("work item %d was created with a state", item.ID)
		}
		if strings.Contains(item.Fields["System.Description"].(string), conversationSecret) {
			s.t.Fatalf("work item %d contains conversation text", item.ID)
		}
	}
	for _, write := range s.boards.writes() {
		if write.method == "PATCH" && !created[filepath.Base(write.url.Path)] {
			s.t.Fatalf("publication updated %s, which the plan did not create", write.url.Path)
		}
		if strings.Contains(write.body, tracker.LabelAgentReady) {
			s.t.Fatalf("write %s sends %s", write.route(), tracker.LabelAgentReady)
		}
	}
	if run, err := s.store.CurrentRun(s.t.Context()); err != nil || run != nil {
		s.t.Fatalf("CurrentRun() = %#v, %v; publication must not start a run", run, err)
	}
}

// requiredFeatureMapping is the Agile mapping with a required Feature level.
func requiredFeatureMapping() *config.AzurePlanMapping {
	mapping := config.AgileAzurePlanMapping()
	mapping.Levels[1].Required = true
	return &mapping
}

// TestPublishCreatesTheHierarchyAndDependenciesInValidOrder verifies that an
// approved plan with a required intermediate Feature publishes every item
// once, each parent and each predecessor before the items that need it,
// with parent and predecessor links in the approved direction, and records
// the work item ID and URL of every local node.
func TestPublishCreatesTheHierarchyAndDependenciesInValidOrder(t *testing.T) {
	t.Parallel()

	s := approvedBoardsSession(t, newScriptedBoards(t, agileProcess()), requiredFeatureMapping())
	result := s.publish("attempt-1")
	if result.Receipt.Outcome != store.PublicationOutcomePublished {
		t.Fatalf("receipt = %#v, want published", result.Receipt)
	}
	order := []string{}
	for _, item := range s.boards.snapshot() {
		order = append(order, item.Fields["System.Title"].(string)+"/"+item.Type)
	}
	want := []string{"Faster onboarding/Epic", "Faster onboarding/Feature", "Cache images/User Story", "Document setup/User Story"}
	if !slices.Equal(order, want) {
		t.Fatalf("create order = %v, want %v", order, want)
	}
	items := s.itemsByTitle()
	epic, feature := items["Faster onboarding/Epic"], items["Faster onboarding/Feature"]
	cache, docs := items["Cache images/User Story"], items["Document setup/User Story"]
	link := func(rel string, to boardsItem) boardsRelation {
		return boardsRelation{Rel: rel, URL: "https://dev.azure.com/contoso/_apis/wit/workItems/" + strconv.Itoa(to.ID)}
	}
	wantRelations := map[int][]boardsRelation{
		epic.ID:    nil,
		feature.ID: {link("System.LinkTypes.Hierarchy-Reverse", epic)},
		cache.ID:   {link("System.LinkTypes.Hierarchy-Reverse", feature)},
		docs.ID:    {link("System.LinkTypes.Hierarchy-Reverse", feature), link("System.LinkTypes.Dependency-Reverse", cache)},
	}
	for _, item := range s.boards.snapshot() {
		if !slices.Equal(item.Relations, wantRelations[item.ID]) {
			t.Fatalf("work item %d relations = %#v, want %#v", item.ID, item.Relations, wantRelations[item.ID])
		}
	}
	recorded := map[string]string{}
	for _, item := range result.Receipt.Items {
		recorded[item.NodeID] = item.ExternalID + " " + item.URL
	}
	for node, item := range map[string]boardsItem{"n-1": epic, "n-1--feature": feature, "n-2": cache, "n-3": docs} {
		id := strconv.Itoa(item.ID)
		if recorded[node] != id+" https://dev.azure.com/contoso/Factory%20Pilot/_workitems/edit/"+id {
			t.Fatalf("receipt item of %s = %q, want work item %s", node, recorded[node], id)
		}
	}
	s.assertInert()
}

// TestPublishResumesAnInterruptedPublicationWithoutDuplicates verifies that a
// create whose response is lost stops publication as incomplete, and that a
// retry after a coordinator restart finds the item by its correlation tag
// instead of creating it again and then completes the plan.
func TestPublishResumesAnInterruptedPublicationWithoutDuplicates(t *testing.T) {
	t.Parallel()

	boards := newScriptedBoards(t, agileProcess())
	s := approvedBoardsSession(t, boards, nil)
	boards.failOnce("create:Cache images", "az rest: connection reset by peer", true)
	first := s.publish("attempt-1")
	if first.Receipt.Outcome != store.PublicationOutcomeIncomplete || operation(t, first, "item/n-2").State != store.PublicationOperationAmbiguous {
		t.Fatalf("first attempt = %#v, want incomplete with an ambiguous create", first.Receipt)
	}
	s.reopen()
	second := s.publish("attempt-2")
	if second.Receipt.Outcome != store.PublicationOutcomePublished {
		t.Fatalf("second attempt = %#v, want published", second.Receipt)
	}
	if boards.creates() != 3 || len(s.itemsByTitle()) != 3 {
		t.Fatalf("creates = %d, want each of the 3 items created once", boards.creates())
	}
	if adopted := operation(t, second, "item/n-2"); adopted.Detail != "found by correlation lookup" {
		t.Fatalf("item/n-2 = %#v, want it adopted by correlation lookup", adopted)
	}
	s.assertInert()
}

// TestPublishStopsOnUncertainOrDuplicateCreates verifies that publication
// never repeats a create blindly: an unknown outcome with no matching item,
// or more than one item with the correlation tag, stops for explicit
// reconciliation without another create.
func TestPublishStopsOnUncertainOrDuplicateCreates(t *testing.T) {
	t.Parallel()

	t.Run("no item after an unknown outcome", func(t *testing.T) {
		t.Parallel()

		boards := newScriptedBoards(t, agileProcess())
		s := approvedBoardsSession(t, boards, nil)
		boards.failOnce("create:Cache images", "az rest: connection reset by peer", false)
		s.publish("attempt-1")
		creates := boards.creates()
		retry := s.publish("attempt-2")
		if retry.Receipt.Outcome != store.PublicationOutcomeIncomplete || operation(t, retry, "item/n-2").State != store.PublicationOperationAmbiguous || boards.creates() != creates {
			t.Fatalf("retry = %#v with %d creates, want a stop without a create", retry.Receipt, boards.creates()-creates)
		}
	})
	t.Run("duplicate candidates", func(t *testing.T) {
		t.Parallel()

		boards := newScriptedBoards(t, agileProcess())
		s := approvedBoardsSession(t, boards, nil)
		boards.seed("User Story", "Cache images", "factory-plan:session-1/1/n-2")
		boards.seed("User Story", "Cache images (copy)", "factory-plan:session-1/1/n-2")
		result := s.publish("attempt-1")
		stopped := operation(t, result, "item/n-2")
		if result.Receipt.Outcome != store.PublicationOutcomeIncomplete || stopped.State != store.PublicationOperationAmbiguous || !strings.Contains(stopped.Detail, "2 tracker items") {
			t.Fatalf("result = %#v, item/n-2 = %#v; want a stop on two candidates", result.Receipt, stopped)
		}
		if boards.creates() != 1 {
			t.Fatalf("creates = %d, want only the parent created", boards.creates())
		}
	})
}

// TestPublishStopsOnAPermissionFailureWithoutFurtherWrites verifies that a
// create refused for permission is recorded as failed, makes no further
// write, and leaves an incomplete receipt.
func TestPublishStopsOnAPermissionFailureWithoutFurtherWrites(t *testing.T) {
	t.Parallel()

	boards := newScriptedBoards(t, agileProcess())
	s := approvedBoardsSession(t, boards, nil)
	boards.failOnce("create:Faster onboarding", `az rest: Forbidden({"message":"TF237111: no permission to save work items"})`, false)
	result := s.publish("attempt-1")
	if result.Receipt.Outcome != store.PublicationOutcomeIncomplete || operation(t, result, "item/n-1").State != store.PublicationOperationFailed {
		t.Fatalf("result = %#v, want incomplete with a failed create", result.Receipt)
	}
	if len(boards.snapshot()) != 0 || boards.creates() != 1 {
		t.Fatalf("items = %#v after %d creates, want one refused create", boards.snapshot(), boards.creates())
	}
}

// TestPublishVerifiesALinkWhoseResponseWasLost verifies that a link whose
// response is lost is found on retry and not added a second time.
func TestPublishVerifiesALinkWhoseResponseWasLost(t *testing.T) {
	t.Parallel()

	boards := newScriptedBoards(t, agileProcess())
	s := approvedBoardsSession(t, boards, nil)
	boards.failOnce("link:103", "az rest: connection reset by peer", true)
	if first := s.publish("attempt-1"); first.Receipt.Outcome != store.PublicationOutcomeIncomplete {
		t.Fatalf("first attempt = %#v, want incomplete", first.Receipt)
	}
	if second := s.publish("attempt-2"); second.Receipt.Outcome != store.PublicationOutcomePublished {
		t.Fatalf("second attempt = %#v, want published", second.Receipt)
	}
	docs := s.itemsByTitle()["Document setup/User Story"]
	if docs.ID != 103 || len(docs.Relations) != 2 {
		t.Fatalf("Document setup = %#v, want work item 103 with one parent and one predecessor link", docs)
	}
	s.assertInert()
}
