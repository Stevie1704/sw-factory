package azuredevops_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/azuredevops"
	"github.com/Stevie1704/sw-factory/internal/azuredevops/azuredevopstest"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// planContext is the repository commit of every test plan.
const planContext = "0123456789abcdef0123456789abcdef01234567"

// planGraph returns a plan graph of session-1, revision 2, with nodes.
func planGraph(items ...tracker.PlanGraphItem) tracker.PlanGraph {
	graph := tracker.PlanGraph{SessionID: "session-1", PlanRevision: 2, ContextRevision: planContext}
	for _, item := range items {
		item.Correlation = tracker.PlanCorrelation("session-1", 2, item.NodeID)
		graph.Items = append(graph.Items, item)
	}
	return graph
}

// threeNodePlan is a parent with two tasks; n-3 depends on n-2.
func threeNodePlan() tracker.PlanGraph {
	return planGraph(
		tracker.PlanGraphItem{NodeID: "n-1", Kind: "parent", Title: "Faster onboarding", Body: "Parent.", AcceptanceCriteria: []string{"One command."}, BriefRefs: []string{"scope-1"}},
		tracker.PlanGraphItem{NodeID: "n-2", Kind: "task", Title: "Cache images", Body: "Child.", ParentID: "n-1"},
		tracker.PlanGraphItem{NodeID: "n-3", Kind: "task", Title: "Document setup", Body: "Docs.", ParentID: "n-1", DependsOn: []string{"n-2"}},
	)
}

// planClient returns an adapter over the scripted project with mapping; a
// nil mapping selects the Agile default.
func planClient(boards *azuredevopstest.Boards, mapping *config.AzurePlanMapping) *azuredevops.Client {
	return &azuredevops.Client{Runner: boards, Organization: "contoso", Planning: mapping}
}

// TestPreflightPlanRendersTheAgileMappingWithoutWrites verifies that the
// default Agile mapping renders a plan parent as an Epic and tasks as User Stories
// with native parent and predecessor links, tags each item only with its
// namespaced correlation tag, writes nothing, and renders the same preview
// twice.
func TestPreflightPlanRendersTheAgileMappingWithoutWrites(t *testing.T) {
	t.Parallel()

	boards := azuredevopstest.NewBoards(t, azuredevopstest.AgileProcess())
	client := planClient(boards, nil)
	preview, err := client.PreflightPlan(t.Context(), repository, threeNodePlan())
	if err != nil {
		t.Fatalf("PreflightPlan() error = %v", err)
	}
	if preview.Target != "azure-devops:contoso/Factory Pilot" {
		t.Fatalf("Target = %q", preview.Target)
	}
	wantTypes := []string{"Epic", "User Story", "User Story"}
	for index, item := range preview.Items {
		if item.Type != wantTypes[index] {
			t.Fatalf("item %s type = %q, want %q", item.NodeID, item.Type, wantTypes[index])
		}
		if !reflect.DeepEqual(item.Labels, []string{"factory-plan:session-1/2/" + item.NodeID}) {
			t.Fatalf("item %s labels = %#v, want only the correlation tag", item.NodeID, item.Labels)
		}
	}
	wantBody := "Parent.\n\n" +
		"## Acceptance criteria\n\n" +
		"- [ ] One command.\n\n" +
		"## Traceability\n\n" +
		"- Factory plan: session `session-1`, plan revision 2, node `n-1`\n" +
		"- Context revision: `" + planContext + "`\n" +
		"- Brief items: scope-1\n" +
		"- Correlation tag: `factory-plan:session-1/2/n-1`\n"
	if preview.Items[0].Body != wantBody {
		t.Fatalf("Body =\n%s\nwant\n%s", preview.Items[0].Body, wantBody)
	}
	wantRelationships := []tracker.PlanRelationshipMapping{
		{Kind: tracker.PlanRelationshipParent, FromNodeID: "n-2", ToNodeID: "n-1", Mode: tracker.PlanRelationshipNative},
		{Kind: tracker.PlanRelationshipParent, FromNodeID: "n-3", ToNodeID: "n-1", Mode: tracker.PlanRelationshipNative},
		{Kind: tracker.PlanRelationshipDependsOn, FromNodeID: "n-3", ToNodeID: "n-2", Mode: tracker.PlanRelationshipNative},
	}
	if !reflect.DeepEqual(preview.Relationships, wantRelationships) {
		t.Fatalf("Relationships = %#v, want %#v", preview.Relationships, wantRelationships)
	}
	if writes := boards.Writes(); len(writes) != 0 {
		t.Fatalf("PreflightPlan() wrote %#v; preflight is read-only", writes)
	}
	again, err := client.PreflightPlan(t.Context(), repository, threeNodePlan())
	if err != nil || !reflect.DeepEqual(again, preview) {
		t.Fatalf("second PreflightPlan() = %#v, %v; want the same preview", again, err)
	}
}

// pbiMapping is a configured Scrum mapping with a required priority value.
func pbiMapping() *config.AzurePlanMapping {
	return &config.AzurePlanMapping{Levels: []config.AzurePlanLevel{
		{Type: "Epic", Kinds: []string{"parent"}},
		{Type: "Feature"},
		{Type: "Product Backlog Item", Kinds: []string{"task", "investigation"}, Fields: map[string]string{"Microsoft.VSTS.Common.Priority": "1"}},
	}}
}

// TestPreflightPlanRendersAConfiguredBacklogItemMapping verifies that a
// configured mapping renders tasks as Product Backlog Items with their
// configured field values on a Scrum project.
func TestPreflightPlanRendersAConfiguredBacklogItemMapping(t *testing.T) {
	t.Parallel()

	boards := azuredevopstest.NewBoards(t, azuredevopstest.ScrumProcess())
	preview, err := planClient(boards, pbiMapping()).PreflightPlan(t.Context(), repository, threeNodePlan())
	if err != nil {
		t.Fatalf("PreflightPlan() error = %v", err)
	}
	if preview.Items[0].Type != "Epic" || preview.Items[0].Fields != nil {
		t.Fatalf("parent item = %#v, want an Epic without fields", preview.Items[0])
	}
	for _, item := range preview.Items[1:] {
		if item.Type != "Product Backlog Item" || !reflect.DeepEqual(item.Fields, map[string]string{"Microsoft.VSTS.Common.Priority": "1"}) {
			t.Fatalf("task item = %#v, want a Product Backlog Item with priority 1", item)
		}
	}
}

// TestPreflightPlanShowsRequiredIntermediateItems verifies that a parent link
// that skips a required level gets one intermediate item per parent in the
// preview, with its complete content, and that the children link to it.
func TestPreflightPlanShowsRequiredIntermediateItems(t *testing.T) {
	t.Parallel()

	mapping := config.AgileAzurePlanMapping()
	mapping.Levels[1].Required = true
	boards := azuredevopstest.NewBoards(t, azuredevopstest.AgileProcess())
	preview, err := planClient(boards, &mapping).PreflightPlan(t.Context(), repository, threeNodePlan())
	if err != nil {
		t.Fatalf("PreflightPlan() error = %v", err)
	}
	nodes := []string{}
	for _, item := range preview.Items {
		nodes = append(nodes, item.NodeID+":"+item.Type)
	}
	wantNodes := []string{"n-1:Epic", "n-1--feature:Feature", "n-2:User Story", "n-3:User Story"}
	if !reflect.DeepEqual(nodes, wantNodes) {
		t.Fatalf("items = %v, want %v", nodes, wantNodes)
	}
	intermediate := preview.Items[1]
	wantBody := "Required intermediate Feature: azure_devops.planning requires the Feature level between the Epic \"Faster onboarding\" and these plan items:\n\n" +
		"- Cache images (node `n-2`)\n" +
		"- Document setup (node `n-3`)\n\n" +
		"## Traceability\n\n" +
		"- Factory plan: session `session-1`, plan revision 2, node `n-1--feature`\n" +
		"- Intermediate item below node `n-1`\n" +
		"- Context revision: `" + planContext + "`\n" +
		"- Correlation tag: `factory-plan:session-1/2/n-1--feature`\n"
	if intermediate.Title != "Faster onboarding" || intermediate.Body != wantBody || intermediate.Correlation != "session-1/2/n-1--feature" {
		t.Fatalf("intermediate = %#v\nwant body\n%s", intermediate, wantBody)
	}
	wantRelationships := []tracker.PlanRelationshipMapping{
		{Kind: tracker.PlanRelationshipParent, FromNodeID: "n-1--feature", ToNodeID: "n-1", Mode: tracker.PlanRelationshipNative},
		{Kind: tracker.PlanRelationshipParent, FromNodeID: "n-2", ToNodeID: "n-1--feature", Mode: tracker.PlanRelationshipNative},
		{Kind: tracker.PlanRelationshipParent, FromNodeID: "n-3", ToNodeID: "n-1--feature", Mode: tracker.PlanRelationshipNative},
		{Kind: tracker.PlanRelationshipDependsOn, FromNodeID: "n-3", ToNodeID: "n-2", Mode: tracker.PlanRelationshipNative},
	}
	if !reflect.DeepEqual(preview.Relationships, wantRelationships) {
		t.Fatalf("Relationships = %#v, want %#v", preview.Relationships, wantRelationships)
	}
}

// TestPreflightPlanRejectsMappingsTheProjectCannotTake verifies that an
// unsupported hierarchy, an unknown type or kind, an invalid or missing
// field, a started state, and a missing permission fail with the typed
// no-change error before approval and before any write.
func TestPreflightPlanRejectsMappingsTheProjectCannotTake(t *testing.T) {
	t.Parallel()

	levels := func(change func([]config.AzurePlanLevel)) *config.AzurePlanMapping {
		mapping := config.AgileAzurePlanMapping()
		change(mapping.Levels)
		return &mapping
	}
	requireCustomField := func(process []azuredevopstest.WorkItemType) {
		process[2].Fields = append(process[2].Fields, azuredevopstest.Field{ReferenceName: "Custom.Team", Name: "Team", AlwaysRequired: true})
	}
	tests := []struct {
		name    string
		mapping *config.AzurePlanMapping
		process func([]azuredevopstest.WorkItemType)
		setup   func(*azuredevopstest.Boards)
		want    tracker.PlanErrorKind
		detail  string
	}{
		{name: "child above its parent", detail: "at or above", want: tracker.PlanErrorUnsupportedCapability, mapping: &config.AzurePlanMapping{Levels: []config.AzurePlanLevel{
			{Type: "Epic", Kinds: []string{"task", "investigation"}}, {Type: "User Story", Kinds: []string{"parent"}}}}},
		{name: "hierarchy against the backlog order", detail: "backlog levels of project", want: tracker.PlanErrorUnsupportedCapability, mapping: &config.AzurePlanMapping{Levels: []config.AzurePlanLevel{
			{Type: "User Story", Kinds: []string{"parent"}}, {Type: "Epic", Kinds: []string{"task", "investigation"}}}}},
		{name: "type on no backlog", detail: "on no backlog level", want: tracker.PlanErrorUnsupportedCapability, mapping: levels(func(l []config.AzurePlanLevel) { l[2].Type = "Issue" })},
		{name: "unmapped kind", detail: "maps to no work-item type", want: tracker.PlanErrorInvalidMapping, mapping: levels(func(l []config.AzurePlanLevel) { l[2].Kinds = []string{"investigation"} })},
		{name: "default mapping on a Scrum project", detail: `"User Story" does not exist`, want: tracker.PlanErrorInvalidMapping, process: func(p []azuredevopstest.WorkItemType) { copy(p, azuredevopstest.ScrumProcess()) }},
		{name: "disabled type", detail: `"Feature" does not exist or is disabled`, want: tracker.PlanErrorInvalidMapping, process: func(p []azuredevopstest.WorkItemType) { p[1].Disabled = true }},
		{name: "missing required field", detail: `requires field "Custom.Team"`, want: tracker.PlanErrorInvalidMapping, process: requireCustomField},
		{name: "unknown field", detail: `has no field "Custom.Missing"`, want: tracker.PlanErrorInvalidMapping, mapping: levels(func(l []config.AzurePlanLevel) { l[2].Fields = map[string]string{"Custom.Missing": "x"} })},
		{name: "value not allowed", detail: `does not allow the value "9"`, want: tracker.PlanErrorInvalidMapping, mapping: levels(func(l []config.AzurePlanLevel) {
			l[2].Fields = map[string]string{"Microsoft.VSTS.Common.Priority": "9"}
		})},
		{name: "started state", detail: "InProgress category", want: tracker.PlanErrorInvalidMapping, mapping: levels(func(l []config.AzurePlanLevel) { l[2].Fields = map[string]string{"System.State": "Active"} })},
		{name: "unknown state", detail: `has no state "Doing"`, want: tracker.PlanErrorInvalidMapping, mapping: levels(func(l []config.AzurePlanLevel) { l[0].Fields = map[string]string{"System.State": "Doing"} })},
		{name: "no work item edit permission", detail: "view and edit work items", want: tracker.PlanErrorPermission, setup: func(b *azuredevopstest.Boards) { b.Deny("83e28ad4-2d72-4ceb-97b0-c7726d5502c3", 48) }},
		{name: "no tag create permission", detail: "create tag definitions", want: tracker.PlanErrorPermission, setup: func(b *azuredevopstest.Boards) { b.Deny("bb50f182-8e5e-40b8-bc21-e8752a1e7ae2", 2) }},
		{name: "forbidden process read", detail: "TF400813", want: tracker.PlanErrorPermission, setup: func(b *azuredevopstest.Boards) {
			b.FailOnce("GET "+projectPath+"/wit/workitemtypes", `az rest: Forbidden({"message":"TF400813: not authorized"})`, false)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			process := azuredevopstest.AgileProcess()
			if tc.process != nil {
				tc.process(process)
			}
			boards := azuredevopstest.NewBoards(t, process)
			if tc.setup != nil {
				tc.setup(boards)
			}
			_, err := planClient(boards, tc.mapping).PreflightPlan(t.Context(), repository, threeNodePlan())
			if kind, ok := tracker.PlanErrorKindOf(err); !ok || kind != tc.want || !tracker.WriteNotApplied(err) || !strings.Contains(err.Error(), tc.detail) {
				t.Fatalf("PreflightPlan() error = %v, want a %s planning error about %q", err, tc.want, tc.detail)
			}
			if writes := boards.Writes(); len(writes) != 0 {
				t.Fatalf("PreflightPlan() wrote %#v", writes)
			}
		})
	}
}
