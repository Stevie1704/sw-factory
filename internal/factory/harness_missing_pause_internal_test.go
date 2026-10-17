package factory

import (
	"context"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// harnessMissingHarness is a detached harness whose process never started,
// because the worker image has no executable for it.
type harnessMissingHarness struct{ *headlessRecoveryHarness }

// NativeSessionRunning reports the native process gone.
func (harnessMissingHarness) NativeSessionRunning(context.Context, harness.NativeSessionRequest) (bool, error) {
	return false, nil
}

// HeadlessFailureFor reports the typed harness-missing exit.
func (h harnessMissingHarness) HeadlessFailureFor(context.Context, harness.HeadlessInspectionRequest) error {
	return &harness.HeadlessFailure{Cause: harness.NewHarnessMissingError(h.name), ExitCode: 127}
}

// assertWorkerUpgradePause verifies a human pause that names the missing
// harness executable and carries the worker upgrade instruction. The Run keeps
// its frozen image, so the pause refuses resume and points to cancel.
func assertWorkerUpgradePause(t *testing.T, paused store.Run, harnessName string) {
	t.Helper()
	if paused.Status != store.StatusWaitingForHuman || !strings.HasPrefix(paused.LifecycleReason, LifecycleReasonHarnessMissing) || !strings.Contains(paused.LifecycleReason, "("+harnessName+")") {
		t.Fatalf("paused run = status %q reason %q, want a human pause that names the missing harness", paused.Status, paused.LifecycleReason)
	}
	for _, wanted := range []string{"scripts/build-worker.sh", "worker_build.digest"} {
		if !strings.Contains(paused.LifecycleReason, wanted) {
			t.Fatalf("pause reason = %q, want the upgrade instruction %q", paused.LifecycleReason, wanted)
		}
	}
	if reason := resumeAdmissionReason(paused); !strings.Contains(reason, "/factory cancel") {
		t.Fatalf("resumeAdmissionReason() = %q, want resume refused with the cancel action", reason)
	}
	if comment := resumeCommandStatusComment(paused); !strings.Contains(comment, "scripts/build-worker.sh") || !strings.Contains(comment, "/factory cancel") || strings.Contains(comment, "/factory resume") {
		t.Fatalf("status comment = %q, want the upgrade and the cancel command only", comment)
	}
}

// TestLivenessPausesAHarnessMissingFromTheWorkerImageForAnUpgrade verifies a
// detached harness that could not start pauses for a worker upgrade instead
// of an automatic resume on the same image.
func TestLivenessPausesAHarnessMissingFromTheWorkerImageForAnUpgrade(t *testing.T) {
	t.Parallel()

	run := store.Run{ID: "run-harness-missing", Stage: store.StageImplementation, Status: store.StatusActive}
	runStore := &activeInvocationStore{active: store.Invocation{ID: "inv-harness-missing", RunID: run.ID, Role: "implementation", Harness: "copilot", NativeSessionID: "session-missing", Status: store.InvocationStatusActive}}
	var persisted []store.Run
	adapter := harnessMissingHarness{&headlessRecoveryHarness{name: "copilot"}}
	module := newInvocationLifecycle(nil, &repairContractWorker{}, nil, nil, nil, nil, invocationLifecycleHooks{
		persistRun: func(_ context.Context, _ config.RepositoryRegistration, _ RunStore, _ store.Run, next store.Run) error {
			persisted = append(persisted, next)
			return nil
		},
		reconcileInterrupted: func(context.Context, config.RepositoryRegistration, RunStore, store.Run, bool) (store.Run, RecoveryDiagnosis, RecoveryOutcome, error) {
			t.Fatal("a harness missing from the worker image must not be resumed automatically")
			return store.Run{}, RecoveryDiagnosis{}, "", nil
		},
	}, map[config.Harness]harness.HeadlessRuntime{config.HarnessCopilot: adapter})

	if err := module.reconcileActiveHarnessLiveness(t.Context(), config.RepositoryRegistration{}, runStore, run); err != nil {
		t.Fatalf("reconcileActiveHarnessLiveness() error = %v", err)
	}
	if len(persisted) != 1 {
		t.Fatalf("persisted runs = %#v, want one pause", persisted)
	}
	assertWorkerUpgradePause(t, persisted[0], "copilot")
}

// TestLaunchPausesAHarnessMissingFromTheWorkerImageForAnUpgrade verifies the
// launch path: a typed harness-missing failure pauses for a worker upgrade
// and does not record the manual native-resume boundary.
func TestLaunchPausesAHarnessMissingFromTheWorkerImageForAnUpgrade(t *testing.T) {
	t.Parallel()

	run := store.Run{ID: "run-launch-missing", Stage: store.StageImplementation, Status: store.StatusActive}
	var persisted []store.Run
	module := newInvocationLifecycle(nil, &repairContractWorker{}, nil, nil, nil, nil, invocationLifecycleHooks{
		persistRun: func(_ context.Context, _ config.RepositoryRegistration, _ RunStore, _ store.Run, next store.Run) error {
			persisted = append(persisted, next)
			return nil
		},
	}, nil)

	if _, err := module.pauseForHarnessExit(t.Context(), config.RepositoryRegistration{}, &repairContractStore{}, run, "copilot", harness.NewHarnessMissingError("copilot")); err != nil {
		t.Fatalf("pauseForHarnessExit() error = %v", err)
	}
	if len(persisted) != 1 {
		t.Fatalf("persisted runs = %#v, want one pause", persisted)
	}
	assertWorkerUpgradePause(t, persisted[0], "copilot")
}
