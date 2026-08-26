package factory_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestTestStageAcceptsVerifiedRedTestsAndLaunchesImplementation verifies the
// complete public handoff: baseline -> test invocation -> independent red
// command -> scoped test checkpoint -> protected implementation launch.
func TestTestStageAcceptsVerifiedRedTestsAndLaunchesImplementation(t *testing.T) {
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	worktreePath := filepath.Join(root, "worktree")
	if err := os.MkdirAll(filepath.Join(repositoryPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryPath, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(worktreePath, "internal", "factory"), 0o755); err != nil {
		t.Fatal(err)
	}
	protectedPath := filepath.Join(worktreePath, "internal", "factory", "agent_test.go")
	if err := os.WriteFile(protectedPath, []byte("package factory_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	policy := validRepositoryConfig()
	policy.RoleHarnessDefaults["test"] = config.HarnessCodex
	policy.ModelOptions["test"] = []string{"gpt-5"}
	storeRuntime := &agentRunStore{runs: map[string]store.Run{}, invocations: map[string]store.Invocation{}, gateResults: map[string][]store.GateResult{}}
	workerRuntime := &agentWorker{results: []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}, {ExitCode: 1, Stdout: "expected behavior assertion"}}}
	terminalRuntime := &agentTerminal{}
	harnessRuntime := &agentHarness{}
	githubRuntime := &fakeGitHub{issueValue: github.Issue{Number: 12, Title: "Test stage", State: "open", Labels: []string{github.LabelAgentReady}}}
	workspace := &testStageWorkspace{
		workspace: gitadapter.Workspace{BaseSHA: factoryGateCheckpoint, Branch: "factory/run-test-stage", Worktree: worktreePath},
		state:     gitadapter.WorktreeState{RepositoryPath: repositoryPath, Branch: "factory/run-test-stage", HeadSHA: factoryGateCheckpoint},
	}
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
		Path:                 repositoryPath,
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		Cmux:                 config.CmuxConfig{ControlWorkspace: "factory-control"},
		OperationalDataPath:  filepath.Join(root, "state", "factory.db"),
		RepositoryConfigPath: filepath.Join(repositoryPath, config.RepositoryConfigFileName),
	}}}
	ids := []string{"run-test-stage", "test-invocation", "implementation-invocation"}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:         &fakeConfig{value: host},
		OpenStore:      func(context.Context, string) (factory.OperationalStore, error) { return storeRuntime, nil },
		LoadRepository: func(string) (config.RepositoryConfig, error) { return policy, nil },
		GitHub:         githubRuntime,
		CommitStatuses: &gateStatuses{},
		Worktree:       workspace,
		GitWorkspace:   workspace,
		Worker:         workerRuntime,
		Terminal:       terminalRuntime,
		Harness:        harnessRuntime,
		Now:            func() time.Time { return time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC) },
		NewRunID: func() (string, error) {
			if len(ids) == 0 {
				return "", errors.New("test-stage id fixture exhausted")
			}
			id := ids[0]
			ids = ids[1:]
			return id, nil
		},
		Coordinator: "coordinator-test",
	})

	claimed, err := service.ClaimIssue(context.Background(), 12)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}
	if _, err := service.RunBaseline(context.Background(), factory.BaselineRequest{RunID: claimed.Run.ID}); err != nil {
		t.Fatalf("RunBaseline() error = %v", err)
	}
	if storeRuntime.current.Stage != store.StageTest {
		t.Fatalf("baseline stage = %q, want test", storeRuntime.current.Stage)
	}
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() test error = %v", err)
	}
	workspace.state.ChangedPaths = []string{"internal/factory/agent_test.go"}
	value := report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  launch.Invocation.ID,
		RunID:         launch.Invocation.RunID,
		Harness:       launch.Invocation.Harness,
		Role:          "test",
		Stage:         "test",
		Outcome:       report.OutcomeCompleted,
		Summary:       "focused behavior test is red on base",
		TestHandoff: &report.TestHandoff{
			AcceptanceCoverage:    []report.AcceptanceMapping{{Criterion: "behavior", Evidence: "focused test"}},
			ChangedFiles:          []string{"internal/factory/agent_test.go"},
			FocusedTestCommand:    "go test ./internal/factory -run TestBehavior",
			ExpectedFailureReason: "expected behavior assertion",
			ObservedFailureEvidence: []report.Evidence{{
				Kind:   "exit_code",
				Detail: "focused command exited with code 1",
			}},
		},
		NativeSessionID: "test-session",
		ReportedAt:      time.Now().UTC(),
	}
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, value); err != nil {
		t.Fatalf("WriteAtomicForInvocation() error = %v", err)
	}
	accepted, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: claimed.Run.ID, InvocationID: launch.Invocation.ID})
	if err != nil {
		t.Fatalf("AcceptAgentReport() error = %v", err)
	}
	if accepted.Invocation.Status != store.InvocationStatusCompleted {
		t.Fatalf("test invocation = %#v, want completed", accepted.Invocation)
	}
	if storeRuntime.current.Stage != store.StageImplementation || storeRuntime.current.TestCheckpointSHA == "" || len(storeRuntime.current.ProtectedTestPaths) != 1 {
		t.Fatalf("handoff run = %#v, want implementation with protected test checkpoint", storeRuntime.current)
	}
	if len(workerRuntime.commands) != 3 || workerRuntime.commands[2].Role != "test" || workerRuntime.commands[2].Command != value.TestHandoff.FocusedTestCommand {
		t.Fatalf("worker commands = %#v, want independently rerun focused test", workerRuntime.commands)
	}
	if len(workerRuntime.starts) < 3 || workerRuntime.starts[len(workerRuntime.starts)-1].Role != "implementation" {
		t.Fatalf("worker starts = %#v, want automatic implementation launch", workerRuntime.starts)
	}
	if launch, ok := storeRuntime.invocations["inv-implementation-invocation"]; !ok || launch.Status != store.InvocationStatusActive || launch.Role != "implementation" {
		t.Fatalf("implementation invocation = %#v, want active protected handoff consumer", launch)
	}

	// A direct implementation edit to a protected test path is rejected before
	// the implementation report can finish its invocation.
	if err := os.WriteFile(protectedPath, []byte("package factory_test\n// edited by implementation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace.state.ChangedPaths = []string{"internal/factory/agent_test.go"}
	implementation := storeRuntime.invocations["inv-implementation-invocation"]
	implementationReport := report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  implementation.ID,
		RunID:         implementation.RunID,
		Harness:       implementation.Harness,
		Role:          "implementation",
		Stage:         "implementation",
		Outcome:       report.OutcomeCompleted,
		Summary:       "implementation attempted to finish",
		Handoff: &report.Handoff{
			ChangeSummary:          "implementation change",
			AcceptanceMapping:      []report.AcceptanceMapping{{Criterion: "behavior", Evidence: "focused test"}},
			ProductionFilesChanged: []string{"internal/factory/agent_test.go"},
			FocusedCommands:        []string{"go test ./internal/factory"},
		},
		NativeSessionID: "implementation-session",
		ReportedAt:      time.Now().UTC(),
	}
	if _, err := report.WriteAtomicForInvocation(implementation.ResultDirectory, implementation.ID, implementationReport); err != nil {
		t.Fatalf("WriteAtomicForInvocation() implementation error = %v", err)
	}
	if _, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: claimed.Run.ID, InvocationID: implementation.ID}); err == nil {
		t.Fatal("AcceptAgentReport() accepted an implementation edit to a protected test path")
	}
	if got := storeRuntime.invocations[implementation.ID].Status; got != store.InvocationStatusActive {
		t.Fatalf("protected-path invocation status = %q, want active after rejection", got)
	}
}

// TestTestStagePausesAnUnverifiableCompletedReportForHumanDisposition verifies
// malformed red-test evidence cannot leave the test run active for an
// implementation bypass or an automated revision loop.
func TestTestStagePausesAnUnverifiableCompletedReportForHumanDisposition(t *testing.T) {
	service, runStore, workerRuntime, _, _ := newAgentService(t)
	run := *runStore.current
	run.Stage = store.StageTest
	run.Status = store.StatusActive
	run.TestStageSkipped = false
	if err := runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("persist test-stage fixture: %v", err)
	}
	worktree := serviceTestWorktree(runStore)
	worktree.state.ChangedPaths = []string{"internal/factory/agent_test.go"}
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() test error = %v", err)
	}
	malformed := report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  launch.Invocation.ID,
		RunID:         launch.Invocation.RunID,
		Harness:       launch.Invocation.Harness,
		Role:          "test",
		Stage:         "test",
		Outcome:       report.OutcomeCompleted,
		Summary:       "test report omitted required red evidence",
		TestHandoff: &report.TestHandoff{
			AcceptanceCoverage: []report.AcceptanceMapping{{Criterion: "behavior", Evidence: "focused test"}},
			ChangedFiles:       []string{"internal/factory/agent_test.go"},
			FocusedTestCommand: "go test ./internal/factory -run TestBehavior",
		},
		ReportedAt: time.Now().UTC(),
	}
	data, err := json.Marshal(malformed)
	if err != nil {
		t.Fatalf("marshal malformed test report: %v", err)
	}
	if err := os.WriteFile(filepath.Join(launch.Invocation.ResultDirectory, report.ReportFileName), data, 0o600); err != nil {
		t.Fatalf("write malformed test report: %v", err)
	}
	accepted, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: launch.Invocation.ID})
	if err != nil {
		t.Fatalf("AcceptAgentReport() error = %v", err)
	}
	if accepted.Invocation.Status != store.InvocationStatusWaitingForHuman || runStore.current.Status != store.StatusWaitingForHuman || runStore.current.Stage != store.StageTest {
		t.Fatalf("disposition = invocation %#v run %#v, want waiting_for_human/test", accepted.Invocation, runStore.current)
	}
	if runStore.current.LifecycleReason != "focused red-test report is unverifiable" {
		t.Fatalf("lifecycle reason = %q, want content-free unverifiable disposition", runStore.current.LifecycleReason)
	}
	if workerRuntime.stops == 0 {
		t.Fatal("worker was not stopped for human disposition")
	}
}

// TestTestStageRejectsProductionChangesBeforeTechnicalExemption verifies a
// technical exemption cannot turn an out-of-scope test edit into an
// implementation handoff.
func TestTestStageRejectsProductionChangesBeforeTechnicalExemption(t *testing.T) {
	service, runStore, workerRuntime, _, _ := newAgentService(t)
	run := *runStore.current
	run.Stage = store.StageTest
	run.Status = store.StatusActive
	run.TestStageSkipped = false
	run.TestExemption = nil
	if err := runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("persist test-stage fixture: %v", err)
	}
	worktree := serviceTestWorktree(runStore)
	worktree.state.ChangedPaths = []string{"internal/factory/agent.go"}
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() test error = %v", err)
	}
	value := report.Report{
		SchemaVersion:   report.SchemaVersion,
		InvocationID:    launch.Invocation.ID,
		RunID:           launch.Invocation.RunID,
		Harness:         launch.Invocation.Harness,
		Role:            "test",
		Stage:           "test",
		Outcome:         report.OutcomeCompleted,
		Summary:         "test infrastructure is unavailable",
		Exemptions:      []report.Exemption{report.ExemptionTechnical},
		NativeSessionID: "test-session",
		ReportedAt:      time.Now().UTC(),
	}
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, value); err != nil {
		t.Fatalf("WriteAtomicForInvocation() error = %v", err)
	}
	accepted, err := service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: run.ID, InvocationID: launch.Invocation.ID})
	if err != nil {
		t.Fatalf("AcceptAgentReport() error = %v", err)
	}
	if accepted.Invocation.Status != store.InvocationStatusWaitingForHuman || runStore.current.Status != store.StatusWaitingForHuman {
		t.Fatalf("disposition = invocation %#v run %#v, want waiting_for_human", accepted.Invocation, runStore.current)
	}
	if runStore.current.Stage != store.StageTest || len(runStore.invocations) != 1 {
		t.Fatalf("technical exemption run = %#v invocations=%d, want paused test without implementation", runStore.current, len(runStore.invocations))
	}
	if workerRuntime.stops == 0 {
		t.Fatal("worker was not stopped after the production-path dispute")
	}
}

// serviceTestWorktree returns the inspecting worktree owned by the agent
// fixture so a test-stage report can expose a test-owned observed path.
func serviceTestWorktree(runStore *agentRunStore) *inspectingWorktree {
	return runStore.worktree
}

// testStageWorkspace is a deterministic host Git workspace for the public
// coordinator test; it records the scoped test checkpoint without shelling out.
type testStageWorkspace struct {
	workspace  gitadapter.Workspace
	state      gitadapter.WorktreeState
	checkpoint gitadapter.CheckpointRequest
}

// Create returns the isolated test-stage workspace.
func (w *testStageWorkspace) Create(context.Context, string, string, string) (gitadapter.Workspace, error) {
	return w.workspace, nil
}

// Remove releases the deterministic test workspace.
func (*testStageWorkspace) Remove(context.Context, string, gitadapter.Workspace) error { return nil }

// Inspect returns the current checkpoint and changed-path projection.
func (w *testStageWorkspace) Inspect(context.Context, string) (gitadapter.WorktreeState, error) {
	return w.state, nil
}

// CreateCheckpoint records a scoped test checkpoint and clears uncommitted test
// paths as a real Git checkpoint would.
func (w *testStageWorkspace) CreateCheckpoint(_ context.Context, request gitadapter.CheckpointRequest) (gitadapter.CheckpointResult, error) {
	w.checkpoint = request
	w.state.HeadSHA = testStageCheckpoint
	w.state.ChangedPaths = nil
	return gitadapter.CheckpointResult{SHA: testStageCheckpoint, Created: true}, nil
}

// Push is unused by the test-stage handoff.
func (*testStageWorkspace) Push(context.Context, gitadapter.PushRequest) error { return nil }

// SynchronizeBase is unused by the test-stage handoff.
func (*testStageWorkspace) SynchronizeBase(context.Context, gitadapter.BaseSyncRequest) error {
	return nil
}

// testStageCheckpoint is the deterministic full commit identity returned by
// the scoped test-checkpoint fake.
const testStageCheckpoint = "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"

var _ gitadapter.GitWorkspace = (*testStageWorkspace)(nil)
