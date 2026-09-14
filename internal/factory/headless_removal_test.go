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

// TestCleanupConfirmsHeadlessResourcesForEveryHarness verifies confirmed
// retention cleanup removes only Git, worker, output, and store projections.
func TestCleanupConfirmsHeadlessResourcesForEveryHarness(t *testing.T) {
	for _, harnessName := range []config.Harness{config.HarnessCodex, config.HarnessClaude} {
		t.Run(string(harnessName), func(t *testing.T) {
			fixture := newHeadlessRemovalFixture(t, harnessName)
			preview, err := fixture.service.Cleanup(t.Context(), factory.CleanupRequest{})
			var confirmation *factory.CleanupConfirmationRequiredError
			if !errors.As(err, &confirmation) || len(preview.Plan.Runs) != 1 {
				t.Fatalf("Cleanup(preview) = %#v, %v; want one confirmation plan", preview, err)
			}

			result, err := fixture.service.Cleanup(t.Context(), factory.CleanupRequest{
				Confirm: true, Before: preview.Plan.Before, ExpectedPlan: &preview.Plan,
			})
			if err != nil {
				t.Fatalf("Cleanup(confirm) error = %v", err)
			}
			if len(result.Deleted) != 1 || len(fixture.worker.cleanups) != 1 || len(fixture.git.removed) != 1 {
				t.Fatalf("confirmed cleanup = deleted %#v worker %#v Git %#v", result.Deleted, fixture.worker.cleanups, fixture.git.removed)
			}
			if _, err := os.Stat(fixture.worktreePath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("worktree after confirmed cleanup error = %v, want absent", err)
			}
			opened, err := store.Open(t.Context(), fixture.databasePath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = opened.Close() }()
			if latest, err := opened.LatestRun(t.Context()); err != nil || latest != nil {
				t.Fatalf("LatestRun() = %#v, %v; want deleted operational run", latest, err)
			}
		})
	}
}

// TestResetConfirmsHeadlessResourcesForEveryHarness verifies a full reset
// deletes the registered local installation without any local-UI discovery.
func TestResetConfirmsHeadlessResourcesForEveryHarness(t *testing.T) {
	for _, harnessName := range []config.Harness{config.HarnessCodex, config.HarnessClaude} {
		t.Run(string(harnessName), func(t *testing.T) {
			fixture := newHeadlessRemovalFixture(t, harnessName)
			result, err := fixture.service.Reset(t.Context(), factory.ResetRequest{Confirm: true})
			if err != nil {
				t.Fatalf("Reset(confirm) error = %v", err)
			}
			if len(result.Remaining) != 0 || len(fixture.worker.cleanups) != 1 || len(fixture.worker.credentials) != 1 || len(fixture.git.removed) != 1 {
				t.Fatalf("confirmed reset = %#v, worker=%#v credentials=%#v Git=%#v", result, fixture.worker.cleanups, fixture.worker.credentials, fixture.git.removed)
			}
			for _, path := range []string{fixture.configPath, fixture.databasePath, fixture.worktreePath} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("reset target %q error = %v, want absent", path, err)
				}
			}
			if _, err := os.Stat(fixture.repositoryPath); err != nil {
				t.Fatalf("registered source checkout was not retained: %v", err)
			}
		})
	}
}

// TestCleanupRetainsGitAndStoreStateWhenWorkerCleanupFails verifies the
// destructive ordering preserves recoverable state after an adapter failure.
func TestCleanupRetainsGitAndStoreStateWhenWorkerCleanupFails(t *testing.T) {
	fixture := newHeadlessRemovalFixture(t, config.HarnessCodex)
	preview, err := fixture.service.Cleanup(t.Context(), factory.CleanupRequest{})
	var confirmation *factory.CleanupConfirmationRequiredError
	if !errors.As(err, &confirmation) {
		t.Fatalf("Cleanup(preview) error = %v", err)
	}
	fixture.worker.cleanupErr = errors.New("worker cleanup unavailable")
	if _, err := fixture.service.Cleanup(t.Context(), factory.CleanupRequest{
		Confirm: true, Before: preview.Plan.Before, ExpectedPlan: &preview.Plan,
	}); err == nil {
		t.Fatal("Cleanup(confirm) error = nil, want worker failure")
	}
	if len(fixture.git.removed) != 0 {
		t.Fatalf("Git removals = %#v, want none after worker failure", fixture.git.removed)
	}
	opened, err := store.Open(t.Context(), fixture.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	if latest, err := opened.LatestRun(t.Context()); err != nil || latest == nil {
		t.Fatalf("LatestRun() = %#v, %v; want retained run", latest, err)
	}
}

// TestResetPreflightsGitBeforeDeletingHeadlessResources verifies a confirmed
// reset fails closed before worker, store, worktree, or configuration removal.
func TestResetPreflightsGitBeforeDeletingHeadlessResources(t *testing.T) {
	fixture := newHeadlessRemovalFixture(t, config.HarnessClaude)
	fixture.git.checkErr = errors.New("Git removal preflight unavailable")
	if _, err := fixture.service.Reset(t.Context(), factory.ResetRequest{Confirm: true}); err == nil {
		t.Fatal("Reset(confirm) error = nil, want preflight refusal")
	}
	if len(fixture.worker.cleanups) != 0 || len(fixture.git.removed) != 0 {
		t.Fatalf("destructive calls after failed preflight = worker %#v Git %#v", fixture.worker.cleanups, fixture.git.removed)
	}
	for _, path := range []string{fixture.configPath, fixture.databasePath, fixture.worktreePath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("failed reset removed %q: %v", path, err)
		}
	}
}

// TestResetRefusesALiveGitHubRunBeforeDeletingHeadlessResources verifies reset
// cannot invent a terminal outcome for an open supervised run.
func TestResetRefusesALiveGitHubRunBeforeDeletingHeadlessResources(t *testing.T) {
	fixture := newHeadlessRemovalFixture(t, config.HarnessCodex)
	opened, err := store.Open(t.Context(), fixture.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	run, err := opened.LatestRun(t.Context())
	if err != nil || run == nil {
		t.Fatalf("LatestRun() = %#v, %v", run, err)
	}
	run.Status = store.StatusActive
	run.Stage = store.StageImplementation
	run.TerminalAt = time.Time{}
	if err := opened.SaveRun(t.Context(), *run); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.Reset(t.Context(), factory.ResetRequest{Confirm: true})
	var blocked *factory.ResetBlockedError
	if !errors.As(err, &blocked) || len(blocked.Blockers) != 1 || blocked.Blockers[0].RunID != run.ID {
		t.Fatalf("Reset(confirm) = %#v, %v; want live-run blocker", result, err)
	}
	if len(fixture.worker.cleanups) != 0 || len(fixture.git.removed) != 0 {
		t.Fatalf("live-run refusal made destructive calls: worker %#v Git %#v", fixture.worker.cleanups, fixture.git.removed)
	}
}

// TestResetRefusesAnUnavailableWorkerBeforeDeletingHeadlessResources verifies
// adapter preflight retains every target when Docker cannot be reached.
func TestResetRefusesAnUnavailableWorkerBeforeDeletingHeadlessResources(t *testing.T) {
	fixture := newHeadlessRemovalFixture(t, config.HarnessClaude)
	fixture.worker.dockerErr = errors.New("Docker unavailable")
	if _, err := fixture.service.Reset(t.Context(), factory.ResetRequest{Confirm: true}); err == nil {
		t.Fatal("Reset(confirm) error = nil, want worker preflight refusal")
	}
	if len(fixture.worker.cleanups) != 0 || len(fixture.git.removed) != 0 {
		t.Fatalf("worker refusal made destructive calls: worker %#v Git %#v", fixture.worker.cleanups, fixture.git.removed)
	}
	for _, path := range []string{fixture.configPath, fixture.databasePath, fixture.worktreePath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("failed reset removed %q: %v", path, err)
		}
	}
}

// TestResetPreviewIsReadOnly verifies inspecting a headless installation does
// not remove resources or open the operational store through its mutating seam.
func TestResetPreviewIsReadOnly(t *testing.T) {
	fixture := newHeadlessRemovalFixture(t, config.HarnessCodex)
	result, err := fixture.service.Reset(t.Context(), factory.ResetRequest{})
	var confirmation *factory.ResetConfirmationRequiredError
	if !errors.As(err, &confirmation) {
		t.Fatalf("Reset(preview) error = %v, want confirmation requirement", err)
	}
	if len(result.Plan.Runs) != 1 || len(fixture.worker.cleanups) != 0 || len(fixture.git.removed) != 0 {
		t.Fatalf("preview = %#v worker %#v Git %#v, want one run and no mutations", result.Plan, fixture.worker.cleanups, fixture.git.removed)
	}
	for _, path := range []string{fixture.configPath, fixture.databasePath, fixture.worktreePath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("preview changed %q: %v", path, err)
		}
	}
}

// TestResetRetriesAfterPartialHeadlessRemoval verifies a failed Git removal
// retains the store and registration needed to retry the reduced reset plan.
func TestResetRetriesAfterPartialHeadlessRemoval(t *testing.T) {
	fixture := newHeadlessRemovalFixture(t, config.HarnessClaude)
	fixture.git.removeErr = errors.New("worktree is locked")
	result, err := fixture.service.Reset(t.Context(), factory.ResetRequest{Confirm: true})
	var incomplete *factory.ResetIncompleteError
	if !errors.As(err, &incomplete) || len(result.Remaining) != 1 {
		t.Fatalf("Reset(first) = %#v, %v; want one remaining Git target", result, err)
	}
	for _, path := range []string{fixture.configPath, fixture.databasePath, fixture.worktreePath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("partial reset removed retry state %q: %v", path, err)
		}
	}

	fixture.git.removeErr = nil
	retried, err := fixture.service.Reset(t.Context(), factory.ResetRequest{Confirm: true})
	if err != nil || len(retried.Remaining) != 0 {
		t.Fatalf("Reset(retry) = %#v, %v; want completed reset", retried, err)
	}
	for _, path := range []string{fixture.configPath, fixture.databasePath, fixture.worktreePath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retry target %q error = %v, want absent", path, err)
		}
	}
}

type headlessRemovalFixture struct {
	service                                                *factory.Service
	worker                                                 *headlessRemovalWorker
	git                                                    *headlessRemovalGit
	configPath, databasePath, repositoryPath, worktreePath string
}

// newHeadlessRemovalFixture creates one old terminal-state run whose active
// resources are exclusively headless.
func newHeadlessRemovalFixture(t *testing.T, harnessName config.Harness) headlessRemovalFixture {
	t.Helper()
	root := t.TempDir()
	configPath := filepath.Join(root, "host", "config.yaml")
	databasePath := filepath.Join(root, "state", "factory.db")
	repositoryPath := filepath.Join(root, "repository")
	runID := "run-headless-" + string(harnessName)
	worktreePath := filepath.Join(root, "worktrees", runID)
	for _, path := range []string{repositoryPath, worktreePath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	host := config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{{
		Path: repositoryPath, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
		AuthorizedUsers: []string{"alice"}, Polling: config.PollingConfig{Interval: "30s", Backoff: "5m"},
		OperationalDataPath: databasePath, RepositoryConfigPath: filepath.Join(repositoryPath, config.RepositoryConfigFileName),
	}}}
	if err := config.SaveHost(configPath, host); err != nil {
		t.Fatal(err)
	}
	opened, err := store.Open(t.Context(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Now().UTC().Add(-8 * 24 * time.Hour)
	if err := opened.SaveRun(t.Context(), store.Run{
		ID: runID, RepositoryPath: repositoryPath, IssueNumber: 165,
		Stage: store.StageReady, Status: store.StatusComplete,
		Branch: "factory/" + runID, Worktree: worktreePath, TerminalAt: when,
		CreatedAt: when.Add(-time.Hour), UpdatedAt: when,
	}); err != nil {
		_ = opened.Close()
		t.Fatal(err)
	}
	if err := opened.SaveInvocation(t.Context(), store.Invocation{
		ID: "inv-" + string(harnessName), RunID: runID, Harness: string(harnessName),
		Role: "implementation", Stage: store.StageImplementation,
		Status: store.InvocationStatusCompleted, NativeSessionID: "native-" + string(harnessName),
		CredentialStoreID: repositoryPath, CreatedAt: when, UpdatedAt: when,
	}); err != nil {
		_ = opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	runtime := &headlessRemovalWorker{}
	gitRuntime := &headlessRemovalGit{}
	service := factory.NewWithDependencies(configPath, factory.Dependencies{
		Config: &fakeConfig{value: host},
		OpenStore: func(ctx context.Context, path string) (factory.OperationalStore, error) {
			return store.Open(ctx, path)
		},
		OpenStoreReadOnly: func(ctx context.Context, path string) (factory.OperationalStore, error) {
			return store.OpenReadOnly(ctx, path)
		},
		Worker: runtime, GitWorkspace: gitRuntime,
		GitHub: &fakeGitHub{issueValue: github.Issue{Number: 165, State: "open"}},
		Now:    func() time.Time { return time.Now().UTC() },
	})
	return headlessRemovalFixture{
		service: service, worker: runtime, git: gitRuntime, configPath: configPath,
		databasePath: databasePath, repositoryPath: repositoryPath, worktreePath: worktreePath,
	}
}

// headlessRemovalWorker records only worker-resource cleanup operations.
type headlessRemovalWorker struct {
	cleanups    []worker.CleanupRequest
	credentials []worker.RemoveCredentialStoreRequest
	cleanupErr  error
	dockerErr   error
}

// Start is unused by removal tests.
func (*headlessRemovalWorker) Start(context.Context, worker.StartRequest) error { return nil }

// Resume is unused by removal tests.
func (*headlessRemovalWorker) Resume(context.Context, worker.ResumeRequest) error { return nil }

// RunCommand is unused by removal tests.
func (*headlessRemovalWorker) RunCommand(context.Context, worker.CommandRequest) (worker.CommandResult, error) {
	return worker.CommandResult{}, nil
}

// Stop is unused by removal tests.
func (*headlessRemovalWorker) Stop(context.Context, string) error { return nil }

// Inspect reports no live worker during removal planning.
func (*headlessRemovalWorker) Inspect(context.Context, string) (worker.Inspection, error) {
	return worker.Inspection{}, nil
}

// Cleanup records and removes only explicitly supplied temporary outputs.
func (w *headlessRemovalWorker) Cleanup(_ context.Context, request worker.CleanupRequest) error {
	w.cleanups = append(w.cleanups, request)
	if w.cleanupErr != nil {
		return w.cleanupErr
	}
	for _, path := range request.StoredOutputs {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}

// RemoveCredentialStore records exact reset-only credential removal.
func (w *headlessRemovalWorker) RemoveCredentialStore(_ context.Context, request worker.RemoveCredentialStoreRequest) error {
	w.credentials = append(w.credentials, request)
	return nil
}

// CheckDocker reports the injected worker adapter ready for reset.
func (w *headlessRemovalWorker) CheckDocker(context.Context) error { return w.dockerErr }

// headlessRemovalGit records and performs exact temporary worktree removal.
type headlessRemovalGit struct {
	removed   []gitadapter.Workspace
	checkErr  error
	removeErr error
}

// Create is outside the removal test contract.
func (*headlessRemovalGit) Create(context.Context, string, string, string) (gitadapter.Workspace, error) {
	return gitadapter.Workspace{}, errors.New("unexpected create")
}

// Remove records and deletes the exact temporary worktree.
func (g *headlessRemovalGit) Remove(_ context.Context, _ string, workspace gitadapter.Workspace) error {
	g.removed = append(g.removed, workspace)
	if g.removeErr != nil {
		return g.removeErr
	}
	return os.RemoveAll(workspace.Worktree)
}

// Inspect is unused by removal tests.
func (*headlessRemovalGit) Inspect(context.Context, string) (gitadapter.WorktreeState, error) {
	return gitadapter.WorktreeState{}, nil
}

// CreateCheckpoint is outside the removal test contract.
func (*headlessRemovalGit) CreateCheckpoint(context.Context, gitadapter.CheckpointRequest) (gitadapter.CheckpointResult, error) {
	return gitadapter.CheckpointResult{}, errors.New("unexpected checkpoint")
}

// Push is outside the removal test contract.
func (*headlessRemovalGit) Push(context.Context, gitadapter.PushRequest) error {
	return errors.New("unexpected push")
}

// SynchronizeBase is outside the removal test contract.
func (*headlessRemovalGit) SynchronizeBase(context.Context, gitadapter.BaseSyncRequest) error {
	return errors.New("unexpected base synchronization")
}

// CheckRemoval returns the configured preflight outcome.
func (g *headlessRemovalGit) CheckRemoval(context.Context, string, gitadapter.Workspace) error {
	return g.checkErr
}
