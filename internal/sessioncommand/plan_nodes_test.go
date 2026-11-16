package sessioncommand_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestPlanNodesKeepTheTargetRepositoryThroughTheBrowserShape verifies that a
// draft edit keeps each node's explicit target repository, so an edit never
// silently drops a repository assignment, and that a node without a target
// carries no target key.
func TestPlanNodesKeepTheTargetRepositoryThroughTheBrowserShape(t *testing.T) {
	t.Parallel()

	stored := []store.PlanNode{
		{ID: "a", Kind: "task", Title: "A", TargetRepository: "web"},
		{ID: "b", Kind: "task", Title: "B"},
	}
	browser := sessioncommand.PlanNodesFromStore(stored)
	encoded, err := json.Marshal(browser)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"target_repository":"web"`) || strings.Count(string(encoded), "target_repository") != 1 {
		t.Fatalf("browser nodes = %s, want target_repository only on node a", encoded)
	}
	var decoded []sessioncommand.PlanNodeJSON
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	back := sessioncommand.PlanNodes(decoded)
	if back[0].TargetRepository != "web" || back[1].TargetRepository != "" {
		t.Fatalf("round trip targets = %q, %q; want web and none", back[0].TargetRepository, back[1].TargetRepository)
	}
}
