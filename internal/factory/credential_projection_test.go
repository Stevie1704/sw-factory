package factory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestCredentialProjectionErrorPreservesSafeCaptureLimitCause verifies the
// coordinator keeps the typed capture-limit cause while retaining the legacy
// authentication classification for every other projection failure.
func TestCredentialProjectionErrorPreservesSafeCaptureLimitCause(t *testing.T) {
	t.Parallel()

	captureLimit := testCredentialProjectionCaptureLimitError()
	overflow := newCredentialProjectionError("codex", fmt.Errorf("worker projection: %w", captureLimit))
	var expectedLimit *worker.OutputLimitExceededError
	if !errors.As(captureLimit, &expectedLimit) {
		t.Fatalf("test capture-limit error = %v, want OutputLimitExceededError", captureLimit)
	}
	var gotLimit *worker.OutputLimitExceededError
	if !errors.As(overflow, &gotLimit) {
		t.Fatalf("overflow error = %v, want OutputLimitExceededError", overflow)
	}
	if gotLimit == expectedLimit || gotLimit.Operation != expectedLimit.Operation || gotLimit.Stream != expectedLimit.Stream || gotLimit.Limit != expectedLimit.Limit {
		t.Fatalf("preserved cause = %#v, want a safe copy of %#v", gotLimit, expectedLimit)
	}
	if harness.IsAuthenticationExpired(overflow) {
		t.Fatalf("overflow error = %v, must not classify as authentication expired", overflow)
	}
	if !strings.Contains(overflow.Error(), "capture limit") || strings.Contains(overflow.Error(), "credential projection could not be restored") {
		t.Fatalf("overflow message = %q, want capture-limit guidance without the legacy verdict", overflow.Error())
	}

	ordinary := newCredentialProjectionError("codex", errors.New("host path and adapter output must stay private"))
	if errors.As(ordinary, &gotLimit) {
		t.Fatalf("ordinary error = %v, must not expose an arbitrary typed cause", ordinary)
	}
	if !harness.IsAuthenticationExpired(ordinary) {
		t.Fatalf("ordinary error = %v, want legacy authentication-expired classification", ordinary)
	}
	const wantOrdinary = "codex credential projection could not be restored; restore the configured source and retry `factory auth refresh`"
	if ordinary.Error() != wantOrdinary {
		t.Fatalf("ordinary message = %q, want %q", ordinary.Error(), wantOrdinary)
	}
	if strings.Contains(ordinary.Error(), "host path") || strings.Contains(ordinary.Error(), "adapter output") {
		t.Fatalf("ordinary message leaked the discarded cause: %q", ordinary.Error())
	}
}

// TestCaptureLimitCredentialProjectionWaitsForHumanRecovery verifies the
// recorded recovery decision does not send a deterministic capture-limit
// failure through the authentication-refresh state.
func TestCaptureLimitCredentialProjectionWaitsForHumanRecovery(t *testing.T) {
	t.Parallel()

	run := store.Run{
		ID:                  "run-capture-limit",
		Stage:               store.StageImplementation,
		Status:              store.StatusActive,
		ActiveInvocationIDs: []string{"inv-capture-limit"},
	}
	runStore := &repairContractStore{}
	workerRuntime := &repairContractWorker{}
	service := &Service{deps: Dependencies{Worker: workerRuntime}}
	diagnosis := newRecoveryDiagnosis(run.ID)
	cause := newCredentialProjectionError("codex", testCredentialProjectionCaptureLimitError())

	paused, _, outcome, err := service.pauseForCredentialProjection(t.Context(), config.RepositoryRegistration{}, runStore, run, &diagnosis, "codex", cause)
	if err == nil {
		t.Fatal("pauseForCredentialProjection() error = nil, want the typed capture-limit cause")
	}
	if !errors.As(err, new(*worker.OutputLimitExceededError)) {
		t.Fatalf("pauseForCredentialProjection() error = %v, want OutputLimitExceededError", err)
	}
	if harness.IsAuthenticationExpired(err) {
		t.Fatalf("pauseForCredentialProjection() error = %v, must not classify as authentication expired", err)
	}
	if outcome != RecoveryOutcomeWaitingForHuman || paused.Status != store.StatusWaitingForHuman {
		t.Fatalf("paused recovery = %#v/%q, want human-waiting state", paused, outcome)
	}
	if !strings.Contains(paused.LifecycleReason, "capture limit") || strings.Contains(paused.LifecycleReason, "authentication expired") {
		t.Fatalf("lifecycle reason = %q, want capture-limit guidance without auth-refresh guidance", paused.LifecycleReason)
	}
	if workerRuntime.stops != 1 || len(runStore.saved) != 1 {
		t.Fatalf("recovery effects = stops:%d saves:%d, want one worker stop and one state save", workerRuntime.stops, len(runStore.saved))
	}
	if len(diagnosis.Discrepancies) != 1 || diagnosis.Discrepancies[0].Observed != "worker capture limit exceeded" {
		t.Fatalf("recovery diagnosis = %#v, want capture-limit observation", diagnosis.Discrepancies)
	}
}

// TestManualResumeCaptureLimitWaitsForHuman verifies an explicit native
// resume preserves the dedicated capture-limit recovery state as well.
func TestManualResumeCaptureLimitWaitsForHuman(t *testing.T) {
	t.Parallel()

	run := store.Run{
		ID:                  "run-manual-capture-limit",
		Stage:               store.StageImplementation,
		Status:              store.StatusActive,
		ActiveInvocationIDs: []string{"inv-manual-capture-limit"},
	}
	runStore := &retryCredentialProjectionStore{}
	workerRuntime := &repairContractWorker{}
	module := newInvocationLifecycle(nil, workerRuntime, nil, nil, nil, nil, invocationLifecycleHooks{
		persistRun: func(_ context.Context, _ config.RepositoryRegistration, target RunStore, _, next store.Run) error {
			return target.SaveRun(context.Background(), next)
		},
	}, nil)
	active := store.Invocation{ID: "inv-manual-capture-limit", RunID: run.ID, Harness: "codex"}
	result, err := module.resumeActiveInvocationError(t.Context(), InvocationRecoveryRequest{
		Registration: config.RepositoryRegistration{}, RunStore: runStore,
	}, run, active, active, newCredentialProjectionError(active.Harness, testCredentialProjectionCaptureLimitError()))
	if err == nil || !errors.As(err, new(*worker.OutputLimitExceededError)) {
		t.Fatalf("resumeActiveInvocationError() error = %v, want capture-limit cause", err)
	}
	if harness.IsAuthenticationExpired(err) {
		t.Fatalf("resumeActiveInvocationError() error = %v, must not classify as authentication expired", err)
	}
	if result.Run.Status != store.StatusWaitingForHuman || !strings.Contains(result.Run.LifecycleReason, "capture limit") {
		t.Fatalf("manual resume result = %#v, want capture-limit human state", result.Run)
	}
	if workerRuntime.stops != 1 || len(runStore.saved) != 1 {
		t.Fatalf("manual resume effects = stops:%d saves:%d, want one worker stop and one state save", workerRuntime.stops, len(runStore.saved))
	}
}

// TestOrdinaryCredentialProjectionKeepsAuthenticationRecovery verifies a
// non-capture-limit projection failure retains the established auth-refresh
// waiting message and classification.
func TestOrdinaryCredentialProjectionKeepsAuthenticationRecovery(t *testing.T) {
	t.Parallel()

	run := store.Run{ID: "run-ordinary-credential", Stage: store.StageImplementation, Status: store.StatusActive}
	runStore := &repairContractStore{}
	service := &Service{deps: Dependencies{Worker: &repairContractWorker{}}}
	diagnosis := newRecoveryDiagnosis(run.ID)
	privateCause := errors.New("private adapter output and host path")
	cause := newCredentialProjectionError("codex", privateCause)

	paused, _, outcome, err := service.pauseForCredentialProjection(t.Context(), config.RepositoryRegistration{}, runStore, run, &diagnosis, "codex", cause)
	if err == nil || !harness.IsAuthenticationExpired(err) {
		t.Fatalf("pauseForCredentialProjection() error = %v, want authentication-expired classification", err)
	}
	if strings.Contains(err.Error(), privateCause.Error()) {
		t.Fatalf("pauseForCredentialProjection() error leaked private cause: %v", err)
	}
	if outcome != RecoveryOutcomeWaitingForHuman || paused.Status != store.StatusWaitingForHuman {
		t.Fatalf("paused recovery = %#v/%q, want human-waiting auth recovery", paused, outcome)
	}
	wantReason := "harness authentication expired (codex); run is waiting for `factory auth refresh`"
	if paused.LifecycleReason != wantReason {
		t.Fatalf("lifecycle reason = %q, want %q", paused.LifecycleReason, wantReason)
	}
}

// TestRetryWaitingForHarnessRoutesCaptureLimitToHuman verifies a projection
// failure returned by a fresh harness retry cannot leave the run waiting for
// capacity after the worker reports a deterministic capture overflow.
func TestRetryWaitingForHarnessRoutesCaptureLimitToHuman(t *testing.T) {
	t.Parallel()

	run := store.Run{ID: "run-retry-capture-limit", Status: store.StatusWaitingForHarness}
	runStore := &retryCredentialProjectionStore{}
	workerRuntime := &repairContractWorker{}
	module := newInvocationLifecycle(nil, workerRuntime, nil, nil, nil, nil, invocationLifecycleHooks{
		persistRun: func(_ context.Context, _ config.RepositoryRegistration, target RunStore, previous, next store.Run) error {
			if err := target.SaveRun(context.Background(), next); err != nil {
				return err
			}
			if previous.ID != run.ID {
				t.Fatalf("persisted previous run = %#v, want %q", previous, run.ID)
			}
			return nil
		},
	}, nil)

	err := module.retryWaitingForHarness(t.Context(), config.RepositoryRegistration{}, runStore, run, func(store.Run) (AgentLaunchResult, error) {
		return AgentLaunchResult{}, newCredentialProjectionError("codex", testCredentialProjectionCaptureLimitError())
	})
	if err != nil {
		t.Fatalf("retryWaitingForHarness() error = %v, want capture-limit pause", err)
	}
	if workerRuntime.stops != 1 || len(runStore.saved) != 1 {
		t.Fatalf("retry effects = stops:%d saves:%d, want one worker stop and one state save", workerRuntime.stops, len(runStore.saved))
	}
	paused := runStore.saved[0]
	if paused.Status != store.StatusWaitingForHuman || !strings.Contains(paused.LifecycleReason, "capture limit") {
		t.Fatalf("paused run = %#v, want capture-limit human state", paused)
	}
}

// TestAutomaticHarnessRetryResumeRoutesCaptureLimitToHuman verifies a native
// resume failure uses the same capture-limit state and stops its worker.
func TestAutomaticHarnessRetryResumeRoutesCaptureLimitToHuman(t *testing.T) {
	t.Parallel()

	run := store.Run{ID: "run-native-retry-capture-limit", Status: store.StatusWaitingForHarness}
	runStore := &retryCredentialProjectionStore{}
	workerRuntime := &repairContractWorker{}
	module := newInvocationLifecycle(nil, workerRuntime, nil, nil, nil, nil, invocationLifecycleHooks{
		persistRun: func(_ context.Context, _ config.RepositoryRegistration, target RunStore, _, next store.Run) error {
			return target.SaveRun(context.Background(), next)
		},
	}, nil)
	active := store.Invocation{ID: "inv-native-retry-capture-limit", RunID: run.ID, Harness: "codex"}

	err := module.handleRetryResumeError(t.Context(), config.RepositoryRegistration{}, runStore, run, active, newCredentialProjectionError(active.Harness, testCredentialProjectionCaptureLimitError()))
	if err != nil {
		t.Fatalf("handleRetryResumeError() error = %v, want capture-limit pause", err)
	}
	if workerRuntime.stops != 1 || len(runStore.saved) != 1 {
		t.Fatalf("native retry effects = stops:%d saves:%d, want one worker stop and one state save", workerRuntime.stops, len(runStore.saved))
	}
	paused := runStore.saved[0]
	if paused.Status != store.StatusWaitingForHuman || !strings.Contains(paused.LifecycleReason, "capture limit") {
		t.Fatalf("paused native retry = %#v, want capture-limit human state", paused)
	}
}

// TestRecoveryErrorWithCausePreservesCaptureLimit verifies the generic
// infrastructure wrapper does not discard the safe capture-limit cause.
func TestRecoveryErrorWithCausePreservesCaptureLimit(t *testing.T) {
	t.Parallel()

	cause := newCredentialProjectionError("codex", testCredentialProjectionCaptureLimitError())
	err := recoveryErrorWithCause(newRecoveryDiagnosis("run-capture-limit-cause"), cause)
	var captureLimit *worker.OutputLimitExceededError
	if !errors.As(err, &captureLimit) {
		t.Fatalf("recoveryErrorWithCause() error = %v, want OutputLimitExceededError", err)
	}
}

// retryCredentialProjectionStore exposes no active invocation for a fresh
// retry while retaining the small direct-routing store projection.
type retryCredentialProjectionStore struct{ repairContractStore }

// ActiveInvocation reports that a fresh retry has no persisted native session.
func (*retryCredentialProjectionStore) ActiveInvocation(context.Context, string) (*store.Invocation, error) {
	return nil, nil
}

// testCredentialProjectionCaptureLimitError returns the bounded worker
// failure used by credential projection recovery tests.
func testCredentialProjectionCaptureLimitError() error {
	return &worker.OutputLimitExceededError{
		Operation: "worker command",
		Stream:    "stdout",
		Limit:     worker.MaxCapturedOutputBytes,
	}
}
