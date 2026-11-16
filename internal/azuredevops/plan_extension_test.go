package azuredevops_test

import (
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/azuredevops"
	"github.com/Stevie1704/sw-factory/internal/azuredevops/azuredevopstest"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// epicPBIMapping is the explicit planning configuration of the extension
// workflow: a plan parent is an Epic and every task or investigation is a
// Product Backlog Item directly below it, with no Feature level.
func epicPBIMapping() *config.AzurePlanMapping {
	return &config.AzurePlanMapping{Levels: []config.AzurePlanLevel{
		{Type: "Epic", Kinds: []string{"parent"}},
		{Type: "Product Backlog Item", Kinds: []string{"task", "investigation"}},
	}}
}

// extensionPlanClient returns a devops_extension client over the scripted
// project that may also assign PBIs to the web repository.
func extensionPlanClient(runner azuredevops.CommandRunner, mapping *config.AzurePlanMapping) *azuredevops.Client {
	return &azuredevops.Client{
		Runner: runner, Organization: "contoso", Planning: mapping,
		Route: config.AzureRouteDevOpsExtension, PlanRepositories: []string{azuredevopstest.WebRepository},
	}
}

// targetedGraph is one Epic with two PBIs: n-2 targets the web repository
// explicitly, n-3 names no target and depends on n-2.
func targetedGraph() tracker.PlanGraph {
	return tracker.PlanGraph{SessionID: "s", PlanRevision: 1, ContextRevision: planContext, Items: []tracker.PlanGraphItem{
		{NodeID: "n-1", Kind: "parent", Title: "Faster onboarding", Body: "Epic body.", Correlation: "s/1/n-1"},
		{NodeID: "n-2", Kind: "task", Title: "Web onboarding", Body: "Web body.", ParentID: "n-1", Correlation: "s/1/n-2", TargetRepository: azuredevopstest.WebRepository},
		{NodeID: "n-3", Kind: "investigation", Title: "Service onboarding", Body: "Service body.", ParentID: "n-1", DependsOn: []string{"n-2"}, Correlation: "s/1/n-3"},
	}}
}

// TestExtensionPreflightRendersAnEpicWithDirectPBIsAndTheirTargets verifies
// that the extension route previews one Epic and two PBIs linked directly to
// it, gives each PBI its resolved target and exactly one assignment tag next
// to its correlation tag, never gives the Epic a target, and writes nothing.
func TestExtensionPreflightRendersAnEpicWithDirectPBIsAndTheirTargets(t *testing.T) {
	t.Parallel()

	boards := azuredevopstest.NewBoards(t, azuredevopstest.ScrumProcess())
	preview, err := extensionPlanClient(boards, epicPBIMapping()).PreflightPlan(t.Context(), repository, targetedGraph())
	if err != nil {
		t.Fatalf("PreflightPlan() error = %v", err)
	}
	if len(preview.Items) != 3 {
		t.Fatalf("preview items = %d, want one Epic and two PBIs", len(preview.Items))
	}
	epic, web, service := preview.Items[0], preview.Items[1], preview.Items[2]
	if epic.Type != "Epic" || epic.TargetRepository != nil || strings.Join(epic.Labels, ",") != "factory-plan:s/1/n-1" {
		t.Fatalf("epic = %+v, want an Epic with only its correlation tag and no target", epic)
	}
	wantWeb := tracker.PlanTargetRepository{Name: azuredevopstest.WebRepository, ID: azuredevopstest.WebRepositoryID}
	if web.Type != "Product Backlog Item" || web.TargetRepository == nil || *web.TargetRepository != wantWeb ||
		strings.Join(web.Labels, ",") != "factory-plan:s/1/n-2,factory-repo:"+azuredevopstest.WebRepositoryID {
		t.Fatalf("web PBI = %+v target %+v, want the web target and its assignment tag", web, web.TargetRepository)
	}
	wantService := tracker.PlanTargetRepository{Name: azuredevopstest.Repository, ID: azuredevopstest.RepositoryID}
	if service.TargetRepository == nil || *service.TargetRepository != wantService ||
		strings.Join(service.Labels, ",") != "factory-plan:s/1/n-3,factory-repo:"+azuredevopstest.RepositoryID {
		t.Fatalf("service PBI = %+v target %+v, want the registered repository as target", service, service.TargetRepository)
	}
	want := []tracker.PlanRelationshipMapping{
		{Kind: tracker.PlanRelationshipParent, FromNodeID: "n-2", ToNodeID: "n-1", Mode: tracker.PlanRelationshipNative},
		{Kind: tracker.PlanRelationshipParent, FromNodeID: "n-3", ToNodeID: "n-1", Mode: tracker.PlanRelationshipNative},
		{Kind: tracker.PlanRelationshipDependsOn, FromNodeID: "n-3", ToNodeID: "n-2", Mode: tracker.PlanRelationshipNative},
	}
	if len(preview.Relationships) != len(want) {
		t.Fatalf("relationships = %+v, want %+v", preview.Relationships, want)
	}
	for index := range want {
		if preview.Relationships[index] != want[index] {
			t.Fatalf("relationship %d = %+v, want %+v", index, preview.Relationships[index], want[index])
		}
	}
	if writes := boards.ExtensionWrites(); len(writes) != 0 || len(boards.Calls()) != 0 {
		t.Fatalf("preflight wrote %d commands and made %d az rest calls, want none", len(writes), len(boards.Calls()))
	}
}

// TestExtensionPreflightRefusesDeniedPermissions proves that approval cannot
// be obtained when the caller cannot edit work items or create metadata tags.
func TestExtensionPreflightRefusesDeniedPermissions(t *testing.T) {
	t.Parallel()
	boards := azuredevopstest.NewBoards(t, azuredevopstest.ScrumProcess())
	boards.FailOnce("factory-devops permission", "TF401027: permission denied", false)
	_, err := extensionPlanClient(boards, epicPBIMapping()).PreflightPlan(t.Context(), repository, targetedGraph())
	if err == nil || len(boards.ExtensionWrites()) != 0 {
		t.Fatalf("PreflightPlan() = %v, %d writes; want permission refusal without writes", err, len(boards.ExtensionWrites()))
	}
}

// TestExtensionPreflightRefusesWithoutWrites verifies that every
// incompatible configuration or plan fails before approval with a typed
// error that proves no write, and never substitutes a mapping.
func TestExtensionPreflightRefusesWithoutWrites(t *testing.T) {
	t.Parallel()

	agileStory := &config.AzurePlanMapping{Levels: []config.AzurePlanLevel{
		{Type: "Epic", Kinds: []string{"parent"}},
		{Type: "Product Backlog Item", Kinds: []string{"task", "investigation"}},
	}}
	tests := []struct {
		name    string
		process []azuredevopstest.WorkItemType
		mapping *config.AzurePlanMapping
		mutate  func(*tracker.PlanGraph)
		kind    tracker.PlanErrorKind
		reason  string
	}{
		{"no explicit mapping", azuredevopstest.ScrumProcess(), nil, func(*tracker.PlanGraph) {}, tracker.PlanErrorInvalidMapping, "azure_devops.planning"},
		{"process without PBIs", azuredevopstest.AgileProcess(), agileStory, func(*tracker.PlanGraph) {}, tracker.PlanErrorInvalidMapping, "Product Backlog Item"},
		{"hierarchy against the backlog order", azuredevopstest.ScrumProcess(), &config.AzurePlanMapping{Levels: []config.AzurePlanLevel{
			{Type: "Product Backlog Item", Kinds: []string{"parent"}}, {Type: "Epic", Kinds: []string{"task", "investigation"}},
		}}, func(graph *tracker.PlanGraph) { graph.Items[1].TargetRepository = "" }, tracker.PlanErrorUnsupportedCapability, "backlog levels"},
		{"unregistered target", azuredevopstest.ScrumProcess(), epicPBIMapping(), func(graph *tracker.PlanGraph) { graph.Items[1].TargetRepository = "other" }, tracker.PlanErrorInvalidMapping, "plan_repositories"},
		{"target on the Epic", azuredevopstest.ScrumProcess(), epicPBIMapping(), func(graph *tracker.PlanGraph) { graph.Items[0].TargetRepository = "service" }, tracker.PlanErrorInvalidMapping, "not executable"},
		{"title read as a file", azuredevopstest.ScrumProcess(), epicPBIMapping(), func(graph *tracker.PlanGraph) { graph.Items[1].Title = "@secrets" }, tracker.PlanErrorInvalidMapping, "'@'"},
		{"description beyond one argument", azuredevopstest.ScrumProcess(), epicPBIMapping(), func(graph *tracker.PlanGraph) { graph.Items[1].Body = strings.Repeat("<&>", 20_000) }, tracker.PlanErrorInvalidMapping, "argument"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			boards := azuredevopstest.NewBoards(t, test.process)
			graph := targetedGraph()
			test.mutate(&graph)
			_, err := extensionPlanClient(boards, test.mapping).PreflightPlan(t.Context(), repository, graph)
			if planErrorKind(err) != test.kind || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("PreflightPlan() error = %v, want %s naming %q", err, test.kind, test.reason)
			}
			if !tracker.WriteNotApplied(err) || len(boards.ExtensionWrites()) != 0 || len(boards.Calls()) != 0 {
				t.Fatalf("refusal wrote %d commands or used az rest %d times", len(boards.ExtensionWrites()), len(boards.Calls()))
			}
		})
	}
}

// planErrorKind returns the plan error kind of err, or "" for another error.
func planErrorKind(err error) tracker.PlanErrorKind {
	kind, _ := tracker.PlanErrorKindOf(err)
	return kind
}

// TestAzRestPreflightRefusesRepositoryTargets verifies that the az rest
// route, which has no repository assignment, refuses a plan that names a
// target instead of dropping it silently.
func TestAzRestPreflightRefusesRepositoryTargets(t *testing.T) {
	t.Parallel()

	boards := azuredevopstest.NewBoards(t, azuredevopstest.ScrumProcess())
	_, err := planClient(boards, epicPBIMapping()).PreflightPlan(t.Context(), repository, targetedGraph())
	if planErrorKind(err) != tracker.PlanErrorUnsupportedCapability || !strings.Contains(err.Error(), "devops_extension") {
		t.Fatalf("PreflightPlan() error = %v, want unsupported_capability naming devops_extension", err)
	}
}
