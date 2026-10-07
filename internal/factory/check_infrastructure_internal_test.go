package factory

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestResumeRetriesChecksForACheckInfrastructurePause verifies an explicit
// resume of a check-infrastructure pause re-enters gate evaluation through the
// recovered-check continuation and never launches an agent from check.
func TestResumeRetriesChecksForACheckInfrastructurePause(t *testing.T) {
	t.Parallel()

	for _, run := range []store.Run{
		{ID: "run-check-pause", Stage: store.StageCheck, Status: store.StatusWaitingForHuman, LifecycleReason: LifecycleReasonCheckInfrastructureUnavailable + ": setup execution failed: worker unavailable"},
		{ID: "run-legacy-wait", Stage: store.StageCheck, Status: store.StatusWaitingForHarness, LifecycleReason: "check repair waiting for infrastructure: setup command failed with exit code 1"},
	} {
		t.Run(run.ID, func(t *testing.T) {
			var continued []store.Run
			module := newInvocationLifecycle(nil, &repairContractWorker{}, nil, nil, nil, nil, invocationLifecycleHooks{
				resumeRecoveredCheck: func(_ context.Context, _ config.RepositoryRegistration, _ RunStore, target store.Run) (store.Run, error) {
					continued = append(continued, target)
					target.Status = store.StatusActive
					return target, nil
				},
			}, nil)

			result, err := module.Resume(t.Context(), InvocationRecoveryRequest{
				RunStore:        &retryCredentialProjectionStore{},
				Run:             &run,
				NewInvocationID: func() (string, error) { t.Fatal("check retry must not create an invocation"); return "", nil },
			})

			if err != nil {
				t.Fatalf("Resume() error = %v", err)
			}
			if len(continued) != 1 || result.Run.Stage != store.StageCheck || result.Run.Status != store.StatusActive || result.Invocation.ID != "" {
				t.Fatalf("Resume() = %#v, continued %d times, want one check continuation and no invocation", result, len(continued))
			}
		})
	}
}

// TestRetryWaitingForHarnessRecoversALegacyCheckWait verifies the polling
// retry recognizes a persisted check infrastructure wait and retries checks
// instead of launching an agent that the check stage cannot host.
func TestRetryWaitingForHarnessRecoversALegacyCheckWait(t *testing.T) {
	t.Parallel()

	run := store.Run{ID: "run-legacy-retry", Stage: store.StageCheck, Status: store.StatusWaitingForHarness, LifecycleReason: "check repair harness unavailable", CheckRepairAttempts: 1, CheckRepairBudget: 3}
	continued := 0
	module := newInvocationLifecycle(nil, &repairContractWorker{}, nil, nil, nil, nil, invocationLifecycleHooks{
		resumeRecoveredCheck: func(_ context.Context, _ config.RepositoryRegistration, _ RunStore, target store.Run) (store.Run, error) {
			continued++
			if target.ID != run.ID {
				t.Fatalf("continued run = %q, want %q", target.ID, run.ID)
			}
			return target, nil
		},
	}, nil)

	err := module.retryWaitingForHarness(t.Context(), config.RepositoryRegistration{}, &retryCredentialProjectionStore{}, run, func(store.Run) (AgentLaunchResult, error) {
		t.Fatal("legacy check wait must not launch an agent")
		return AgentLaunchResult{}, nil
	})

	if err != nil {
		t.Fatalf("retryWaitingForHarness() error = %v", err)
	}
	if continued != 1 {
		t.Fatalf("check continuations = %d, want 1", continued)
	}
}

// TestRetryWaitingForHarnessKeepsLaunchingForOtherWaits verifies only the
// recognized check infrastructure evidence selects check recovery.
func TestRetryWaitingForHarnessKeepsLaunchingForOtherWaits(t *testing.T) {
	t.Parallel()

	run := store.Run{ID: "run-capacity", Stage: store.StageImplementation, Status: store.StatusWaitingForHarness, LifecycleReason: LifecycleReasonHarnessCapacityUnavailable + " (codex); waiting for capacity"}
	module := newInvocationLifecycle(nil, &repairContractWorker{}, nil, nil, nil, nil, invocationLifecycleHooks{
		resumeRecoveredCheck: func(context.Context, config.RepositoryRegistration, RunStore, store.Run) (store.Run, error) {
			t.Fatal("capacity wait must not select check recovery")
			return store.Run{}, nil
		},
	}, nil)
	launches := 0

	err := module.retryWaitingForHarness(t.Context(), config.RepositoryRegistration{}, &retryCredentialProjectionStore{}, run, func(store.Run) (AgentLaunchResult, error) {
		launches++
		return AgentLaunchResult{}, nil
	})

	if err != nil || launches != 1 {
		t.Fatalf("retryWaitingForHarness() error = %v, launches = %d, want one launch", err, launches)
	}
}

// TestResumeRecoveredCheckActivatesACheckInfrastructurePause verifies the
// continuation persists check / active without spending a repair attempt and
// keeps refusing pauses that are not check continuations.
func TestResumeRecoveredCheckActivatesACheckInfrastructurePause(t *testing.T) {
	t.Parallel()

	service := &Service{deps: Dependencies{Worker: &repairContractWorker{}, Now: func() time.Time { return time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC) }}}
	paused := store.Run{ID: "run-check-pause", Stage: store.StageCheck, Status: store.StatusWaitingForHuman, LifecycleReason: LifecycleReasonCheckInfrastructureUnavailable + ": setup execution failed", CheckRepairAttempts: 1, CheckRepairBudget: 3, Revision: 4}
	runStore := &repairContractStore{}

	resumed, err := service.resumeRecoveredCheck(t.Context(), config.RepositoryRegistration{}, runStore, paused)

	if err != nil {
		t.Fatalf("resumeRecoveredCheck() error = %v", err)
	}
	if resumed.Stage != store.StageCheck || resumed.Status != store.StatusActive || resumed.CheckRepairAttempts != 1 || resumed.CheckRepairBudget != 3 || !strings.Contains(resumed.LifecycleReason, "check infrastructure") {
		t.Fatalf("resumed run = %#v, want active check with the unchanged repair budget", resumed)
	}
	if len(runStore.saved) != 1 || runStore.saved[0].Status != store.StatusActive {
		t.Fatalf("saved runs = %#v, want one active check", runStore.saved)
	}

	for _, refused := range []store.Run{
		{ID: "run-exhausted", Stage: store.StageCheck, Status: store.StatusWaitingForHuman, LifecycleReason: "check-repair budget exhausted"},
		{ID: "run-session", Stage: store.StageCheck, Status: store.StatusWaitingForHuman, LifecycleReason: "implementation session unavailable for check repair"},
	} {
		unchanged, refusedErr := service.resumeRecoveredCheck(t.Context(), config.RepositoryRegistration{}, &repairContractStore{}, refused)
		if refusedErr != nil || unchanged.Status != store.StatusWaitingForHuman {
			t.Fatalf("resumeRecoveredCheck(%q) = %#v, %v, want the human pause unchanged", refused.ID, unchanged, refusedErr)
		}
	}

	pending := paused
	pending.CheckRepairPendingAttempt = 2
	if _, pendingErr := service.resumeRecoveredCheck(t.Context(), config.RepositoryRegistration{}, &repairContractStore{}, pending); pendingErr == nil {
		t.Fatal("resumeRecoveredCheck() error = nil, want refusal while a repair attempt awaits reconciliation")
	}
	clarification := paused
	clarification.PendingQuestions = []store.PendingQuestion{{ID: "question-1", Prompt: "choose one"}}
	if _, clarificationErr := service.resumeRecoveredCheck(t.Context(), config.RepositoryRegistration{}, &repairContractStore{}, clarification); clarificationErr == nil {
		t.Fatal("resumeRecoveredCheck() error = nil, want refusal while clarification is pending")
	}
}

// TestCheckContinuationActionNamesTheRetry verifies status and reconcile
// output name the retry-checks continuation only where resume admits it.
func TestCheckContinuationActionNamesTheRetry(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		run  store.Run
		want bool
	}{
		{name: "check infrastructure pause", run: store.Run{Stage: store.StageCheck, Status: store.StatusWaitingForHuman, LifecycleReason: LifecycleReasonCheckInfrastructureUnavailable}, want: true},
		{name: "legacy check wait", run: store.Run{Stage: store.StageCheck, Status: store.StatusWaitingForHarness, LifecycleReason: "check repair waiting for infrastructure"}, want: true},
		{name: "pending clarification", run: store.Run{Stage: store.StageCheck, Status: store.StatusWaitingForHuman, LifecycleReason: LifecycleReasonCheckInfrastructureUnavailable, PendingQuestions: []store.PendingQuestion{{ID: "question-1"}}}},
		{name: "capacity wait", run: store.Run{Stage: store.StageImplementation, Status: store.StatusWaitingForHarness, LifecycleReason: LifecycleReasonHarnessCapacityUnavailable}},
	} {
		t.Run(test.name, func(t *testing.T) {
			diagnosis := newRecoveryDiagnosis("run-status")
			appendCheckContinuationAction(&diagnosis, test.run)
			got := false
			for _, action := range diagnosis.SafeActions {
				got = got || action == checkContinuationAction
			}
			if got != test.want {
				t.Fatalf("safe actions = %#v, want retry-checks action %t", diagnosis.SafeActions, test.want)
			}
		})
	}
}
