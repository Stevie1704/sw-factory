package factory_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestStartAgentPreservesCaptureLimitCauseThroughCredentialProjection verifies
// the launch coordinator boundary does not discard a worker capture-limit
// failure while rolling back the incomplete invocation.
func TestStartAgentPreservesCaptureLimitCauseThroughCredentialProjection(t *testing.T) {
	t.Parallel()

	_, runStore, runtime, _ := newAgentService(t)
	headlessWorker := &headlessAgentWorker{agentWorker: runtime}
	policy := validRepositoryConfig()
	authPath := filepath.Join(t.TempDir(), "codex-auth.json")
	service := newDispatchingAgentService(t, runStore, headlessWorker, policy, config.AuthenticationConfig{CodexAuthPath: authPath})
	runtime.seedErr = testCaptureLimitError()

	_, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	assertCaptureLimitCause(t, err, authPath)
}

// TestRefreshAuthPreservesCaptureLimitCauseThroughCredentialProjection
// verifies the explicit authentication-refresh coordinator boundary keeps a
// worker overflow typed even though the operation itself is not auth expiry.
func TestRefreshAuthPreservesCaptureLimitCauseThroughCredentialProjection(t *testing.T) {
	t.Parallel()

	_, runStore, runtime, _ := newAgentService(t)
	headlessWorker := &headlessAgentWorker{agentWorker: runtime}
	policy := validRepositoryConfig()
	authPath := filepath.Join(t.TempDir(), "codex-auth.json")
	service := newDispatchingAgentService(t, runStore, headlessWorker, policy, config.AuthenticationConfig{CodexAuthPath: authPath})
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() setup error = %v", err)
	}
	runtime.seedErr = testCaptureLimitError()

	_, err = service.RefreshAuth(context.Background(), factory.AuthRefreshRequest{RunID: launch.Invocation.RunID})
	assertCaptureLimitCause(t, err, authPath)
}

// assertCaptureLimitCause checks the safe coordinator-visible representation of
// an overflowing credential projection.
func assertCaptureLimitCause(t *testing.T, err error, forbidden ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("credential projection error = nil, want capture-limit failure")
	}
	var captureLimit *worker.OutputLimitExceededError
	if !errors.As(err, &captureLimit) {
		t.Fatalf("credential projection error = %v, want OutputLimitExceededError", err)
	}
	message := err.Error()
	if !strings.Contains(message, "capture limit") || strings.Contains(message, "credential projection could not be restored") {
		t.Fatalf("credential projection message = %q, want capture-limit wording without the legacy verdict", message)
	}
	for _, value := range forbidden {
		if strings.Contains(message, value) {
			t.Fatalf("credential projection message = %q, leaked forbidden value %q", message, value)
		}
	}
}

// testCaptureLimitError returns the safe worker failure used by coordinator
// seam tests without involving Docker.
func testCaptureLimitError() error {
	return &worker.OutputLimitExceededError{
		Operation: "worker command",
		Stream:    "stdout",
		Limit:     worker.MaxCapturedOutputBytes,
	}
}
