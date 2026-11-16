package publication

import (
	"testing"

	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// previewWithTarget returns a one-item preview whose PBI targets target.
func previewWithTarget(target *tracker.PlanTargetRepository) tracker.PlanPreview {
	return tracker.PlanPreview{
		Target: "azure-devops:contoso/Factory Pilot",
		Items: []tracker.RenderedPlanItem{{
			NodeID: "task-a", Type: "Product Backlog Item", Title: "Task A", Body: "Body",
			Labels: []string{"factory-plan:s/1/task-a"}, Correlation: "s/1/task-a", TargetRepository: target,
		}},
	}
}

// TestPlanGraphCarriesEachNodesExplicitTarget verifies that the
// provider-neutral graph names exactly the target each stored node selects
// and that a child without a target does not take its parent's target.
func TestPlanGraphCarriesEachNodesExplicitTarget(t *testing.T) {
	t.Parallel()

	graph := planGraph("s", store.PlanRevision{Revision: 1, Nodes: []store.PlanNode{
		{ID: "epic", Kind: "parent", Title: "Epic", TargetRepository: "service"},
		{ID: "a", Kind: "task", Title: "A", ParentID: "epic", TargetRepository: "web"},
		{ID: "b", Kind: "task", Title: "B", ParentID: "epic"},
	}})
	got := []string{graph.Items[0].TargetRepository, graph.Items[1].TargetRepository, graph.Items[2].TargetRepository}
	if got[0] != "service" || got[1] != "web" || got[2] != "" {
		t.Fatalf("graph targets = %q, want service, web, and none", got)
	}
}

// TestPreviewWithoutATargetKeepsTheApprovedPayloadBytes verifies that a
// preview without per-item targets, such as every GitHub, local Markdown,
// and az rest preview, encodes exactly the bytes approvals bound before the
// target existed.
func TestPreviewWithoutATargetKeepsTheApprovedPayloadBytes(t *testing.T) {
	t.Parallel()

	payload, _, err := encodePreview(previewWithTarget(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"target":"azure-devops:contoso/Factory Pilot","items":[{"node_id":"task-a","type":"Product Backlog Item","title":"Task A","body":"Body","fields":{},"labels":["factory-plan:s/1/task-a"],"correlation":"s/1/task-a"}]}`
	if string(payload) != want {
		t.Fatalf("payload = %s\nwant      %s", payload, want)
	}
}

// TestPreviewBindsEachItemsTargetRepository verifies that the approved
// payload carries each item's resolved target repository, decodes it back,
// and changes when the target changes, so a new target needs a new approval.
func TestPreviewBindsEachItemsTargetRepository(t *testing.T) {
	t.Parallel()

	service := &tracker.PlanTargetRepository{Name: "service", ID: "3b1f6a2e-8c4d-4e5f-9a6b-7c8d9e0f1a2b"}
	payload, mapping, err := encodePreview(previewWithTarget(service))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodePreview(payload, mapping)
	if err != nil {
		t.Fatalf("DecodePreview() error = %v", err)
	}
	if got := decoded.Items[0].TargetRepository; got == nil || *got != *service {
		t.Fatalf("decoded target = %+v, want %+v", got, service)
	}
	web := &tracker.PlanTargetRepository{Name: "web", ID: "9d8c7b6a-5f4e-4d3c-8b2a-1f0e9d8c7b6a"}
	other, _, err := encodePreview(previewWithTarget(web))
	if err != nil {
		t.Fatal(err)
	}
	if string(other) == string(payload) {
		t.Fatal("payloads for different target repositories are equal, want a changed approval binding")
	}
}
