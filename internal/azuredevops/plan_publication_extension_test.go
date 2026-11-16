package azuredevops_test

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/azuredevops"
	"github.com/Stevie1704/sw-factory/internal/azuredevops/azuredevopstest"
	"github.com/Stevie1704/sw-factory/internal/publication"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// Assignment tags of the two scripted repositories.
var (
	serviceTag = azuredevops.RepositoryAssignmentTag(azuredevopstest.RepositoryID)
	webTag     = azuredevops.RepositoryAssignmentTag(azuredevopstest.WebRepositoryID)
)

// approvedExtensionSession approves, on the extension route, one Epic with
// two PBIs: n-2 names no target and so executes on the registered service
// repository, and n-3 targets the web repository and depends on n-2.
func approvedExtensionSession(t *testing.T, boards *azuredevopstest.Boards) *boardsSession {
	t.Helper()
	return approvedSession(t, boards, extensionPlanClient(boards, epicPBIMapping()), []store.PlanNode{
		{ID: "n-1", Kind: "parent", Title: "Faster onboarding", Body: "Parent.", AcceptanceCriteria: []string{"One command."}, BriefRefs: []string{"scope-1"}},
		{ID: "n-3", Kind: "task", Title: "Document setup", Body: "Docs.", AcceptanceCriteria: []string{"Documented."}, ParentID: "n-1", DependsOn: []string{"n-2"}, TargetRepository: azuredevopstest.WebRepository},
		{ID: "n-2", Kind: "investigation", Title: "Cache images", Body: "Child.", AcceptanceCriteria: []string{"Cached."}, ParentID: "n-1"},
	})
}

// assertExtensionInert verifies that publication on the extension route
// wrote only creates and relation adds, never az rest, a tag update, a
// state, or agent-ready; that every item carries only its correlation tag
// and at most its assignment tag; and that no run started.
func (s *boardsSession) assertExtensionInert() {
	s.t.Helper()
	if len(s.boards.Calls()) != 0 {
		s.t.Fatalf("az rest calls = %d, want none on the extension route", len(s.boards.Calls()))
	}
	for _, write := range s.boards.ExtensionWrites() {
		if write.Route() != "boards work-item create" && write.Route() != "boards work-item relation add" {
			s.t.Fatalf("publication ran %s, want only creates and relation adds", write.Route())
		}
		for _, values := range write.Flags {
			if slices.ContainsFunc(values, func(value string) bool {
				return strings.Contains(value, tracker.LabelAgentReady) || strings.Contains(value, "System.State")
			}) {
				s.t.Fatalf("write %s sends a state or %s: %v", write.Route(), tracker.LabelAgentReady, write.Flags)
			}
		}
	}
	for _, item := range s.boards.Snapshot() {
		for _, tag := range item.Tags() {
			if !strings.HasPrefix(tag, "factory-plan:session-1/") && tag != serviceTag && tag != webTag {
				s.t.Fatalf("work item %d carries tag %q", item.ID, tag)
			}
		}
	}
	if run, err := s.store.CurrentRun(s.t.Context()); err != nil || run != nil {
		s.t.Fatalf("CurrentRun() = %#v, %v; publication must not start a run", run, err)
	}
}

// TestExtensionPublishCreatesOneEpicWithTwoDirectPBIs verifies the spec's
// acceptance on scripted Boards: one Epic and two PBIs, each PBI with a
// native Parent link directly to the Epic and its own repository
// assignment, the predecessor link on the dependent PBI, and every work item
// ID and URL recorded.
func TestExtensionPublishCreatesOneEpicWithTwoDirectPBIs(t *testing.T) {
	t.Parallel()

	s := approvedExtensionSession(t, azuredevopstest.NewBoards(t, azuredevopstest.ScrumProcess()))
	result := s.publish("attempt-1")
	if result.Receipt.Outcome != store.PublicationOutcomePublished {
		t.Fatalf("receipt = %#v, want published", result.Receipt)
	}
	items := s.itemsByTitle()
	if len(items) != 3 {
		t.Fatalf("items = %d, want one Epic and two PBIs and no Feature", len(items))
	}
	epic := items["Faster onboarding/Epic"]
	cache := items["Cache images/Product Backlog Item"]
	docs := items["Document setup/Product Backlog Item"]
	parent := azuredevopstest.Relation{Rel: "System.LinkTypes.Hierarchy-Reverse", URL: "https://dev.azure.com/contoso/_apis/wit/workItems/" + strconv.Itoa(epic.ID)}
	predecessor := azuredevopstest.Relation{Rel: "System.LinkTypes.Dependency-Reverse", URL: "https://dev.azure.com/contoso/_apis/wit/workItems/" + strconv.Itoa(cache.ID)}
	if len(epic.Relations) != 0 || !slices.Equal(cache.Relations, []azuredevopstest.Relation{parent}) ||
		!slices.Equal(docs.Relations, []azuredevopstest.Relation{parent, predecessor}) {
		t.Fatalf("relations: epic %v, cache %v, docs %v; want both PBIs directly below the Epic", epic.Relations, cache.Relations, docs.Relations)
	}
	if slices.Contains(epic.Tags(), serviceTag) || slices.Contains(epic.Tags(), webTag) {
		t.Fatalf("epic tags = %v, want no repository assignment", epic.Tags())
	}
	if !slices.Equal(cache.Tags(), []string{"factory-plan:session-1/1/n-2", serviceTag}) || !slices.Equal(docs.Tags(), []string{"factory-plan:session-1/1/n-3", webTag}) {
		t.Fatalf("PBI tags = %v and %v, want each PBI's correlation and its own assignment tag", cache.Tags(), docs.Tags())
	}
	recorded := map[string]string{}
	for _, item := range result.Receipt.Items {
		recorded[item.NodeID] = item.ExternalID + " " + item.URL
	}
	for node, item := range map[string]azuredevopstest.Item{"n-1": epic, "n-2": cache, "n-3": docs} {
		id := strconv.Itoa(item.ID)
		if recorded[node] != id+" https://dev.azure.com/contoso/Factory%20Pilot/_workitems/edit/"+id {
			t.Fatalf("receipt item of %s = %q, want work item %s", node, recorded[node], id)
		}
	}
	s.assertExtensionInert()
}

// TestExtensionPublishReconcilesLostResponsesWithoutDuplicates verifies that
// a lost create response and a lost link response, each followed by a
// coordinator restart, are reconciled by correlation and relation lookups:
// every item and every link exists exactly once.
func TestExtensionPublishReconcilesLostResponsesWithoutDuplicates(t *testing.T) {
	t.Parallel()

	boards := azuredevopstest.NewBoards(t, azuredevopstest.ScrumProcess())
	s := approvedExtensionSession(t, boards)
	boards.FailOnce("create:Cache images", "az boards work-item create: connection reset by peer", true)
	if first := s.publish("attempt-1"); first.Receipt.Outcome != store.PublicationOutcomeIncomplete || operation(t, first, "item/n-2").State != store.PublicationOperationAmbiguous {
		t.Fatalf("first attempt = %#v, want incomplete with an ambiguous create", first.Receipt)
	}
	s.reopen()
	boards.FailOnce("link:103", "az boards work-item relation add: connection reset by peer", true)
	if second := s.publish("attempt-2"); second.Receipt.Outcome != store.PublicationOutcomeIncomplete {
		t.Fatalf("second attempt = %#v, want incomplete after the lost link", second.Receipt)
	}
	s.reopen()
	third := s.publish("attempt-3")
	if third.Receipt.Outcome != store.PublicationOutcomePublished {
		t.Fatalf("third attempt = %#v, want published", third.Receipt)
	}
	creates, links := 0, 0
	for _, write := range boards.ExtensionWrites() {
		switch write.Route() {
		case "boards work-item create":
			creates++
		case "boards work-item relation add":
			links++
		}
	}
	docs := s.itemsByTitle()["Document setup/Product Backlog Item"]
	if creates != 3 || links != 3 || docs.ID != 103 || len(docs.Relations) != 2 {
		t.Fatalf("creates = %d, links = %d, docs = %#v; want 3 creates, 3 link writes, and 2 links on docs", creates, links, docs)
	}
	if adopted := operation(t, third, "item/n-2"); adopted.Detail != "found by correlation lookup" {
		t.Fatalf("item/n-2 = %#v, want it adopted by correlation lookup", adopted)
	}
	cache := s.itemsByTitle()["Cache images/Product Backlog Item"]
	if !slices.Equal(cache.Tags(), []string{"factory-plan:session-1/1/n-2", serviceTag}) || !slices.Equal(docs.Tags(), []string{"factory-plan:session-1/1/n-3", webTag}) {
		t.Fatalf("PBI tags after the retries = %v and %v, want each PBI's correlation and assignment tag", cache.Tags(), docs.Tags())
	}
	s.assertExtensionInert()
}

// TestExtensionPublishStopsOnDuplicateCorrelationMatches verifies that two
// work items with one correlation tag stop publication without a create.
func TestExtensionPublishStopsOnDuplicateCorrelationMatches(t *testing.T) {
	t.Parallel()

	boards := azuredevopstest.NewBoards(t, azuredevopstest.ScrumProcess())
	s := approvedExtensionSession(t, boards)
	boards.Seed("Product Backlog Item", "Cache images", "factory-plan:session-1/1/n-2", serviceTag)
	boards.Seed("Product Backlog Item", "Cache images (copy)", "factory-plan:session-1/1/n-2", serviceTag)
	result := s.publish("attempt-1")
	stopped := operation(t, result, "item/n-2")
	if result.Receipt.Outcome != store.PublicationOutcomeIncomplete || stopped.State != store.PublicationOperationAmbiguous || !strings.Contains(stopped.Detail, "2 tracker items") {
		t.Fatalf("result = %#v, item/n-2 = %#v; want a stop on two candidates", result.Receipt, stopped)
	}
	if writes := boards.ExtensionWrites(); len(writes) != 1 {
		t.Fatalf("writes = %d, want only the Epic created", len(writes))
	}
}

// TestExtensionPublishRefusesAnExistingItemWithAnotherTarget verifies that an
// item that carries the correlation tag but assigns another repository is
// neither adopted nor retargeted: publication stops without a write to it,
// and the operator cannot adopt it through reconciliation either.
func TestExtensionPublishRefusesAnExistingItemWithAnotherTarget(t *testing.T) {
	t.Parallel()

	boards := azuredevopstest.NewBoards(t, azuredevopstest.ScrumProcess())
	s := approvedExtensionSession(t, boards)
	foreign := boards.Seed("Product Backlog Item", "Document setup", "factory-plan:session-1/1/n-3", serviceTag)
	result := s.publish("attempt-1")
	stopped := operation(t, result, "item/n-3")
	if result.Receipt.Outcome != store.PublicationOutcomeIncomplete || stopped.State != store.PublicationOperationAmbiguous || !strings.Contains(stopped.Detail, webTag) {
		t.Fatalf("result = %#v, item/n-3 = %#v; want a stop naming the approved assignment", result.Receipt, stopped)
	}
	for _, write := range boards.ExtensionWrites() {
		if write.Flag("--id") == strconv.Itoa(foreign) || write.Flag("--title") == "Document setup" {
			t.Fatalf("publication wrote %s for the mismatched item: %v", write.Route(), write.Flags)
		}
	}
	_, err := s.publisher().Reconcile(t.Context(), publication.Reconciliation{SessionID: "session-1", OperationID: "item/n-3", ExternalID: strconv.Itoa(foreign)})
	if !errors.Is(err, publication.ErrReconciliationRefused) || !strings.Contains(err.Error(), webTag) {
		t.Fatalf("Reconcile() error = %v, want a refusal naming the approved assignment", err)
	}
	if got := boards.Snapshot()[0].Tags(); !slices.Equal(got, []string{"factory-plan:session-1/1/n-3", serviceTag}) {
		t.Fatalf("foreign item tags = %v, want them unchanged", got)
	}
}

// TestExtensionPublishRevalidatesTargetsBeforeWrites verifies that an
// approved target that is no longer registered stops publication before any
// write, so a changed assignment needs a new preview and approval.
func TestExtensionPublishRevalidatesTargetsBeforeWrites(t *testing.T) {
	t.Parallel()

	boards := azuredevopstest.NewBoards(t, azuredevopstest.ScrumProcess())
	s := approvedExtensionSession(t, boards)
	s.client.PlanRepositories = nil
	_, err := s.publisher().Publish(t.Context(), publication.Request{SessionID: "session-1", AttemptID: "attempt-1"})
	if err == nil || !strings.Contains(err.Error(), "plan_repositories") {
		t.Fatalf("Publish() error = %v, want the unregistered target refused", err)
	}
	if writes := boards.ExtensionWrites(); len(writes) != 0 {
		t.Fatalf("writes = %d, want none", len(writes))
	}
}
