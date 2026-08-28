package factory_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/gate"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestRunBaselineRecordsAHealthyPreEditSuiteBeforeAgentProgression verifies
// the real claim/store boundary records a baseline at the base checkpoint.
func TestRunBaselineRecordsAHealthyPreEditSuiteBeforeAgentProgression(t *testing.T) {
	fixture := newBaselineFixture(t, "", []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}})
	claimed, err := fixture.service.ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}

	baseline, err := fixture.service.RunBaseline(context.Background(), factory.BaselineRequest{RunID: claimed.Run.ID})
	if err != nil {
		t.Fatalf("RunBaseline() error = %v", err)
	}
	if baseline.Run.Status != store.StatusActive || baseline.Run.Stage != store.StageTest || len(baseline.Gates) != 1 || baseline.Gates[0].Phase != gate.PhaseBaseline {
		t.Fatalf("baseline = %#v, want active test stage with one baseline result", baseline)
	}
	if len(fixture.worker.commands) != 2 || fixture.worker.commands[0].Command != fixture.policy.Setup || fixture.worker.commands[1].Command != fixture.policy.Gates[0].Command {
		t.Fatalf("baseline commands = %#v, want setup then declared gate", fixture.worker.commands)
	}
	results, err := fixture.openedGateResults(t, claimed.Run.ID, store.GatePhaseBaseline, claimed.Run.CheckpointSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Outcome != store.GateOutcomePassed || results[0].Status != string(github.CommitStatusSuccess) {
		t.Fatalf("stored baseline results = %#v, want exact-checkpoint success", results)
	}
}

// TestRunBaselineAdvancesToTheConfiguredTestStage verifies a repository that
// declares a test role enters test-stage handoff before implementation.
func TestRunBaselineAdvancesToTheConfiguredTestStage(t *testing.T) {
	fixture := newBaselineFixture(t, "", []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}})
	fixture.policy.RoleHarnessDefaults["test"] = config.HarnessCodex
	fixture.policy.ModelOptions["test"] = []string{"gpt-5"}
	claimed, err := fixture.service.ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}

	baseline, err := fixture.service.RunBaseline(context.Background(), factory.BaselineRequest{RunID: claimed.Run.ID})
	if err != nil {
		t.Fatalf("RunBaseline() error = %v", err)
	}
	if baseline.Run.Stage != store.StageTest || baseline.Run.Status != store.StatusActive {
		t.Fatalf("baseline run = %#v, want active test stage", baseline.Run)
	}
}

// TestRunBaselineStartsImplementationOwnedTDDInAdvisoryMode verifies advisory
// mode advances directly to implementation without creating test-stage state.
func TestRunBaselineStartsImplementationOwnedTDDInAdvisoryMode(t *testing.T) {
	fixture := newBaselineFixture(t, "<!-- factory-test-exemption: human | documentation-only change -->", []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}})
	fixture.policy.TestPolicy.Mode = config.TestModeAdvisory
	fixture.policy.TestPolicy.AllowHumanExemption = true
	delete(fixture.policy.RoleHarnessDefaults, "test")
	delete(fixture.policy.ModelOptions, "test")
	claimed, err := fixture.service.ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}

	baseline, err := fixture.service.RunBaseline(context.Background(), factory.BaselineRequest{RunID: claimed.Run.ID})
	if err != nil {
		t.Fatalf("RunBaseline() error = %v", err)
	}
	if baseline.Run.Stage != store.StageImplementation || baseline.Run.Status != store.StatusActive {
		t.Fatalf("baseline run = %#v, want active implementation stage", baseline.Run)
	}
	if baseline.Run.TestStageSkipped || baseline.Run.TestHandoff != nil || baseline.Run.TestExemption != nil || baseline.Run.TestCheckpointSHA != "" || len(baseline.Run.ProtectedTestPaths) != 0 {
		t.Fatalf("advisory test projection = %#v, want no independent test-stage state", baseline.Run)
	}
	if !strings.Contains(factory.StatusCommentBody(baseline.Run), "advisory") || !strings.Contains(factory.StatusCommentBody(baseline.Run), "implementation-owned TDD") {
		t.Fatalf("advisory status projection = %q, want explicit implementation-owned policy", factory.StatusCommentBody(baseline.Run))
	}
	status, err := fixture.service.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.TestPolicyMode != config.TestModeAdvisory {
		t.Fatalf("status test policy = %q, want advisory", status.TestPolicyMode)
	}
}

// TestAdvisoryTransitionStillRequiresTheHealthyBaseline verifies advisory mode
// removes only the independent test gate, not the deterministic pre-edit gate.
func TestAdvisoryTransitionStillRequiresTheHealthyBaseline(t *testing.T) {
	fixture := newBaselineFixture(t, "", []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}})
	fixture.policy.TestPolicy.Mode = config.TestModeAdvisory
	delete(fixture.policy.RoleHarnessDefaults, "test")
	delete(fixture.policy.ModelOptions, "test")
	claimed, err := fixture.service.ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}
	if _, err := fixture.service.Transition(context.Background(), factory.TransitionRequest{RunID: claimed.Run.ID, Stage: store.StageImplementation, Status: store.StatusActive}); err == nil || !strings.Contains(err.Error(), "baseline") {
		t.Fatalf("Transition() error = %v, want advisory transition to require baseline", err)
	}
}

// TestRunBaselineRecordsTheFrozenHumanTestExemption verifies an exact issue
// marker can skip the default test stage only when repository policy allows it.
func TestRunBaselineRecordsTheFrozenHumanTestExemption(t *testing.T) {
	fixture := newBaselineFixture(t, "<!-- factory-test-exemption: human | documentation-only change -->", []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}})
	fixture.policy.RoleHarnessDefaults["test"] = config.HarnessCodex
	fixture.policy.ModelOptions["test"] = []string{"gpt-5"}
	fixture.policy.TestPolicy.AllowHumanExemption = true
	claimed, err := fixture.service.ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}

	baseline, err := fixture.service.RunBaseline(context.Background(), factory.BaselineRequest{RunID: claimed.Run.ID})
	if err != nil {
		t.Fatalf("RunBaseline() error = %v", err)
	}
	if baseline.Run.Stage != store.StageImplementation || !baseline.Run.TestStageSkipped || baseline.Run.TestExemption == nil || baseline.Run.TestExemption.Justification != "documentation-only change" {
		t.Fatalf("baseline exemption run = %#v, want recorded human skip", baseline.Run)
	}
}

// TestTransitionCannotBypassAConfiguredTestStage verifies the generic stage
// transition seam cannot move a healthy configured run directly to implementation.
func TestTransitionCannotBypassAConfiguredTestStage(t *testing.T) {
	fixture := newBaselineFixture(t, "", []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}})
	fixture.policy.RoleHarnessDefaults["test"] = config.HarnessCodex
	fixture.policy.ModelOptions["test"] = []string{"gpt-5"}
	claimed, err := fixture.service.ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}
	if _, err := fixture.service.Transition(context.Background(), factory.TransitionRequest{RunID: claimed.Run.ID, Stage: store.StageImplementation, Status: store.StatusActive}); err == nil || !strings.Contains(err.Error(), "completed test handoff") {
		t.Fatalf("Transition() error = %v, want test-stage bypass rejection", err)
	}
	opened, err := store.Open(context.Background(), fixture.operationalPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	current, err := opened.CurrentRun(context.Background())
	_ = opened.Close()
	if err != nil {
		t.Fatalf("read current run: %v", err)
	}
	if current == nil || current.Stage != store.StageClaim {
		t.Fatalf("run stage = %#v, want claim after rejected bypass", current)
	}
}

// TestStartAgentRejectsAClaimWithoutBaselineResults verifies the agent seam
// cannot bypass the pre-edit gate boundary when the CLI is not involved.
func TestStartAgentRejectsAClaimWithoutBaselineResults(t *testing.T) {
	fixture := newBaselineFixture(t, "", []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}})
	claimed, err := fixture.service.ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}
	_, err = fixture.service.StartAgent(context.Background(), factory.AgentRequest{RunID: claimed.Run.ID})
	if err == nil || !strings.Contains(err.Error(), "baseline gate results are incomplete") {
		t.Fatalf("StartAgent() error = %v, want missing baseline refusal", err)
	}
	if len(fixture.worker.starts) != 0 {
		t.Fatalf("worker starts = %#v, want no agent effect", fixture.worker.starts)
	}
}

// TestRunBaselineReusesSetupForAnUnchangedDependencyFingerprint verifies the
// durable setup cache is scoped to one exact checkpoint and phase.
func TestRunBaselineReusesSetupForAnUnchangedDependencyFingerprint(t *testing.T) {
	fixture := newBaselineFixture(t, "", []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}, {ExitCode: 0}})
	manifest := filepath.Join(fixture.worktreePath, "go.mod")
	if err := os.WriteFile(manifest, []byte("module unchanged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.policy.SetupFiles = []string{"go.mod"}
	claimed, err := fixture.service.ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}
	if first, err := fixture.service.RunBaseline(context.Background(), factory.BaselineRequest{RunID: claimed.Run.ID}); err != nil {
		t.Fatalf("first RunBaseline() error = %v", err)
	} else if len(first.Gates) != 1 || !first.Gates[0].SetupRan {
		t.Fatalf("first baseline gates = %#v, want setup execution", first.Gates)
	}
	if second, err := fixture.service.RunBaseline(context.Background(), factory.BaselineRequest{RunID: claimed.Run.ID}); err != nil {
		t.Fatalf("second RunBaseline() error = %v", err)
	} else if len(second.Gates) != 1 || second.Gates[0].SetupRan {
		t.Fatalf("second baseline gates = %#v, want cached setup", second.Gates)
	}
	if len(fixture.worker.commands) != 3 || fixture.worker.commands[0].Command != fixture.policy.Setup || fixture.worker.commands[1].Command != fixture.policy.Gates[0].Command || fixture.worker.commands[2].Command != fixture.policy.Gates[0].Command {
		t.Fatalf("setup reuse commands = %#v, want setup then two gate commands", fixture.worker.commands)
	}
}

// TestRunBaselineBlocksAnUnhealthyPreEditSuite verifies a blocking baseline
// failure becomes visible and cannot proceed to an agent invocation.
func TestRunBaselineBlocksAnUnhealthyPreEditSuite(t *testing.T) {
	fixture := newBaselineFixture(t, "", []worker.CommandResult{{ExitCode: 0}, {ExitCode: 9}})
	claimed, err := fixture.service.ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}

	baseline, err := fixture.service.RunBaseline(context.Background(), factory.BaselineRequest{RunID: claimed.Run.ID})
	if err == nil {
		t.Fatal("RunBaseline() succeeded for an unhealthy blocking baseline")
	}
	var suiteFailure *gate.SuiteFailure
	if !errors.As(err, &suiteFailure) {
		t.Fatalf("RunBaseline() error = %v, want SuiteFailure", err)
	}
	if baseline.Run.Status != store.StatusFailed || baseline.Run.Stage != store.StagePreflight {
		t.Fatalf("blocked baseline run = %#v, want failed preflight", baseline.Run)
	}
	if got := fixture.github.replacedHistory[len(fixture.github.replacedHistory)-1]; len(got) != 1 || got[0] != github.LabelAgentFailed {
		t.Fatalf("baseline failure labels = %#v, want agent-failed", got)
	}
	results, err := fixture.openedGateResults(t, claimed.Run.ID, store.GatePhaseBaseline, claimed.Run.CheckpointSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Outcome != store.GateOutcomeFailed {
		t.Fatalf("stored failed baseline results = %#v, want failed result", results)
	}
}

// TestRunBaselineAllowsOnlyTheExplicitlyTargetedFailure verifies the frozen
// issue marker is the sole exception to the unhealthy-baseline boundary.
func TestRunBaselineAllowsAnExplicitlyTargetedFailure(t *testing.T) {
	fixture := newBaselineFixture(t, "<!-- factory-baseline-target: test -->", []worker.CommandResult{{ExitCode: 0}, {ExitCode: 9}})
	claimed, err := fixture.service.ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}

	baseline, err := fixture.service.RunBaseline(context.Background(), factory.BaselineRequest{RunID: claimed.Run.ID})
	if err != nil {
		t.Fatalf("RunBaseline() error = %v, want explicit baseline target to permit progression", err)
	}
	if baseline.Run.Status != store.StatusActive || baseline.Run.Stage != store.StageTest {
		t.Fatalf("targeted baseline run = %#v, want active test stage", baseline.Run)
	}
}

// TestRunBaselineRerunsSetupWhenAConfiguredDependencyInputChanges verifies
// setup fingerprints change when a manifest changes between baseline suites.
func TestRunBaselineRerunsSetupWhenAConfiguredDependencyInputChanges(t *testing.T) {
	fixture := newBaselineFixture(t, "", []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}, {ExitCode: 0}, {ExitCode: 0}})
	manifest := filepath.Join(fixture.worktreePath, "go.mod")
	if err := os.WriteFile(manifest, []byte("module before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.policy.SetupFiles = []string{"go.mod"}
	claimed, err := fixture.service.ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}
	if first, err := fixture.service.RunBaseline(context.Background(), factory.BaselineRequest{RunID: claimed.Run.ID}); err != nil {
		t.Fatalf("first RunBaseline() error = %v", err)
	} else if len(first.Gates) != 1 || !first.Gates[0].SetupRan {
		t.Fatalf("first baseline gates = %#v, want setup execution", first.Gates)
	}
	firstResults, err := fixture.openedGateResults(t, claimed.Run.ID, store.GatePhaseBaseline, claimed.Run.CheckpointSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstResults) != 1 || firstResults[0].SetupFingerprint == "" {
		t.Fatalf("stored first result = %#v, want dependency fingerprint", firstResults)
	}
	firstFingerprint := firstResults[0].SetupFingerprint
	if err := os.WriteFile(manifest, []byte("module after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if second, err := fixture.service.RunBaseline(context.Background(), factory.BaselineRequest{RunID: claimed.Run.ID}); err != nil {
		t.Fatalf("second RunBaseline() error = %v", err)
	} else if len(second.Gates) != 1 || !second.Gates[0].SetupRan {
		t.Fatalf("second baseline gates = %#v, want setup after fingerprint change", second.Gates)
	}
	if len(fixture.worker.commands) != 4 || fixture.worker.commands[0].Command != fixture.policy.Setup || fixture.worker.commands[2].Command != fixture.policy.Setup {
		t.Fatalf("setup rerun commands = %#v, want setup before both suites", fixture.worker.commands)
	}
	results, err := fixture.openedGateResults(t, claimed.Run.ID, store.GatePhaseBaseline, claimed.Run.CheckpointSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].SetupFingerprint == "" || results[0].SetupFingerprint == firstFingerprint {
		t.Fatalf("stored rerun result = %#v, want changed dependency fingerprint", results)
	}
}

// baselineFixture wires a real SQLite operational store to controlled external
// seams so baseline behavior is tested at the factory boundary.
type baselineFixture struct {
	service         *factory.Service
	worktreePath    string
	operationalPath string
	policy          *config.RepositoryConfig
	worker          *gateWorker
	github          *fakeGitHub
}

// newBaselineFixture creates a claimable repository and a deterministic gate
// worker without using Docker or a live GitHub connection.
func newBaselineFixture(t *testing.T, issueBody string, results []worker.CommandResult) *baselineFixture {
	t.Helper()
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	worktreePath := filepath.Join(root, "worktree")
	operationalPath := filepath.Join(root, "state", "factory.db")
	if err := os.MkdirAll(filepath.Join(repositoryPath, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryPath, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktreePath, 0o700); err != nil {
		t.Fatal(err)
	}
	policy := validRepositoryConfig()
	policy.Gates = []config.GateConfig{{Name: "test", Command: "test", Timeout: "1m", Blocking: true, EnvironmentPolicy: config.EnvironmentPolicyClean}}
	policyRef := &policy
	githubRuntime := &fakeGitHub{issueValue: github.Issue{Number: 42, Title: "Baseline", Body: issueBody, State: "open", Labels: []string{github.LabelAgentReady}}}
	workerRuntime := &gateWorker{results: results}
	worktree := &draftGitWorkspace{
		workspace: gitadapter.Workspace{BaseSHA: factoryGateCheckpoint, Branch: "factory/run-baseline", Worktree: worktreePath},
		state:     gitadapter.WorktreeState{Branch: "factory/run-baseline", HeadSHA: factoryGateCheckpoint},
	}
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
		Path:                 repositoryPath,
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		OperationalDataPath:  operationalPath,
		RepositoryConfigPath: filepath.Join(repositoryPath, config.RepositoryConfigFileName),
	}}}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:         &fakeConfig{value: host},
		OpenStore:      func(ctx context.Context, path string) (factory.OperationalStore, error) { return store.Open(ctx, path) },
		LoadRepository: func(string) (config.RepositoryConfig, error) { return *policyRef, nil },
		GitHub:         githubRuntime, Worktree: worktree, GitWorkspace: worktree, Worker: workerRuntime,
		CommitStatuses: &gateStatuses{},
		Now:            func() time.Time { return time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC) },
		NewRunID:       func() (string, error) { return "run-baseline", nil },
		Coordinator:    "coordinator-test",
	})
	return &baselineFixture{service: service, worktreePath: worktreePath, operationalPath: operationalPath, policy: policyRef, worker: workerRuntime, github: githubRuntime}
}

// openedGateResults reopens the real store and selects one exact baseline
// projection for assertions about checkpoint identity and setup fingerprints.
func (f *baselineFixture) openedGateResults(t *testing.T, runID string, phase store.GatePhase, checkpoint string) ([]store.GateResult, error) {
	t.Helper()
	opened, err := store.Open(context.Background(), f.operationalPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = opened.Close() }()
	return opened.GateResults(context.Background(), runID, phase, checkpoint)
}
