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

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

const implementationCheckpoint = "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"
const repairedImplementationCheckpoint = "1234561234561234561234561234561234561234561234561234561234561234"

// TestCreateDraftPullRequestPushesTheCheckpointBeforeGatesAndCreatesOneDraft
// verifies the complete host-owned implementation boundary at the Factory
// seam.
func TestCreateDraftPullRequestPushesTheCheckpointBeforeGatesAndCreatesOneDraft(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repo")
	worktreePath := filepath.Join(root, "worktree")
	if err := os.MkdirAll(filepath.Join(repositoryPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryPath, ".git", "HEAD"), []byte("ref: refs/heads/factory/run-draft\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := validRepositoryConfig()
	policy.Gates = []config.GateConfig{
		{Name: "format", Command: "format", Timeout: "5m", Blocking: true, EnvironmentPolicy: config.EnvironmentPolicyClean},
		{Name: "test", Command: "test", Timeout: "5m", Blocking: true, DependsOn: []string{"format"}, EnvironmentPolicy: config.EnvironmentPolicyClean},
	}
	runStore := &fakeRunStore{}
	githubAdapter := &fakeGitHub{issueValue: github.Issue{Number: 42, Title: "Add the factory handoff", Body: "Implement the supervised draft PR boundary.", State: "open", Labels: []string{github.LabelAgentReady}}}
	workspace := &draftGitWorkspace{
		workspace: gitadapter.Workspace{BaseSHA: factoryGateCheckpoint, Branch: "factory/run-draft", Worktree: worktreePath},
		state:     gitadapter.WorktreeState{Branch: "factory/run-draft", HeadSHA: factoryGateCheckpoint, ChangedPaths: []string{"internal/factory.go"}},
	}
	workerRuntime := &gateWorker{results: []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}, {ExitCode: 0}}}
	statuses := &gateStatuses{}
	pullRequests := &fakePullRequests{created: github.PullRequest{Number: 17, URL: "https://github.com/example/project/pull/17", State: "open", Draft: true, HeadBranch: "factory/run-draft", BaseBranch: "main"}}

	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
		Path: repositoryPath, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
		OperationalDataPath: filepath.Join(root, "state", "factory.db"), RepositoryConfigPath: filepath.Join(repositoryPath, "factory.yaml"),
	}}}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:         &fakeConfig{value: host},
		OpenStore:      func(context.Context, string) (factory.OperationalStore, error) { return runStore, nil },
		LoadRepository: func(string) (config.RepositoryConfig, error) { return policy, nil },
		GitHub:         &fakeGitHubWithPullRequests{fakeGitHub: githubAdapter},
		PullRequests:   pullRequests,
		Worktree:       workspace,
		GitWorkspace:   workspace,
		Worker:         workerRuntime,
		CommitStatuses: statuses,
		Now:            func() time.Time { return time.Date(2026, 8, 21, 10, 1, 0, 0, time.UTC) },
		NewRunID:       func() (string, error) { return "run-draft", nil },
	})
	claimed, err := service.ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() fixture setup error = %v", err)
	}
	claimed.Run.Stage = store.StageImplementation
	claimed.Run.Status = store.StatusActive
	if err := runStore.SaveRun(context.Background(), claimed.Run); err != nil {
		t.Fatalf("SaveRun() fixture setup error = %v", err)
	}

	result, err := service.CreateDraftPullRequest(context.Background(), factory.DraftPullRequestRequest{RunID: claimed.Run.ID})
	if err != nil {
		t.Fatalf("CreateDraftPullRequest() error = %v", err)
	}
	if result.Run.Stage != store.StageDraftPR || result.Run.CheckpointSHA != implementationCheckpoint || result.PullRequest.Number != 17 || !result.PullRequest.Draft {
		t.Fatalf("result = %#v, want draft stage, checkpoint, and PR identity", result)
	}
	if len(workspace.checkpoints) != 1 || len(workspace.pushes) != 1 {
		t.Fatalf("Git workspace effects = checkpoints %#v pushes %#v, want one each", workspace.checkpoints, workspace.pushes)
	}
	if len(pullRequests.createdRequests) != 1 || len(pullRequests.updatedRequests) != 0 {
		t.Fatalf("PR effects = creates %#v updates %#v, want one create", pullRequests.createdRequests, pullRequests.updatedRequests)
	}
	if len(workerRuntime.commands) != 3 || workerRuntime.commands[0].Command != policy.Setup || workerRuntime.commands[1].Command != "format" || workerRuntime.commands[2].Command != "test" {
		t.Fatalf("gate commands = %#v, want one setup followed by ordered gates", workerRuntime.commands)
	}
	body := pullRequests.createdRequests[0].Body
	for _, expected := range []string{
		"<!-- factory-generated:start -->", "Add the factory handoff", "Implement the supervised draft PR boundary.",
		"stage: `draft_pr`", "format", "test", "intervention: `none`", "factory status", "factory draft-pr --run-id run-draft",
		"Closes #42",
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("generated PR body %q does not contain %q", body, expected)
		}
	}
	if len(githubAdapter.editedComments) != 2 {
		t.Fatalf("status comment edits = %d, want check and draft stage", len(githubAdapter.editedComments))
	}
	if !strings.Contains(githubAdapter.editedComments[1].body, "stage: `draft_pr`") {
		t.Fatalf("final status comment = %q, want draft_pr stage", githubAdapter.editedComments[1].body)
	}

	// A repeated command recovers the existing pull request and does not make
	// another checkpoint, push, or pull request.
	pullRequests.existing = github.PullRequest{Number: 17, URL: pullRequests.created.URL, Body: "human-authored notes", State: "open", Draft: true, HeadBranch: "factory/run-draft", BaseBranch: "main"}
	repeated, err := service.CreateDraftPullRequest(context.Background(), factory.DraftPullRequestRequest{RunID: "run-draft", Intervention: "human reviewed"})
	if err != nil {
		t.Fatalf("repeated CreateDraftPullRequest() error = %v", err)
	}
	if repeated.PullRequest.Number != 17 || len(workspace.checkpoints) != 1 || len(workspace.pushes) != 1 || len(pullRequests.createdRequests) != 1 || len(pullRequests.updatedRequests) != 1 {
		t.Fatalf("repeated effects/result = %#v, checkpoints=%d pushes=%d creates=%d updates=%d", repeated, len(workspace.checkpoints), len(workspace.pushes), len(pullRequests.createdRequests), len(pullRequests.updatedRequests))
	}
	updatedBody := pullRequests.updatedRequests[0].Body
	if !strings.Contains(updatedBody, "human-authored notes") || strings.Count(updatedBody, "<!-- factory-generated:start -->") != 1 || !strings.Contains(updatedBody, "intervention: `human reviewed`") {
		t.Fatalf("updated PR body = %q, want preserved human text and one regenerated section", updatedBody)
	}
	// The closing keyword is what makes GitHub link the pull request to the
	// issue and close it on merge, so regeneration must never drop it.
	if strings.Count(updatedBody, "Closes #42") != 1 {
		t.Fatalf("updated PR body = %q, want exactly one closing keyword for issue 42", updatedBody)
	}
}

// TestCreateDraftPullRequestPublishesGateStatusesForAPushedCheckpoint
// verifies the coordinator never asks GitHub for a status on a commit the
// remote cannot resolve. GitHub rejects such a status with an unknown-SHA
// error, which previously left the run's journaled status effect pending and
// the run stuck in recovery.
func TestCreateDraftPullRequestPublishesGateStatusesForAPushedCheckpoint(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repo")
	worktreePath := filepath.Join(root, "worktree")
	if err := os.MkdirAll(filepath.Join(repositoryPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryPath, ".git", "HEAD"), []byte("ref: refs/heads/factory/run-unpushed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := validRepositoryConfig()
	policy.Gates = []config.GateConfig{{Name: "format", Command: "format", Timeout: "5m", Blocking: true, EnvironmentPolicy: config.EnvironmentPolicyClean}}
	runStore := &fakeRunStore{}
	githubAdapter := &fakeGitHub{issueValue: github.Issue{Number: 42, Title: "Publish the checkpoint first", Body: "Attach gate statuses to a resolvable commit.", State: "open", Labels: []string{github.LabelAgentReady}}}
	workspace := &draftGitWorkspace{
		workspace: gitadapter.Workspace{BaseSHA: factoryGateCheckpoint, Branch: "factory/run-unpushed", Worktree: worktreePath},
		state:     gitadapter.WorktreeState{Branch: "factory/run-unpushed", HeadSHA: factoryGateCheckpoint, ChangedPaths: []string{"internal/factory.go"}},
	}
	workerRuntime := &gateWorker{results: []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}}}
	statuses := &unpushedCheckpointStatuses{workspace: workspace}
	pullRequests := &fakePullRequests{created: github.PullRequest{Number: 19, URL: "https://github.com/example/project/pull/19", State: "open", Draft: true, HeadBranch: "factory/run-unpushed", BaseBranch: "main"}}

	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
		Path: repositoryPath, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
		OperationalDataPath: filepath.Join(root, "state", "factory.db"), RepositoryConfigPath: filepath.Join(repositoryPath, "factory.yaml"),
	}}}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:         &fakeConfig{value: host},
		OpenStore:      func(context.Context, string) (factory.OperationalStore, error) { return runStore, nil },
		LoadRepository: func(string) (config.RepositoryConfig, error) { return policy, nil },
		GitHub:         &fakeGitHubWithPullRequests{fakeGitHub: githubAdapter},
		PullRequests:   pullRequests,
		Worktree:       workspace,
		GitWorkspace:   workspace,
		Worker:         workerRuntime,
		CommitStatuses: statuses,
		Now:            func() time.Time { return time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC) },
		NewRunID:       func() (string, error) { return "run-unpushed", nil },
	})
	claimed, err := service.ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() fixture setup error = %v", err)
	}
	claimed.Run.Stage = store.StageImplementation
	claimed.Run.Status = store.StatusActive
	if err := runStore.SaveRun(context.Background(), claimed.Run); err != nil {
		t.Fatalf("SaveRun() fixture setup error = %v", err)
	}

	result, err := service.CreateDraftPullRequest(context.Background(), factory.DraftPullRequestRequest{RunID: claimed.Run.ID})
	if err != nil {
		t.Fatalf("CreateDraftPullRequest() error = %v", err)
	}
	if result.Run.Stage != store.StageDraftPR || result.PullRequest.Number != 19 {
		t.Fatalf("result = %#v, want a draft PR at the published checkpoint", result)
	}
	if len(workspace.pushedSHAs) != 1 || workspace.pushedSHAs[0] != implementationCheckpoint {
		t.Fatalf("pushed checkpoints = %#v, want the evaluated checkpoint", workspace.pushedSHAs)
	}
	if len(statuses.values) != 1 || statuses.values[0].SHA != implementationCheckpoint {
		t.Fatalf("published gate statuses = %#v, want one status on the pushed checkpoint", statuses.values)
	}
	if statuses.rejected != 0 {
		t.Fatalf("statuses published for an unresolvable commit = %d, want zero", statuses.rejected)
	}
}

// TestCreateDraftPullRequestReentersRecoveryPausedCheck verifies that a
// reconciled check-stage pause can continue through the normal draft-PR
// command without starting a replacement implementation agent.
func TestCreateDraftPullRequestReentersRecoveryPausedCheck(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repo")
	worktreePath := filepath.Join(root, "worktree")
	if err := os.MkdirAll(filepath.Join(repositoryPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryPath, ".git", "HEAD"), []byte("ref: refs/heads/factory/run-recovered-check\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := validRepositoryConfig()
	policy.Gates = []config.GateConfig{{Name: "format", Command: "format", Timeout: "5m", Blocking: true, EnvironmentPolicy: config.EnvironmentPolicyClean}}
	runStore := &fakeRunStore{}
	githubAdapter := &fakeGitHub{issueValue: github.Issue{
		Number: 42, Title: "Recover the check stage", Body: "Continue gate evaluation after recovery.", State: "open",
		Labels: []string{github.LabelAgentReady},
	}}
	workspace := &draftGitWorkspace{
		workspace: gitadapter.Workspace{BaseSHA: factoryGateCheckpoint, Branch: "factory/run-recovered-check", Worktree: worktreePath},
		state:     gitadapter.WorktreeState{Branch: "factory/run-recovered-check", HeadSHA: factoryGateCheckpoint},
	}
	workerRuntime := &gateWorker{results: []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}}}
	statuses := &gateStatuses{}
	pullRequests := &fakePullRequests{created: github.PullRequest{
		Number: 20, URL: "https://github.com/example/project/pull/20", State: "open", Draft: true,
		HeadBranch: "factory/run-recovered-check", BaseBranch: "main",
	}}
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
		Path: repositoryPath, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
		OperationalDataPath: filepath.Join(root, "state", "factory.db"), RepositoryConfigPath: filepath.Join(repositoryPath, "factory.yaml"),
	}}}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:         &fakeConfig{value: host},
		OpenStore:      func(context.Context, string) (factory.OperationalStore, error) { return runStore, nil },
		LoadRepository: func(string) (config.RepositoryConfig, error) { return policy, nil },
		GitHub:         &fakeGitHubWithPullRequests{fakeGitHub: githubAdapter},
		PullRequests:   pullRequests,
		Worktree:       workspace,
		GitWorkspace:   workspace,
		Worker:         workerRuntime,
		CommitStatuses: statuses,
		Now:            func() time.Time { return time.Date(2026, 8, 28, 13, 0, 0, 0, time.UTC) },
		NewRunID:       func() (string, error) { return "run-recovered-check", nil },
	})
	claimed, err := service.ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() fixture setup error = %v", err)
	}
	claimed.Run.Stage = store.StageCheck
	claimed.Run.Status = store.StatusWaitingForHuman
	claimed.Run.LifecycleReason = "restart reconciliation paused: stale GitHub projection"
	if err := runStore.SaveRun(context.Background(), claimed.Run); err != nil {
		t.Fatalf("SaveRun() fixture setup error = %v", err)
	}
	githubAdapter.issueValue.Labels = []string{github.LabelAgentNeedsInput}
	githubAdapter.statusComment = github.Comment{ID: claimed.Run.StatusCommentID, Body: factory.StatusCommentBody(claimed.Run)}

	result, err := service.CreateDraftPullRequest(context.Background(), factory.DraftPullRequestRequest{RunID: claimed.Run.ID})
	if err != nil {
		t.Fatalf("CreateDraftPullRequest() error = %v", err)
	}
	if result.Run.Stage != store.StageDraftPR || result.PullRequest.Number != 20 {
		t.Fatalf("result = %#v, want draft PR after recovered check", result)
	}
	if len(workspace.checkpoints) != 0 || len(workspace.pushes) != 1 || len(statuses.values) != 1 || statuses.values[0].SHA != factoryGateCheckpoint {
		t.Fatalf("check effects = pushes %#v statuses %#v, want one evaluated checkpoint", workspace.pushes, statuses.values)
	}
	if len(workerRuntime.commands) != 2 || workerRuntime.commands[0].Command != policy.Setup || workerRuntime.commands[1].Command != "format" {
		t.Fatalf("check commands = %#v, want setup and format only", workerRuntime.commands)
	}
	if got := githubAdapter.issueValue.Labels; len(got) != 1 || got[0] != github.LabelAgentRunning {
		t.Fatalf("final issue labels = %#v, want agent-running", got)
	}
}

// unpushedCheckpointStatuses answers a status for an unpushed commit the way
// GitHub does, so a test proves the checkpoint reaches the remote first.
type unpushedCheckpointStatuses struct {
	workspace *draftGitWorkspace
	values    []github.CommitStatus
	rejected  int
}

// CreateCommitStatus records one status and rejects any SHA the remote cannot
// resolve yet.
func (s *unpushedCheckpointStatuses) CreateCommitStatus(_ context.Context, _ github.Repository, status github.CommitStatus) error {
	if !s.workspace.remoteHasCheckpoint(status.SHA) {
		s.rejected++
		return fmt.Errorf("No commit found for SHA: %s (HTTP 422)", status.SHA)
	}
	s.values = append(s.values, status)
	return nil
}

var _ github.CommitStatusPublisher = (*unpushedCheckpointStatuses)(nil)

// TestCreateDraftPullRequestRoutesDeterministicFailuresThroughNativeRepair
// verifies the full failed-check to resumed-session to fresh-checkpoint loop.
func TestCreateDraftPullRequestRoutesDeterministicFailuresThroughNativeRepair(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repo")
	worktreePath := filepath.Join(root, "worktree")
	if err := os.MkdirAll(filepath.Join(repositoryPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryPath, ".git", "HEAD"), []byte("ref: refs/heads/factory/run-repair\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := validRepositoryConfig()
	policy.Gates = []config.GateConfig{{Name: "test", Command: "test", Timeout: "5m", Blocking: true, EnvironmentPolicy: config.EnvironmentPolicyClean}}
	runStore := &agentRunStore{runs: map[string]store.Run{}, invocations: map[string]store.Invocation{}, gateResults: map[string][]store.GateResult{}}
	githubAdapter := &fakeGitHub{issueValue: github.Issue{Number: 42, Title: "Repair the implementation", Body: "Implement and repair the supervised handoff.", State: "open", Labels: []string{github.LabelAgentReady}}}
	workspace := &draftGitWorkspace{
		workspace:      gitadapter.Workspace{BaseSHA: factoryGateCheckpoint, Branch: "factory/run-repair", Worktree: worktreePath},
		state:          gitadapter.WorktreeState{Branch: "factory/run-repair", HeadSHA: factoryGateCheckpoint, ChangedPaths: []string{"internal/factory/agent.go"}},
		checkpointSHAs: []string{implementationCheckpoint, repairedImplementationCheckpoint},
	}
	runtime := &agentWorker{results: []worker.CommandResult{{ExitCode: 0}, {ExitCode: 1}}}
	terminalRuntime := &agentTerminal{}
	harnessRuntime := &agentHarness{}
	statuses := &gateStatuses{}
	pullRequests := &fakePullRequests{created: github.PullRequest{Number: 18, URL: "https://github.com/example/project/pull/18", State: "open", Draft: true, HeadBranch: "factory/run-repair", BaseBranch: "main"}}
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
		Path: repositoryPath, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
		Cmux: config.CmuxConfig{ControlWorkspace: "factory-control"}, OperationalDataPath: filepath.Join(root, "state", "factory.db"), RepositoryConfigPath: filepath.Join(repositoryPath, "factory.yaml"),
	}}}
	ids := []string{"run-repair", "initial", "repair"}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:         &fakeConfig{value: host},
		OpenStore:      func(context.Context, string) (factory.OperationalStore, error) { return runStore, nil },
		LoadRepository: func(string) (config.RepositoryConfig, error) { return policy, nil },
		GitHub:         &fakeGitHubWithPullRequests{fakeGitHub: githubAdapter},
		PullRequests:   pullRequests,
		Worktree:       workspace,
		GitWorkspace:   workspace,
		Worker:         runtime,
		Terminal:       terminalRuntime,
		Harness:        harnessRuntime,
		CommitStatuses: statuses,
		Now:            func() time.Time { return time.Date(2026, 8, 21, 10, 1, 0, 0, time.UTC) },
		NewRunID: func() (string, error) {
			if len(ids) == 0 {
				return "", errors.New("repair test identifiers exhausted")
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
	claimed.Run.TestStageSkipped = true
	claimed.Run.TestExemption = &store.TestExemption{Kind: "human", Justification: "implementation repair fixture"}
	if err := runStore.SaveRun(context.Background(), claimed.Run); err != nil {
		t.Fatalf("save implementation repair fixture: %v", err)
	}
	runStore.gateResults[claimed.Run.ID] = []store.GateResult{{
		RunID: claimed.Run.ID, CheckpointSHA: claimed.Run.CheckpointSHA, Phase: store.GatePhaseBaseline,
		Ordinal: 0, GateName: policy.Gates[0].Name, Outcome: store.GateOutcomePassed,
		Status: string(github.CommitStatusSuccess), Blocking: policy.Gates[0].Blocking,
	}}
	launch, err := service.StartAgent(context.Background(), factory.AgentRequest{})
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	initial := runStore.invocations[launch.Invocation.ID]
	initial.NativeSessionID = "session-initial"
	initial.Status = store.InvocationStatusCompleted
	initial.UpdatedAt = initial.CreatedAt.Add(time.Minute)
	runStore.invocations[launch.Invocation.ID] = initial

	first, err := service.CreateDraftPullRequest(context.Background(), factory.DraftPullRequestRequest{RunID: claimed.Run.ID})
	if err != nil {
		t.Fatalf("first CreateDraftPullRequest() error = %v", err)
	}
	if first.Repair == nil || first.Repair.Outcome != factory.CheckRepairStarted || first.Repair.Run.Stage != store.StageImplementation || first.Repair.Run.CheckRepairAttempts != 1 || first.Repair.Remaining != 2 {
		t.Fatalf("first result = %#v, want one active check repair with two attempts remaining", first)
	}
	if first.Repair.Packet.CheckpointSHA != implementationCheckpoint || len(first.Repair.Packet.Gates) != 1 || first.Repair.Packet.Gates[0].Outcome != "failed" {
		t.Fatalf("repair packet = %#v, want all failed gates at the failed checkpoint", first.Repair.Packet)
	}
	if len(harnessRuntime.resumes) != 1 || harnessRuntime.resumes[0].ResumeSessionID != "session-initial" || harnessRuntime.resumes[0].Surface.ID != "surface-implementation" {
		t.Fatalf("resume requests = %#v, want native session and existing surface", harnessRuntime.resumes)
	}
	if !strings.Contains(githubAdapter.editedComments[len(githubAdapter.editedComments)-1].body, "check-repair attempts: 1/3") || !strings.Contains(githubAdapter.editedComments[len(githubAdapter.editedComments)-1].body, "check-repair remaining: 2") {
		t.Fatalf("repair status comment = %q, want visible attempt budget", githubAdapter.editedComments[len(githubAdapter.editedComments)-1].body)
	}

	// Simulate the resumed agent's accepted handoff. The next draft-pr command
	// must create a new checkpoint and rerun the complete suite against it.
	repairedInvocation := first.Repair.Invocation
	repairedInvocation.Status = store.InvocationStatusCompleted
	repairedInvocation.UpdatedAt = repairedInvocation.CreatedAt.Add(2 * time.Minute)
	runStore.invocations[repairedInvocation.ID] = repairedInvocation
	workspace.state.ChangedPaths = []string{"internal/factory/repair.go"}
	runtime.results = append(runtime.results, worker.CommandResult{ExitCode: 0}, worker.CommandResult{ExitCode: 0})
	second, err := service.CreateDraftPullRequest(context.Background(), factory.DraftPullRequestRequest{RunID: claimed.Run.ID})
	if err != nil {
		t.Fatalf("second CreateDraftPullRequest() error = %v", err)
	}
	if second.Repair != nil || second.Run.Stage != store.StageDraftPR || second.Run.CheckpointSHA != repairedImplementationCheckpoint || second.PullRequest.Number != 18 {
		t.Fatalf("second result = %#v, want a draft PR at a fresh checkpoint", second)
	}
	if len(workspace.checkpoints) != 2 || workspace.checkpoints[1].ParentSHA != implementationCheckpoint || len(workspace.pushedSHAs) != 2 {
		t.Fatalf("checkpoint/push effects = %#v/%#v, want repair checkpoint parent and one push per evaluated checkpoint", workspace.checkpoints, workspace.pushedSHAs)
	}
	if workspace.pushedSHAs[0] != implementationCheckpoint || workspace.pushedSHAs[1] != repairedImplementationCheckpoint {
		t.Fatalf("pushed checkpoints = %#v, want each evaluated checkpoint published before its statuses", workspace.pushedSHAs)
	}
	if len(statuses.values) != 2 || statuses.values[0].SHA != implementationCheckpoint || statuses.values[1].SHA != repairedImplementationCheckpoint {
		t.Fatalf("published gate statuses = %#v, want one status per exact checkpoint", statuses.values)
	}
	if len(runtime.commands) != 4 {
		t.Fatalf("gate commands = %#v, want setup and gate for each checkpoint", runtime.commands)
	}
}

// TestCreateDraftPullRequestRejectsAnActiveImplementationInvocation verifies
// the coordinator does not checkpoint while the visible agent still owns the
// implementation stage.
func TestCreateDraftPullRequestRejectsAnActiveImplementationInvocation(t *testing.T) {
	t.Parallel()

	policy := validRepositoryConfig()
	runStore := &activeInvocationRunStore{fakeRunStore: &fakeRunStore{}, active: &store.Invocation{ID: "inv-active", RunID: "run-active", Status: store.InvocationStatusActive}}
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{Path: "/repo", OperationalDataPath: "/outside/factory.db", RepositoryConfigPath: "/repo/factory.yaml"}}}
	githubAdapter := &fakeGitHub{issueValue: github.Issue{Number: 42, State: "open", Labels: []string{github.LabelAgentReady}}}
	workspace := &draftGitWorkspace{workspace: gitadapter.Workspace{BaseSHA: factoryGateCheckpoint, Branch: "factory/run-active", Worktree: "/worktree/run-active"}}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config: &fakeConfig{value: host}, OpenStore: func(context.Context, string) (factory.OperationalStore, error) { return runStore, nil },
		LoadRepository: func(string) (config.RepositoryConfig, error) { return policy, nil },
		GitHub:         githubAdapter, GitWorkspace: workspace, Worktree: workspace,
		NewRunID: func() (string, error) { return "run-active", nil },
	})
	claimed, err := service.ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() fixture setup error = %v", err)
	}
	claimed.Run.Stage = store.StageImplementation
	claimed.Run.Status = store.StatusActive
	if err := runStore.SaveRun(context.Background(), claimed.Run); err != nil {
		t.Fatalf("SaveRun() fixture setup error = %v", err)
	}
	_, err = service.CreateDraftPullRequest(context.Background(), factory.DraftPullRequestRequest{RunID: claimed.Run.ID})
	if err == nil || !strings.Contains(err.Error(), "still has active implementation invocation") {
		t.Fatalf("CreateDraftPullRequest() error = %v, want validation before Git effects", err)
	}
}

// draftGitWorkspace records the host Git effects for coordinator seam tests.
type draftGitWorkspace struct {
	workspace      gitadapter.Workspace
	state          gitadapter.WorktreeState
	checkpoints    []gitadapter.CheckpointRequest
	checkpointSHAs []string
	pushes         []gitadapter.PushRequest
	pushedSHAs     []string
}

// activeInvocationRunStore adds the real-store invocation guard to the small
// run-store fake used by this coordinator test.
type activeInvocationRunStore struct {
	*fakeRunStore
	active *store.Invocation
}

// ActiveInvocation returns the configured active visible implementation.
func (s *activeInvocationRunStore) ActiveInvocation(context.Context, string) (*store.Invocation, error) {
	return s.active, nil
}

// Create implements GitWorkspace for the coordinator test.
func (w *draftGitWorkspace) Create(context.Context, string, string, string) (gitadapter.Workspace, error) {
	if w.workspace.Branch == "" {
		return gitadapter.Workspace{}, errors.New("unexpected Create()")
	}
	return w.workspace, nil
}

// Remove implements GitWorkspace for the coordinator test.
func (*draftGitWorkspace) Remove(context.Context, string, gitadapter.Workspace) error { return nil }

// Inspect returns the configured worktree state.
func (w *draftGitWorkspace) Inspect(context.Context, string) (gitadapter.WorktreeState, error) {
	return w.state, nil
}

// CreateCheckpoint records one idempotent checkpoint effect.
func (w *draftGitWorkspace) CreateCheckpoint(_ context.Context, request gitadapter.CheckpointRequest) (gitadapter.CheckpointResult, error) {
	w.checkpoints = append(w.checkpoints, request)
	checkpoint := implementationCheckpoint
	if len(w.checkpointSHAs) > 0 {
		checkpoint = w.checkpointSHAs[0]
		w.checkpointSHAs = w.checkpointSHAs[1:]
	}
	w.state.HeadSHA = checkpoint
	w.state.ChangedPaths = nil
	return gitadapter.CheckpointResult{SHA: checkpoint, Created: true}, nil
}

// Push records the host-side branch push and the checkpoint it publishes.
func (w *draftGitWorkspace) Push(_ context.Context, request gitadapter.PushRequest) error {
	w.pushes = append(w.pushes, request)
	w.pushedSHAs = append(w.pushedSHAs, w.state.HeadSHA)
	return nil
}

// remoteHasCheckpoint reports whether the branch push already published one
// exact commit, so a status publisher fake can reject an unknown SHA the way
// GitHub does.
func (w *draftGitWorkspace) remoteHasCheckpoint(sha string) bool {
	for _, pushed := range w.pushedSHAs {
		if pushed == sha {
			return true
		}
	}
	return false
}

// SynchronizeBase implements GitWorkspace for the coordinator test.
func (*draftGitWorkspace) SynchronizeBase(context.Context, gitadapter.BaseSyncRequest) error {
	return nil
}

// fakeGitHubWithPullRequests embeds the claim adapter and proves the pull
// request seam remains separate from ordinary issue mutations.
type fakeGitHubWithPullRequests struct {
	*fakeGitHub
}

// PullRequest methods are intentionally absent; PullRequests is injected as a
// separate adapter in the test.

// fakePullRequests records draft pull-request discovery and mutations.
type fakePullRequests struct {
	existing        github.PullRequest
	created         github.PullRequest
	createdRequests []github.PullRequestRequest
	updatedRequests []github.PullRequestRequest
	readyHeadSHA    string
}

// FindPullRequest returns the currently known branch pull request.
func (f *fakePullRequests) FindPullRequest(context.Context, github.Repository, string, string) (github.PullRequest, error) {
	return f.existing, nil
}

// CreatePullRequest records the first draft pull-request mutation.
func (f *fakePullRequests) CreatePullRequest(_ context.Context, _ github.Repository, request github.PullRequestRequest) (github.PullRequest, error) {
	f.createdRequests = append(f.createdRequests, request)
	if f.created.Number == 0 {
		return github.PullRequest{}, errors.New("test pull request identity is not configured")
	}
	return f.created, nil
}

// UpdatePullRequest records regeneration of the generated factory section.
func (f *fakePullRequests) UpdatePullRequest(_ context.Context, _ github.Repository, _ int, request github.PullRequestRequest) (github.PullRequest, error) {
	f.updatedRequests = append(f.updatedRequests, request)
	updated := f.existing
	updated.Title = request.Title
	updated.Body = request.Body
	updated.Draft = request.Draft
	f.existing = updated
	return updated, nil
}

// SetPullRequestDraft records the explicit readiness transition used after all
// review gates pass.
func (f *fakePullRequests) SetPullRequestDraft(_ context.Context, _ github.Repository, _ int, draft bool) (github.PullRequest, error) {
	f.existing.Draft = draft
	updated := f.existing
	if !draft && f.readyHeadSHA != "" {
		updated.HeadSHA = f.readyHeadSHA
	}
	return updated, nil
}

var _ gitadapter.GitWorkspace = (*draftGitWorkspace)(nil)
var _ github.PullRequestClient = (*fakePullRequests)(nil)
