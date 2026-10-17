package factory

import (
	"context"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// copilotSeedingWorker records Copilot credential projections. The embedded
// runtime is never called by credential seeding.
type copilotSeedingWorker struct {
	worker.WorkerRuntime
	seeds []worker.CredentialSeedRequest
}

// SeedCopilotCredentials records one Copilot credential projection.
func (w *copilotSeedingWorker) SeedCopilotCredentials(_ context.Context, request worker.CredentialSeedRequest) error {
	w.seeds = append(w.seeds, request)
	return nil
}

// TestCopilotCredentialSeedingProjectsTheRegisteredSource verifies a Copilot
// worker receives exactly the registered Copilot token source through the
// shared factory-managed credential store, and that no registered source
// leaves the worker without a projection step.
func TestCopilotCredentialSeedingProjectsTheRegisteredSource(t *testing.T) {
	runtime := &copilotSeedingWorker{}
	registration := config.RepositoryRegistration{
		Path:           "/repositories/example",
		Authentication: config.AuthenticationConfig{CopilotAuthPath: "/host/copilot/token", ClaudeAuthPath: "/host/claude/credentials.json"},
	}
	seed, storeID, err := harnessCredentialSeeding(runtime, registration, config.HarnessCopilot, "")
	if err != nil || seed == nil || storeID != registration.Path {
		t.Fatalf("harnessCredentialSeeding() = step:%t store:%q err:%v, want a step on the registration store", seed != nil, storeID, err)
	}
	if err := seed(context.Background(), "run-copilot", "worker-copilot"); err != nil {
		t.Fatalf("seed() error = %v", err)
	}
	want := worker.CredentialSeedRequest{RunID: "run-copilot", WorkerID: "worker-copilot", AuthPath: "/host/copilot/token"}
	if len(runtime.seeds) != 1 || runtime.seeds[0] != want {
		t.Fatalf("Copilot seeds = %#v, want %#v", runtime.seeds, want)
	}

	failure := newCredentialProjectionError(string(config.HarnessCopilot), context.Canceled)
	if got := failure.Error(); got != "copilot credential projection could not be restored; restore the configured source and retry `factory auth refresh`" {
		t.Fatalf("Copilot projection failure = %q, want the copilot recovery guidance", got)
	}

	registration.Authentication.CopilotAuthPath = ""
	if seed, _, err := harnessCredentialSeeding(runtime, registration, config.HarnessCopilot, ""); err != nil || seed != nil {
		t.Fatalf("harnessCredentialSeeding() without a Copilot source = step:%t err:%v, want no step", seed != nil, err)
	}
}
