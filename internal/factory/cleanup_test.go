package factory_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestCleanupPreviewRequiresConfirmationAndProtectsEligibleRun fixes the
// public cleanup preview contract before cleanup effects are implemented.
func TestCleanupPreviewRequiresConfirmationAndProtectsEligibleRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	operationalPath := filepath.Join(root, "state", "factory.db")
	worktreePath := filepath.Join(root, "worktrees", "run-eligible")
	invocationRoot := filepath.Join(root, "worktrees", ".factory-agents", "run-eligible", "invocation-1")
	invocationPath := filepath.Join(invocationRoot, "packet")
	resultPath := filepath.Join(invocationRoot, "results")
	for _, path := range []string{worktreePath, invocationPath, resultPath, filepath.Join(root, "worktrees", ".factory-git", "run-eligible")} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Date(2026, time.January, 20, 12, 0, 0, 0, time.UTC)
	terminalAt := now.Add(-8 * 24 * time.Hour)
	opened, err := store.Open(ctx, operationalPath)
	if err != nil {
		t.Fatal(err)
	}
	run := store.Run{
		ID:             "run-eligible",
		RepositoryPath: repositoryPath,
		IssueNumber:    23,
		Stage:          store.StageReady,
		Status:         store.StatusComplete,
		Branch:         "factory/run-eligible",
		Worktree:       worktreePath,
		TerminalAt:     terminalAt,
		CreatedAt:      terminalAt.Add(-time.Hour),
		UpdatedAt:      terminalAt,
	}
	if err := opened.SaveRun(ctx, run); err != nil {
		_ = opened.Close()
		t.Fatal(err)
	}
	if err := opened.EnsureEvaluationSummary(ctx, run); err != nil {
		_ = opened.Close()
		t.Fatal(err)
	}
	if err := opened.FinalizeEvaluation(ctx, run.ID, store.EvaluationOutcomeComplete, terminalAt); err != nil {
		_ = opened.Close()
		t.Fatal(err)
	}
	if err := opened.SaveInvocation(ctx, store.Invocation{
		ID:                  "invocation-1",
		RunID:               "run-eligible",
		Harness:             "codex",
		Role:                "implementation",
		Stage:               store.StageImplementation,
		Status:              store.InvocationStatusCompleted,
		InvocationDirectory: invocationPath,
		ResultDirectory:     resultPath,
		CreatedAt:           terminalAt,
		UpdatedAt:           terminalAt,
	}); err != nil {
		_ = opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	workspace := &cleanupTestWorkspace{}
	runtime := &cleanupTestWorker{}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config: &fakeConfigRepository{value: config.HostConfig{
			SchemaVersion: 1,
			Repositories: []config.RepositoryRegistration{{
				Path:                repositoryPath,
				OperationalDataPath: operationalPath,
			}},
		}},
		OpenStore: func(ctx context.Context, path string) (factory.OperationalStore, error) {
			return store.Open(ctx, path)
		},
		GitWorkspace: workspace,
		Worker:       runtime,
		Now: func() time.Time {
			return now
		},
	})

	result, err := service.Cleanup(ctx, factory.CleanupRequest{})
	var confirmationErr *factory.CleanupConfirmationRequiredError
	if !errors.As(err, &confirmationErr) {
		t.Fatalf("Cleanup() error = %v, want CleanupConfirmationRequiredError", err)
	}
	if len(result.Plan.Runs) != 1 {
		t.Fatalf("cleanup plan runs = %d, want 1", len(result.Plan.Runs))
	}
	if result.Plan.Runs[0].RunID != "run-eligible" {
		t.Fatalf("planned run = %q, want run-eligible", result.Plan.Runs[0].RunID)
	}
	if len(workspace.removed) != 0 {
		t.Fatalf("workspace removals = %d, want preview to be side-effect free", len(workspace.removed))
	}
	if len(runtime.cleaned) != 0 {
		t.Fatalf("worker cleanups = %d, want preview to be side-effect free", len(runtime.cleaned))
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("worktree after preview: %v", err)
	}
	if _, err := os.Stat(operationalPath); err != nil {
		t.Fatalf("operational store after preview: %v", err)
	}

	confirmed, err := service.Cleanup(ctx, factory.CleanupRequest{Confirm: true})
	if err != nil {
		t.Fatalf("confirmed Cleanup() error = %v", err)
	}
	if len(confirmed.Deleted) != 1 || confirmed.Deleted[0].RunID != "run-eligible" {
		t.Fatalf("confirmed deletions = %#v, want run-eligible", confirmed.Deleted)
	}
	if len(workspace.removed) != 1 || workspace.removed[0].Branch != "factory/run-eligible" {
		t.Fatalf("workspace removals = %#v, want the run-owned branch", workspace.removed)
	}
	if len(runtime.cleaned) != 1 || runtime.cleaned[0].RunID != "run-eligible" {
		t.Fatalf("worker cleanups = %#v, want run-eligible", runtime.cleaned)
	}
	for _, path := range []string{worktreePath, invocationPath, resultPath, filepath.Join(root, "worktrees", ".factory-git", "run-eligible")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cleanup path %q still exists or returned unexpected error: %v", path, err)
		}
	}
	reopened, err := store.Open(ctx, operationalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if latest, err := reopened.LatestRun(ctx); err != nil {
		t.Fatal(err)
	} else if latest != nil {
		t.Fatalf("LatestRun() = %#v, want operational run deleted", latest)
	}
	if summary, err := reopened.EvaluationSummary(ctx, run.ID); err != nil {
		t.Fatal(err)
	} else if summary == nil || summary.Outcome != store.EvaluationOutcomeComplete {
		t.Fatalf("EvaluationSummary() = %#v, want retained completed summary", summary)
	}
	pendingWorktree := filepath.Join(root, "worktrees", "run-pending")
	if err := os.MkdirAll(pendingWorktree, 0o755); err != nil {
		t.Fatal(err)
	}
	pendingRun := store.Run{
		ID:             "run-pending",
		RepositoryPath: repositoryPath,
		IssueNumber:    24,
		Stage:          store.StageReady,
		Status:         store.StatusComplete,
		Branch:         "factory/run-pending",
		Worktree:       pendingWorktree,
		TerminalAt:     terminalAt,
		CreatedAt:      terminalAt.Add(-time.Hour),
		UpdatedAt:      terminalAt,
	}
	if err := reopened.SaveRun(ctx, pendingRun); err != nil {
		t.Fatal(err)
	}
	if err := reopened.SavePendingEffect(ctx, store.PendingEffect{
		RunID:     pendingRun.ID,
		ID:        "effect-pending",
		Kind:      store.PendingEffectKindWorkerLaunch,
		Payload:   `{"run_id":"run-pending"}`,
		CreatedAt: terminalAt,
		UpdatedAt: terminalAt,
	}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	blocked, err := service.Cleanup(ctx, factory.CleanupRequest{Confirm: true})
	var blockedErr *factory.CleanupBlockedError
	if !errors.As(err, &blockedErr) {
		t.Fatalf("Cleanup() with pending effect error = %v, want CleanupBlockedError", err)
	}
	if len(blocked.Plan.Runs) != 0 || len(blocked.Plan.Skipped) != 1 || blocked.Plan.Skipped[0].RunID != pendingRun.ID {
		t.Fatalf("blocked cleanup plan = %#v, want only pending run skipped", blocked.Plan)
	}
	if len(workspace.removed) != 1 || len(runtime.cleaned) != 1 {
		t.Fatalf("blocked cleanup performed effects: workspaces=%#v workers=%#v", workspace.removed, runtime.cleaned)
	}
	if _, err := os.Stat(pendingWorktree); err != nil {
		t.Fatalf("pending run worktree after blocked cleanup: %v", err)
	}
}

// TestCleanupKeepsAnOpenPullRequestRun verifies that terminal age alone does
// not remove local state while the tracked pull request remains open.
func TestCleanupKeepsAnOpenPullRequestRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	worktreePath := filepath.Join(root, "worktrees", "run-open-pr")
	operationalPath := filepath.Join(root, "state", "factory.db")
	for _, path := range []string{repositoryPath, worktreePath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, time.January, 20, 12, 0, 0, 0, time.UTC)
	opened, err := store.Open(ctx, operationalPath)
	if err != nil {
		t.Fatal(err)
	}
	run := store.Run{
		ID:                  "run-open-pr",
		RepositoryPath:      repositoryPath,
		IssueNumber:         23,
		Stage:               store.StageReady,
		Status:              store.StatusCancelled,
		Branch:              "factory/run-open-pr",
		Worktree:            worktreePath,
		PullRequestNumber:   42,
		SpecificationPacket: `{"version":1,"repository_config":{"target_branch":"main"}}`,
		TerminalAt:          now.Add(-8 * 24 * time.Hour),
		CreatedAt:           now.Add(-9 * 24 * time.Hour),
		UpdatedAt:           now.Add(-8 * 24 * time.Hour),
	}
	if err := opened.SaveRun(ctx, run); err != nil {
		_ = opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	workspace := &cleanupTestWorkspace{}
	runtime := &cleanupTestWorker{}
	pullRequests := &fakePullRequests{existing: github.PullRequest{
		Number:     run.PullRequestNumber,
		State:      "open",
		HeadBranch: run.Branch,
		BaseBranch: "main",
	}}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config: &fakeConfigRepository{value: config.HostConfig{
			SchemaVersion: 1,
			Repositories: []config.RepositoryRegistration{{
				Path:                repositoryPath,
				GitHub:              config.GitHubConfig{Owner: "example", Repository: "project"},
				OperationalDataPath: operationalPath,
			}},
		}},
		OpenStore: func(ctx context.Context, path string) (factory.OperationalStore, error) {
			return store.Open(ctx, path)
		},
		GitWorkspace: workspace,
		Worker:       runtime,
		PullRequests: pullRequests,
		Now: func() time.Time {
			return now
		},
	})

	if _, err := service.Cleanup(ctx, factory.CleanupRequest{Before: now.Add(-6 * 24 * time.Hour)}); err == nil {
		t.Fatal("Cleanup() accepted a cutoff newer than the seven-day retention boundary")
	}
	result, err := service.Cleanup(ctx, factory.CleanupRequest{Confirm: true})
	var blockedErr *factory.CleanupBlockedError
	if !errors.As(err, &blockedErr) {
		t.Fatalf("Cleanup() error = %v, want CleanupBlockedError", err)
	}
	if len(result.Plan.Runs) != 0 || len(result.Plan.Skipped) != 1 || result.Plan.Skipped[0].RunID != run.ID {
		t.Fatalf("cleanup plan = %#v, want the open-PR run skipped", result.Plan)
	}
	if len(workspace.removed) != 0 || len(runtime.cleaned) != 0 {
		t.Fatalf("cleanup effects = workspaces=%#v workers=%#v, want none", workspace.removed, runtime.cleaned)
	}
	reopened, err := store.Open(ctx, operationalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if retained, err := reopened.LatestRun(ctx); err != nil {
		t.Fatal(err)
	} else if retained == nil || retained.ID != run.ID {
		t.Fatalf("LatestRun() = %#v, want open-PR run retained", retained)
	}
}

// cleanupTestWorkspace records host cleanup effects for the Factory seam.
type cleanupTestWorkspace struct {
	removed []gitadapter.Workspace
}

// Create returns an unused workspace because preview cleanup never creates one.
func (*cleanupTestWorkspace) Create(context.Context, string, string, string) (gitadapter.Workspace, error) {
	return gitadapter.Workspace{}, errors.New("unexpected workspace creation")
}

// Remove records the exact workspace selected for deletion.
func (w *cleanupTestWorkspace) Remove(_ context.Context, _ string, workspace gitadapter.Workspace) error {
	w.removed = append(w.removed, workspace)
	if err := os.RemoveAll(workspace.Worktree); err != nil {
		return err
	}
	if workspace.RunID != "" {
		return os.RemoveAll(filepath.Join(filepath.Dir(workspace.Worktree), ".factory-git", workspace.RunID))
	}
	return nil
}

// Inspect satisfies the GitWorkspace contract for cleanup tests.
func (*cleanupTestWorkspace) Inspect(context.Context, string) (gitadapter.WorktreeState, error) {
	return gitadapter.WorktreeState{}, nil
}

// CreateCheckpoint satisfies the GitWorkspace contract for cleanup tests.
func (*cleanupTestWorkspace) CreateCheckpoint(context.Context, gitadapter.CheckpointRequest) (gitadapter.CheckpointResult, error) {
	return gitadapter.CheckpointResult{}, nil
}

// Push satisfies the GitWorkspace contract for cleanup tests.
func (*cleanupTestWorkspace) Push(context.Context, gitadapter.PushRequest) error { return nil }

// SynchronizeBase satisfies the GitWorkspace contract for cleanup tests.
func (*cleanupTestWorkspace) SynchronizeBase(context.Context, gitadapter.BaseSyncRequest) error {
	return nil
}

// cleanupTestWorker records worker cleanup effects for the Factory seam.
type cleanupTestWorker struct {
	cleaned []worker.CleanupRequest
}

// Start satisfies WorkerRuntime for cleanup tests.
func (*cleanupTestWorker) Start(context.Context, worker.StartRequest) error { return nil }

// Resume satisfies WorkerRuntime for cleanup tests.
func (*cleanupTestWorker) Resume(context.Context, worker.ResumeRequest) error { return nil }

// RunCommand satisfies WorkerRuntime for cleanup tests.
func (*cleanupTestWorker) RunCommand(context.Context, worker.CommandRequest) (worker.CommandResult, error) {
	return worker.CommandResult{}, nil
}

// Stop satisfies WorkerRuntime for cleanup tests.
func (*cleanupTestWorker) Stop(context.Context, string) error { return nil }

// Inspect satisfies WorkerRuntime for cleanup tests.
func (*cleanupTestWorker) Inspect(context.Context, string) (worker.Inspection, error) {
	return worker.Inspection{}, nil
}

// Cleanup records the exact worker cleanup request.
func (w *cleanupTestWorker) Cleanup(_ context.Context, request worker.CleanupRequest) error {
	w.cleaned = append(w.cleaned, request)
	for _, path := range request.StoredOutputs {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}
