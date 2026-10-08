package factory

import (
	"context"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// outOfMemoryHarness is a detached harness whose process exited after the
// worker memory limit killed it.
type outOfMemoryHarness struct{ *headlessRecoveryHarness }

// NativeSessionRunning reports the native process gone.
func (outOfMemoryHarness) NativeSessionRunning(context.Context, harness.NativeSessionRequest) (bool, error) {
	return false, nil
}

// HeadlessFailureFor reports the typed out-of-memory exit.
func (h outOfMemoryHarness) HeadlessFailureFor(context.Context, harness.HeadlessInspectionRequest) error {
	return &harness.HeadlessFailure{Cause: harness.NewOutOfMemoryError(h.name), ExitCode: 137}
}

// activeInvocationStore returns one active invocation for liveness checks.
type activeInvocationStore struct {
	repairContractStore
	active store.Invocation
}

// ActiveInvocation returns the configured active invocation.
func (s *activeInvocationStore) ActiveInvocation(context.Context, string) (*store.Invocation, error) {
	active := s.active
	return &active, nil
}

// TestLivenessPausesAnOutOfMemoryHarnessWithItsCause verifies a detached
// harness killed at the worker memory limit pauses for a person with the
// cause named, instead of an automatic resume into the same limit, and that
// the pause admits the resume command.
func TestLivenessPausesAnOutOfMemoryHarnessWithItsCause(t *testing.T) {
	t.Parallel()

	run := store.Run{ID: "run-harness-oom", Stage: store.StageImplementation, Status: store.StatusActive}
	runStore := &activeInvocationStore{active: store.Invocation{ID: "inv-harness-oom", RunID: run.ID, Role: "implementation", Harness: "codex", NativeSessionID: "session-oom", Status: store.InvocationStatusActive}}
	var persisted []store.Run
	adapter := outOfMemoryHarness{&headlessRecoveryHarness{name: "codex"}}
	module := newInvocationLifecycle(nil, &repairContractWorker{}, nil, nil, nil, nil, invocationLifecycleHooks{
		persistRun: func(_ context.Context, _ config.RepositoryRegistration, _ RunStore, _ store.Run, next store.Run) error {
			persisted = append(persisted, next)
			return nil
		},
		reconcileInterrupted: func(context.Context, config.RepositoryRegistration, RunStore, store.Run, bool) (store.Run, RecoveryDiagnosis, RecoveryOutcome, error) {
			t.Fatal("an out-of-memory exit must not be resumed automatically")
			return store.Run{}, RecoveryDiagnosis{}, "", nil
		},
	}, map[config.Harness]harness.HeadlessRuntime{config.HarnessCodex: adapter})

	if err := module.reconcileActiveHarnessLiveness(t.Context(), config.RepositoryRegistration{}, runStore, run); err != nil {
		t.Fatalf("reconcileActiveHarnessLiveness() error = %v", err)
	}

	if len(persisted) != 1 {
		t.Fatalf("persisted runs = %#v, want one pause", persisted)
	}
	paused := persisted[0]
	if paused.Status != store.StatusWaitingForHuman || !strings.HasPrefix(paused.LifecycleReason, LifecycleReasonHarnessOutOfMemory) || !strings.Contains(paused.LifecycleReason, "(codex)") || !strings.Contains(paused.LifecycleReason, "worker_limits.memory") {
		t.Fatalf("paused run = status %q reason %q, want a human pause that names the out-of-memory cause", paused.Status, paused.LifecycleReason)
	}
	if reason := resumeAdmissionReason(paused); reason != "" {
		t.Fatalf("resumeAdmissionReason() = %q, want the resume command admitted", reason)
	}
	if comment := resumeCommandStatusComment(paused); !strings.Contains(comment, "/factory resume") {
		t.Fatalf("status comment = %q, want the resume command", comment)
	}
}
