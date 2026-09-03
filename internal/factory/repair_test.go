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
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestDecideCheckRepairTable exercises the legal workflow transitions and
// budget boundary without involving GitHub, workers, or a durable store.
func TestDecideCheckRepairTable(t *testing.T) {
	t.Parallel()

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
		{
			name:         "first deterministic repair",
			run:          store.Run{Stage: store.StageCheck, Status: store.StatusActive, CheckRepairBudget: 3},
			kind:         checkRepairDeterministicFailure,
			wantKind:     checkRepairStartDecision,
			wantStage:    store.StageImplementation,
			wantStatus:   store.StatusActive,
			wantAttempt:  1,
			wantRemain:   2,
			wantConsumed: true,
		},
		{
			name:         "last deterministic repair",
			run:          store.Run{Stage: store.StageCheck, Status: store.StatusActive, CheckRepairAttempts: 2, CheckRepairBudget: 3},
			kind:         checkRepairDeterministicFailure,
			wantKind:     checkRepairStartDecision,
			wantStage:    store.StageImplementation,
			wantStatus:   store.StatusActive,
			wantAttempt:  3,
			wantRemain:   0,
			wantConsumed: true,
		},
		{
			name:        "exhausted deterministic repair",
			run:         store.Run{Stage: store.StageCheck, Status: store.StatusActive, CheckRepairAttempts: 3, CheckRepairBudget: 3},
			kind:        checkRepairDeterministicFailure,
			wantKind:    checkRepairExhaustDecision,
			wantStage:   store.StageCheck,
			wantStatus:  store.StatusWaitingForHuman,
			wantAttempt: 3,
			wantRemain:  0,
		},
		{
			name:        "infrastructure does not consume budget",
			run:         store.Run{Stage: store.StageCheck, Status: store.StatusActive, CheckRepairAttempts: 1, CheckRepairBudget: 3},
			kind:        checkRepairInfrastructureFailure,
			wantKind:    checkRepairWaitDecision,
			wantStage:   store.StageCheck,
			wantStatus:  store.StatusWaitingForHarness,
			wantAttempt: 1,
			wantRemain:  2,
		},
		{
			name:    "wrong stage",
			run:     store.Run{Stage: store.StageImplementation, Status: store.StatusActive, CheckRepairBudget: 3},
			kind:    checkRepairDeterministicFailure,
			wantErr: true,
		},
		{
			name:    "waiting status cannot consume directly",
			run:     store.Run{Stage: store.StageCheck, Status: store.StatusWaitingForHarness, CheckRepairBudget: 3},
			kind:    checkRepairDeterministicFailure,
			wantErr: true,
		},
		{
			name:    "human wait status cannot consume directly",
			run:     store.Run{Stage: store.StageCheck, Status: store.StatusWaitingForHuman, CheckRepairBudget: 3},
			kind:    checkRepairDeterministicFailure,
			wantErr: true,
		},
		{
			name:    "failed status cannot consume directly",
			run:     store.Run{Stage: store.StageCheck, Status: store.StatusFailed, CheckRepairBudget: 3},
			kind:    checkRepairDeterministicFailure,
			wantErr: true,
		},
		{
			name:    "zero budget",
			run:     store.Run{Stage: store.StageCheck, Status: store.StatusActive},
			kind:    checkRepairDeterministicFailure,
			wantErr: true,
		},
		{
			name:    "attempts beyond budget",
			run:     store.Run{Stage: store.StageCheck, Status: store.StatusActive, CheckRepairAttempts: 4, CheckRepairBudget: 3},
			kind:    checkRepairDeterministicFailure,
			wantErr: true,
		},
		{
			name: "pending reservation requires reconciliation",
			run: store.Run{
				Stage: store.StageCheck, Status: store.StatusActive,
				CheckRepairAttempts: 1, CheckRepairBudget: 3, CheckRepairPendingAttempt: 2,
			},
			kind:    checkRepairDeterministicFailure,
			wantErr: true,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			decision, err := decideCheckRepair(test.run, test.kind)
			if test.wantErr {
				if err == nil {
					t.Fatal("decideCheckRepair() error = nil, want error")
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

// TestIsRepairableCheckFailureRejectsInfrastructure verifies that only a
// complete typed deterministic suite failure may consume an attempt.
func TestIsRepairableCheckFailureRejectsInfrastructure(t *testing.T) {
	t.Parallel()

	deterministic := &gate.GateFailure{Name: "test", Result: worker.CommandResult{ExitCode: 1}}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "typed deterministic gate and dependency failures",
			err: &gate.SuiteFailure{Failures: []error{
				deterministic,
				&gate.DependencyFailure{Name: "lint", Dependency: "test"},
			}},
			want: true,
		},
		{
			name: "typed timeout",
			err: &gate.SuiteFailure{Failures: []error{
				&gate.GateFailure{Name: "test", TimedOut: true},
			}},
			want: true,
		},
		{
			name: "worker runtime error",
			err: &gate.SuiteFailure{Failures: []error{
				&gate.GateFailure{Name: "test", Cause: errors.New("worker unavailable")},
			}},
			want: false,
		},
		{
			name: "typed timeout with deadline cause",
			err: &gate.SuiteFailure{Failures: []error{
				&gate.GateFailure{Name: "test", Cause: errors.New("context deadline exceeded"), TimedOut: true},
			}},
			want: true,
		},
		{
			name: "setup failure",
			err: &gate.SuiteFailure{Failures: []error{
				&gate.SetupFailure{Result: worker.CommandResult{ExitCode: 1}},
			}},
			want: false,
		},
		{
			name: "status transport error",
			err:  &gate.SuiteFailure{Failures: []error{deterministic, errors.New("GitHub transport unavailable")}},
			want: false,
		},
		{
			name: "persistence error joined with suite",
			err:  errors.Join(&gate.SuiteFailure{Failures: []error{deterministic}}, errors.New("SQLite busy")),
			want: false,
		},
		{
			name: "dependency only",
			err:  &gate.SuiteFailure{Failures: []error{&gate.DependencyFailure{Name: "lint", Dependency: "test"}}},
			want: false,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			if got := isRepairableCheckFailure(test.err); got != test.want {
				t.Fatalf("isRepairableCheckFailure() = %t, want %t (%v)", got, test.want, test.err)
			}
		})
	}
}

// TestBuildCheckRepairPacketRetainsTheCompleteBoundedSuite verifies that the
// worker packet carries all exact-SHA gate observations without unbounded logs.
func TestBuildCheckRepairPacketRetainsTheCompleteBoundedSuite(t *testing.T) {
	t.Parallel()

	const checkpoint = "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"
	results := []gate.Result{
		{
			CheckpointSHA: checkpoint,
			GateName:      "format",
			Blocking:      true,
			Setup:         worker.CommandResult{ExitCode: 0, Stdout: "setup ok"},
			Gate:          worker.CommandResult{ExitCode: 1, Stdout: strings.Repeat("x", maxRepairDiagnosticRunes+100)},
			Outcome:       gate.OutcomeFailed,
			Status:        github.CommitStatus{SHA: checkpoint, State: github.CommitStatusFailure},
		},
		{
			CheckpointSHA: checkpoint,
			GateName:      "test",
			Blocking:      true,
			Skipped:       true,
			SkipReason:    "dependency \"format\" failed",
			Outcome:       gate.OutcomeSkipped,
			Status:        github.CommitStatus{SHA: checkpoint, State: github.CommitStatusPending},
		},
	}
	suiteErr := &gate.SuiteFailure{Failures: []error{&gate.GateFailure{Name: "format", TimedOut: true}}}
	packet := buildCheckRepairPacket(store.Run{ID: "run-1", CheckpointSHA: checkpoint}, results, suiteErr, 2, 3)
	if packet.Version != checkRepairPacketVersion || packet.Attempt != 2 || packet.Budget != 3 || packet.CheckpointSHA != checkpoint || len(packet.Gates) != 2 {
		t.Fatalf("packet header = %#v, want exact run and suite identity", packet)
	}
	if packet.Gates[0].GateName != "format" || !packet.Gates[0].TimedOut || len([]rune(packet.Gates[0].Command.Stdout)) > maxRepairDiagnosticRunes+len("\n[truncated]") {
		t.Fatalf("first packet gate = %#v, want bounded timed-out failure", packet.Gates[0])
	}
	if !packet.Gates[1].Skipped || packet.Gates[1].SkipReason == "" || packet.Gates[1].CheckpointSHA != checkpoint {
		t.Fatalf("second packet gate = %#v, want exact skipped result", packet.Gates[1])
	}
	if packet.Setup.Stdout != "setup ok" {
		t.Fatalf("packet setup = %#v, want shared setup result", packet.Setup)
	}
	if fmt.Sprint(packet.Gates[0].Status) != string(github.CommitStatusFailure) {
		t.Fatalf("packet status = %v, want failure", packet.Gates[0].Status)
	}
}

// TestRouteCheckRepairWaitsAndExhaustsWithoutConsumingExtraBudget verifies
// the service-level waiting decisions also release the gate worker.
func TestRouteCheckRepairWaitsAndExhaustsWithoutConsumingExtraBudget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		suiteErr    error
		attempts    int
		wantOutcome CheckRepairOutcome
		wantStatus  store.Status
		wantStops   int
	}{
		{
			name:        "infrastructure wait",
			suiteErr:    errors.New("harness rate limit"),
			attempts:    1,
			wantOutcome: CheckRepairWaitingForHarness,
			wantStatus:  store.StatusWaitingForHarness,
			wantStops:   1,
		},
		{
			name: "budget exhaustion",
			suiteErr: &gate.SuiteFailure{Failures: []error{
				&gate.GateFailure{Name: "test", Result: worker.CommandResult{ExitCode: 1}},
			}},
			attempts:    3,
			wantOutcome: CheckRepairExhausted,
			wantStatus:  store.StatusWaitingForHuman,
			wantStops:   1,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			run := store.Run{ID: "run-repair-state", Stage: store.StageCheck, Status: store.StatusActive, CheckRepairAttempts: test.attempts, CheckRepairBudget: 3}
			runStore := &repairStateStore{}
			workerRuntime := &repairStateWorker{}
			service := &Service{deps: Dependencies{
				Worker: workerRuntime,
				Now:    func() time.Time { return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC) },
			}}
			packet := SpecificationPacket{RepositoryConfig: config.RepositoryConfig{RetryLimits: config.RetryLimits{CheckRepair: 3}}}
			result, err := service.routeCheckRepair(context.Background(), config.RepositoryRegistration{}, runStore, run, packet, nil, test.suiteErr)
			if err != nil {
				t.Fatalf("routeCheckRepair() error = %v", err)
			}
			if result.Outcome != test.wantOutcome || result.Run.Status != test.wantStatus || result.Attempt != test.attempts || result.Remaining != 3-test.attempts || workerRuntime.stops != test.wantStops {
				t.Fatalf("result = %#v, stops=%d, want outcome=%q status=%q attempt=%d remaining=%d stops=%d", result, workerRuntime.stops, test.wantOutcome, test.wantStatus, test.attempts, 3-test.attempts, test.wantStops)
			}
			if len(runStore.saved) != 1 || runStore.saved[0].Status != test.wantStatus {
				t.Fatalf("saved states = %#v, want one %s state", runStore.saved, test.wantStatus)
			}
		})
	}
}

// TestStartCheckRepairPreservesReservationBoundaries verifies that a failed
// native launch rolls back, while failures after native resume retain a
// durable pending marker instead of permitting an undercounted relaunch.
func TestStartCheckRepairPreservesReservationBoundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		resumeErr            error
		failInvocationAt     int
		failRunSaveAt        int
		wantAttempts         int
		wantPending          int
		wantInvocationStatus store.InvocationStatus
	}{
		{
			name:                 "resume failure rolls back reservation",
			resumeErr:            errors.New("harness rate limit"),
			wantInvocationStatus: store.InvocationStatusCannotProceed,
		},
		{
			name:                 "native session persistence retains pending reservation",
			failInvocationAt:     2,
			wantPending:          1,
			wantInvocationStatus: store.InvocationStatusCannotProceed,
		},
		{
			name:                 "final state persistence retains pending reservation",
			failRunSaveAt:        2,
			wantPending:          1,
			wantInvocationStatus: store.InvocationStatusCannotProceed,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			service, registration, runStore, harnessRuntime := newRepairLaunchService(t, test.resumeErr, test.failInvocationAt, test.failRunSaveAt)
			run := runStore.run
			repairPacket := CheckRepairPacket{
				Version:       checkRepairPacketVersion,
				RunID:         run.ID,
				CheckpointSHA: run.CheckpointSHA,
				Attempt:       1,
				Budget:        3,
			}
			_, updated, err := service.startCheckRepair(context.Background(), registration, runStore, run, SpecificationPacket{
				Issue: github.Issue{Body: "repair guidance"},
			}, repairPacket)
			if err == nil {
				t.Fatal("startCheckRepair() error = nil, want injected failure")
			}
			if updated.CheckRepairAttempts != test.wantAttempts || updated.CheckRepairPendingAttempt != test.wantPending {
				t.Fatalf("updated run = %#v, want attempts=%d pending=%d", updated, test.wantAttempts, test.wantPending)
			}
			if harnessRuntime.resumes != 1 {
				t.Fatalf("resume calls = %d, want one native resume attempt", harnessRuntime.resumes)
			}
			var repairInvocation *store.Invocation
			for _, invocation := range runStore.invocations {
				if invocation.ID != "inv-initial" {
					copy := invocation
					repairInvocation = &copy
				}
			}
			if repairInvocation == nil || repairInvocation.Status != test.wantInvocationStatus {
				t.Fatalf("repair invocation = %#v, want status %s", repairInvocation, test.wantInvocationStatus)
			}
		})
	}
}

// newRepairLaunchService creates the smallest native-resume fixture, including
// a real Git metadata source so the worker projection path is exercised.
func newRepairLaunchService(t *testing.T, resumeErr error, failInvocationAt, failRunSaveAt int) (*Service, config.RepositoryRegistration, *repairLaunchStore, *repairLaunchHarness) {
	t.Helper()
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	worktreePath := filepath.Join(root, "worktree")
	if err := os.MkdirAll(filepath.Join(repositoryPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryPath, ".git", "HEAD"), []byte("ref: refs/heads/factory/run-repair\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	run := store.Run{
		ID:                  "run-repair-boundary",
		RepositoryPath:      repositoryPath,
		Worktree:            worktreePath,
		Stage:               store.StageCheck,
		Status:              store.StatusActive,
		CheckpointSHA:       "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd",
		CheckRepairBudget:   3,
		SpecificationPacket: "{}",
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	previous := store.Invocation{
		ID:                      "inv-initial",
		RunID:                   run.ID,
		Harness:                 string(config.HarnessCodex),
		Role:                    "implementation",
		Stage:                   store.StageImplementation,
		Model:                   "gpt-test",
		NativeSessionID:         "session-initial",
		WorkspaceID:             "workspace-implementation",
		ImplementationSurfaceID: "surface-implementation",
		Status:                  store.InvocationStatusCompleted,
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	registration := config.RepositoryRegistration{Path: repositoryPath}
	runStore := &repairLaunchStore{
		run:              run,
		invocations:      map[string]store.Invocation{previous.ID: previous},
		failInvocationAt: failInvocationAt,
		failRunSaveAt:    failRunSaveAt,
	}
	harnessRuntime := &repairLaunchHarness{resumeErr: resumeErr}
	service := &Service{deps: Dependencies{
		Worker:   &repairLaunchWorker{},
		Terminal: &repairLaunchTerminal{},
		Harness:  harnessRuntime,
		Now:      func() time.Time { return now },
		NewRunID: func() (string, error) { return "repair", nil },
	}}
	return service, registration, runStore, harnessRuntime
}

// repairLaunchStore persists just enough run and invocation state to exercise
// reservation rollback and post-resume failure handling.
type repairLaunchStore struct {
	run              store.Run
	invocations      map[string]store.Invocation
	failInvocationAt int
	failRunSaveAt    int
	saveInvocationAt int
	saveRunAt        int
}

// CurrentRun returns the fixture run.
func (s *repairLaunchStore) CurrentRun(context.Context) (*store.Run, error) { return &s.run, nil }

// SaveRun records the durable run reservation or completion state.
func (s *repairLaunchStore) SaveRun(_ context.Context, run store.Run) error {
	s.saveRunAt++
	if s.failRunSaveAt > 0 && s.saveRunAt >= s.failRunSaveAt {
		return errors.New("injected run persistence failure")
	}
	s.run = run
	return nil
}

// SaveInvocation records the durable invocation lifecycle state.
func (s *repairLaunchStore) SaveInvocation(_ context.Context, invocation store.Invocation) error {
	s.saveInvocationAt++
	if s.failInvocationAt > 0 && s.saveInvocationAt == s.failInvocationAt {
		return errors.New("injected invocation persistence failure")
	}
	s.invocations[invocation.ID] = invocation
	return nil
}

// Invocation loads one fixture invocation by run and id.
func (s *repairLaunchStore) Invocation(_ context.Context, runID, invocationID string) (*store.Invocation, error) {
	value, ok := s.invocations[invocationID]
	if !ok || value.RunID != runID {
		return nil, nil
	}
	return &value, nil
}

// LatestInvocation returns the newest fixture invocation for the run.
func (s *repairLaunchStore) LatestInvocation(_ context.Context, runID string) (*store.Invocation, error) {
	var latest *store.Invocation
	for _, value := range s.invocations {
		if value.RunID != runID || (latest != nil &&
			(value.UpdatedAt.Before(latest.UpdatedAt) ||
				(value.UpdatedAt.Equal(latest.UpdatedAt) && value.ID <= latest.ID))) {
			continue
		}
		copy := value
		latest = &copy
	}
	return latest, nil
}

// ClaimLifecycleNotification satisfies the run-store seam.
func (*repairLaunchStore) ClaimLifecycleNotification(context.Context, string, store.Status) (bool, error) {
	return true, nil
}

// ReleaseLifecycleNotification satisfies the run-store seam.
func (*repairLaunchStore) ReleaseLifecycleNotification(context.Context, string, store.Status) error {
	return nil
}

// Close satisfies the operational-store seam.
func (*repairLaunchStore) Close() error { return nil }

// repairLaunchWorker records worker lifecycle effects for a repair fixture.
type repairLaunchWorker struct {
	stops int
}

// Start satisfies the worker runtime seam.
func (*repairLaunchWorker) Start(context.Context, worker.StartRequest) error { return nil }

// Resume satisfies the worker runtime seam.
func (*repairLaunchWorker) Resume(context.Context, worker.ResumeRequest) error { return nil }

// RunCommand satisfies the worker runtime seam.
func (*repairLaunchWorker) RunCommand(context.Context, worker.CommandRequest) (worker.CommandResult, error) {
	return worker.CommandResult{}, nil
}

// Stop records worker cleanup after an injected launch failure.
func (w *repairLaunchWorker) Stop(context.Context, string) error {
	w.stops++
	return nil
}

// Inspect satisfies the worker runtime seam.
func (*repairLaunchWorker) Inspect(context.Context, string) (worker.Inspection, error) {
	return worker.Inspection{Exists: true}, nil
}

// repairLaunchTerminal supplies opaque control handles without a live cmux.
type repairLaunchTerminal struct{}

// EnsureControlWorkspace returns the fixture control workspace.
func (*repairLaunchTerminal) EnsureControlWorkspace(context.Context, terminal.WorkspaceRequest) (terminal.Workspace, error) {
	return terminal.Workspace{ID: "workspace-control"}, nil
}

// EnsureRunWorkspace satisfies the terminal runtime seam.
func (*repairLaunchTerminal) EnsureRunWorkspace(context.Context, terminal.RunWorkspaceRequest) (terminal.RunWorkspace, error) {
	return terminal.RunWorkspace{}, nil
}

// CreateSurface satisfies the terminal runtime seam.
func (*repairLaunchTerminal) CreateSurface(context.Context, terminal.SurfaceRequest) (terminal.Surface, error) {
	return terminal.Surface{ID: "surface-repair"}, nil
}

// LaunchSurface satisfies the terminal runtime seam.
func (*repairLaunchTerminal) LaunchSurface(context.Context, terminal.SurfaceID, terminal.Command) error {
	return nil
}

// SendInput satisfies the terminal runtime seam.
func (*repairLaunchTerminal) SendInput(context.Context, terminal.SurfaceID, []byte) error { return nil }

// Notify satisfies the terminal runtime seam.
func (*repairLaunchTerminal) Notify(context.Context, terminal.Notification) error { return nil }

// CloseSurface satisfies the terminal runtime seam.
func (*repairLaunchTerminal) CloseSurface(context.Context, terminal.SurfaceID) error { return nil }

// CloseWorkspace satisfies the terminal runtime seam.
func (*repairLaunchTerminal) CloseWorkspace(context.Context, terminal.WorkspaceID) error { return nil }

// repairLaunchHarness records native resume attempts for the boundary tests.
type repairLaunchHarness struct {
	resumeErr error
	resumes   int
}

// Capabilities satisfies the harness runtime seam.
func (*repairLaunchHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{Name: string(config.HarnessCodex)}
}

// Start satisfies the harness runtime seam.
func (*repairLaunchHarness) Start(context.Context, harness.StartRequest) (harness.Session, error) {
	return harness.Session{}, nil
}

// Resume records and optionally fails the native resume.
func (h *repairLaunchHarness) Resume(_ context.Context, request harness.StartRequest) (harness.Session, error) {
	h.resumes++
	if h.resumeErr != nil {
		return harness.Session{}, h.resumeErr
	}
	return harness.Session{InvocationID: request.InvocationID, NativeSessionID: "session-repair"}, nil
}

// Finish satisfies the harness runtime seam.
func (*repairLaunchHarness) Finish(context.Context, harness.Session) error { return nil }

// repairStateStore is the minimal direct-persistence seam for waiting-state
// decisions, deliberately omitting visible GitHub and invocation adapters.
type repairStateStore struct {
	saved []store.Run
}

// CurrentRun is unused by the direct route test.
func (s *repairStateStore) CurrentRun(context.Context) (*store.Run, error) { return nil, nil }

// SaveRun records the state transition under test.
func (s *repairStateStore) SaveRun(_ context.Context, run store.Run) error {
	s.saved = append(s.saved, run)
	return nil
}

// ClaimLifecycleNotification satisfies the run-store seam.
func (*repairStateStore) ClaimLifecycleNotification(context.Context, string, store.Status) (bool, error) {
	return true, nil
}

// ReleaseLifecycleNotification satisfies the run-store seam.
func (*repairStateStore) ReleaseLifecycleNotification(context.Context, string, store.Status) error {
	return nil
}

// Close satisfies the operational-store seam.
func (*repairStateStore) Close() error { return nil }

// repairStateWorker records worker release effects.
type repairStateWorker struct {
	stops int
}

// Start satisfies the worker runtime seam.
func (*repairStateWorker) Start(context.Context, worker.StartRequest) error { return nil }

// Resume satisfies the worker runtime seam.
func (*repairStateWorker) Resume(context.Context, worker.ResumeRequest) error { return nil }

// RunCommand satisfies the worker runtime seam.
func (*repairStateWorker) RunCommand(context.Context, worker.CommandRequest) (worker.CommandResult, error) {
	return worker.CommandResult{}, nil
}

// Stop records the gate-worker release.
func (w *repairStateWorker) Stop(context.Context, string) error {
	w.stops++
	return nil
}

// Inspect satisfies the worker runtime seam.
func (*repairStateWorker) Inspect(context.Context, string) (worker.Inspection, error) {
	return worker.Inspection{Exists: true}, nil
}

// TestCheckRepairRefusesMidSessionHarnessMigration verifies a native session
// can only be continued by the harness that created it, so a repair cannot
// migrate an implementation session from one harness to another.
func TestCheckRepairRefusesMidSessionHarnessMigration(t *testing.T) {
	service, registration, runStore, harnessRuntime := newRepairLaunchService(t, nil, 0, 0)
	previous := runStore.invocations["inv-initial"]
	previous.Harness = string(config.HarnessClaude)
	runStore.invocations["inv-initial"] = previous
	run := runStore.run

	_, _, err := service.startCheckRepair(context.Background(), registration, runStore, run, SpecificationPacket{
		Issue: github.Issue{Body: "repair guidance"},
	}, CheckRepairPacket{
		Version:       checkRepairPacketVersion,
		RunID:         run.ID,
		CheckpointSHA: run.CheckpointSHA,
		Attempt:       1,
		Budget:        3,
	})
	if !errors.Is(err, ErrCheckRepairSessionUnavailable) {
		t.Fatalf("startCheckRepair() error = %v, want the unavailable-session sentinel", err)
	}
	if !strings.Contains(err.Error(), "cannot resume") {
		t.Fatalf("startCheckRepair() error = %v, want a harness-mismatch refusal", err)
	}
	if harnessRuntime.resumes != 0 {
		t.Fatalf("resume calls = %d, want no cross-harness resume attempt", harnessRuntime.resumes)
	}
}

// TestCheckRepairFailsClosedWithoutThePersistedCredentialSource verifies a
// repair cannot resume a managed credential store after its source is removed.
func TestCheckRepairFailsClosedWithoutThePersistedCredentialSource(t *testing.T) {
	service, registration, runStore, harnessRuntime := newRepairLaunchService(t, nil, 0, 0)
	previous := runStore.invocations["inv-initial"]
	previous.CredentialStoreID = registration.Path
	runStore.invocations["inv-initial"] = previous
	run := runStore.run

	_, _, err := service.startCheckRepair(context.Background(), registration, runStore, run, SpecificationPacket{
		Issue: github.Issue{Body: "repair guidance"},
	}, CheckRepairPacket{
		Version:       checkRepairPacketVersion,
		RunID:         run.ID,
		CheckpointSHA: run.CheckpointSHA,
		Attempt:       1,
		Budget:        3,
	})
	var credentialErr *credentialProjectionError
	if !errors.As(err, &credentialErr) {
		t.Fatalf("startCheckRepair() error = %v, want credential projection failure", err)
	}
	if harnessRuntime.resumes != 0 || len(runStore.invocations) != 1 {
		t.Fatalf("check repair effects = %d resumes/%d invocations, want no launch side effects", harnessRuntime.resumes, len(runStore.invocations))
	}
}
