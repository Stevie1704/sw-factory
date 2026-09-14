package factory_test

import (
	"testing"

	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestPollLifecycleCancelsAHeadlessRunWhenItsIssueCloses verifies the public
// lifecycle command applies the GitHub terminal projection without any local
// terminal process or notification hook.
func TestPollLifecycleCancelsAHeadlessRunWhenItsIssueCloses(t *testing.T) {
	service, runStore, _, _ := newAgentService(t)
	runStore.github.issueValue.State = "closed"
	result, err := service.PollLifecycle(t.Context(), factory.LifecycleRequest{RunID: runStore.current.ID})
	if err != nil {
		t.Fatalf("PollLifecycle() error = %v", err)
	}
	if result.Outcome != factory.LifecycleCancelled || result.Run.Status != store.StatusCancelled {
		t.Fatalf("PollLifecycle() result = %#v, want cancelled run", result)
	}
	if result.Reason != "issue #6 closed" {
		t.Fatalf("PollLifecycle() reason = %q, want closed-issue projection", result.Reason)
	}
}

// TestReconcileAcceptsAnAlreadyTerminalHeadlessRun verifies the public restart
// entry point is idempotent after terminal workflow projection.
func TestReconcileAcceptsAnAlreadyTerminalHeadlessRun(t *testing.T) {
	service, runStore, _, _ := newAgentService(t)
	run := *runStore.current
	run.Status = store.StatusComplete
	run.Stage = store.StageReady
	if err := runStore.SaveRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	result, err := service.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.Outcome != factory.RecoveryOutcomeReconciled || result.Run == nil || result.Run.Status != store.StatusComplete {
		t.Fatalf("Reconcile() result = %#v, want terminal reconciled run", result)
	}
}
