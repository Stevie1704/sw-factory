package factory_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// linkingPullRequests is a code host with native work-item links over the
// recording pull-request fake.
type linkingPullRequests struct {
	*fakePullRequests
	links []codehost.WorkItemLink
}

// EnsurePullRequestWorkItem records the verified association.
func (l *linkingPullRequests) EnsurePullRequestWorkItem(_ context.Context, _ tracker.Repository, _ int, link codehost.WorkItemLink) error {
	l.links = append(l.links, link)
	return nil
}

// TestCreateDraftPullRequestLinksTheSourceWorkItemOfAFrozenRepository
// verifies that the draft pull request of a Run with a frozen Azure Boards
// repository names its issue as the source work item in that repository,
// and that the association is verified after creation (ADR 0030).
func TestCreateDraftPullRequestLinksTheSourceWorkItemOfAFrozenRepository(t *testing.T) {
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
	policy := validRepositoryConfig()
	policy.Gates = []config.GateConfig{{Name: "test", Command: "test", Timeout: "5m", Blocking: true, EnvironmentPolicy: config.EnvironmentPolicyClean}}
	runStore := &fakeRunStore{}
	issues := &fakeGitHub{issueValue: tracker.Issue{Number: 42, Title: "Add export", Body: "Export CSV.", State: "open", Labels: []string{tracker.LabelAgentReady}}}
	workspace := &draftGitWorkspace{
		workspace: gitadapter.Workspace{BaseSHA: factoryGateCheckpoint, Branch: "factory/run-draft", Worktree: worktreePath},
		state:     gitadapter.WorktreeState{Branch: "factory/run-draft", HeadSHA: factoryGateCheckpoint, ChangedPaths: []string{"internal/export.go"}},
	}
	pullRequests := &linkingPullRequests{fakePullRequests: &fakePullRequests{created: codehost.PullRequest{Number: 17, State: "open", Draft: true, HeadBranch: "factory/run-draft", BaseBranch: "main"}}}
	host := config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{{
		Path: repositoryPath, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
		OperationalDataPath: filepath.Join(root, "state", "factory.db"), RepositoryConfigPath: filepath.Join(repositoryPath, "factory.yaml"),
	}}}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:         &fakeConfig{value: host},
		OpenStore:      func(context.Context, string) (factory.OperationalStore, error) { return runStore, nil },
		LoadRepository: func(string) (config.RepositoryConfig, error) { return policy, nil },
		Tracker:        &fakeGitHubWithPullRequests{fakeGitHub: issues},
		PullRequests:   pullRequests,
		Worktree:       workspace,
		GitWorkspace:   workspace,
		Worker:         &gateWorker{results: []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}}},
		CommitStatuses: &gateStatuses{},
		Now:            func() time.Time { return time.Date(2026, 8, 21, 10, 1, 0, 0, time.UTC) },
		NewRunID:       func() (string, error) { return "run-draft", nil },
	})
	claimed, err := service.ClaimIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("ClaimIssue() fixture setup error = %v", err)
	}
	claimed.Run.Stage = store.StageImplementation
	claimed.Run.Status = store.StatusActive
	claimed.Run.TrackerRepository = &store.TrackerRepository{Organization: "contoso", ProjectID: "6f9b0c5e-2a41-4d8e-9a43-1c2f5e7d8a90", Project: "Factory Pilot", RepositoryID: "3b1f6a2e-8c4d-4e5f-9a6b-7c8d9e0f1a2b", Repository: "service"}
	if err := runStore.SaveRun(context.Background(), claimed.Run); err != nil {
		t.Fatalf("SaveRun() fixture setup error = %v", err)
	}

	result, err := service.CreateDraftPullRequest(context.Background(), factory.DraftPullRequestRequest{RunID: claimed.Run.ID})
	if err != nil || result.Repair != nil {
		t.Fatalf("CreateDraftPullRequest() = %#v/%v, want a draft pull request", result, err)
	}
	want := codehost.WorkItemLink{ID: 42, RepositoryID: "3b1f6a2e-8c4d-4e5f-9a6b-7c8d9e0f1a2b"}
	if len(pullRequests.createdRequests) != 1 || pullRequests.createdRequests[0].WorkItem == nil || *pullRequests.createdRequests[0].WorkItem != want {
		t.Fatalf("created requests = %#v, want one naming PBI 42 in the frozen repository", pullRequests.createdRequests)
	}
	if len(pullRequests.links) != 1 || pullRequests.links[0] != want {
		t.Fatalf("verified links = %#v, want PBI 42 verified once", pullRequests.links)
	}
}
