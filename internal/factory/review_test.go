package factory_test

import (
	"context"
	"encoding/json"
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

const reviewCheckpoint = "fedcbafedcbafedcbafedcbafedcbafedcbafedcbafedcbafedcbafedcbafedc"

// TestSpecificationReviewUsesAnImmutablePacketAndRoutesAdvisories verifies
// the independent reviewer receives the exact diff and structured handoffs,
// while taste/scope findings remain visible without blocking readiness.
func TestSpecificationReviewUsesAnImmutablePacketAndRoutesAdvisories(t *testing.T) {
	fixture := newReviewFixture(t)
	launch, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	if launch.Invocation.Role != "spec_review" || launch.Invocation.Stage != store.StageReview {
		t.Fatalf("review invocation = %#v, want spec_review/review", launch.Invocation)
	}
	if len(fixture.statuses.values) != 1 || fixture.statuses.values[0].State != github.CommitStatusPending || fixture.statuses.values[0].SHA != reviewCheckpoint || fixture.statuses.values[0].Context != factory.SpecificationReviewStatusContext {
		t.Fatalf("review statuses after launch = %#v, want pending exact-checkpoint status", fixture.statuses.values)
	}
	if len(fixture.worker.starts) != 1 || len(fixture.worker.commands) != 1 || len(fixture.harness.starts) != 1 || !strings.Contains(fixture.worker.commands[0].Command, reviewCheckpoint) {
		t.Fatalf("review worker effects = starts %#v commands %#v, want one exact-diff command", fixture.worker.starts, fixture.worker.commands)
	}
	packetData, err := os.ReadFile(filepath.Join(launch.Invocation.InvocationDirectory, "specification.json"))
	if err != nil {
		t.Fatalf("read review packet: %v", err)
	}
	var packet factory.InvocationPacket
	if err := json.Unmarshal(packetData, &packet); err != nil {
		t.Fatalf("decode review packet: %v", err)
	}
	if packet.ReviewContext == nil || packet.ReviewContext.CheckpointSHA != reviewCheckpoint || packet.ReviewContext.CurrentDiff != "diff --git a/internal/factory/review.go b/internal/factory/review.go\n" || packet.ReviewContext.ImplementationHandoff == nil || packet.ReviewContext.TestExemption == nil || len(packet.ReviewContext.RelevantLogs) == 0 {
		t.Fatalf("review context = %#v, want exact checkpoint, diff, and implementation handoff", packet.ReviewContext)
	}
	for _, marker := range []string{"specification-review-v1", "exact checkpoint", "--finding", "no upstream harness transcript"} {
		if !strings.Contains(strings.ToLower(launch.Prompt), strings.ToLower(marker)) {
			t.Errorf("review prompt missing %q:\n%s", marker, launch.Prompt)
		}
	}

	reportValue := reviewReport(launch, []report.ReviewFinding{
		{Location: "internal/factory/review.go:20", Claim: "scope could be narrower", Evidence: "the helper is reusable", Severity: report.ReviewSeverityBlocker, Category: report.ReviewCategoryScope, SuggestedResolution: "keep the helper", SuggestedOwner: "implementation"},
		{Location: "internal/factory/review.go:21", Claim: "name is subjective", Evidence: "no behavior changes", Severity: report.ReviewSeverityAdvisory, Category: report.ReviewCategoryTaste, SuggestedResolution: "consider a shorter name", SuggestedOwner: "implementation"},
	})
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, reportValue); err != nil {
		t.Fatalf("write review report: %v", err)
	}
	accepted, err := fixture.service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{RunID: launch.Invocation.RunID, InvocationID: launch.Invocation.ID})
	if err != nil {
		t.Fatalf("AcceptAgentReport() error = %v", err)
	}
	if accepted.Report.ReviewHandoff == nil || fixture.runStore.current.Stage != store.StageReady || fixture.runStore.current.Status != store.StatusActive {
		t.Fatalf("accepted review = %#v run=%#v, want ready/active", accepted.Report, fixture.runStore.current)
	}
	if len(fixture.statuses.values) != 2 || fixture.statuses.values[1].State != github.CommitStatusSuccess {
		t.Fatalf("review statuses after acceptance = %#v, want pending then success", fixture.statuses.values)
	}
	body := fixture.pullRequests.updatedRequests[len(fixture.pullRequests.updatedRequests)-1].Body
	for _, marker := range []string{"Specification review", "scope", "taste", "suggested owner", "keep the helper"} {
		if !strings.Contains(body, marker) {
			t.Errorf("PR review projection missing %q: %s", marker, body)
		}
	}
}

// TestSpecificationReviewBlocksOnlyConcreteViolations verifies a correctness
// blocker moves the run to human review and publishes failure for the exact SHA.
func TestSpecificationReviewBlocksOnlyConcreteViolations(t *testing.T) {
	fixture := newReviewFixture(t)
	launch, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	reportValue := reviewReport(launch, []report.ReviewFinding{{
		Location: "internal/factory/review.go:1", Claim: "checkpoint is not immutable", Evidence: "worktree can be changed", Severity: report.ReviewSeverityBlocker, Category: report.ReviewCategoryCorrectness, SuggestedResolution: "reject changed paths", SuggestedOwner: "implementation",
	}})
	if _, err := report.WriteAtomicForInvocation(launch.Invocation.ResultDirectory, launch.Invocation.ID, reportValue); err != nil {
		t.Fatalf("write review report: %v", err)
	}
	if _, err := fixture.service.AcceptAgentReport(context.Background(), factory.AgentReportRequest{InvocationID: launch.Invocation.ID}); err != nil {
		t.Fatalf("AcceptAgentReport() error = %v", err)
	}
	if fixture.runStore.current.Stage != store.StageReview || fixture.runStore.current.Status != store.StatusWaitingForHuman {
		t.Fatalf("blocked run = %#v, want review/waiting_for_human", fixture.runStore.current)
	}
	if got := fixture.statuses.values[len(fixture.statuses.values)-1].State; got != github.CommitStatusFailure {
		t.Fatalf("final review status = %q, want failure", got)
	}
}

// reviewFixture bundles the Factory seam and all isolated review projections.
type reviewFixture struct {
	service      *factory.Service
	runStore     *agentRunStore
	worker       *agentWorker
	harness      *agentHarness
	statuses     *gateStatuses
	pullRequests *fakePullRequests
}

// newReviewFixture creates a draft-PR run with a valid baseline and checkpoint.
func newReviewFixture(t *testing.T) reviewFixture {
	t.Helper()
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	worktreePath := filepath.Join(root, "worktree")
	if err := os.MkdirAll(filepath.Join(repositoryPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryPath, ".git", "HEAD"), []byte("ref: refs/heads/factory/run-review\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := validRepositoryConfig()
	policy.RoleHarnessDefaults["spec_review"] = config.HarnessCodex
	policy.ModelOptions["spec_review"] = []string{"gpt-5"}
	issue := github.Issue{Number: 42, Title: "Review the checkpoint", Body: "Review the exact implementation checkpoint.", State: "open", Labels: []string{github.LabelAgentReady}}
	githubAdapter := &fakeGitHub{issueValue: issue}
	statuses := &gateStatuses{}
	pullRequests := &fakePullRequests{existing: github.PullRequest{Number: 17, URL: "https://github.com/example/project/pull/17", Body: "<!-- factory-generated:start -->\nold\n<!-- factory-generated:end -->", State: "open", Draft: true, HeadBranch: "factory/run-review", BaseBranch: "main"}}
	worktree := &inspectingWorktree{
		fakeWorktree: fakeWorktree{workspace: gitadapter.Workspace{BaseSHA: factoryGateCheckpoint, Branch: "factory/run-review", Worktree: worktreePath}},
		state:        gitadapter.WorktreeState{RepositoryPath: repositoryPath, Branch: "factory/run-review", HeadSHA: factoryGateCheckpoint},
	}
	runStore := &agentRunStore{runs: map[string]store.Run{}, invocations: map[string]store.Invocation{}, gateResults: map[string][]store.GateResult{}, worktree: worktree}
	runtime := &agentWorker{results: []worker.CommandResult{{ExitCode: 0, Stdout: "diff --git a/internal/factory/review.go b/internal/factory/review.go\n"}}}
	terminalRuntime := &agentTerminal{}
	harnessRuntime := &agentHarness{}
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
		Path: repositoryPath, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
		Cmux: config.CmuxConfig{ControlWorkspace: "factory-control"}, OperationalDataPath: filepath.Join(root, "state", "factory.db"), RepositoryConfigPath: filepath.Join(repositoryPath, "factory.yaml"),
	}}}
	ids := []string{"run-review", "review-session"}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:         &fakeConfig{value: host},
		OpenStore:      func(context.Context, string) (factory.OperationalStore, error) { return runStore, nil },
		LoadRepository: func(string) (config.RepositoryConfig, error) { return policy, nil },
		GitHub:         &fakeGitHubWithPullRequests{fakeGitHub: githubAdapter},
		PullRequests:   pullRequests,
		Worktree:       worktree,
		Worker:         runtime,
		Terminal:       terminalRuntime,
		Harness:        harnessRuntime,
		CommitStatuses: statuses,
		Now:            func() time.Time { return time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC) },
		NewRunID: func() (string, error) {
			id := ids[0]
			ids = ids[1:]
			return id, nil
		},
	})
	claimed, err := service.ClaimIssue(context.Background(), issue.Number)
	if err != nil {
		t.Fatalf("ClaimIssue() fixture setup error = %v", err)
	}
	run := claimed.Run
	run.TestStageSkipped = true
	run.TestExemption = &store.TestExemption{Kind: "human", Justification: "review fixture"}
	run.ImplementationHandoff = &store.ImplementationHandoff{
		ChangeSummary:          "implemented review behavior",
		AcceptanceMapping:      []store.HandoffAcceptance{{Criterion: "review behavior", Evidence: "focused test"}},
		ProductionFilesChanged: []string{"internal/factory/review.go"},
		FocusedCommands:        []string{"go test ./internal/factory"},
	}
	run.Stage = store.StageDraftPR
	run.Status = store.StatusActive
	run.BaseCheckpointSHA = factoryGateCheckpoint
	run.CheckpointSHA = reviewCheckpoint
	run.PullRequestNumber = pullRequests.existing.Number
	run.PullRequestURL = pullRequests.existing.URL
	if err := runStore.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun() review fixture error = %v", err)
	}
	githubAdapter.statusComment = github.Comment{ID: run.StatusCommentID, Body: "<!-- factory-status: " + run.ID + " -->"}
	worktree.state.HeadSHA = reviewCheckpoint
	worktree.state.ChangedPaths = nil
	runStore.gateResults[run.ID] = []store.GateResult{
		{RunID: run.ID, CheckpointSHA: factoryGateCheckpoint, Phase: store.GatePhaseBaseline, Ordinal: 0, GateName: policy.Gates[0].Name, Outcome: store.GateOutcomePassed, Status: string(github.CommitStatusSuccess), Blocking: policy.Gates[0].Blocking},
		{RunID: run.ID, CheckpointSHA: reviewCheckpoint, Phase: store.GatePhaseCheckpoint, Ordinal: 0, GateName: policy.Gates[0].Name, Outcome: store.GateOutcomePassed, Status: string(github.CommitStatusSuccess), Blocking: policy.Gates[0].Blocking},
	}
	return reviewFixture{service: service, runStore: runStore, worker: runtime, harness: harnessRuntime, statuses: statuses, pullRequests: pullRequests}
}

// reviewReport creates a valid completed review report for a launched session.
func reviewReport(launch factory.AgentLaunchResult, findings []report.ReviewFinding) report.Report {
	return report.Report{
		SchemaVersion: report.SchemaVersion, InvocationID: launch.Invocation.ID, RunID: launch.Invocation.RunID,
		Harness: launch.Invocation.Harness, Role: launch.Invocation.Role, Stage: string(launch.Invocation.Stage),
		Outcome: report.OutcomeCompleted, Summary: "review complete", ReviewHandoff: &report.ReviewHandoff{
			ReviewedSHA: reviewCheckpoint, Findings: findings,
		}, NativeSessionID: "session-review", ReportedAt: time.Now().UTC(),
	}
}
