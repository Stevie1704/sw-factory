package factory_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestStartDrivesAnAdvisoryIssueFromQueueToDraftPullRequest is the end-to-end
// proof for unattended claim-to-draft-PR progression: one `factory start`
// takes an eligible advisory-policy issue through baseline, implementation,
// the checkpoint gate suite, the branch push, and one draft pull request
// without any stage-driving CLI command.
func TestStartDrivesAnAdvisoryIssueFromQueueToDraftPullRequest(t *testing.T) {
	t.Parallel()

	fixture := newUnattendedFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture.stopWhenPullRequestExists(cancel)

	if err := fixture.service.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if fixture.commands.claimed != 7 {
		t.Fatalf("claimed issue = %d, want the only eligible issue 7", fixture.commands.claimed)
	}
	if len(fixture.harness.starts) != 1 || fixture.harness.starts[0].Role != "implementation" {
		t.Fatalf("harness starts = %#v, want one implementation invocation", fixture.harness.starts)
	}
	if len(fixture.workspace.checkpoints) != 1 || len(fixture.workspace.pushes) != 1 {
		t.Fatalf("git effects = checkpoints %d pushes %d, want one each", len(fixture.workspace.checkpoints), len(fixture.workspace.pushes))
	}
	if len(fixture.pullRequests.createdRequests) != 1 {
		t.Fatalf("pull request creates = %d, want exactly one", len(fixture.pullRequests.createdRequests))
	}

	final := fixture.currentRun(t)
	if final.Stage != store.StageDraftPR || final.PullRequestNumber != 17 {
		t.Fatalf("final run = stage %q pull request #%d, want draft_pr/#17", final.Stage, final.PullRequestNumber)
	}
	if final.CheckpointSHA != implementationCheckpoint {
		t.Fatalf("final checkpoint = %q, want the implementation checkpoint", final.CheckpointSHA)
	}
	if len(final.ActiveInvocationIDs) != 0 {
		t.Fatalf("final active invocations = %#v, want none once no harness executes", final.ActiveInvocationIDs)
	}
	if final.TestHandoff != nil || final.TestCheckpointSHA != "" || len(final.ProtectedTestPaths) != 0 || final.TestStageSkipped {
		t.Fatalf("advisory run = %#v, want no independent test-stage artifacts", final)
	}
	for _, request := range fixture.worker.starts {
		if request.Role == "test" {
			t.Fatal("advisory progression started a test-role worker")
		}
	}
}

// TestUnattendedProgressionStopsAtAWaitingStateAndPublishesIt verifies that a
// run parked in a human waiting state stops unattended progression instead of
// relaunching a role, and that the published status distinguishes it from a
// run whose harness is executing.
func TestUnattendedProgressionStopsAtAWaitingStateAndPublishesIt(t *testing.T) {
	t.Parallel()

	fixture := newUnattendedFixture(t)
	fixture.agentReport = func(request harness.StartRequest) report.Report {
		value := fixture.completedReport(request)
		value.Outcome = report.OutcomeNeedsClarification
		value.Handoff = nil
		value.Questions = []report.Question{{ID: "q1", Prompt: "Which target branch should the change use?"}}
		return value
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture.stopAfterLeaseRenewals(cancel, 3)

	if err := fixture.service.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if len(fixture.harness.starts) != 1 {
		t.Fatalf("harness starts = %d, want one invocation before the waiting state", len(fixture.harness.starts))
	}
	if len(fixture.workspace.checkpoints) != 0 || len(fixture.pullRequests.createdRequests) != 0 {
		t.Fatalf("waiting run produced checkpoints=%d pull requests=%d, want none", len(fixture.workspace.checkpoints), len(fixture.pullRequests.createdRequests))
	}
	final := fixture.currentRun(t)
	if final.Status != store.StatusWaitingForHuman {
		t.Fatalf("final status = %q, want waiting_for_human", final.Status)
	}
	if activity := factory.RunActivityFor(final); activity != factory.ActivityWaitingForHuman {
		t.Fatalf("activity = %q, want waiting-for-human", activity)
	}
	if !strings.Contains(factory.StatusCommentBody(final), "- activity: `waiting-for-human`") {
		t.Fatalf("status comment = %q, want the waiting activity", factory.StatusCommentBody(final))
	}
}

// TestFailedLaunchCanRetryTheSameRoleAndPacket verifies the complete recovery
// seam: unattended progression voids a launch with no native evidence, the
// operator retry reopens the existing stage, and a fresh invocation reuses the
// same specification packet.
func TestFailedLaunchCanRetryTheSameRoleAndPacket(t *testing.T) {
	fixture := newUnattendedFixture(t)
	fixture.harness.startErr = errors.New("prompt exceeds launch limit")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture.stopAfterLeaseRenewals(cancel, 3)

	if err := fixture.service.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waiting := fixture.currentRun(t)
	if waiting.Status != store.StatusWaitingForHuman || waiting.Stage != store.StageImplementation {
		t.Fatalf("waiting run = stage %q status %q, want implementation/waiting_for_human", waiting.Stage, waiting.Status)
	}
	if !strings.Contains(waiting.LifecycleReason, "harness launch produced no native session or structured report") || !strings.Contains(waiting.LifecycleReason, "/factory retry") {
		t.Fatalf("lifecycle reason = %q, want the void rule and retry command", waiting.LifecycleReason)
	}

	opened, err := store.Open(context.Background(), fixture.operationalPath)
	if err != nil {
		t.Fatalf("open operational store after failed launch: %v", err)
	}
	failed, err := opened.LatestInvocation(context.Background(), waiting.ID)
	if err != nil {
		_ = opened.Close()
		t.Fatalf("read failed invocation: %v", err)
	}
	if failed == nil || failed.Status != store.InvocationStatusSuperseded || !failed.LaunchVoided || failed.Role != "implementation" || failed.Stage != store.StageImplementation {
		_ = opened.Close()
		t.Fatalf("failed invocation = %#v, want superseded implementation launch", failed)
	}
	failedPacket, err := os.ReadFile(filepath.Join(failed.InvocationDirectory, "specification.json"))
	if err != nil {
		_ = opened.Close()
		t.Fatalf("read failed invocation packet: %v", err)
	}
	var failedPacketValue factory.InvocationPacket
	if err := json.Unmarshal(failedPacket, &failedPacketValue); err != nil {
		_ = opened.Close()
		t.Fatalf("decode failed invocation packet: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("close failed-launch store: %v", err)
	}

	retry, err := fixture.restart().HandleCommand(context.Background(), factory.CommandRequest{
		IssueNumber: waiting.IssueNumber,
		Comment:     github.Comment{ID: "retry-failed-launch", Author: "alice", Body: "/factory retry"},
	})
	if err != nil {
		t.Fatalf("HandleCommand() error = %v", err)
	}
	if retry.Outcome != factory.CommandAccepted || retry.Run.Status != store.StatusActive || retry.Run.SpecificationPacket != waiting.SpecificationPacket {
		t.Fatalf("retry result = %#v, want accepted active run with the same packet", retry)
	}

	fixture.harness.startErr = nil
	launch, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{RunID: waiting.ID})
	if err != nil {
		t.Fatalf("StartAgent() after retry error = %v", err)
	}
	if launch.Invocation.ID == failed.ID || launch.Invocation.Role != failed.Role || launch.Invocation.Stage != failed.Stage {
		t.Fatalf("recovered invocation = %#v, want a fresh %s/%s invocation", launch.Invocation, failed.Role, failed.Stage)
	}
	recoveredPacket, err := os.ReadFile(filepath.Join(launch.Invocation.InvocationDirectory, "specification.json"))
	if err != nil {
		t.Fatalf("read recovered invocation packet: %v", err)
	}
	var recoveredPacketValue factory.InvocationPacket
	if err := json.Unmarshal(recoveredPacket, &recoveredPacketValue); err != nil {
		t.Fatalf("decode recovered invocation packet: %v", err)
	}
	if recoveredPacketValue.SpecificationPacket != failedPacketValue.SpecificationPacket {
		t.Fatalf("recovered specification packet changed from %q to %q", failedPacketValue.SpecificationPacket, recoveredPacketValue.SpecificationPacket)
	}
}

// TestStartLaunchesOnlyTheStageTheFrozenRouteSelects verifies the acceptance
// route runs the independent test role first under unattended progression,
// and that no implementation invocation, checkpoint, or pull request is
// created while that invocation is still executing.
func TestStartLaunchesOnlyTheStageTheFrozenRouteSelects(t *testing.T) {
	t.Parallel()

	fixture := newUnattendedFixtureWith(t, config.TestPolicy{Mode: config.TestModeAdvisory},
		"Author the acceptance contract.\n<!-- factory-route: acceptance -->\n")
	// An invocation that has not written a report keeps the coordinator waiting.
	fixture.agentReport = func(harness.StartRequest) report.Report { return report.Report{} }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture.stopAfterLeaseRenewals(cancel, 3)

	if err := fixture.service.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if len(fixture.harness.starts) != 1 || fixture.harness.starts[0].Role != "test" {
		t.Fatalf("harness starts = %#v, want exactly one test-role invocation", fixture.harness.starts)
	}
	if len(fixture.workspace.checkpoints) != 0 || len(fixture.pullRequests.createdRequests) != 0 {
		t.Fatalf("route effects = checkpoints %d pull requests %d, want none while the test role runs", len(fixture.workspace.checkpoints), len(fixture.pullRequests.createdRequests))
	}
	run := fixture.currentRun(t)
	if run.Stage != store.StageTest || len(run.ActiveInvocationIDs) == 0 {
		t.Fatalf("run = stage %q active invocations %#v, want the test stage with a live invocation", run.Stage, run.ActiveInvocationIDs)
	}
	if activity := factory.RunActivityFor(run); activity != factory.ActivityInvocationActive {
		t.Fatalf("activity = %q, want invocation-active", activity)
	}
}

// TestRepeatedPollingAndRestartDoNotDuplicateProgressionEffects verifies that
// continued polling and a coordinator process boundary reconcile the finished
// run instead of creating a second checkpoint, push, or pull request.
func TestRepeatedPollingAndRestartDoNotDuplicateProgressionEffects(t *testing.T) {
	t.Parallel()

	fixture := newUnattendedFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture.stopWhenPullRequestExists(cancel)
	if err := fixture.service.Start(ctx); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	comments := len(fixture.commands.editedComments)
	fixture.pullRequests.existing = fixture.pullRequests.created

	restarted, restartCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer restartCancel()
	fixture.stopAfterLeaseRenewals(restartCancel, 3)
	if err := fixture.restart().Start(restarted); err != nil {
		t.Fatalf("restarted Start() error = %v", err)
	}

	if len(fixture.harness.starts) != 1 {
		t.Fatalf("harness starts after restart = %d, want one", len(fixture.harness.starts))
	}
	if len(fixture.workspace.checkpoints) != 1 || len(fixture.workspace.pushes) != 1 {
		t.Fatalf("git effects after restart = checkpoints %d pushes %d, want one each", len(fixture.workspace.checkpoints), len(fixture.workspace.pushes))
	}
	if len(fixture.pullRequests.createdRequests) != 1 {
		t.Fatalf("pull request creates after restart = %d, want one", len(fixture.pullRequests.createdRequests))
	}
	if len(fixture.commands.createdComments) != 1 {
		t.Fatalf("status comments = %d, want the single editable supervision comment", len(fixture.commands.createdComments))
	}
	if len(fixture.commands.editedComments) != comments {
		t.Fatalf("status comment edits after restart = %d, want the %d edits the first pass made", len(fixture.commands.editedComments), comments)
	}
}

// TestUnattendedProgressionRepairsAFailedGateAndStillReachesTheDraftPR
// verifies the bounded check-repair loop runs under unattended progression: a
// failed checkpoint gate resumes the implementation session, and the repaired
// checkpoint is evaluated, pushed, and published without an operator command.
func TestUnattendedProgressionRepairsAFailedGateAndStillReachesTheDraftPR(t *testing.T) {
	t.Parallel()

	fixture := newUnattendedFixture(t)
	fixture.workspace.checkpointSHAs = []string{implementationCheckpoint, repairedImplementationCheckpoint}
	// Baseline setup and gate pass; the first checkpoint gate fails; every
	// later command succeeds through the fake's zero-value default.
	fixture.worker.results = []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}, {ExitCode: 0}, {ExitCode: 1}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture.stopWhenPullRequestExists(cancel)

	if err := fixture.service.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if len(fixture.harness.starts) != 1 || len(fixture.harness.resumes) != 1 {
		t.Fatalf("harness lifecycle = %d starts %d resumes, want one launch and one repair resume", len(fixture.harness.starts), len(fixture.harness.resumes))
	}
	if fixture.harness.resumes[0].ResumeSessionID != "session-unattended" {
		t.Fatalf("repair resume = %#v, want the accepted native session", fixture.harness.resumes[0])
	}
	if len(fixture.workspace.checkpoints) != 2 || fixture.workspace.checkpoints[1].ParentSHA != implementationCheckpoint {
		t.Fatalf("checkpoints = %#v, want the repair checkpoint on top of the failed one", fixture.workspace.checkpoints)
	}
	if len(fixture.pullRequests.createdRequests) != 1 {
		t.Fatalf("pull request creates = %d, want exactly one after the repair", len(fixture.pullRequests.createdRequests))
	}
	final := fixture.currentRun(t)
	if final.Stage != store.StageDraftPR || final.CheckpointSHA != repairedImplementationCheckpoint {
		t.Fatalf("final run = stage %q checkpoint %q, want the draft PR at the repaired checkpoint", final.Stage, final.CheckpointSHA)
	}
	if final.CheckRepairAttempts != 1 {
		t.Fatalf("check-repair attempts = %d, want exactly one consumed attempt", final.CheckRepairAttempts)
	}
}

// TestUnattendedProgressionPublishesAnUnclassifiedStepFailure verifies a step
// failure that no seam classified still leaves a visible waiting state and an
// actionable reason, instead of an `agent-running` run nobody is working on.
func TestUnattendedProgressionPublishesAnUnclassifiedStepFailure(t *testing.T) {
	t.Parallel()

	fixture := newUnattendedFixture(t)
	fixture.terminal = &unavailableTerminal{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture.stopAfterLeaseRenewals(cancel, 3)

	if err := fixture.restart().Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if len(fixture.harness.starts) != 0 {
		t.Fatalf("harness starts = %d, want none once the terminal is unavailable", len(fixture.harness.starts))
	}
	final := fixture.currentRun(t)
	if final.Status != store.StatusWaitingForHuman {
		t.Fatalf("final status = %q, want waiting_for_human", final.Status)
	}
	if !strings.Contains(final.LifecycleReason, "unattended progression stopped at start agent") {
		t.Fatalf("lifecycle reason = %q, want the failed transition named", final.LifecycleReason)
	}
	if !strings.Contains(factory.StatusCommentBody(final), "- activity: `waiting-for-human`") {
		t.Fatalf("status comment = %q, want the published waiting activity", factory.StatusCommentBody(final))
	}
}

// TestStatusCommentSeparatesAnActiveInvocationFromAnActiveRun verifies the
// published supervision comment never implies that a harness is executing
// when the coordinator owns the next transition.
func TestStatusCommentSeparatesAnActiveInvocationFromAnActiveRun(t *testing.T) {
	t.Parallel()

	run := store.Run{ID: "run-activity", IssueNumber: 4, Stage: store.StageImplementation, Status: store.StatusActive}
	if got := factory.RunActivityFor(run); got != factory.ActivityRunActive {
		t.Fatalf("RunActivityFor(no invocation) = %q, want run-active", got)
	}
	if body := factory.StatusCommentBody(run); !strings.Contains(body, "- activity: `run-active`") || strings.Contains(body, "active invocation") {
		t.Fatalf("status comment = %q, want run-active without an invocation identity", body)
	}
	run.ActiveInvocationIDs = []string{"inv-1"}
	if got := factory.RunActivityFor(run); got != factory.ActivityInvocationActive {
		t.Fatalf("RunActivityFor(active invocation) = %q, want invocation-active", got)
	}
	body := factory.StatusCommentBody(run)
	if !strings.Contains(body, "- activity: `invocation-active`") || !strings.Contains(body, "- active invocation: `inv-1`") {
		t.Fatalf("status comment = %q, want the active invocation identity", body)
	}
	run.Status = store.StatusComplete
	if got := factory.RunActivityFor(run); got != factory.ActivityTerminal {
		t.Fatalf("RunActivityFor(terminal) = %q, want terminal", got)
	}
}

// unattendedFixture wires the real operational store to deterministic
// adapters so the polling coordinator can be driven end to end.
type unattendedFixture struct {
	service         *factory.Service
	commands        *unattendedGitHub
	worker          *unattendedWorker
	harness         *unattendedHarness
	workspace       *unattendedWorkspace
	pullRequests    *fakePullRequests
	lease           *pollingLease
	terminal        terminal.TerminalRuntime
	operationalPath string
	configPath      string
	dependencies    func() factory.Dependencies
	agentReport     func(harness.StartRequest) report.Report
}

// newUnattendedFixture builds one advisory-policy repository whose harness
// answers every invocation with a completed structured result.
func newUnattendedFixture(t *testing.T) *unattendedFixture {
	t.Helper()
	return newUnattendedFixtureWith(t, config.TestPolicy{Mode: config.TestModeAdvisory}, "Implement unattended progression.")
}

// newUnattendedFixtureWith builds the same repository with an explicit frozen
// test policy and issue body, so a route marker or required policy can select
// a different stage sequence.
func newUnattendedFixtureWith(t *testing.T, testPolicy config.TestPolicy, issueBody string) *unattendedFixture {
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
	if err := os.WriteFile(filepath.Join(repositoryPath, ".git", "HEAD"), []byte("ref: refs/heads/factory/run-unattended\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	policy := validRepositoryConfig()
	policy.TestPolicy = testPolicy
	fixture := &unattendedFixture{
		commands: &unattendedGitHub{fakeGitHub: &fakeGitHub{}, issues: []github.Issue{{
			Number: 7, Title: "Drive the routine path", Body: issueBody,
			State: "open", Labels: []string{github.LabelAgentReady},
		}}},
		worker: &unattendedWorker{},
		workspace: &unattendedWorkspace{draftGitWorkspace: &draftGitWorkspace{
			workspace: gitadapter.Workspace{BaseSHA: factoryGateCheckpoint, Branch: "factory/run-unattended", Worktree: worktreePath},
			state:     gitadapter.WorktreeState{RepositoryPath: repositoryPath, Branch: "factory/run-unattended", HeadSHA: factoryGateCheckpoint},
		}},
		pullRequests: &fakePullRequests{created: github.PullRequest{
			Number: 17, URL: "https://github.com/example/project/pull/17", State: "open", Draft: true,
			HeadBranch: "factory/run-unattended", BaseBranch: "main",
		}},
		lease:           &pollingLease{},
		terminal:        &agentTerminal{},
		operationalPath: filepath.Join(root, "state", "factory.db"),
	}
	fixture.agentReport = fixture.completedReport
	fixture.harness = &unattendedHarness{fixture: fixture}

	registration := config.RepositoryRegistration{
		Path:                 repositoryPath,
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		AuthorizedUsers:      []string{"alice"},
		Polling:              config.PollingConfig{Interval: "1ms", Backoff: "1ms"},
		OperationalDataPath:  fixture.operationalPath,
		RepositoryConfigPath: filepath.Join(repositoryPath, "factory.yaml"),
	}
	fixture.configPath = filepath.Join(root, "config.yaml")
	fixture.dependencies = func() factory.Dependencies {
		return factory.Dependencies{
			Config:         &fakeConfig{value: config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{registration}}},
			OpenStore:      func(ctx context.Context, path string) (factory.OperationalStore, error) { return store.Open(ctx, path) },
			LoadRepository: func(string) (config.RepositoryConfig, error) { return policy, nil },
			GitHub:         fixture.commands,
			IssuePoller:    fixture.commands,
			Lease:          fixture.lease,
			PullRequests:   fixture.pullRequests,
			CommitStatuses: &gateStatuses{},
			Worktree:       fixture.workspace,
			GitWorkspace:   fixture.workspace,
			Worker:         fixture.worker,
			Terminal:       fixture.terminal,
			Harness:        fixture.harness,
			Now:            func() time.Time { return time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC) },
			NewRunID:       newSequentialRunID("run-unattended"),
			Coordinator:    "coordinator-test",
			StartupDiagnosis: func(context.Context) (factory.DoctorResult, error) {
				return factory.DoctorResult{Report: doctor.Report{Results: []doctor.Result{doctor.Success("startup")}}}, nil
			},
		}
	}
	fixture.service = factory.NewWithDependencies(fixture.configPath, fixture.dependencies())
	return fixture
}

// restart models a coordinator process boundary: a fresh service opens the
// same operational store and adapters without any in-memory continuity.
func (f *unattendedFixture) restart() *factory.Service {
	f.service = factory.NewWithDependencies(f.configPath, f.dependencies())
	return f.service
}

// stopWhenPullRequestExists cancels the coordinator on the first lease
// renewal observed after the draft pull request was created.
func (f *unattendedFixture) stopWhenPullRequestExists(cancel context.CancelFunc) {
	f.lease.onRenew = func(github.Lease) {
		if len(f.pullRequests.createdRequests) > 0 {
			cancel()
		}
	}
}

// stopAfterLeaseRenewals cancels the coordinator after a bounded number of
// polling ticks, which is enough to prove repeated polling adds no effects.
func (f *unattendedFixture) stopAfterLeaseRenewals(cancel context.CancelFunc, ticks int) {
	f.lease.onRenew = func(github.Lease) {
		if len(f.lease.calls) >= ticks {
			cancel()
		}
	}
}

// currentRun reads the persisted run from the real operational store.
func (f *unattendedFixture) currentRun(t *testing.T) store.Run {
	t.Helper()
	opened, err := store.Open(context.Background(), f.operationalPath)
	if err != nil {
		t.Fatalf("open operational store: %v", err)
	}
	defer func() { _ = opened.Close() }()
	run, err := opened.LatestRun(context.Background())
	if err != nil {
		t.Fatalf("read latest run: %v", err)
	}
	if run == nil {
		t.Fatal("no persisted run")
	}
	return *run
}

// completedReport is the valid implementation handoff the fake harness writes
// for one launched invocation.
func (f *unattendedFixture) completedReport(request harness.StartRequest) report.Report {
	return report.Report{
		SchemaVersion: report.SchemaVersion,
		InvocationID:  request.InvocationID,
		RunID:         request.RunID,
		Harness:       string(config.HarnessCodex),
		Role:          request.Role,
		Stage:         request.Stage,
		Outcome:       report.OutcomeCompleted,
		Summary:       "implemented unattended progression with a focused test",
		Handoff: &report.Handoff{
			ChangeSummary:          "drove the routine path to a draft pull request",
			AcceptanceMapping:      []report.AcceptanceMapping{{Criterion: "unattended progression", Evidence: "internal/factory.go"}},
			ProductionFilesChanged: []string{"internal/factory.go"},
			FocusedCommands:        []string{"go test ./internal/factory"},
		},
		NativeSessionID: "session-unattended",
		ReportedAt:      time.Date(2026, 8, 29, 9, 5, 0, 0, time.UTC),
	}
}

// newSequentialRunID returns distinct identifiers for the run and each of its
// invocations, matching the production generator's uniqueness contract.
func newSequentialRunID(runID string) func() (string, error) {
	calls := 0
	return func() (string, error) {
		calls++
		if calls == 1 {
			return runID, nil
		}
		return runID + "-" + string(rune('a'+calls-2)), nil
	}
}

// unattendedGitHub serves the queue and the claimed issue from one fixture.
type unattendedGitHub struct {
	*fakeGitHub
	issues  []github.Issue
	claimed int
}

// ListEligibleIssues returns the configured queue.
func (f *unattendedGitHub) ListEligibleIssues(context.Context, github.Repository) ([]github.Issue, error) {
	return append([]github.Issue(nil), f.issues...), nil
}

// Issue serves the queue snapshot once and then the mutated projection, so
// later label transitions remain observable to restart reconciliation.
func (f *unattendedGitHub) Issue(ctx context.Context, repository github.Repository, number int) (github.Issue, error) {
	if f.fakeGitHub.issueValue.Number != number {
		for _, issue := range f.issues {
			if issue.Number == number {
				f.claimed = number
				f.fakeGitHub.issueValue = issue
			}
		}
	}
	return f.fakeGitHub.Issue(ctx, repository, number)
}

// CreateIssueComment records the created supervision comment so a restarted
// coordinator can recover it by identity instead of creating a second one.
func (f *unattendedGitHub) CreateIssueComment(ctx context.Context, repository github.Repository, number int, body string) (github.Comment, error) {
	comment, err := f.fakeGitHub.CreateIssueComment(ctx, repository, number, body)
	if err == nil {
		f.fakeGitHub.statusComment = comment
	}
	return comment, err
}

// EditIssueComment keeps the recoverable comment body current.
func (f *unattendedGitHub) EditIssueComment(ctx context.Context, repository github.Repository, id, body string) error {
	if err := f.fakeGitHub.EditIssueComment(ctx, repository, id, body); err != nil {
		return err
	}
	if f.fakeGitHub.statusComment.ID == id {
		f.fakeGitHub.statusComment.Body = body
	}
	return nil
}

// unattendedWorkspace adds the read-only remote-head projection that restart
// reconciliation needs once a pull request tracks the pushed branch.
type unattendedWorkspace struct {
	*draftGitWorkspace
}

// RemoteBranchHead reports the last checkpoint the branch push published.
func (w *unattendedWorkspace) RemoteBranchHead(context.Context, gitadapter.PushRequest) (string, error) {
	if len(w.pushedSHAs) == 0 {
		return "", nil
	}
	return w.pushedSHAs[len(w.pushedSHAs)-1], nil
}

// unattendedWorker records the result directory of the invocation currently
// being launched so the fake harness can write its structured report there.
type unattendedWorker struct {
	agentWorker
	resultPath string
}

// Start records the mounted result directory before delegating.
func (w *unattendedWorker) Start(ctx context.Context, request worker.StartRequest) error {
	if request.ResultPath != "" {
		w.resultPath = request.ResultPath
	}
	return w.agentWorker.Start(ctx, request)
}

// unattendedHarness answers every launch with a completed structured result,
// standing in for an interactive harness that finished its work.
type unattendedHarness struct {
	agentHarness
	fixture *unattendedFixture
}

// Start applies the edits a role would make and writes its structured
// report, so the coordinator observes the same evidence a real invocation
// leaves behind.
func (h *unattendedHarness) Start(ctx context.Context, request harness.StartRequest) (harness.Session, error) {
	session, err := h.agentHarness.Start(ctx, request)
	if err != nil {
		return session, err
	}
	h.fixture.workspace.state.ChangedPaths = []string{"internal/factory.go"}
	value := h.fixture.agentReport(request)
	if value.Outcome == "" {
		// The invocation is still working; the coordinator must wait for it.
		return session, nil
	}
	if _, err := report.WriteAtomicForInvocation(h.fixture.worker.resultPath, request.InvocationID, value); err != nil {
		return session, err
	}
	return session, nil
}

// Resume applies the repair edits and writes the resumed invocation's report,
// standing in for an implementation session that fixed a failed gate.
func (h *unattendedHarness) Resume(ctx context.Context, request harness.StartRequest) (harness.Session, error) {
	session, err := h.agentHarness.Resume(ctx, request)
	if err != nil {
		return session, err
	}
	h.fixture.workspace.state.ChangedPaths = []string{"internal/factory.go"}
	value := h.fixture.agentReport(request)
	if value.Outcome == "" {
		return session, nil
	}
	value.NativeSessionID = session.NativeSessionID
	if _, err := report.WriteAtomicForInvocation(h.fixture.worker.resultPath, request.InvocationID, value); err != nil {
		return session, err
	}
	return session, nil
}

// unavailableTerminal refuses the run workspace, standing in for a terminal
// failure the coordinator cannot classify into a known waiting state.
type unavailableTerminal struct {
	agentTerminal
}

// EnsureRunWorkspace always fails.
func (*unavailableTerminal) EnsureRunWorkspace(context.Context, terminal.RunWorkspaceRequest) (terminal.RunWorkspace, error) {
	return terminal.RunWorkspace{}, errors.New("terminal runtime unavailable")
}

var _ terminal.TerminalRuntime = (*unavailableTerminal)(nil)
var _ worker.WorkerRuntime = (*unattendedWorker)(nil)
var _ harness.Runtime = (*unattendedHarness)(nil)
var _ github.IssuePoller = (*unattendedGitHub)(nil)
var _ gitadapter.RemoteBranchInspector = (*unattendedWorkspace)(nil)
