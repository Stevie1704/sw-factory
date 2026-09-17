package factory

import (
	"context"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestReconcileBeforeResumeDrainsPendingEffectsForExplicitContinuation
// verifies the shared precondition used by both host resume and auth refresh
// --resume paths.
func TestReconcileBeforeResumeDrainsPendingEffectsForExplicitContinuation(t *testing.T) {
	t.Parallel()

	run := store.Run{ID: "run-resume-pending", Status: store.StatusWaitingForHuman}
	storeValue := &resumeReconciliationStore{
		run:     run,
		pending: &store.PendingEffect{RunID: run.ID, ID: "effect-resume-pending", Kind: store.PendingEffectKindStateTransition},
	}
	reconciled := 0
	module := newInvocationLifecycle(nil, nil, nil, nil, nil, nil, invocationLifecycleHooks{
		reconcileInterrupted: func(_ context.Context, _ config.RepositoryRegistration, _ RunStore, current store.Run, _ bool) (store.Run, RecoveryDiagnosis, RecoveryOutcome, error) {
			reconciled++
			current.LifecycleReason = "pending effect reconciled"
			return current, RecoveryDiagnosis{}, RecoveryOutcomeReconciled, nil
		},
	}, nil)

	updated, wasReconciled, err := module.reconcileBeforeResume(context.Background(), InvocationRecoveryRequest{RunStore: storeValue}, run)
	if err != nil {
		t.Fatalf("reconcileBeforeResume() error = %v", err)
	}
	if !wasReconciled || reconciled != 1 || updated.LifecycleReason != "pending effect reconciled" {
		t.Fatalf("reconciliation = %t calls:%d run:%#v, want one updated reconciliation", wasReconciled, reconciled, updated)
	}
}

// resumeReconciliationStore supplies only the durable run and pending-effect
// seams needed to exercise the shared explicit-resume precondition.
type resumeReconciliationStore struct {
	run     store.Run
	pending *store.PendingEffect
}

// CurrentRun returns the fixture run.
func (s *resumeReconciliationStore) CurrentRun(context.Context) (*store.Run, error) {
	copy := s.run
	return &copy, nil
}

// Close leaves the fixture unchanged.
func (*resumeReconciliationStore) Close() error { return nil }

// SaveRun persists the fixture run.
func (s *resumeReconciliationStore) SaveRun(_ context.Context, run store.Run) error {
	s.run = run
	return nil
}

// PendingEffect returns the configured interrupted effect.
func (s *resumeReconciliationStore) PendingEffect(context.Context, string) (*store.PendingEffect, error) {
	return s.pending, nil
}

// SavePendingEffect is unused by this read-side precondition test.
func (*resumeReconciliationStore) SavePendingEffect(context.Context, store.PendingEffect) error {
	return nil
}

// ClearPendingEffect is unused by this read-side precondition test.
func (*resumeReconciliationStore) ClearPendingEffect(context.Context, string, string) error {
	return nil
}

var _ RunStore = (*resumeReconciliationStore)(nil)
var _ PendingEffectStore = (*resumeReconciliationStore)(nil)
