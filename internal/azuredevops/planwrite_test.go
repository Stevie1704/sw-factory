package azuredevops_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/azuredevops"
	"github.com/Stevie1704/sw-factory/internal/hostcmd"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// renderedStory is one rendered User Story with a configured field.
func renderedStory() tracker.RenderedPlanItem {
	return tracker.RenderedPlanItem{
		NodeID: "n-2", Type: "User Story", Title: "Cache images", Body: "Child.\n",
		Fields: map[string]string{"Microsoft.VSTS.Common.Priority": "1"},
		Labels: []string{"factory-plan:session-1/2/n-2"}, Correlation: "session-1/2/n-2",
	}
}

// TestCreatePlanItemCreatesExactlyTheRenderedWorkItem verifies that a create
// sends the rendered type, title, Markdown description, correlation tag, and
// fields, and nothing else: no relation, no state, and no factory tag.
func TestCreatePlanItemCreatesExactlyTheRenderedWorkItem(t *testing.T) {
	t.Parallel()

	boards := newScriptedBoards(t, agileProcess())
	ref, err := planClient(boards, nil).CreatePlanItem(t.Context(), repository, renderedStory())
	if err != nil {
		t.Fatalf("CreatePlanItem() error = %v", err)
	}
	if ref.ExternalID != "101" || ref.URL != "https://dev.azure.com/contoso/Factory%20Pilot/_workitems/edit/101" {
		t.Fatalf("ref = %#v", ref)
	}
	created := boards.snapshot()[0]
	wantFields := map[string]any{
		"System.Title": "Cache images", "System.Description": "Child.\n",
		"System.Tags": "factory-plan:session-1/2/n-2", "Microsoft.VSTS.Common.Priority": "1",
	}
	if created.Type != "User Story" || !reflect.DeepEqual(created.Fields, wantFields) || created.Formats["System.Description"] != "Markdown" || len(created.Relations) != 0 {
		t.Fatalf("created = %#v, want exactly the rendered fields", created)
	}
	if writes := boards.writes(); len(writes) != 1 || writes[0].route() != "POST "+workItemsPath+"/$User Story" {
		t.Fatalf("writes = %#v, want one create", writes)
	}
}

// TestCreatePlanItemRefusesFactoryStateTags verifies decision D6: an item
// that carries agent-ready or another factory state tag is refused before
// any call, so publication never authorizes execution.
func TestCreatePlanItemRefusesFactoryStateTags(t *testing.T) {
	t.Parallel()

	for _, label := range tracker.FactoryStateLabels {
		boards := newScriptedBoards(t, agileProcess())
		item := renderedStory()
		item.Labels = append(item.Labels, label)
		_, err := planClient(boards, nil).CreatePlanItem(t.Context(), repository, item)
		if kind, _ := tracker.PlanErrorKindOf(err); kind != tracker.PlanErrorInvalidMapping {
			t.Fatalf("CreatePlanItem(%s) error = %v, want invalid mapping", label, err)
		}
		if len(boards.calls) != 0 {
			t.Fatalf("CreatePlanItem(%s) called az %#v", label, boards.calls)
		}
	}
}

// timeoutRunner fails every call with a host command timeout.
type timeoutRunner struct{}

// Run returns a timeout.
func (timeoutRunner) Run(context.Context, []string) ([]byte, error) {
	return nil, &hostcmd.TimeoutError{Operation: "az rest", Timeout: time.Minute}
}

// TestCreatePlanItemClassifiesFailuresByProvenOutcome verifies that only
// responses that prove no change are typed no-change errors, that a timeout
// is ambiguous, and that any other failure leaves the outcome unknown.
func TestCreatePlanItemClassifiesFailuresByProvenOutcome(t *testing.T) {
	t.Parallel()

	route := "POST " + workItemsPath + "/$User Story"
	tests := []struct {
		name       string
		runner     azuredevops.CommandRunner
		want       tracker.PlanErrorKind
		notApplied bool
	}{
		{name: "forbidden", runner: newFakeAz(t).on(route, `error:az rest: Forbidden({"message":"TF237111: no permission to save work items"})`), want: tracker.PlanErrorPermission, notApplied: true},
		{name: "unauthorized", runner: newFakeAz(t).on(route, `error:az rest: Unauthorized({"message":"sign in"})`), want: tracker.PlanErrorPermission, notApplied: true},
		{name: "rule violation", runner: newFakeAz(t).on(route, `error:az rest: Bad Request({"message":"TF401320: Rule Error for field Team"})`), want: tracker.PlanErrorInvalidMapping, notApplied: true},
		{name: "timeout", runner: timeoutRunner{}, want: tracker.PlanErrorAmbiguous},
		{name: "server error", runner: newFakeAz(t).on(route, `error:az rest: Internal Server Error({"message":"VS402490"})`)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client := &azuredevops.Client{Runner: tc.runner, Organization: "contoso"}
			_, err := client.CreatePlanItem(t.Context(), repository, renderedStory())
			kind, _ := tracker.PlanErrorKindOf(err)
			if err == nil || kind != tc.want || tracker.WriteNotApplied(err) != tc.notApplied {
				t.Fatalf("CreatePlanItem() error = %v (kind %q), want kind %q with not-applied %v", err, kind, tc.want, tc.notApplied)
			}
		})
	}
}

// TestFindPlanItemsReturnsEveryItemWithTheExactCorrelationTag verifies that
// the WIQL lookup finds every work item that carries the correlation tag, in
// ID order, and no item with another tag.
func TestFindPlanItemsReturnsEveryItemWithTheExactCorrelationTag(t *testing.T) {
	t.Parallel()

	boards := newScriptedBoards(t, agileProcess())
	first := boards.seed("User Story", "Cache images", "factory-plan:session-1/2/n-2", "team-a")
	boards.seed("User Story", "Other", "factory-plan:session-1/2/n-20")
	second := boards.seed("User Story", "Cache images copy", "factory-plan:session-1/2/n-2")
	refs, err := planClient(boards, nil).FindPlanItems(t.Context(), repository, "session-1/2/n-2")
	if err != nil {
		t.Fatalf("FindPlanItems() error = %v", err)
	}
	ids := []string{}
	for _, ref := range refs {
		ids = append(ids, ref.ExternalID)
	}
	if !reflect.DeepEqual(ids, []string{strconv.Itoa(first), strconv.Itoa(second)}) {
		t.Fatalf("FindPlanItems() = %v, want %d and %d", ids, first, second)
	}
	var query struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(boards.calls[0].body), &query); err != nil || !strings.Contains(query.Query, "[System.TeamProject] = @project") {
		t.Fatalf("WIQL = %q, %v; want a project-scoped query", query.Query, err)
	}
}

// TestPlanRelationshipsUseParentAndPredecessorLinks verifies that a parent
// relationship adds a Parent link on the child, that a dependency adds a
// Predecessor link to the item that must be done first, and that the lookup
// sees each link only after it exists.
func TestPlanRelationshipsUseParentAndPredecessorLinks(t *testing.T) {
	t.Parallel()

	boards := newScriptedBoards(t, agileProcess())
	parent := tracker.PlanItemRef{ExternalID: strconv.Itoa(boards.seed("Epic", "Parent"))}
	first := tracker.PlanItemRef{ExternalID: strconv.Itoa(boards.seed("User Story", "First"))}
	second := tracker.PlanItemRef{ExternalID: strconv.Itoa(boards.seed("User Story", "Second"))}
	client := planClient(boards, nil)
	relationships := []tracker.PlanRelationship{
		{Kind: tracker.PlanRelationshipParent, Mode: tracker.PlanRelationshipNative, From: second, To: parent},
		{Kind: tracker.PlanRelationshipDependsOn, Mode: tracker.PlanRelationshipNative, From: second, To: first},
	}
	for _, relationship := range relationships {
		if found, err := client.FindPlanRelationship(t.Context(), repository, relationship); err != nil || found {
			t.Fatalf("FindPlanRelationship() before create = %v, %v", found, err)
		}
		if err := client.CreatePlanRelationship(t.Context(), repository, relationship); err != nil {
			t.Fatalf("CreatePlanRelationship() error = %v", err)
		}
		if found, err := client.FindPlanRelationship(t.Context(), repository, relationship); err != nil || !found {
			t.Fatalf("FindPlanRelationship() after create = %v, %v", found, err)
		}
	}
	items := boards.snapshot()
	want := []boardsRelation{
		{Rel: "System.LinkTypes.Hierarchy-Reverse", URL: "https://dev.azure.com/contoso/_apis/wit/workItems/" + parent.ExternalID},
		{Rel: "System.LinkTypes.Dependency-Reverse", URL: "https://dev.azure.com/contoso/_apis/wit/workItems/" + first.ExternalID},
	}
	if !reflect.DeepEqual(items[2].Relations, want) || len(items[0].Relations)+len(items[1].Relations) != 0 {
		t.Fatalf("relations = %#v, want the parent and predecessor links on the dependent child only", items)
	}
	textual := relationships[0]
	textual.Mode = tracker.PlanRelationshipTextual
	if err := client.CreatePlanRelationship(t.Context(), repository, textual); mustKind(err) != tracker.PlanErrorUnsupportedCapability {
		t.Fatalf("CreatePlanRelationship(textual) error = %v, want unsupported capability", err)
	}
}

// mustKind returns the planning error kind of err.
func mustKind(err error) tracker.PlanErrorKind {
	kind, _ := tracker.PlanErrorKindOf(err)
	return kind
}
