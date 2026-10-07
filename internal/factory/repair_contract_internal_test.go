package factory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/gate"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestDecideCheckRepairCoversEveryBudgetOutcome verifies the pure policy
// distinguishes repair, exhaustion, infrastructure pause, and invalid state.
func TestDecideCheckRepairCoversEveryBudgetOutcome(t *testing.T) {
	tests := []struct {
		name         string
		run          store.Run
		kind         checkRepairFailureKind
		wantKind     checkRepairDecisionKind
		wantStage    store.Stage
		wantStatus   store.Status
		wantAttempt  int
		wantRemain   int
		wantConsumed bool
		wantErr      bool
	}{
		{name: "first repair", run: store.Run{Stage: store.StageCheck, Status: store.StatusActive, CheckRepairBudget: 3}, kind: checkRepairDeterministicFailure, wantKind: checkRepairStartDecision, wantStage: store.StageImplementation, wantStatus: store.StatusActive, wantAttempt: 1, wantRemain: 2, wantConsumed: true},
		{name: "last repair", run: store.Run{Stage: store.StageCheck, Status: store.StatusActive, CheckRepairAttempts: 2, CheckRepairBudget: 3}, kind: checkRepairDeterministicFailure, wantKind: checkRepairStartDecision, wantStage: store.StageImplementation, wantStatus: store.StatusActive, wantAttempt: 3, wantRemain: 0, wantConsumed: true},
		{name: "exhausted", run: store.Run{Stage: store.StageCheck, Status: store.StatusActive, CheckRepairAttempts: 3, CheckRepairBudget: 3}, kind: checkRepairDeterministicFailure, wantKind: checkRepairExhaustDecision, wantStage: store.StageCheck, wantStatus: store.StatusWaitingForHuman, wantAttempt: 3},
		{name: "configured budget above old ceiling", run: store.Run{Stage: store.StageCheck, Status: store.StatusActive, CheckRepairBudget: 7}, kind: checkRepairDeterministicFailure, wantKind: checkRepairStartDecision, wantStage: store.StageImplementation, wantStatus: store.StatusActive, wantAttempt: 1, wantRemain: 6, wantConsumed: true},
		{name: "infrastructure pause", run: store.Run{Stage: store.StageCheck, Status: store.StatusActive, CheckRepairAttempts: 1, CheckRepairBudget: 3}, kind: checkRepairInfrastructureFailure, wantKind: checkRepairPauseDecision, wantStage: store.StageCheck, wantStatus: store.StatusWaitingForHuman, wantAttempt: 1, wantRemain: 2},
		{name: "wrong stage", run: store.Run{Stage: store.StageImplementation, Status: store.StatusActive, CheckRepairBudget: 3}, kind: checkRepairDeterministicFailure, wantErr: true},
		{name: "wrong status", run: store.Run{Stage: store.StageCheck, Status: store.StatusWaitingForHuman, CheckRepairBudget: 3}, kind: checkRepairDeterministicFailure, wantErr: true},
		{name: "zero budget", run: store.Run{Stage: store.StageCheck, Status: store.StatusActive}, kind: checkRepairDeterministicFailure, wantErr: true},
		{name: "attempts beyond budget", run: store.Run{Stage: store.StageCheck, Status: store.StatusActive, CheckRepairAttempts: 4, CheckRepairBudget: 3}, kind: checkRepairDeterministicFailure, wantErr: true},
		{name: "pending reservation", run: store.Run{Stage: store.StageCheck, Status: store.StatusActive, CheckRepairAttempts: 1, CheckRepairBudget: 3, CheckRepairPendingAttempt: 2}, kind: checkRepairDeterministicFailure, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision, err := decideCheckRepair(test.run, test.kind)
			if test.wantErr {
				if err == nil {
					t.Fatal("decideCheckRepair() error = nil, want refusal")
				}
				return
			}
			if err != nil {
				t.Fatalf("decideCheckRepair() error = %v", err)
			}
			if decision.kind != test.wantKind || decision.nextStage != test.wantStage || decision.nextStatus != test.wantStatus || decision.attempt != test.wantAttempt || decision.remaining != test.wantRemain || decision.consumesAttempt != test.wantConsumed {
				t.Fatalf("decision = %#v, want kind=%q stage=%q status=%q attempt=%d remaining=%d consumed=%t", decision, test.wantKind, test.wantStage, test.wantStatus, test.wantAttempt, test.wantRemain, test.wantConsumed)
			}
		})
	}
}

// TestRepairableCheckFailureRejectsInfrastructure verifies only complete typed
// deterministic suites can spend the repair budget. A setup command that exited
// non-zero is repairable; a setup the worker could not execute is not.
func TestRepairableCheckFailureRejectsInfrastructure(t *testing.T) {
	deterministic := &gate.GateFailure{Name: "test", Result: worker.CommandResult{ExitCode: 1}}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "deterministic suite", err: &gate.SuiteFailure{Failures: []error{deterministic, &gate.DependencyFailure{Name: "lint", Dependency: "test"}}}, want: true},
		{name: "timeout", err: &gate.SuiteFailure{Failures: []error{&gate.GateFailure{Name: "test", TimedOut: true}}}, want: true},
		{name: "worker runtime", err: &gate.SuiteFailure{Failures: []error{&gate.GateFailure{Name: "test", Cause: errors.New("worker unavailable")}}}},
		{name: "unconfirmed termination", err: &gate.SuiteFailure{Failures: []error{&gate.GateFailure{Name: "test", Cause: &worker.CommandTerminationError{Reason: "the worker did not confirm the cancellation"}}}}},
		{name: "setup command", err: &gate.SuiteFailure{Failures: []error{&gate.SetupFailure{Result: worker.CommandResult{ExitCode: 1}}}}, want: true},
		{name: "setup runtime", err: &gate.SuiteFailure{Failures: []error{&gate.SetupFailure{Cause: errors.New("worker unavailable")}}}},
		{name: "setup without exit code", err: &gate.SuiteFailure{Failures: []error{&gate.SetupFailure{}}}},
		{name: "setup with status publication", err: &gate.SuiteFailure{Failures: []error{&gate.SetupFailure{Result: worker.CommandResult{ExitCode: 1}}, errors.New("GitHub unavailable")}}},
		{name: "joined setup persistence", err: errors.Join(&gate.SuiteFailure{Failures: []error{&gate.SetupFailure{Result: worker.CommandResult{ExitCode: 1}}}}, errors.New("SQLite busy"))},
		{name: "transport", err: &gate.SuiteFailure{Failures: []error{deterministic, errors.New("GitHub unavailable")}}},
		{name: "joined persistence", err: errors.Join(&gate.SuiteFailure{Failures: []error{deterministic}}, errors.New("SQLite busy"))},
		{name: "dependency only", err: &gate.SuiteFailure{Failures: []error{&gate.DependencyFailure{Name: "lint", Dependency: "test"}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isRepairableCheckFailure(test.err); got != test.want {
				t.Fatalf("isRepairableCheckFailure() = %t, want %t (%v)", got, test.want, test.err)
			}
		})
	}
}

// TestBuildCheckRepairPacketRetainsTheBoundedSuite verifies repair input keeps
// exact checkpoint evidence while bounding command output.
func TestBuildCheckRepairPacketRetainsTheBoundedSuite(t *testing.T) {
	const checkpoint = "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"
	results := []gate.Result{
		{CheckpointSHA: checkpoint, GateName: "format", Blocking: true, Setup: worker.CommandResult{Stdout: "setup ok"}, Gate: worker.CommandResult{ExitCode: 1, Stdout: strings.Repeat("x", maxRepairDiagnosticRunes+100)}, Outcome: gate.OutcomeFailed, Status: github.CommitStatus{SHA: checkpoint, State: github.CommitStatusFailure}},
		{CheckpointSHA: checkpoint, GateName: "test", Blocking: true, Skipped: true, SkipReason: "dependency format failed", Outcome: gate.OutcomeSkipped, Status: github.CommitStatus{SHA: checkpoint, State: github.CommitStatusPending}},
	}
	packet := buildCheckRepairPacket(store.Run{ID: "run-1", CheckpointSHA: checkpoint}, results, &gate.SuiteFailure{Failures: []error{&gate.GateFailure{Name: "format", TimedOut: true}}}, 2, 3)
	if packet.Version != checkRepairPacketVersion || packet.Attempt != 2 || packet.Budget != 3 || packet.CheckpointSHA != checkpoint || len(packet.Gates) != 2 {
		t.Fatalf("packet header = %#v, want exact run and suite identity", packet)
	}
	if packet.Gates[0].GateName != "format" || !packet.Gates[0].TimedOut || len([]rune(packet.Gates[0].Command.Stdout)) > maxRepairDiagnosticRunes+len("\n[truncated]") {
		t.Fatalf("first packet gate = %#v, want bounded timed-out failure", packet.Gates[0])
	}
	if !packet.Gates[1].Skipped || packet.Gates[1].SkipReason == "" || packet.Setup.Stdout != "setup ok" || fmt.Sprint(packet.Gates[0].Status) != string(github.CommitStatusFailure) {
		t.Fatalf("packet suite = %#v, want exact setup, status, and skipped evidence", packet)
	}
}

// TestRouteCheckRepairWaitsAndExhaustsWithoutSpendingExtraBudget verifies both
// non-launch service decisions stop the gate worker and persist once.
func TestRouteCheckRepairWaitsAndExhaustsWithoutSpendingExtraBudget(t *testing.T) {
	tests := []struct {
		name        string
		suiteErr    error
		attempts    int
		wantOutcome CheckRepairOutcome
		wantStatus  store.Status
	}{
		{name: "infrastructure pause", suiteErr: errors.New("harness rate limit"), attempts: 1, wantOutcome: CheckRepairInfrastructurePause, wantStatus: store.StatusWaitingForHuman},
		{name: "budget exhaustion", suiteErr: &gate.SuiteFailure{Failures: []error{&gate.GateFailure{Name: "test", Result: worker.CommandResult{ExitCode: 1}}}}, attempts: 3, wantOutcome: CheckRepairExhausted, wantStatus: store.StatusWaitingForHuman},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run := store.Run{ID: "run-repair-state", Stage: store.StageCheck, Status: store.StatusActive, CheckRepairAttempts: test.attempts, CheckRepairBudget: 3}
			runStore := &repairContractStore{}
			workerRuntime := &repairContractWorker{}
			service := &Service{deps: Dependencies{Worker: workerRuntime, Now: func() time.Time { return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC) }}}
			packet := SpecificationPacket{RepositoryConfig: config.RepositoryConfig{RetryLimits: config.RetryLimits{CheckRepair: 3}}}
			result, err := service.routeCheckRepair(t.Context(), config.RepositoryRegistration{}, runStore, run, packet, nil, test.suiteErr)
			if err != nil {
				t.Fatalf("routeCheckRepair() error = %v", err)
			}
			if result.Outcome != test.wantOutcome || result.Run.Status != test.wantStatus || result.Attempt != test.attempts || result.Remaining != 3-test.attempts || workerRuntime.stops != 1 {
				t.Fatalf("result = %#v, stops=%d", result, workerRuntime.stops)
			}
			if len(runStore.saved) != 1 || runStore.saved[0].Status != test.wantStatus {
				t.Fatalf("saved states = %#v, want one %s state", runStore.saved, test.wantStatus)
			}
		})
	}
}

// TestRouteCheckRepairParksAnUnconfirmedTerminationWithoutSpendingBudget
// verifies that a gate command which may still be running pauses the run for
// infrastructure, releases the worker, and tells the operator how to recover.
func TestRouteCheckRepairParksAnUnconfirmedTerminationWithoutSpendingBudget(t *testing.T) {
	const checkpoint = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"
	unconfirmed := fmt.Errorf("run command in worker %q: %w", "issue-210-run-20261005T094500Z", &worker.CommandTerminationError{Reason: "the command process group survived forced termination"})
	results := []gate.Result{
		{CheckpointSHA: checkpoint, GateName: "test", Phase: gate.PhaseCheckpoint, Blocking: true, Outcome: gate.OutcomeError, SetupRan: true, Status: github.CommitStatus{State: github.CommitStatusError}},
		{CheckpointSHA: checkpoint, GateName: "build", Phase: gate.PhaseCheckpoint, Blocking: true, Outcome: gate.OutcomeSkipped, Skipped: true, SkipReason: "worker command termination is unconfirmed", Status: github.CommitStatus{State: github.CommitStatusPending}},
	}
	suiteErr := &gate.SuiteFailure{Failures: []error{&gate.GateFailure{Name: "test", Blocking: true, Cause: unconfirmed}}}
	run := store.Run{ID: "issue-210-run-20261005T094500Z", Stage: store.StageCheck, Status: store.StatusActive, CheckpointSHA: checkpoint, CheckRepairAttempts: 1, CheckRepairBudget: 3, Worktree: filepath.Join(t.TempDir(), "worktrees", "run")}
	runStore := &repairContractStore{}
	workerRuntime := &repairContractWorker{}
	service := &Service{deps: Dependencies{Worker: workerRuntime, Now: func() time.Time { return time.Date(2026, 10, 5, 9, 45, 0, 0, time.UTC) }}}
	packet := SpecificationPacket{RepositoryConfig: config.RepositoryConfig{RetryLimits: config.RetryLimits{CheckRepair: 3}}}

	result, err := service.routeCheckRepair(t.Context(), config.RepositoryRegistration{}, runStore, run, packet, results, suiteErr)

	if err != nil {
		t.Fatalf("routeCheckRepair() error = %v", err)
	}
	if result.Outcome != CheckRepairInfrastructurePause || result.Run.Status != store.StatusWaitingForHuman || result.Attempt != 1 || result.Remaining != 2 {
		t.Fatalf("result = %#v, want an infrastructure pause without a consumed attempt", result)
	}
	if workerRuntime.stops != 1 {
		t.Fatalf("worker stops = %d, want the worker released so its processes end", workerRuntime.stops)
	}
	if reason := result.Run.LifecycleReason; !strings.Contains(reason, "termination is unconfirmed") || !strings.Contains(reason, "stop or recreate the worker") {
		t.Fatalf("lifecycle reason = %q, want the typed discrepancy and its recovery action", reason)
	}
}

// repairContractStore records the direct waiting-state projection.
type repairContractStore struct{ saved []store.Run }

// CurrentRun is unused by direct routing.
func (*repairContractStore) CurrentRun(context.Context) (*store.Run, error) { return nil, nil }

// SaveRun records one projection.
func (s *repairContractStore) SaveRun(_ context.Context, run store.Run) error {
	s.saved = append(s.saved, run)
	return nil
}

// Close satisfies the operational-store seam.
func (*repairContractStore) Close() error { return nil }

// repairContractWorker records release of the gate worker.
type repairContractWorker struct{ stops int }

// Start is unused by direct routing.
func (*repairContractWorker) Start(context.Context, worker.StartRequest) error { return nil }

// Resume is unused by direct routing.
func (*repairContractWorker) Resume(context.Context, worker.ResumeRequest) error { return nil }

// RunCommand is unused by direct routing.
func (*repairContractWorker) RunCommand(context.Context, worker.CommandRequest) (worker.CommandResult, error) {
	return worker.CommandResult{}, nil
}

// Stop records the gate worker release.
func (w *repairContractWorker) Stop(context.Context, string) error { w.stops++; return nil }

// Inspect reports the fixture worker present.
func (*repairContractWorker) Inspect(context.Context, string) (worker.Inspection, error) {
	return worker.Inspection{Exists: true}, nil
}

// TestRouteCheckRepairNamesTheDeterministicCauseWhenParking verifies both
// parked outcomes report what the failed command printed and retain the full
// output on disk. A setup runtime failure takes the infrastructure pause branch
// and an exhausted budget takes the human branch; neither builds a repair
// packet, so the lifecycle reason is the only evidence a run leaves.
func TestRouteCheckRepairNamesTheDeterministicCauseWhenParking(t *testing.T) {
	const checkpoint = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"
	setup := worker.CommandResult{ExitCode: 1, Stderr: "error: Failed to create virtual environment\n  Caused by: A virtual environment already exists at '.venv'. Use '--clear' to replace it"}
	gateCommand := worker.CommandResult{ExitCode: 1, Stderr: "internal/gate/runner_test.go:41: unexpected outcome"}
	tests := []struct {
		name        string
		attempts    int
		results     []gate.Result
		suiteErr    error
		wantOutcome CheckRepairOutcome
		wantReason  string
		wantOutput  string
	}{
		{
			name:        "setup runtime failure pauses for a check retry",
			results:     []gate.Result{{CheckpointSHA: checkpoint, GateName: "build", Phase: gate.PhaseCheckpoint, Blocking: true, Skipped: true, SkipReason: "setup failed", Outcome: gate.OutcomeSetupFailed, SetupRan: true, Setup: setup, Status: github.CommitStatus{State: github.CommitStatusError}}},
			suiteErr:    &gate.SuiteFailure{Failures: []error{&gate.SetupFailure{Result: setup, Cause: errors.New("worker unavailable")}}},
			wantOutcome: CheckRepairInfrastructurePause,
			wantReason:  LifecycleReasonCheckInfrastructureUnavailable + ": ",
			wantOutput:  "--clear",
		},
		{
			name:        "exhausted budget waits for a human",
			attempts:    3,
			results:     []gate.Result{{CheckpointSHA: checkpoint, GateName: "test", Phase: gate.PhaseCheckpoint, Blocking: true, Outcome: gate.OutcomeFailed, SetupRan: true, Gate: gateCommand, Status: github.CommitStatus{State: github.CommitStatusFailure}}},
			suiteErr:    &gate.SuiteFailure{Failures: []error{&gate.GateFailure{Name: "test", Blocking: true, Result: gateCommand}}},
			wantOutcome: CheckRepairExhausted,
			wantReason:  "check-repair budget exhausted: ",
			wantOutput:  "unexpected outcome",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run := store.Run{
				ID:                  "run-repair-cause",
				Stage:               store.StageCheck,
				Status:              store.StatusActive,
				CheckpointSHA:       checkpoint,
				CheckRepairAttempts: test.attempts,
				CheckRepairBudget:   3,
				Worktree:            filepath.Join(t.TempDir(), "worktrees", "run-repair-cause"),
			}
			runStore := &repairContractStore{}
			service := &Service{deps: Dependencies{Worker: &repairContractWorker{}, Now: func() time.Time { return time.Date(2026, 9, 17, 9, 30, 0, 0, time.UTC) }}}
			packet := SpecificationPacket{RepositoryConfig: config.RepositoryConfig{RetryLimits: config.RetryLimits{CheckRepair: 3}}}

			result, err := service.routeCheckRepair(t.Context(), config.RepositoryRegistration{}, runStore, run, packet, test.results, test.suiteErr)
			if err != nil {
				t.Fatalf("routeCheckRepair() error = %v", err)
			}

			if result.Outcome != test.wantOutcome {
				t.Fatalf("outcome = %q, want %q", result.Outcome, test.wantOutcome)
			}
			if !strings.HasPrefix(result.Run.LifecycleReason, test.wantReason) || !strings.Contains(result.Run.LifecycleReason, test.wantOutput) {
				t.Fatalf("lifecycle reason = %q, want %q followed by the command cause", result.Run.LifecycleReason, test.wantReason)
			}
			body, readErr := os.ReadFile(filepath.Join(filepath.Dir(run.Worktree), ".factory-agents", run.ID, gateFailureDiagnosticDirectoryName, "checkpoint.log"))
			if readErr != nil {
				t.Fatalf("read parked diagnostic: %v", readErr)
			}
			if !strings.Contains(string(body), test.wantOutput) {
				t.Fatalf("diagnostic body = %q, want the complete command output", body)
			}
		})
	}
}
