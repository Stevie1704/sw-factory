package factory

import (
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestEnsureAgentRuntimeUsesTheRegisteredSocketPath verifies lazy construction
// and caching when the service starts without injected visible runtimes.
func TestEnsureAgentRuntimeUsesTheRegisteredSocketPath(t *testing.T) {
	service := NewWithDependencies("", Dependencies{Worker: worker.NewDockerRuntime()})
	firstTerminal, firstHarness, err := service.ensureAgentRuntime("/tmp/factory-cmux.sock", config.HarnessCodex)
	if err != nil {
		t.Fatalf("ensureAgentRuntime() error = %v", err)
	}
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

	if _, _, err := service.ensureAgentRuntime("/tmp/other-cmux.sock", config.HarnessCodex); err == nil || !strings.Contains(err.Error(), "conflicts with cached path") {
		t.Fatalf("ensureAgentRuntime() conflict error = %v, want cached-path rejection", err)
	}
	cachedTerminal, cachedHarness, err := service.ensureAgentRuntime("/tmp/factory-cmux.sock", config.HarnessCodex)
	if err != nil {
		t.Fatalf("ensureAgentRuntime() with original path error = %v", err)
	}
	if cachedTerminal != firstTerminal || cachedHarness != firstHarness {
		t.Fatal("ensureAgentRuntime() did not reuse the cached runtimes for the original path")
	}
	if got := cmuxRuntime.ConfiguredSocketPath(); got != "/tmp/factory-cmux.sock" {
		t.Fatalf("cached configured socket path = %q, want original registered path", got)
	}
}

// TestEnsureAgentRuntimeSelectsTheAdapterForEachHarness verifies each declared
// harness resolves to its own adapter and that adapters are cached separately.
func TestEnsureAgentRuntimeSelectsTheAdapterForEachHarness(t *testing.T) {
	service := NewWithDependencies("", Dependencies{Worker: worker.NewDockerRuntime()})
	_, codexRuntime, err := service.ensureAgentRuntime("/tmp/factory-cmux.sock", config.HarnessCodex)
	if err != nil {
		t.Fatalf("ensureAgentRuntime(codex) error = %v", err)
	}
	_, claudeRuntime, err := service.ensureAgentRuntime("/tmp/factory-cmux.sock", config.HarnessClaude)
	if err != nil {
		t.Fatalf("ensureAgentRuntime(claude) error = %v", err)
	}
	if _, ok := claudeRuntime.(*harness.Claude); !ok {
		t.Fatalf("harness runtime = %T, want *harness.Claude", claudeRuntime)
	}
	if codexRuntime.Capabilities().Name != harness.NameCodex || claudeRuntime.Capabilities().Name != harness.NameClaude {
		t.Fatalf("capabilities = %q/%q, want distinct harness identities", codexRuntime.Capabilities().Name, claudeRuntime.Capabilities().Name)
	}
	_, cached, err := service.ensureAgentRuntime("/tmp/factory-cmux.sock", config.HarnessClaude)
	if err != nil {
		t.Fatalf("ensureAgentRuntime(claude) repeat error = %v", err)
	}
	if cached != claudeRuntime {
		t.Fatal("ensureAgentRuntime() did not reuse the cached Claude adapter")
	}
}

// TestEnsureAgentRuntimeRefusesAnUnconfiguredHarness verifies an unknown
// harness never silently falls back to another adapter.
func TestEnsureAgentRuntimeRefusesAnUnconfiguredHarness(t *testing.T) {
	service := NewWithDependencies("", Dependencies{Worker: worker.NewDockerRuntime()})
	if _, _, err := service.ensureAgentRuntime("/tmp/factory-cmux.sock", config.Harness("gemini")); err == nil || !strings.Contains(err.Error(), "gemini") {
		t.Fatalf("ensureAgentRuntime() error = %v, want an unknown-harness refusal", err)
	}
}
