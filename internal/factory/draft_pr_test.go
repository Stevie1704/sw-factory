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
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

const implementationCheckpoint = "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"

// TestCreateDraftPullRequestRunsGatesBeforeTheFirstPushAndCreatesOneDraft
// verifies the complete host-owned implementation boundary at the Factory
// seam.
func TestCreateDraftPullRequestRunsGatesBeforeTheFirstPushAndCreatesOneDraft(t *testing.T) {
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
	workerRuntime := &gateWorker{results: []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}, {ExitCode: 0}, {ExitCode: 0}}}
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
	if len(workerRuntime.commands) != 4 || workerRuntime.commands[1].Command != "format" || workerRuntime.commands[3].Command != "test" {
		t.Fatalf("gate commands = %#v, want setup/gate for each declared gate", workerRuntime.commands)
	}
	body := pullRequests.createdRequests[0].Body
	for _, expected := range []string{
		"<!-- factory-generated:start -->", "Add the factory handoff", "Implement the supervised draft PR boundary.",
		"stage: `draft_pr`", "format", "test", "intervention: `none`", "factory status", "factory draft-pr --run-id run-draft",
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
	workspace   gitadapter.Workspace
	state       gitadapter.WorktreeState
	checkpoints []gitadapter.CheckpointRequest
	pushes      []gitadapter.PushRequest
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
	w.state.HeadSHA = implementationCheckpoint
	w.state.ChangedPaths = nil
	return gitadapter.CheckpointResult{SHA: implementationCheckpoint, Created: true}, nil
}

// Push records the host-side branch push.
func (w *draftGitWorkspace) Push(_ context.Context, request gitadapter.PushRequest) error {
	w.pushes = append(w.pushes, request)
	return nil
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
	updated.Body = request.Body
	return updated, nil
}

var _ gitadapter.GitWorkspace = (*draftGitWorkspace)(nil)
var _ github.PullRequestClient = (*fakePullRequests)(nil)
