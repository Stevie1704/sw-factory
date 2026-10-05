package factory_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// TestAdvisoryGateFailureReachesReadiness drives one run from claim through
// an accepted gate suite and an independent review to a ready pull request
// while an independent non-blocking gate fails. It uses a real operational
// store and fake external adapters, so no GitHub state changes.
func TestAdvisoryGateFailureReachesReadiness(t *testing.T) {
	fixture := newAdvisoryReadinessFixture(t)
	ctx := context.Background()

	claimed, err := fixture.service.ClaimIssue(ctx, 42)
	if err != nil {
		t.Fatalf("ClaimIssue() error = %v", err)
	}
	if _, err := fixture.service.RunBaseline(ctx, factory.BaselineRequest{RunID: claimed.Run.ID}); err != nil {
		t.Fatalf("RunBaseline() error = %v", err)
	}
	implementation, err := fixture.service.StartAgent(ctx, factory.AgentRequest{RunID: claimed.Run.ID})
	if err != nil {
		t.Fatalf("StartAgent(implementation) error = %v", err)
	}
	fixture.workspace.state.ChangedPaths = []string{"internal/factory/advisory.go"}
	fixture.acceptReport(t, implementation, report.Report{
		Outcome: report.OutcomeCompleted, Summary: "implementation complete",
		Handoff: &report.Handoff{
			ChangeSummary:          "implemented advisory behavior",
			AcceptanceMapping:      []report.AcceptanceMapping{{Criterion: "advisory readiness", Evidence: "factory regression"}},
			ProductionFilesChanged: []string{"internal/factory/advisory.go"},
			FocusedCommands:        []string{"go test ./internal/factory"},
		},
	})

	// Setup passes, the independent advisory gate fails, and the required
	// gate passes at the implementation checkpoint.
	fixture.worker.results = append(fixture.worker.results, worker.CommandResult{ExitCode: 0}, worker.CommandResult{ExitCode: 3}, worker.CommandResult{ExitCode: 0})
	draft, err := fixture.service.CreateDraftPullRequest(ctx, factory.DraftPullRequestRequest{RunID: claimed.Run.ID})
	if err != nil {
		t.Fatalf("CreateDraftPullRequest() error = %v", err)
	}
	if draft.Repair != nil || draft.Run.Stage != store.StageDraftPR || draft.Run.CheckRepairAttempts != 0 {
		t.Fatalf("draft result = %#v, want a draft PR without spending repair budget on an advisory failure", draft)
	}
	fixture.pullRequests.existing = fixture.pullRequests.created
	fixture.pullRequests.existing.HeadSHA = draft.Run.CheckpointSHA

	review, err := fixture.service.StartAgent(ctx, factory.AgentRequest{RunID: claimed.Run.ID})
	if err != nil {
		t.Fatalf("StartAgent(review) error = %v", err)
	}
	if review.Invocation.Role != "spec_review" {
		t.Fatalf("review invocation = %#v, want spec_review", review.Invocation)
	}
	fixture.acceptReport(t, review, report.Report{
		Outcome: report.OutcomeCompleted, Summary: "review complete",
		ReviewHandoff: &report.ReviewHandoff{ReviewedSHA: draft.Run.CheckpointSHA, UnitID: review.Invocation.ReviewUnitID},
	})

	ready := fixture.currentRun(t)
	if ready.Stage != store.StageReady || ready.Status != store.StatusActive || ready.CheckRepairAttempts != 0 {
		t.Fatalf("run after review = %#v, want ready/active without check repair", ready)
	}
	if fixture.pullRequests.existing.Draft {
		t.Fatal("pull request remained draft despite only an advisory gate failure")
	}
	results := fixture.checkpointGateResults(t, ready)
	if len(results) != 2 || results[0].GateName != "advisory" || results[0].Outcome != store.GateOutcomeFailed || results[0].Status != string(github.CommitStatusFailure) || results[1].Outcome != store.GateOutcomePassed {
		t.Fatalf("persisted checkpoint results = %#v, want the advisory failure retained beside the required success", results)
	}
	if !fixture.publishedStatus("advisory", draft.Run.CheckpointSHA, github.CommitStatusFailure) {
		t.Fatalf("published statuses = %#v, want an exact-checkpoint advisory failure", fixture.statuses.values)
	}
	body := fixture.pullRequests.createdRequests[0].Body
	if !strings.Contains(body, "advisory") || !strings.Contains(body, "failed") {
		t.Fatalf("generated PR body = %q, want the advisory failure visible", body)
	}

	// A replayed review acceptance keeps the run ready and never rewrites the
	// advisory failure evidence to success. Unattended readiness retries use
	// the same final-checkpoint validator, covered by its unit tests.
	if _, err := fixture.service.AcceptAgentReport(ctx, factory.AgentReportRequest{RunID: review.Invocation.RunID, InvocationID: review.Invocation.ID}); err != nil {
		t.Fatalf("replayed AcceptAgentReport() error = %v", err)
	}
	replayed := fixture.currentRun(t)
	if replayed.Stage != store.StageReady || fixture.pullRequests.existing.Draft {
		t.Fatalf("run after replay = %#v draft=%v, want ready non-draft", replayed, fixture.pullRequests.existing.Draft)
	}
	again := fixture.checkpointGateResults(t, replayed)
	if len(again) != 2 || again[0].Outcome != store.GateOutcomeFailed || again[0].Status != string(github.CommitStatusFailure) {
		t.Fatalf("checkpoint results after replay = %#v, want unchanged advisory failure", again)
	}
}

// advisoryReadinessFixture wires a real operational store to fake Git,
// GitHub, worker, and harness adapters for a claim-to-ready run.
type advisoryReadinessFixture struct {
	service         *factory.Service
	operationalPath string
	workspace       *reviewableDraftWorkspace
	worker          *agentWorker
	statuses        *gateStatuses
	pullRequests    *fakePullRequests
}

// newAdvisoryReadinessFixture declares an independent advisory gate before a
// required gate and selects implementation-owned tests so the run reaches
// review without a separate test stage.
func newAdvisoryReadinessFixture(t *testing.T) *advisoryReadinessFixture {
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
	policy.Gates = []config.GateConfig{
		{Name: "advisory", Command: "advisory", Timeout: "1m", Blocking: false, EnvironmentPolicy: config.EnvironmentPolicyClean},
		{Name: "required", Command: "required", Timeout: "1m", Blocking: true, EnvironmentPolicy: config.EnvironmentPolicyClean},
	}
	policy.TestPolicy.Mode = config.TestModeAdvisory
	delete(policy.RoleHarnessDefaults, "test")
	delete(policy.ModelOptions, "test")
	policy.RoleHarnessDefaults["spec_review"] = config.HarnessCodex
	policy.ModelOptions["spec_review"] = []string{"gpt-5"}
	issue := github.Issue{Number: 42, Title: "Honor advisory gates", Body: "Reach readiness with an advisory gate failure.", State: "open", Labels: []string{github.LabelAgentReady}}
	workspace := &reviewableDraftWorkspace{draftGitWorkspace: &draftGitWorkspace{
		workspace: gitadapter.Workspace{BaseSHA: factoryGateCheckpoint, Branch: "factory/run-advisory", Worktree: worktreePath},
		state:     gitadapter.WorktreeState{RepositoryPath: repositoryPath, Branch: "factory/run-advisory", HeadSHA: factoryGateCheckpoint},
	}}
	// Baseline: setup plus both gates pass before any agent edit.
	workerRuntime := &agentWorker{results: []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}, {ExitCode: 0}}}
	statuses := &gateStatuses{}
	pullRequests := &fakePullRequests{created: github.PullRequest{Number: 21, URL: "https://github.com/example/project/pull/21", State: "open", Draft: true, HeadBranch: "factory/run-advisory", BaseBranch: "main"}}
	host := config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{{
		Path: repositoryPath, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"}, AuthorizedUsers: []string{"alice"},
		OperationalDataPath: operationalPath, RepositoryConfigPath: filepath.Join(repositoryPath, config.RepositoryConfigFileName),
		Authentication: config.AuthenticationConfig{CodexAuthPath: filepath.Join(root, "codex-auth.json")},
	}}}
	ids := []string{"run-advisory", "implementation", "review"}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:            &fakeConfig{value: host},
		OpenStore:         func(ctx context.Context, path string) (factory.OperationalStore, error) { return store.Open(ctx, path) },
		LoadRepository:    func(string) (config.RepositoryConfig, error) { return policy, nil },
		GitHub:            &fakeGitHubWithPullRequests{fakeGitHub: &fakeGitHub{issueValue: issue}},
		PullRequests:      pullRequests,
		Worktree:          workspace,
		GitWorkspace:      workspace,
		Worker:            workerRuntime,
		HeadlessHarnesses: testHeadlessHarnesses(&agentHarness{}),
		CommitStatuses:    statuses,
		Now:               func() time.Time { return time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC) },
		NewRunID: func() (string, error) {
			id := ids[0]
			ids = ids[1:]
			return id, nil
		},
		Coordinator: "coordinator-test",
	})
	return &advisoryReadinessFixture{service: service, operationalPath: operationalPath, workspace: workspace, worker: workerRuntime, statuses: statuses, pullRequests: pullRequests}
}

// acceptReport completes one launched invocation with the given role payload.
func (f *advisoryReadinessFixture) acceptReport(t *testing.T, launch factory.AgentLaunchResult, value report.Report) {
	t.Helper()
	value.SchemaVersion = report.SchemaVersion
	value.InvocationID = launch.Invocation.ID
	value.RunID = launch.Invocation.RunID
	value.Harness = launch.Invocation.Harness
	value.Role = launch.Invocation.Role
	value.Stage = string(launch.Invocation.Stage)
	value.NativeSessionID = "session-" + launch.Invocation.Role
	value.ReportedAt = time.Now().UTC()
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, value); err != nil {
		t.Fatalf("write %s report: %v", launch.Invocation.Role, err)
	}
	if _, err := f.service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: launch.Invocation.RunID, InvocationID: launch.Invocation.ID}); err != nil {
		t.Fatalf("AcceptAgentReport(%s) error = %v", launch.Invocation.Role, err)
	}
}

// currentRun reopens the real store and returns the active or latest run.
func (f *advisoryReadinessFixture) currentRun(t *testing.T) store.Run {
	t.Helper()
	opened, err := store.Open(context.Background(), f.operationalPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = opened.Close() }()
	run, err := opened.CurrentRun(context.Background())
	if err != nil || run == nil {
		t.Fatalf("CurrentRun() = %#v, %v", run, err)
	}
	return *run
}

// checkpointGateResults reads the persisted final-checkpoint projection.
func (f *advisoryReadinessFixture) checkpointGateResults(t *testing.T, run store.Run) []store.GateResult {
	t.Helper()
	opened, err := store.Open(context.Background(), f.operationalPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = opened.Close() }()
	results, err := opened.GateResults(context.Background(), run.ID, store.GatePhaseCheckpoint, run.CheckpointSHA)
	if err != nil {
		t.Fatalf("GateResults() error = %v", err)
	}
	return results
}

// publishedStatus reports whether one exact-checkpoint gate status was sent.
func (f *advisoryReadinessFixture) publishedStatus(gateName, sha string, state github.CommitStatusState) bool {
	for _, status := range f.statuses.values {
		if status.SHA == sha && status.State == state && strings.HasSuffix(status.Context, gateName) {
			return true
		}
	}
	return false
}

// reviewableDraftWorkspace adds the read-only review diff projection to the
// recording draft workspace.
type reviewableDraftWorkspace struct {
	*draftGitWorkspace
}

// StreamDiff writes a fixed diff for the reviewer.
func (*reviewableDraftWorkspace) StreamDiff(_ context.Context, _, _, _ string, destination io.Writer) error {
	_, err := io.WriteString(destination, "diff --git a/internal/factory/advisory.go b/internal/factory/advisory.go\n")
	return err
}
