package factory_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestStartAgentUsesTheRealStoreAcrossAClaimProcessBoundary verifies the
// permitted clean handoff with a real SQLite store and a freshly opened service.
func TestStartAgentUsesTheRealStoreAcrossAClaimProcessBoundary(t *testing.T) {
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	worktreePath := filepath.Join(root, "worktree")
	operationalPath := filepath.Join(root, "state", "factory.db")
	if err := mkdir(filepath.Join(repositoryPath, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := mkdir(worktreePath); err != nil {
		t.Fatal(err)
	}
	worktree := &inspectingWorktree{
		fakeWorktree: fakeWorktree{workspace: gitadapter.Workspace{BaseSHA: factoryGateCheckpoint, Branch: "factory/run-real", Worktree: worktreePath}},
		state:        gitadapter.WorktreeState{RepositoryPath: repositoryPath, Branch: "factory/run-real", HeadSHA: factoryGateCheckpoint},
	}
	githubRuntime := &fakeGitHub{issueValue: github.Issue{
		Number: 42, Title: "Real store handoff", Body: "Repository guidance", State: "open", Labels: []string{github.LabelAgentReady},
	}}
	runtime := &agentWorker{}
	terminalRuntime := &agentTerminal{}
	harnessRuntime := &agentHarness{}
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
		Path:                 repositoryPath,
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		Polling:              config.PollingConfig{Interval: "30s", Backoff: "5m"},
		Cmux:                 config.CmuxConfig{ControlWorkspace: "factory-control"},
		OperationalDataPath:  operationalPath,
		RepositoryConfigPath: filepath.Join(repositoryPath, "factory.yaml"),
	}}}
	dependencies := func(runID string) factory.Dependencies {
		return factory.Dependencies{
			Config: &fakeConfig{value: host},
			OpenStore: func(ctx context.Context, path string) (factory.OperationalStore, error) {
				return store.Open(ctx, path)
			},
			LoadRepository: func(string) (config.RepositoryConfig, error) { return validRepositoryConfig(), nil },
			Worker:         runtime, Terminal: terminalRuntime, Harness: harnessRuntime,
			GitHub: githubRuntime, Worktree: worktree,
			Now:         func() time.Time { return time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC) },
			NewRunID:    func() (string, error) { return runID, nil },
			Coordinator: "coordinator-test",
		}
	}

	claimService := factory.NewWithDependencies("/host/config.yaml", dependencies("run-real"))
	claimed, err := claimService.ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}
	if err := githubRuntime.setStatusComment(claimed.Run); err != nil {
		t.Fatal(err)
	}
	recordReadyBaseline(t, operationalPath, claimed.Run)
	// A separate service models the next process opening the same host store.
	startService := factory.NewWithDependencies("/host/config.yaml", dependencies("generated"))
	launch, err := startService.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() after store reopen error = %v", err)
	}
	if launch.Invocation.RunID != claimed.Run.ID || len(runtime.starts) != 1 || len(harnessRuntime.starts) != 1 {
		t.Fatalf("launch = %#v, worker starts = %d, harness starts = %d", launch.Invocation, len(runtime.starts), len(harnessRuntime.starts))
	}

	reopened, err := store.Open(context.Background(), operationalPath)
	if err != nil {
		t.Fatalf("reopen operational store error = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	hasInvocation, err := reopened.HasInvocation(context.Background(), claimed.Run.ID)
	if err != nil {
		t.Fatalf("HasInvocation() error = %v", err)
	}
	if !hasInvocation {
		t.Fatal("HasInvocation() = false after first agent start")
	}
}

// TestStartAgentUsesTheRealStoreToRefuseADiscrepancy verifies a fresh service
// returns recovery-required before worker, terminal, or harness effects.
func TestStartAgentUsesTheRealStoreToRefuseADiscrepancy(t *testing.T) {
	fixture := newRealHandoffFixture(t)
	fixture.worktree.state.HeadSHA = "different-checkpoint"

	_, err := fixture.newService("generated").StartAgent(context.Background(), factory.AgentRequest{})
	var recoveryErr *factory.RecoveryRequiredError
	if !errors.As(err, &recoveryErr) {
		t.Fatalf("StartAgent() error = %v, want RecoveryRequiredError", err)
	}
	if len(fixture.worker.starts) != 0 || len(fixture.terminal.notifications) != 0 || len(fixture.harness.starts) != 0 {
		t.Fatalf("recovery effects: workers=%d notifications=%d harnesses=%d, want none", len(fixture.worker.starts), len(fixture.terminal.notifications), len(fixture.harness.starts))
	}
}

// TestStartAgentUsesTheRealStoreToRefuseInvocationHistory verifies a terminal
// invocation is still history that requires issue #21 reconciliation.
func TestStartAgentUsesTheRealStoreToRefuseInvocationHistory(t *testing.T) {
	fixture := newRealHandoffFixture(t)
	opened, err := store.Open(context.Background(), fixture.operationalPath)
	if err != nil {
		t.Fatalf("reopen operational store error = %v", err)
	}
	if err := opened.SaveInvocation(context.Background(), store.Invocation{
		ID: "inv-terminal", RunID: fixture.claimed.ID, Harness: "codex", Role: "implementation", Stage: store.StageClaim,
		Status: store.InvocationStatusCompleted,
	}); err != nil {
		_ = opened.Close()
		t.Fatalf("SaveInvocation() error = %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("close operational store error = %v", err)
	}

	_, err = fixture.newService("generated").StartAgent(context.Background(), factory.AgentRequest{})
	var recoveryErr *factory.RecoveryRequiredError
	if !errors.As(err, &recoveryErr) {
		t.Fatalf("StartAgent() error = %v, want RecoveryRequiredError", err)
	}
	if !recoveryErr.Diagnosis.InvocationExists {
		t.Fatalf("diagnosis = %#v, want persisted invocation history", recoveryErr.Diagnosis)
	}
	if len(fixture.worker.starts) != 0 || len(fixture.terminal.notifications) != 0 || len(fixture.harness.starts) != 0 {
		t.Fatalf("existing-invocation effects: workers=%d notifications=%d harnesses=%d, want none", len(fixture.worker.starts), len(fixture.terminal.notifications), len(fixture.harness.starts))
	}
}

// TestConcurrentFirstAgentAttemptsUseStoreUniqueness verifies two fresh
// coordinators cannot turn one clean claim into two workers.
func TestConcurrentFirstAgentAttemptsUseStoreUniqueness(t *testing.T) {
	fixture := newRealHandoffFixture(t)
	first, firstWorker, firstHarness := fixture.newIsolatedService("generated-one")
	second, secondWorker, secondHarness := fixture.newIsolatedService("generated-two")
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := first.StartAgent(context.Background(), factory.AgentRequest{})
		results <- err
	}()
	go func() {
		<-start
		_, err := second.StartAgent(context.Background(), factory.AgentRequest{})
		results <- err
	}()
	close(start)
	firstErr, secondErr := <-results, <-results
	if (firstErr == nil) == (secondErr == nil) {
		t.Fatalf("concurrent StartAgent() errors = %v, %v, want exactly one success", firstErr, secondErr)
	}
	if len(firstWorker.starts)+len(secondWorker.starts) != 1 || len(firstHarness.starts)+len(secondHarness.starts) != 1 {
		t.Fatalf("concurrent effects: workers=%d harnesses=%d, want one each", len(firstWorker.starts)+len(secondWorker.starts), len(firstHarness.starts)+len(secondHarness.starts))
	}
}

// realHandoffFixture contains a claimed run and the external projections used
// by tests that reopen a real operational store in a new coordinator.
type realHandoffFixture struct {
	repositoryPath  string
	operationalPath string
	worktree        *inspectingWorktree
	github          *fakeGitHub
	worker          *agentWorker
	terminal        *agentTerminal
	harness         *agentHarness
	host            config.HostConfig
	claimed         store.Run
}

// newRealHandoffFixture performs a real claim so startup tests begin at the
// exact persisted state produced by the issue command.
func newRealHandoffFixture(t *testing.T) *realHandoffFixture {
	t.Helper()
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	worktreePath := filepath.Join(root, "worktree")
	operationalPath := filepath.Join(root, "state", "factory.db")
	if err := mkdir(filepath.Join(repositoryPath, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := mkdir(worktreePath); err != nil {
		t.Fatal(err)
	}
	worktree := &inspectingWorktree{
		fakeWorktree: fakeWorktree{workspace: gitadapter.Workspace{BaseSHA: factoryGateCheckpoint, Branch: "factory/run-real", Worktree: worktreePath}},
		state:        gitadapter.WorktreeState{RepositoryPath: repositoryPath, Branch: "factory/run-real", HeadSHA: factoryGateCheckpoint},
	}
	githubRuntime := &fakeGitHub{issueValue: github.Issue{
		Number: 42, Title: "Real store handoff", Body: "Repository guidance", State: "open", Labels: []string{github.LabelAgentReady},
	}}
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
		Path:                 repositoryPath,
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		Polling:              config.PollingConfig{Interval: "30s", Backoff: "5m"},
		Cmux:                 config.CmuxConfig{ControlWorkspace: "factory-control"},
		OperationalDataPath:  operationalPath,
		RepositoryConfigPath: filepath.Join(repositoryPath, "factory.yaml"),
	}}}
	fixture := &realHandoffFixture{
		repositoryPath:  repositoryPath,
		operationalPath: operationalPath,
		worktree:        worktree,
		github:          githubRuntime,
		worker:          &agentWorker{}, terminal: &agentTerminal{}, harness: &agentHarness{}, host: host,
	}
	claimed, err := fixture.newService("run-real").ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}
	fixture.claimed = claimed.Run
	if err := githubRuntime.setStatusComment(claimed.Run); err != nil {
		t.Fatal(err)
	}
	recordReadyBaseline(t, operationalPath, claimed.Run)
	return fixture
}

// recordReadyBaseline seeds the persisted projection representing the issue
// command's successful pre-edit gate suite for handoff-only tests.
func recordReadyBaseline(t *testing.T, operationalPath string, run store.Run) {
	t.Helper()
	policy := validRepositoryConfig()
	if len(policy.Gates) == 0 {
		t.Fatal("baseline fixture policy has no declared gates")
	}
	declared := policy.Gates[0]
	opened, err := store.Open(context.Background(), operationalPath)
	if err != nil {
		t.Fatalf("open baseline fixture store: %v", err)
	}
	err = opened.SaveGateResults(context.Background(), []store.GateResult{{
		RunID: run.ID, CheckpointSHA: run.CheckpointSHA, Phase: store.GatePhaseBaseline,
		Ordinal: 0, GateName: declared.Name, Outcome: store.GateOutcomePassed,
		Status: string(github.CommitStatusSuccess), Blocking: declared.Blocking,
	}})
	closeErr := opened.Close()
	if err != nil {
		t.Fatalf("save baseline fixture result: %v", err)
	}
	if closeErr != nil {
		t.Fatalf("close baseline fixture store: %v", closeErr)
	}
}

// newService creates a coordinator with a selected run-id generator while
// reopening the fixture's real SQLite store on every command.
func (f *realHandoffFixture) newService(runID string) *factory.Service {
	return factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config: &fakeConfig{value: f.host},
		OpenStore: func(ctx context.Context, path string) (factory.OperationalStore, error) {
			return store.Open(ctx, path)
		},
		LoadRepository: func(string) (config.RepositoryConfig, error) { return validRepositoryConfig(), nil },
		Worker:         f.worker, Terminal: f.terminal, Harness: f.harness,
		GitHub: f.github, Worktree: f.worktree,
		Now:         func() time.Time { return time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC) },
		NewRunID:    func() (string, error) { return runID, nil },
		Coordinator: "coordinator-test",
	})
}

// newIsolatedService creates a coordinator with separate effect recorders but
// the same reopened operational store and read-only projections.
func (f *realHandoffFixture) newIsolatedService(runID string) (*factory.Service, *agentWorker, *agentHarness) {
	githubRuntime := &fakeGitHub{issueValue: f.github.issueValue, statusComment: f.github.statusComment}
	workerRuntime := &agentWorker{}
	terminalRuntime := &agentTerminal{}
	harnessRuntime := &agentHarness{}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config: &fakeConfig{value: f.host},
		OpenStore: func(ctx context.Context, path string) (factory.OperationalStore, error) {
			return store.Open(ctx, path)
		},
		LoadRepository: func(string) (config.RepositoryConfig, error) { return validRepositoryConfig(), nil },
		Worker:         workerRuntime, Terminal: terminalRuntime, Harness: harnessRuntime,
		GitHub: githubRuntime, Worktree: f.worktree,
		Now:         func() time.Time { return time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC) },
		NewRunID:    func() (string, error) { return runID, nil },
		Coordinator: "coordinator-test",
	})
	return service, workerRuntime, harnessRuntime
}

// setStatusComment mirrors the status comment persisted by the claim
// transition so the reopened coordinator can complete its read-only diagnosis.
func (f *fakeGitHub) setStatusComment(run store.Run) error {
	if run.StatusCommentID == "" {
		return &statusCommentFixtureError{message: "claim did not persist a status comment identity"}
	}
	f.statusComment = github.Comment{ID: run.StatusCommentID, Body: "<!-- factory-status: " + run.ID + " -->"}
	return nil
}

// statusCommentFixtureError reports an invalid real-store handoff fixture.
type statusCommentFixtureError struct {
	message string
}

// Error returns the fixture setup failure.
func (e *statusCommentFixtureError) Error() string { return e.message }
