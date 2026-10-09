package factory_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestDomainReachesTheTrackerOnlyThroughPorts verifies ADR 0018: the
// coordinator, the effect journal, the gate runner, and the local web UI
// depend on the tracker and code-host ports, never on a provider adapter,
// even transitively. Only the composition root in internal/cli selects the
// GitHub or Azure DevOps adapter.
func TestDomainReachesTheTrackerOnlyThroughPorts(t *testing.T) {
	adapters := map[string]bool{
		"github.com/Stevie1704/sw-factory/internal/github":      true,
		"github.com/Stevie1704/sw-factory/internal/azuredevops": true,
	}
	output, err := exec.CommandContext(t.Context(), "go", "list", "-deps", "-f", "{{.ImportPath}}", ".", "../effect", "../gate", "../webui").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	for _, dependency := range strings.Fields(string(output)) {
		if adapters[dependency] {
			t.Fatalf("domain packages depend on %s; reach it through internal/tracker and internal/codehost", dependency)
		}
	}
}
