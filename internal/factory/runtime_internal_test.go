package factory

import (
	"testing"

	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestEnsureAgentRuntimeUsesTheRegisteredSocketPath verifies lazy construction
// and caching when the service starts without injected visible runtimes.
func TestEnsureAgentRuntimeUsesTheRegisteredSocketPath(t *testing.T) {
	service := NewWithDependencies("", Dependencies{Worker: worker.NewDockerRuntime()})
	firstTerminal, firstHarness := service.ensureAgentRuntime("/tmp/factory-cmux.sock")
	cmuxRuntime, ok := firstTerminal.(*terminal.CmuxRuntime)
	if !ok {
		t.Fatalf("terminal runtime = %T, want *terminal.CmuxRuntime", firstTerminal)
	}
	if got := cmuxRuntime.ConfiguredSocketPath(); got != "/tmp/factory-cmux.sock" {
		t.Fatalf("configured socket path = %q, want registered path", got)
	}
	if _, ok := firstHarness.(*harness.Codex); !ok {
		t.Fatalf("harness runtime = %T, want *harness.Codex", firstHarness)
	}

	secondTerminal, secondHarness := service.ensureAgentRuntime("/tmp/other-cmux.sock")
	if secondTerminal != firstTerminal || secondHarness != firstHarness {
		t.Fatal("ensureAgentRuntime() replaced the cached runtimes")
	}
	if got := cmuxRuntime.ConfiguredSocketPath(); got != "/tmp/factory-cmux.sock" {
		t.Fatalf("cached configured socket path = %q, want original registered path", got)
	}
}
