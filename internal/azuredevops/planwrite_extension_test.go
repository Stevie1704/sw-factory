package azuredevops_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/azuredevops"
	"github.com/Stevie1704/sw-factory/internal/azuredevops/azuredevopstest"
	"github.com/Stevie1704/sw-factory/internal/hostcmd"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// markdownBody is a description with Markdown and HTML-significant text,
// indentation, and blank lines.
const markdownBody = "Cache the <base> image & reuse it.\n\n## Acceptance criteria\n\n- [ ] Cached \"twice\"\n  - nested 'item'\n\n```\nx := a < b\n```\n"

// renderedPBI returns one rendered PBI that targets the web repository.
func renderedPBI() tracker.RenderedPlanItem {
	return tracker.RenderedPlanItem{
		NodeID: "n-2", Type: "Product Backlog Item", Title: "--cache images", Body: markdownBody,
		Fields:           map[string]string{"Microsoft.VSTS.Common.Priority": "1"},
		Labels:           []string{"factory-plan:s/1/n-2", "factory-repo:" + azuredevopstest.WebRepositoryID},
		Correlation:      "s/1/n-2",
		TargetRepository: &tracker.PlanTargetRepository{Name: azuredevopstest.WebRepository, ID: azuredevopstest.WebRepositoryID},
	}
}

// TestExtensionCreatePlanItemUsesTheDedicatedCommand verifies that the
// extension route creates the item with az boards work-item create, with
// its exact title, its correlation and assignment tags, and its fields; that
// the description, which the command can only store as HTML, reads back as
// the approved Markdown text; and that no az rest call is made.
func TestExtensionCreatePlanItemUsesTheDedicatedCommand(t *testing.T) {
	t.Parallel()

	boards := azuredevopstest.NewBoards(t, azuredevopstest.ScrumProcess())
	client := extensionPlanClient(boards, epicPBIMapping())
	ref, err := client.CreatePlanItem(t.Context(), repository, renderedPBI())
	if err != nil {
		t.Fatalf("CreatePlanItem() error = %v", err)
	}
	if ref.ExternalID != "101" || ref.URL != "https://dev.azure.com/contoso/Factory%20Pilot/_workitems/edit/101" {
		t.Fatalf("ref = %#v", ref)
	}
	writes := boards.ExtensionWrites()
	if len(writes) != 1 || writes[0].Route() != "boards work-item create" || writes[0].Flag("--type") != "Product Backlog Item" || writes[0].Flag("--project") != azuredevopstest.Project {
		t.Fatalf("writes = %#v, want one dedicated create in the project", writes)
	}
	created := boards.Snapshot()[0]
	if created.Title() != "--cache images" || created.Fields["Microsoft.VSTS.Common.Priority"] != "1" ||
		strings.Join(created.Tags(), ",") != "factory-plan:s/1/n-2,factory-repo:"+azuredevopstest.WebRepositoryID || len(created.Formats) != 0 {
		t.Fatalf("created = %#v, want the exact title, tags, and field in HTML format", created)
	}
	if strings.Contains(created.Description(), "<base>") || !strings.Contains(created.Description(), "&lt;base&gt;") {
		t.Fatalf("description = %q, want every Markdown character escaped", created.Description())
	}
	issue, err := client.Issue(t.Context(), repository, 101)
	if err != nil {
		t.Fatal(err)
	}
	if issue.Body != strings.TrimSpace(markdownBody) {
		t.Fatalf("read back body = %q\nwant %q", issue.Body, strings.TrimSpace(markdownBody))
	}
	if len(boards.Calls()) != 0 {
		t.Fatalf("az rest calls = %d, want none", len(boards.Calls()))
	}
}

// TestExtensionCreatePlanItemRefusesAMismatchedAssignment verifies that an
// item whose assignment tags do not match its target, or that is executable
// without a target, is refused before any write.
func TestExtensionCreatePlanItemRefusesAMismatchedAssignment(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*tracker.RenderedPlanItem){
		"tag for another repository": func(item *tracker.RenderedPlanItem) {
			item.Labels[1] = "factory-repo:" + azuredevopstest.RepositoryID
		},
		"two assignment tags": func(item *tracker.RenderedPlanItem) {
			item.Labels = append(item.Labels, "factory-repo:"+azuredevopstest.RepositoryID)
		},
		"tag without a target": func(item *tracker.RenderedPlanItem) { item.TargetRepository = nil },
		"target without a tag": func(item *tracker.RenderedPlanItem) { item.Labels = item.Labels[:1] },
		"agent-ready":          func(item *tracker.RenderedPlanItem) { item.Labels = append(item.Labels, tracker.LabelAgentReady) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			boards := azuredevopstest.NewBoards(t, azuredevopstest.ScrumProcess())
			item := renderedPBI()
			mutate(&item)
			_, err := extensionPlanClient(boards, epicPBIMapping()).CreatePlanItem(t.Context(), repository, item)
			if planErrorKind(err) != tracker.PlanErrorInvalidMapping || len(boards.ExtensionWrites()) != 0 {
				t.Fatalf("CreatePlanItem() error = %v with %d writes, want invalid_mapping and no write", err, len(boards.ExtensionWrites()))
			}
		})
	}
}

// TestExtensionPlanWritesClassifyOnlyProvenOutcomes verifies that only a
// failure the extension source proves to happen before the write is a
// write-not-applied error; any other failure, and a timeout, stay unknown.
func TestExtensionPlanWritesClassifyOnlyProvenOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		err     error
		applied bool
	}{
		{"login", &azuredevops.LoginRequiredError{OrganizationURL: "https://dev.azure.com/contoso"}, false},
		{"rule validation", errors.New("az boards work-item create: ERROR: TF401320: Rule Error for field Value Area. To specify a value for this field, use the --field argument and set the name of the name/value pair to Microsoft.VSTS.Common.ValueArea.: exit status 1"), false},
		{"service failure", errors.New("az boards work-item create: ERROR: VS402371: the service is unavailable: exit status 1"), true},
		{"timeout", &hostcmd.TimeoutError{Operation: "az boards work-item create", Timeout: time.Minute}, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := extensionPlanClient(failingRunner{test.err}, epicPBIMapping()).CreatePlanItem(t.Context(), repository, renderedPBI())
			if err == nil || tracker.WriteNotApplied(err) == test.applied {
				t.Fatalf("CreatePlanItem() error = %v, WriteNotApplied = %v; want unknown outcome %v", err, tracker.WriteNotApplied(err), test.applied)
			}
		})
	}
}

// TestExtensionFindPlanItemsMatchesTheExactCorrelationTag verifies that the
// correlation lookup queries the registered project explicitly, in any
// state, and keeps only items whose tag equals the correlation tag.
func TestExtensionFindPlanItemsMatchesTheExactCorrelationTag(t *testing.T) {
	t.Parallel()

	boards := azuredevopstest.NewBoards(t, azuredevopstest.ScrumProcess())
	boards.Seed("Product Backlog Item", "Longer", "factory-plan:s/1/n-22")
	want := boards.Seed("Product Backlog Item", "Exact", "factory-plan:s/1/n-2", "factory-repo:"+azuredevopstest.WebRepositoryID)
	refs, err := extensionPlanClient(boards, epicPBIMapping()).FindPlanItems(t.Context(), repository, "s/1/n-2")
	if err != nil {
		t.Fatalf("FindPlanItems() error = %v", err)
	}
	if len(refs) != 1 || refs[0].ExternalID != strconv.Itoa(want) {
		t.Fatalf("refs = %#v, want only work item %d", refs, want)
	}
}

// TestExtensionRelationsUseNamedLinksInTheApprovedDirection verifies that a
// parent link is added on the child with the organization's name of
// System.LinkTypes.Hierarchy-Reverse, so the Epic becomes the child's
// Parent, and that a dependency is added on the dependent item with the
// name of System.LinkTypes.Dependency-Reverse, so the item it depends on is
// its Predecessor; and that the lookup finds both afterwards.
func TestExtensionRelationsUseNamedLinksInTheApprovedDirection(t *testing.T) {
	t.Parallel()

	boards := azuredevopstest.NewBoards(t, azuredevopstest.ScrumProcess())
	client := extensionPlanClient(boards, epicPBIMapping())
	epic := boards.Seed("Epic", "Epic")
	first := boards.Seed("Product Backlog Item", "First")
	second := boards.Seed("Product Backlog Item", "Second")
	ref := func(id int) tracker.PlanItemRef { return tracker.PlanItemRef{ExternalID: strconv.Itoa(id)} }
	relationships := []tracker.PlanRelationship{
		{Kind: tracker.PlanRelationshipParent, Mode: tracker.PlanRelationshipNative, From: ref(second), To: ref(epic)},
		{Kind: tracker.PlanRelationshipDependsOn, Mode: tracker.PlanRelationshipNative, From: ref(second), To: ref(first)},
	}
	for _, relationship := range relationships {
		if found, err := client.FindPlanRelationship(t.Context(), repository, relationship); err != nil || found {
			t.Fatalf("FindPlanRelationship() before = %v, %v; want absent", found, err)
		}
		if err := client.CreatePlanRelationship(t.Context(), repository, relationship); err != nil {
			t.Fatalf("CreatePlanRelationship() error = %v", err)
		}
		if found, err := client.FindPlanRelationship(t.Context(), repository, relationship); err != nil || !found {
			t.Fatalf("FindPlanRelationship() after = %v, %v; want found", found, err)
		}
	}
	writes := boards.ExtensionWrites()
	want := [][3]string{{strconv.Itoa(second), "Parent", strconv.Itoa(epic)}, {strconv.Itoa(second), "Predecessor", strconv.Itoa(first)}}
	for index, call := range writes {
		got := [3]string{call.Flag("--id"), call.Flag("--relation-type"), call.Flag("--target-id")}
		if call.Route() != "boards work-item relation add" || got != want[index] {
			t.Fatalf("write %d = %s %v, want relation add %v", index, call.Route(), got, want[index])
		}
	}
	child := boards.Snapshot()[2]
	if len(child.Relations) != 2 || child.Relations[0].Rel != "System.LinkTypes.Hierarchy-Reverse" || child.Relations[1].Rel != "System.LinkTypes.Dependency-Reverse" {
		t.Fatalf("relations = %#v, want parent then predecessor on the dependent child", child.Relations)
	}
}

// TestExtensionRelationWithALostResponseIsVerifiedNotRepeated verifies that
// a relation add whose response is lost is reported with an unknown
// outcome, and that the lookup then finds the applied relation.
func TestExtensionRelationWithALostResponseIsVerifiedNotRepeated(t *testing.T) {
	t.Parallel()

	boards := azuredevopstest.NewBoards(t, azuredevopstest.ScrumProcess())
	client := extensionPlanClient(boards, epicPBIMapping())
	epic := boards.Seed("Epic", "Epic")
	child := boards.Seed("Product Backlog Item", "Child")
	relationship := tracker.PlanRelationship{Kind: tracker.PlanRelationshipParent, Mode: tracker.PlanRelationshipNative,
		From: tracker.PlanItemRef{ExternalID: strconv.Itoa(child)}, To: tracker.PlanItemRef{ExternalID: strconv.Itoa(epic)}}
	boards.FailOnce("link:"+strconv.Itoa(child), "az boards work-item relation add: connection reset", true)
	err := client.CreatePlanRelationship(t.Context(), repository, relationship)
	if err == nil || tracker.WriteNotApplied(err) {
		t.Fatalf("CreatePlanRelationship() error = %v, want an unknown outcome", err)
	}
	if found, err := client.FindPlanRelationship(t.Context(), repository, relationship); err != nil || !found {
		t.Fatalf("FindPlanRelationship() = %v, %v; want the applied relation", found, err)
	}
}
