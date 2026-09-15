package factory

import (
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

	captureLimit := &worker.OutputLimitExceededError{
		Operation: "worker command",
		Stream:    "stdout",
		Limit:     worker.MaxCapturedOutputBytes,
	}
	overflow := newCredentialProjectionError("codex", fmt.Errorf("worker projection: %w", captureLimit))
	var gotLimit *worker.OutputLimitExceededError
	if !errors.As(overflow, &gotLimit) {
		t.Fatalf("overflow error = %v, want OutputLimitExceededError", overflow)
	}
	if gotLimit == captureLimit || gotLimit.Operation != captureLimit.Operation || gotLimit.Stream != captureLimit.Stream || gotLimit.Limit != captureLimit.Limit {
		t.Fatalf("preserved cause = %#v, want a safe copy of %#v", gotLimit, captureLimit)
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
	cause := newCredentialProjectionError("codex", &worker.OutputLimitExceededError{
		Operation: "worker command",
		Stream:    "stdout",
		Limit:     worker.MaxCapturedOutputBytes,
	})

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
