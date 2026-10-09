package factory_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestDomainReachesTheTrackerOnlyThroughPorts verifies ADR 0018: the
// coordinator, the effect journal, and the gate runner depend on the tracker
// and code-host ports, never on a provider adapter, even transitively. Only
// the composition root in internal/cli selects the GitHub adapter.
func TestDomainReachesTheTrackerOnlyThroughPorts(t *testing.T) {
	const adapter = "github.com/Stevie1704/sw-factory/internal/github"
	output, err := exec.CommandContext(t.Context(), "go", "list", "-deps", "-f", "{{.ImportPath}}", ".", "../effect", "../gate").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	for _, dependency := range strings.Fields(string(output)) {
		if dependency == adapter {
			t.Fatalf("domain packages depend on %s; reach it through internal/tracker and internal/codehost", adapter)
		}
	}
}
