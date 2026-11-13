package webui_test

import (
	"os/exec"
	"testing"
)

// TestBrowserAssetsPreserveInputAndSubmitBoundScope executes the shipped assets
// against scripted DOM/network boundaries without a production JS dependency.
func TestBrowserAssetsPreserveInputAndSubmitBoundScope(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable; browser interaction proof remains required in acceptance")
	}
	output, err := exec.CommandContext(t.Context(), node, "testdata/local_interactions.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("browser asset interactions: %v\n%s", err, output)
	}
}
