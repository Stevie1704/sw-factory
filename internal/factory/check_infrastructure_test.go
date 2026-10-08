package factory_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/gate"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// journaledAgentRunStore adds the pending-effect journal every production
// store provides. A resume resets startup reconciliation, which refuses any
// non-terminal run on a store without the journal.
type journaledAgentRunStore struct {
	*agentRunStore
	pending map[string]store.PendingEffect
}

// PendingEffect returns the run's reserved external effect, if any.
func (s *journaledAgentRunStore) PendingEffect(_ context.Context, runID string) (*store.PendingEffect, error) {
	effect, ok := s.pending[runID]
	if !ok {
		return nil, nil
	}
	return &effect, nil
}

// SavePendingEffect reserves one external effect for its run.
func (s *journaledAgentRunStore) SavePendingEffect(_ context.Context, effect store.PendingEffect) error {
	s.pending[effect.RunID] = effect
	return nil
}

// ClearPendingEffect releases the run's effect when its identity matches.
func (s *journaledAgentRunStore) ClearPendingEffect(_ context.Context, runID, effectID string) error {
	if effect, ok := s.pending[runID]; ok && effect.ID == effectID {
		delete(s.pending, runID)
	}
	return nil
}

// checkRecoveryFixture is a claimed run whose first implementation invocation
// completed with a resumable native session, ready for its first checkpoint.
type checkRecoveryFixture struct {
	service   *factory.Service
	runStore  *agentRunStore
	runtime   *agentWorker
	harness   *agentHarness
	workspace *draftGitWorkspace
	run       store.Run
}

// newCheckRecoveryFixture builds the coordinator seam used by the check
// recovery tests. Each gate suite consumes one setup and one gate result from
// the worker fixture, in that order. A journaled store matches production; an
// unjournaled one reaches the branches that run when no effect is pending.
func newCheckRecoveryFixture(t *testing.T, runID string, journaled bool) checkRecoveryFixture {
	t.Helper()
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repo")
	worktreePath := filepath.Join(root, "worktree")
	if err := os.MkdirAll(filepath.Join(repositoryPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatal(err)
	}
	branch := "factory/" + runID
	if err := os.WriteFile(filepath.Join(repositoryPath, ".git", "HEAD"), []byte("ref: refs/heads/"+branch+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := validRepositoryConfig()
	policy.Gates = []config.GateConfig{{Name: "test", Command: "test", Timeout: "5m", Blocking: true, EnvironmentPolicy: config.EnvironmentPolicyClean}}
	runStore := &agentRunStore{runs: map[string]store.Run{}, invocations: map[string]store.Invocation{}, gateResults: map[string][]store.GateResult{}}
	var opened factory.OperationalStore = runStore
	if journaled {
		opened = &journaledAgentRunStore{agentRunStore: runStore, pending: map[string]store.PendingEffect{}}
	}
	githubAdapter := &fakeGitHub{issueValue: tracker.Issue{Number: 42, Title: "Recover checks", Body: "Retry checks after setup failures.", State: "open", Labels: []string{tracker.LabelAgentReady}}}
	workspace := &draftGitWorkspace{
		workspace:      gitadapter.Workspace{BaseSHA: factoryGateCheckpoint, Branch: branch, Worktree: worktreePath},
		state:          gitadapter.WorktreeState{RepositoryPath: repositoryPath, Branch: branch, HeadSHA: factoryGateCheckpoint, ChangedPaths: []string{"python/pyproject.toml"}},
		checkpointSHAs: []string{implementationCheckpoint, repairedImplementationCheckpoint},
	}
	runtime := &agentWorker{}
	harnessRuntime := &agentHarness{}
	pullRequests := &fakePullRequests{created: codehost.PullRequest{Number: 18, URL: "https://github.com/example/project/pull/18", State: "open", Draft: true, HeadBranch: branch, BaseBranch: "main"}}
	host := config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{{
		Path: repositoryPath, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
		OperationalDataPath: filepath.Join(root, "state", "factory.db"), RepositoryConfigPath: filepath.Join(repositoryPath, "factory.yaml"),
	}}}
	ids := []string{runID, "initial", "repair"}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:            &fakeConfig{value: host},
		OpenStore:         func(context.Context, string) (factory.OperationalStore, error) { return opened, nil },
		LoadRepository:    func(string) (config.RepositoryConfig, error) { return policy, nil },
		Tracker:           &fakeGitHubWithPullRequests{fakeGitHub: githubAdapter},
		PullRequests:      pullRequests,
		Worktree:          workspace,
		GitWorkspace:      workspace,
		Worker:            runtime,
		HeadlessHarnesses: testHeadlessHarnesses(harnessRuntime),
		CommitStatuses:    &gateStatuses{},
		Now:               func() time.Time { return time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC) },
		NewRunID: func() (string, error) {
			if len(ids) == 0 {
				return "", errors.New("check recovery test identifiers exhausted")
			}
			id := ids[0]
			ids = ids[1:]
			return id, nil
		},
	})
	claimed, err := service.ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() fixture setup error = %v", err)
	}
	// Resume restarts reconciliation, which compares the run with the issue's
	// status comment; edits keep this fixture comment current.
	githubAdapter.statusComment = tracker.Comment{ID: claimed.Run.StatusCommentID, Body: githubAdapter.createdComments[len(githubAdapter.createdComments)-1]}
	claimed.Run.TestStageSkipped = true
	claimed.Run.TestExemption = &store.TestExemption{Kind: "human", Justification: "check recovery fixture"}
	if err := runStore.SaveRun(context.Background(), claimed.Run); err != nil {
		t.Fatalf("save check recovery fixture: %v", err)
	}
	runStore.gateResults[claimed.Run.ID] = []store.GateResult{{
		RunID: claimed.Run.ID, CheckpointSHA: claimed.Run.CheckpointSHA, Phase: store.GatePhaseBaseline,
		Ordinal: 0, GateName: policy.Gates[0].Name, Outcome: store.GateOutcomePassed,
		Status: string(codehost.CommitStatusSuccess), Blocking: policy.Gates[0].Blocking,
	}}
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() fixture setup error = %v", err)
	}
	initial := runStore.invocations[launch.Invocation.ID]
	initial.NativeSessionID = "session-initial"
	initial.Status = store.InvocationStatusCompleted
	initial.UpdatedAt = initial.CreatedAt.Add(time.Minute)
	runStore.invocations[launch.Invocation.ID] = initial
	return checkRecoveryFixture{service: service, runStore: runStore, runtime: runtime, harness: harnessRuntime, workspace: workspace, run: claimed.Run}
}

// TestSetupCommandFailureEntersBoundedImplementationRepair verifies a setup
// command that exits non-zero at a checkpoint is repaired by the implementation
// session with the setup diagnostics, and that the repaired checkpoint passes.
func TestSetupCommandFailureEntersBoundedImplementationRepair(t *testing.T) {
	t.Parallel()
	fixture := newCheckRecoveryFixture(t, "run-setup-repair", true)
	setupFailure := "error: Failed to create virtual environment\n  Caused by: A virtual environment already exists at '.venv'"
	fixture.runtime.results = []worker.CommandResult{{ExitCode: 1, Stderr: setupFailure}}

	first, err := fixture.service.CreateDraftPullRequest(context.Background(), factory.DraftPullRequestRequest{RunID: fixture.run.ID})

	if err != nil {
		t.Fatalf("first CreateDraftPullRequest() error = %v", err)
	}
	if first.Repair == nil || first.Repair.Outcome != factory.CheckRepairStarted || first.Repair.Run.Stage != store.StageImplementation || first.Repair.Run.CheckRepairAttempts != 1 {
		t.Fatalf("first result = %#v, want one active check repair", first)
	}
	packet := first.Repair.Packet
	if packet.Setup.ExitCode != 1 || !strings.Contains(packet.Setup.Stderr, "already exists") || len(packet.Gates) != 1 || packet.Gates[0].Outcome != gate.OutcomeSetupFailed {
		t.Fatalf("repair packet = %#v, want the failed setup diagnostics and the setup-failed gate", packet)
	}
	if len(fixture.harness.resumes) != 1 || fixture.harness.resumes[0].ResumeSessionID != "session-initial" {
		t.Fatalf("resume requests = %#v, want the existing native session", fixture.harness.resumes)
	}

	repaired := first.Repair.Invocation
	repaired.Status = store.InvocationStatusCompleted
	repaired.UpdatedAt = repaired.CreatedAt.Add(2 * time.Minute)
	fixture.runStore.invocations[repaired.ID] = repaired
	fixture.workspace.state.ChangedPaths = []string{"python/pyproject.toml"}
	fixture.runtime.results = append(fixture.runtime.results, worker.CommandResult{ExitCode: 0}, worker.CommandResult{ExitCode: 0})

	second, err := fixture.service.CreateDraftPullRequest(context.Background(), factory.DraftPullRequestRequest{RunID: fixture.run.ID})

	if err != nil {
		t.Fatalf("second CreateDraftPullRequest() error = %v", err)
	}
	if second.Repair != nil || second.Run.Stage != store.StageDraftPR || second.Run.CheckpointSHA != repairedImplementationCheckpoint {
		t.Fatalf("second result = %#v, want a draft PR at the repaired checkpoint", second)
	}
}

// TestCheckInfrastructurePauseResumesThroughGateEvaluation verifies a setup
// the worker cannot execute pauses without spending a repair attempt, that
// explicit resume re-evaluates the same checkpoint instead of launching an
// agent, and that a repeated failure returns to the same recoverable pause.
func TestCheckInfrastructurePauseResumesThroughGateEvaluation(t *testing.T) {
	t.Parallel()
	fixture := newCheckRecoveryFixture(t, "run-check-pause", true)
	fixture.runtime.nextCommandErr = errors.New("worker container is not running")

	paused, err := fixture.service.CreateDraftPullRequest(context.Background(), factory.DraftPullRequestRequest{RunID: fixture.run.ID})

	if err != nil {
		t.Fatalf("CreateDraftPullRequest() error = %v", err)
	}
	assertCheckInfrastructurePause(t, paused.Run)
	if paused.Repair == nil || paused.Repair.Outcome != factory.CheckRepairInfrastructurePause {
		t.Fatalf("repair result = %#v, want a check infrastructure pause", paused.Repair)
	}
	invocations := len(fixture.runStore.invocations)

	for attempt := 1; attempt <= 2; attempt++ {
		resumed, resumeErr := fixture.service.Resume(context.Background(), factory.ResumeRequest{RunID: fixture.run.ID})
		if resumeErr != nil {
			t.Fatalf("Resume() attempt %d error = %v", attempt, resumeErr)
		}
		if resumed.Run.Stage != store.StageCheck || resumed.Run.Status != store.StatusActive || resumed.Invocation.ID != "" {
			t.Fatalf("Resume() attempt %d = %#v, want active check without an invocation", attempt, resumed)
		}
		if attempt == 1 {
			fixture.runtime.nextCommandErr = errors.New("worker container is not running")
			again, againErr := fixture.service.CreateDraftPullRequest(context.Background(), factory.DraftPullRequestRequest{RunID: fixture.run.ID})
			if againErr != nil {
				t.Fatalf("repeated CreateDraftPullRequest() error = %v", againErr)
			}
			assertCheckInfrastructurePause(t, again.Run)
		}
	}
	fixture.runtime.results = []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}}

	passed, err := fixture.service.CreateDraftPullRequest(context.Background(), factory.DraftPullRequestRequest{RunID: fixture.run.ID})

	if err != nil {
		t.Fatalf("CreateDraftPullRequest() after resume error = %v", err)
	}
	if passed.Run.Stage != store.StageDraftPR || passed.Run.CheckpointSHA != implementationCheckpoint || passed.Run.CheckRepairAttempts != 0 {
		t.Fatalf("result = %#v, want a draft PR at the same checkpoint without a repair attempt", passed)
	}
	if len(fixture.runStore.invocations) != invocations || len(fixture.harness.resumes) != 0 || len(fixture.workspace.checkpoints) != 1 {
		t.Fatalf("effects = invocations %d resumes %d checkpoints %d, want no agent launch and one checkpoint", len(fixture.runStore.invocations), len(fixture.harness.resumes), len(fixture.workspace.checkpoints))
	}
}

// TestCheckRepairLaunchFailurePausesForACheckRetry verifies a repair whose
// harness cannot start, with no pending effect for reconciliation to own,
// leaves a recoverable check pause, not the harness wait that only an agent
// launch could leave.
func TestCheckRepairLaunchFailurePausesForACheckRetry(t *testing.T) {
	t.Parallel()
	fixture := newCheckRecoveryFixture(t, "run-repair-launch", false)
	fixture.runtime.results = []worker.CommandResult{{ExitCode: 0}, {ExitCode: 1, Stderr: "FAIL"}}
	fixture.harness.resumeErr = errors.New("harness process could not start")

	result, err := fixture.service.CreateDraftPullRequest(context.Background(), factory.DraftPullRequestRequest{RunID: fixture.run.ID})

	if err == nil {
		t.Fatal("CreateDraftPullRequest() error = nil, want the failed repair launch")
	}
	assertCheckInfrastructurePause(t, result.Run)
	stored := fixture.runStore.runs[fixture.run.ID]
	assertCheckInfrastructurePause(t, stored)

	fixture.harness.resumeErr = nil
	resumed, resumeErr := fixture.service.Resume(context.Background(), factory.ResumeRequest{RunID: fixture.run.ID})
	if resumeErr != nil {
		t.Fatalf("Resume() error = %v", resumeErr)
	}
	if resumed.Run.Stage != store.StageCheck || resumed.Run.Status != store.StatusActive {
		t.Fatalf("Resume() = %#v, want active check", resumed.Run)
	}
}

// TestLegacyCheckInfrastructureWaitResumesThroughGateEvaluation verifies a
// run persisted by an older coordinator in check / waiting_for_harness is
// recovered by resume instead of being refused as an agent launch from check.
func TestLegacyCheckInfrastructureWaitResumesThroughGateEvaluation(t *testing.T) {
	t.Parallel()
	fixture := newCheckRecoveryFixture(t, "run-legacy-wait", true)
	fixture.runtime.nextCommandErr = errors.New("worker container is not running")
	paused, err := fixture.service.CreateDraftPullRequest(context.Background(), factory.DraftPullRequestRequest{RunID: fixture.run.ID})
	if err != nil {
		t.Fatalf("CreateDraftPullRequest() fixture error = %v", err)
	}
	legacy := paused.Run
	legacy.Status = store.StatusWaitingForHarness
	legacy.LifecycleReason = "check repair waiting for infrastructure: setup execution failed: worker container is not running"
	if err := fixture.runStore.SaveRun(context.Background(), legacy); err != nil {
		t.Fatalf("save legacy wait: %v", err)
	}

	resumed, err := fixture.service.Resume(context.Background(), factory.ResumeRequest{RunID: fixture.run.ID})

	if err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	if resumed.Run.Stage != store.StageCheck || resumed.Run.Status != store.StatusActive || resumed.Run.CheckRepairAttempts != 0 || len(fixture.harness.resumes) != 0 {
		t.Fatalf("Resume() = %#v, want active check without a repair or agent launch", resumed)
	}
}

// assertCheckInfrastructurePause verifies the run is in the recoverable
// check-infrastructure pause and has not spent a repair attempt.
func assertCheckInfrastructurePause(t *testing.T, run store.Run) {
	t.Helper()
	if run.Stage != store.StageCheck || run.Status != store.StatusWaitingForHuman || !strings.HasPrefix(run.LifecycleReason, factory.LifecycleReasonCheckInfrastructureUnavailable) || run.CheckRepairAttempts != 0 {
		t.Fatalf("run = stage %q status %q reason %q attempts %d, want a check infrastructure pause without a spent attempt", run.Stage, run.Status, run.LifecycleReason, run.CheckRepairAttempts)
	}
}

// TestReconcileNamesTheCheckRetryForACheckInfrastructurePause verifies
// `factory reconcile` names the retry-checks continuation of a parked check,
// not only the generic recovery advice.
func TestReconcileNamesTheCheckRetryForACheckInfrastructurePause(t *testing.T) {
	t.Parallel()
	fixture := newCheckRecoveryFixture(t, "run-reconcile-pause", true)
	fixture.runtime.nextCommandErr = errors.New("worker container is not running")
	if _, err := fixture.service.CreateDraftPullRequest(context.Background(), factory.DraftPullRequestRequest{RunID: fixture.run.ID}); err != nil {
		t.Fatalf("CreateDraftPullRequest() fixture error = %v", err)
	}

	result, err := fixture.service.Reconcile(context.Background())

	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.Run == nil {
		t.Fatal("Reconcile() run = nil, want the paused check")
	}
	assertCheckInfrastructurePause(t, *result.Run)
	retries := 0
	for _, action := range result.Diagnosis.SafeActions {
		if strings.Contains(action, "retry checks") && strings.Contains(action, "factory resume") {
			retries++
		}
	}
	if retries != 1 {
		t.Fatalf("safe actions = %#v, want the retry-checks continuation exactly once", result.Diagnosis.SafeActions)
	}
}

// TestOutOfMemoryCheckPausesWithTheNamedCause verifies a check command the
// worker memory limit killed is a check infrastructure pause, not a repairable
// failure, and that the paused run names the out-of-memory cause.
func TestOutOfMemoryCheckPausesWithTheNamedCause(t *testing.T) {
	t.Parallel()
	fixture := newCheckRecoveryFixture(t, "run-check-oom", true)
	fixture.runtime.nextCommandErr = fmt.Errorf("run command in worker %q: %w", fixture.run.ID, &worker.OutOfMemoryError{})

	paused, err := fixture.service.CreateDraftPullRequest(context.Background(), factory.DraftPullRequestRequest{RunID: fixture.run.ID})

	if err != nil {
		t.Fatalf("CreateDraftPullRequest() error = %v", err)
	}
	assertCheckInfrastructurePause(t, paused.Run)
	if !strings.Contains(paused.Run.LifecycleReason, "out of memory") {
		t.Fatalf("lifecycle reason = %q, want the out-of-memory cause", paused.Run.LifecycleReason)
	}
}
