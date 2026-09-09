package factory_test

import (
	"context"
	"encoding/json"
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
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
	"golang.org/x/sys/unix"
)

// TestResetPreviewsAnEmptyInstallationWithoutMutation fixes the read-only
// preview contract for a registered installation that never claimed a run.
func TestResetPreviewsAnEmptyInstallationWithoutMutation(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t)
	result, err := fixture.service.Reset(context.Background(), factory.ResetRequest{})
	var confirmationErr *factory.ResetConfirmationRequiredError
	if !errors.As(err, &confirmationErr) {
		t.Fatalf("Reset() error = %v, want ResetConfirmationRequiredError", err)
	}
	if len(result.Plan.Runs) != 0 {
		t.Fatalf("planned runs = %d, want none", len(result.Plan.Runs))
	}
	if result.Plan.ConfigPath != fixture.configPath || result.Plan.OperationalDataPath != fixture.operationalPath {
		t.Fatalf("plan config/database = %q/%q, want %q/%q", result.Plan.ConfigPath, result.Plan.OperationalDataPath, fixture.configPath, fixture.operationalPath)
	}
	if result.Plan.CoordinatorLock == "" || result.Plan.ControlWorkspace == "" {
		t.Fatalf("plan lock/control workspace = %q/%q, want both named", result.Plan.CoordinatorLock, result.Plan.ControlWorkspace)
	}
	if len(result.Plan.Retained) == 0 {
		t.Fatal("plan retained resources = none, want the deliberately retained resources named")
	}
	fixture.assertNoMutation(t)
	if _, err := os.Stat(fixture.configPath); err != nil {
		t.Fatalf("host configuration missing after preview: %v", err)
	}
}

// TestResetRemovesEveryTerminalRunResource verifies that a confirmed reset
// removes each persisted run's local resources through the owning adapters and
// then removes the store and configuration last.
func TestResetRemovesEveryTerminalRunResource(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t)
	fixture.saveRun(t, "run-one", store.StatusComplete, 0)
	fixture.saveRun(t, "run-two", store.StatusCancelled, 0)
	// The lock path is read before the destructive call, because a preview
	// reopens the store and would recreate the database this test asserts gone.
	lockPath := fixture.lockPath()

	result, err := fixture.service.Reset(context.Background(), factory.ResetRequest{Confirm: true})
	if err != nil {
		t.Fatalf("Reset() error = %v", err)
	}
	if len(result.Remaining) != 0 {
		t.Fatalf("remaining targets = %#v, want none", result.Remaining)
	}
	if len(result.Plan.Runs) != 2 {
		t.Fatalf("planned runs = %d, want two", len(result.Plan.Runs))
	}
	if len(fixture.worker.cleaned) != 2 {
		t.Fatalf("worker cleanups = %d, want one per run", len(fixture.worker.cleaned))
	}
	if len(fixture.workspace.removed) != 2 {
		t.Fatalf("Git workspace removals = %d, want one per run", len(fixture.workspace.removed))
	}
	if got := len(fixture.worker.credentialStores); got != 1 {
		t.Fatalf("credential store removals = %d, want one shared store removed once", got)
	}
	if fixture.worker.credentialStores[0].CredentialStoreID != "credential-store" {
		t.Fatalf("credential store = %q, want credential-store", fixture.worker.credentialStores[0].CredentialStoreID)
	}
	if len(fixture.terminal.closed) != 3 {
		t.Fatalf("terminal closes = %#v, want both run workspaces and the control workspace", fixture.terminal.closed)
	}
	if fixture.terminal.closed[2] != "workspace-control" {
		t.Fatalf("control workspace close = %q, want workspace-control", fixture.terminal.closed[2])
	}
	for _, path := range []string{fixture.configPath, fixture.operationalPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("path %q still exists after reset: err = %v", path, err)
		}
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("coordinator lock still exists after reset: err = %v", err)
	}
}

// TestResetRemovesOnlyItsOwnDatabaseSidecarsAndBackups verifies that reset
// removes the SQLite sidecars and the migration backups proven to belong to its
// exact database, and retains every unrelated neighbour.
func TestResetRemovesOnlyItsOwnDatabaseSidecarsAndBackups(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t)
	directory := filepath.Dir(fixture.operationalPath)
	owned := []string{
		fixture.operationalPath + "-wal",
		fixture.operationalPath + "-shm",
		fixture.operationalPath + ".bak-20260101T000000.000000000Z",
	}
	foreign := []string{
		filepath.Join(directory, "other.db"),
		filepath.Join(directory, "other.db.bak-20260101T000000.000000000Z"),
	}
	for _, path := range append(append([]string(nil), owned...), foreign...) {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	result, err := fixture.service.Reset(context.Background(), factory.ResetRequest{Confirm: true})
	if err != nil {
		t.Fatalf("Reset() error = %v", err)
	}
	if len(result.Plan.MigrationBackups) != 1 {
		t.Fatalf("planned backups = %#v, want only the owned backup", result.Plan.MigrationBackups)
	}
	for _, path := range owned {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned store file %q still exists: err = %v", path, err)
		}
	}
	for _, path := range foreign {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("unrelated file %q was removed: %v", path, err)
		}
	}
}

// TestResetCompletesAMergedRunThroughTheNormalLifecycle verifies that a run
// still marked non-terminal locally is completed through the ordinary lifecycle
// projection, publishing the normal label and status comment, before its local
// state is removed.
func TestResetCompletesAMergedRunThroughTheNormalLifecycle(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t)
	fixture.saveRun(t, "run-merged", store.StatusActive, 17)
	fixture.github.pullRequest = github.PullRequest{Number: 17, State: "closed", Merged: true, MergeCommitSHA: strings.Repeat("a", 40), HeadBranch: "factory/run-merged", BaseBranch: "main"}

	result, err := fixture.service.Reset(context.Background(), factory.ResetRequest{Confirm: true})
	if err != nil {
		t.Fatalf("Reset() error = %v", err)
	}
	if len(result.Lifecycle) != 1 || result.Lifecycle[0].Outcome != factory.LifecycleCompleted {
		t.Fatalf("lifecycle transitions = %#v, want one completion", result.Lifecycle)
	}
	if !equalStrings(fixture.github.replacedLabels, []string{github.LabelAgentComplete}) {
		t.Fatalf("final labels = %#v, want exactly agent-complete", fixture.github.replacedLabels)
	}
	if len(fixture.github.editedComments) != 1 {
		t.Fatalf("status comment edits = %d, want the normal terminal projection", len(fixture.github.editedComments))
	}
	if len(fixture.workspace.removed) != 1 {
		t.Fatalf("Git workspace removals = %d, want the completed run removed", len(fixture.workspace.removed))
	}
	// The terminal notification published by the transition creates a second
	// workspace under the registered control name, and reset closes both.
	if len(fixture.terminal.created) != 1 {
		t.Fatalf("control workspace creations = %d, want the notification's one", len(fixture.terminal.created))
	}
	if got := len(fixture.terminal.closed); got != 3 {
		t.Fatalf("terminal closes = %#v, want the run workspace and both control workspaces", fixture.terminal.closed)
	}
}

// TestResetCancelsAnUnmergedClosedRunThroughTheNormalLifecycle verifies the
// cancellation branch of the same coordinator-owned lifecycle rules.
func TestResetCancelsAnUnmergedClosedRunThroughTheNormalLifecycle(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t)
	fixture.saveRun(t, "run-closed", store.StatusActive, 18)
	fixture.github.pullRequest = github.PullRequest{Number: 18, State: "closed", HeadBranch: "factory/run-closed", BaseBranch: "main"}

	result, err := fixture.service.Reset(context.Background(), factory.ResetRequest{Confirm: true})
	if err != nil {
		t.Fatalf("Reset() error = %v", err)
	}
	if len(result.Lifecycle) != 1 || result.Lifecycle[0].Outcome != factory.LifecycleCancelled {
		t.Fatalf("lifecycle transitions = %#v, want one cancellation", result.Lifecycle)
	}
	if !equalStrings(fixture.github.replacedLabels, []string{github.LabelAgentCancelled}) {
		t.Fatalf("final labels = %#v, want exactly agent-cancelled", fixture.github.replacedLabels)
	}
}

// TestResetRefusesAGenuinelyLiveRun verifies that reset never invents a
// terminal outcome and names the supervised cancellation instruction instead.
func TestResetRefusesAGenuinelyLiveRun(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t)
	fixture.saveRun(t, "run-live", store.StatusActive, 19)
	fixture.github.pullRequest = github.PullRequest{Number: 19, State: "open", HeadBranch: "factory/run-live", BaseBranch: "main"}

	_, err := fixture.service.Reset(context.Background(), factory.ResetRequest{Confirm: true})
	var blocked *factory.ResetBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("Reset() error = %v, want ResetBlockedError", err)
	}
	if len(blocked.Blockers) != 1 || blocked.Blockers[0].RunID != "run-live" {
		t.Fatalf("blockers = %#v, want the live run named", blocked.Blockers)
	}
	if !strings.Contains(blocked.Blockers[0].Action, "/factory cancel") {
		t.Fatalf("blocker action = %q, want the supervised cancellation instruction", blocked.Blockers[0].Action)
	}
	fixture.assertNoMutation(t)
}

// TestResetRefusesWhenGitHubIsUnreachable verifies that transport failure
// blocks reset while a non-terminal run exists and leaves local state intact.
func TestResetRefusesWhenGitHubIsUnreachable(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t)
	fixture.saveRun(t, "run-unreachable", store.StatusActive, 20)
	fixture.github.issueErr = errors.New("401 Unauthorized")

	_, err := fixture.service.Reset(context.Background(), factory.ResetRequest{Confirm: true})
	var blocked *factory.ResetBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("Reset() error = %v, want ResetBlockedError", err)
	}
	if len(blocked.Blockers) != 1 || blocked.Blockers[0].RunID != "run-unreachable" {
		t.Fatalf("blockers = %#v, want the unreadable run named", blocked.Blockers)
	}
	fixture.assertNoMutation(t)
	if _, err := os.Stat(fixture.operationalPath); err != nil {
		t.Fatalf("operational store missing after a blocked reset: %v", err)
	}
}

// TestResetRefusesWhileTheCoordinatorLockIsHeld verifies that reset directs the
// operator to factory stop rather than signalling the coordinator itself.
func TestResetRefusesWhileTheCoordinatorLockIsHeld(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t)
	release := holdCoordinatorLock(t, fixture.lockPath())
	defer release()

	_, err := fixture.service.Reset(context.Background(), factory.ResetRequest{})
	var blocked *factory.ResetBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("Reset() error = %v, want ResetBlockedError", err)
	}
	if len(blocked.Blockers) != 1 || !strings.Contains(blocked.Blockers[0].Action, "factory stop") {
		t.Fatalf("blockers = %#v, want the factory stop instruction", blocked.Blockers)
	}
	fixture.assertNoMutation(t)
}

// TestResetRefusesAPendingEffect verifies that an unresolved external mutation
// blocks reset before any deletion.
func TestResetRefusesAPendingEffect(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t)
	fixture.saveRun(t, "run-pending", store.StatusComplete, 0)
	fixture.reservePendingEffect(t, "run-pending")

	_, err := fixture.service.Reset(context.Background(), factory.ResetRequest{})
	var blocked *factory.ResetBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("Reset() error = %v, want ResetBlockedError", err)
	}
	if len(blocked.Blockers) != 1 || !strings.Contains(blocked.Blockers[0].Reason, "pending external effect") {
		t.Fatalf("blockers = %#v, want the pending effect named", blocked.Blockers)
	}
	fixture.assertNoMutation(t)
}

// TestResetRefusesUnsafePersistedIdentities verifies that a malformed or
// redirected persisted target cannot widen reset into an arbitrary deletion.
func TestResetRefusesUnsafePersistedIdentities(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		prepare func(*testing.T, *resetFixture) string
		reason  string
	}{
		{
			name: "worktree outside the run scope",
			prepare: func(t *testing.T, fixture *resetFixture) string {
				fixture.saveRunWithWorktree(t, "run-escape", store.StatusComplete, filepath.Join(fixture.root, "worktrees", "elsewhere"))
				return "run-escape"
			},
			reason: "worktree is not an absolute run-scoped path",
		},
		{
			name: "worktree overlapping the registered repository",
			prepare: func(t *testing.T, fixture *resetFixture) string {
				fixture.saveRunWithWorktree(t, "run-overlap", store.StatusComplete, filepath.Join(fixture.repositoryPath, "run-overlap"))
				return "run-overlap"
			},
			reason: "worktree overlaps the registered repository",
		},
		{
			name: "worktree redirected through a symbolic link",
			prepare: func(t *testing.T, fixture *resetFixture) string {
				worktree := filepath.Join(fixture.root, "worktrees", "run-symlink")
				if err := os.Symlink(filepath.Join(fixture.root, "elsewhere"), worktree); err != nil {
					t.Fatal(err)
				}
				fixture.saveRunWithWorktree(t, "run-symlink", store.StatusComplete, worktree)
				return "run-symlink"
			},
			reason: "worktree must not be a symbolic link",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fixture := newResetFixture(t)
			runID := test.prepare(t, fixture)
			_, err := fixture.service.Reset(context.Background(), factory.ResetRequest{Confirm: true})
			var blocked *factory.ResetBlockedError
			if !errors.As(err, &blocked) {
				t.Fatalf("Reset() error = %v, want ResetBlockedError", err)
			}
			if len(blocked.Blockers) != 1 || blocked.Blockers[0].RunID != runID {
				t.Fatalf("blockers = %#v, want the unsafe run named", blocked.Blockers)
			}
			if !strings.Contains(blocked.Blockers[0].Reason, test.reason) {
				t.Fatalf("blocker reason = %q, want %q", blocked.Blockers[0].Reason, test.reason)
			}
			fixture.assertNoMutation(t)
		})
	}
}

// TestResetRefusesAnUnavailableWorkerAdapter verifies that known adapter
// unavailability is detected before deletion, so reset cannot leave a half-reset
// installation behind.
func TestResetRefusesAnUnavailableWorkerAdapter(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t)
	fixture.saveRun(t, "run-adapter", store.StatusComplete, 0)
	fixture.worker.dockerErr = errors.New("docker daemon is not running")

	_, err := fixture.service.Reset(context.Background(), factory.ResetRequest{Confirm: true})
	var blocked *factory.ResetBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("Reset() error = %v, want ResetBlockedError", err)
	}
	if len(blocked.Blockers) != 1 || !strings.Contains(blocked.Blockers[0].Reason, "worker runtime is unavailable") {
		t.Fatalf("blockers = %#v, want the unavailable worker runtime named", blocked.Blockers)
	}
	fixture.assertNoMutation(t)
}

// TestResetRetriesAfterAPartialExecution verifies that a bounded per-target
// failure retains the store and configuration, reports what remains, and that a
// rerun completes the reduced plan.
func TestResetRetriesAfterAPartialExecution(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t)
	fixture.saveRun(t, "run-partial", store.StatusComplete, 0)
	fixture.workspace.removeErr = errors.New("worktree is locked")

	result, err := fixture.service.Reset(context.Background(), factory.ResetRequest{Confirm: true})
	var incomplete *factory.ResetIncompleteError
	if !errors.As(err, &incomplete) {
		t.Fatalf("Reset() error = %v, want ResetIncompleteError", err)
	}
	if len(result.Remaining) != 1 || !strings.Contains(result.Remaining[0].Target, "run-partial") {
		t.Fatalf("remaining targets = %#v, want the Git workspace named", result.Remaining)
	}
	if len(result.Removed) == 0 {
		t.Fatal("removed targets = none, want the completed targets reported")
	}
	for _, path := range []string{fixture.configPath, fixture.operationalPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retry information %q was removed after a partial reset: %v", path, err)
		}
	}

	fixture.workspace.removeErr = nil
	retried, err := fixture.service.Reset(context.Background(), factory.ResetRequest{Confirm: true})
	if err != nil {
		t.Fatalf("retried Reset() error = %v", err)
	}
	if len(retried.Remaining) != 0 {
		t.Fatalf("retried remaining targets = %#v, want none", retried.Remaining)
	}
	if _, err := os.Stat(fixture.configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("host configuration still exists after the retry: err = %v", err)
	}
}

// TestResetAfterCompletionReportsTheMissingRegistration verifies that a second
// reset of an already reset installation fails on the absent configuration
// rather than deleting anything else.
func TestResetAfterCompletionReportsTheMissingRegistration(t *testing.T) {
	t.Parallel()

	fixture := newResetFixture(t)
	if _, err := fixture.service.Reset(context.Background(), factory.ResetRequest{Confirm: true}); err != nil {
		t.Fatalf("Reset() error = %v", err)
	}
	fixture.terminal.closed = nil
	fixture.workspace.removed = nil
	fixture.worker.cleaned = nil
	fixture.worker.credentialStores = nil
	fixture.config.loadErr = errors.New("configuration does not exist")
	if _, err := fixture.service.Reset(context.Background(), factory.ResetRequest{}); err == nil {
		t.Fatal("repeated Reset() error = nil, want the absent registration reported")
	}
	fixture.assertNoMutation(t)
}

// resetFixture is one registered installation whose destructive seams are all
// test doubles, so a reset test observes effects instead of performing them.
type resetFixture struct {
	root            string
	configPath      string
	repositoryPath  string
	operationalPath string
	registration    config.RepositoryRegistration
	config          *resetConfigRepository
	github          *resetGitHub
	workspace       *resetWorkspace
	worker          *resetWorker
	terminal        *resetTerminal
	service         *factory.Service
}

// newResetFixture creates a registered installation with a real operational
// store and a real host configuration file.
func newResetFixture(t *testing.T) *resetFixture {
	t.Helper()

	root := t.TempDir()
	fixture := &resetFixture{
		root:            root,
		configPath:      filepath.Join(root, "config", "config.yaml"),
		repositoryPath:  filepath.Join(root, "repository"),
		operationalPath: filepath.Join(root, "state", "factory.db"),
	}
	for _, path := range []string{fixture.repositoryPath, filepath.Join(root, "worktrees")} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fixture.registration = config.RepositoryRegistration{
		Path:                 fixture.repositoryPath,
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		AuthorizedUsers:      []string{"alice"},
		Polling:              config.PollingConfig{Interval: "30s", Backoff: "5m"},
		Cmux:                 config.CmuxConfig{ControlWorkspace: "factory-control"},
		OperationalDataPath:  fixture.operationalPath,
		RepositoryConfigPath: filepath.Join(fixture.repositoryPath, "factory.yaml"),
	}
	if err := config.SaveHost(fixture.configPath, config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{fixture.registration}}); err != nil {
		t.Fatal(err)
	}
	opened, err := store.Open(context.Background(), fixture.operationalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	fixture.config = &resetConfigRepository{value: config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{fixture.registration}}}
	fixture.github = &resetGitHub{issue: github.Issue{Number: 42, State: "open", Labels: []string{github.LabelAgentRunning}}, statusComment: github.Comment{ID: "status-1"}}
	fixture.workspace = &resetWorkspace{}
	fixture.worker = &resetWorker{}
	fixture.terminal = &resetTerminal{control: terminal.Workspace{ID: "workspace-control", Name: "factory-control"}}
	fixture.service = factory.NewWithDependencies(fixture.configPath, factory.Dependencies{
		Config:       fixture.config,
		OpenStore:    func(ctx context.Context, path string) (factory.OperationalStore, error) { return store.Open(ctx, path) },
		GitHub:       fixture.github,
		PullRequests: fixture.github,
		GitWorkspace: fixture.workspace,
		Worker:       fixture.worker,
		Terminal:     fixture.terminal,
		Now:          func() time.Time { return time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC) },
	})
	return fixture
}

// lockPath resolves the exact coordinator lock for the fixture's checkout by
// asking the service for the plan that names it.
func (f *resetFixture) lockPath() string {
	result, _ := f.service.Reset(context.Background(), factory.ResetRequest{})
	return result.Plan.CoordinatorLock
}

// saveRun persists one run with its worktree, invocation, credential store, and
// terminal workspace handle.
func (f *resetFixture) saveRun(t *testing.T, runID string, status store.Status, pullRequestNumber int) {
	t.Helper()
	f.saveRunWithWorktree(t, runID, status, filepath.Join(f.root, "worktrees", runID), pullRequestNumber)
}

// saveRunWithWorktree persists one run at an explicit worktree path so a test
// can supply a deliberately unsafe persisted target.
func (f *resetFixture) saveRunWithWorktree(t *testing.T, runID string, status store.Status, worktree string, pullRequestNumber ...int) {
	t.Helper()

	packet, err := json.Marshal(factory.SpecificationPacket{Version: 1, RepositoryConfig: commandRepositoryConfig()})
	if err != nil {
		t.Fatal(err)
	}
	number := 0
	if len(pullRequestNumber) > 0 {
		number = pullRequestNumber[0]
	}
	at := time.Date(2026, time.February, 1, 12, 0, 0, 0, time.UTC)
	opened, err := store.Open(context.Background(), f.operationalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	run := store.Run{
		ID:                  runID,
		RepositoryPath:      f.repositoryPath,
		IssueNumber:         42,
		Stage:               store.StageImplementation,
		Status:              status,
		Branch:              "factory/" + runID,
		Worktree:            worktree,
		StatusCommentID:     "status-1",
		SpecificationPacket: string(packet),
		PullRequestNumber:   number,
		CreatedAt:           at,
		UpdatedAt:           at,
	}
	if store.IsTerminalStatus(status) {
		run.TerminalAt = at
	}
	if err := opened.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	invocationRoot := filepath.Join(f.root, "worktrees", ".factory-agents", runID, "invocation-"+runID)
	if err := opened.SaveInvocation(context.Background(), store.Invocation{
		ID:                  "invocation-" + runID,
		RunID:               runID,
		Harness:             "codex",
		Role:                "implementation",
		Stage:               store.StageImplementation,
		Status:              store.InvocationStatusCompleted,
		InvocationDirectory: filepath.Join(invocationRoot, "packet"),
		ResultDirectory:     filepath.Join(invocationRoot, "results"),
		WorkspaceID:         "workspace-" + runID,
		CredentialStoreID:   "credential-store",
		CreatedAt:           at,
		UpdatedAt:           at,
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{worktree, filepath.Join(invocationRoot, "packet"), filepath.Join(invocationRoot, "results"), filepath.Join(f.root, "worktrees", ".factory-git", runID)} {
		if _, err := os.Lstat(path); err == nil {
			continue
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// reservePendingEffect journals one unresolved external mutation for a run.
func (f *resetFixture) reservePendingEffect(t *testing.T, runID string) {
	t.Helper()

	opened, err := store.Open(context.Background(), f.operationalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	at := time.Date(2026, time.February, 1, 12, 0, 0, 0, time.UTC)
	if err := opened.SavePendingEffect(context.Background(), store.PendingEffect{
		ID:        "effect-" + runID,
		RunID:     runID,
		Kind:      store.PendingEffectKindStatusComment,
		Payload:   "{}",
		CreatedAt: at,
		UpdatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
}

// assertNoMutation verifies that no destructive adapter was reached.
func (f *resetFixture) assertNoMutation(t *testing.T) {
	t.Helper()

	if len(f.workspace.removed) != 0 {
		t.Fatalf("Git workspace removals = %#v, want none", f.workspace.removed)
	}
	if len(f.worker.cleaned) != 0 || len(f.worker.credentialStores) != 0 {
		t.Fatalf("worker effects = cleanups %#v credential stores %#v, want none", f.worker.cleaned, f.worker.credentialStores)
	}
	if len(f.terminal.closed) != 0 || len(f.terminal.created) != 0 {
		t.Fatalf("terminal effects = closes %#v creations %#v, want none", f.terminal.closed, f.terminal.created)
	}
	if len(f.github.replacedLabels) != 0 || len(f.github.editedComments) != 0 || len(f.github.createdComments) != 0 {
		t.Fatalf("GitHub mutations = labels %#v edits %#v comments %#v, want none", f.github.replacedLabels, f.github.editedComments, f.github.createdComments)
	}
	if f.worker.stopCalls != 0 || len(f.terminal.notifications) != 0 {
		t.Fatalf("coordinator effects = worker stops %d notifications %#v, want none", f.worker.stopCalls, f.terminal.notifications)
	}
}

// holdCoordinatorLock takes the exact repository lock in this process so a test
// observes the running-coordinator refusal.
func holdCoordinatorLock(t *testing.T, path string) func() {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	return func() { _ = file.Close() }
}

// resetConfigRepository loads the registration and records configuration writes.
type resetConfigRepository struct {
	value   config.HostConfig
	loadErr error
}

// Load returns the registered host configuration.
func (r *resetConfigRepository) Load(string) (config.HostConfig, error) {
	if r.loadErr != nil {
		return config.HostConfig{}, r.loadErr
	}
	return r.value, nil
}

// Save records a configuration write, which reset never performs.
func (r *resetConfigRepository) Save(_ string, value config.HostConfig) error {
	r.value = value
	return nil
}

// Create satisfies the configuration seam for reset tests.
func (r *resetConfigRepository) Create(string) (config.HostConfig, error) {
	return config.NewHostConfig(), nil
}

// resetGitHub answers lifecycle observation and records every GitHub mutation.
type resetGitHub struct {
	issue           github.Issue
	issueErr        error
	statusComment   github.Comment
	pullRequest     github.PullRequest
	replacedLabels  []string
	editedComments  []string
	createdComments []string
}

// Issue returns the configured issue lifecycle state.
func (g *resetGitHub) Issue(context.Context, github.Repository, int) (github.Issue, error) {
	if g.issueErr != nil {
		return github.Issue{}, g.issueErr
	}
	return g.issue, nil
}

// CreateLabel satisfies the GitHub seam; reset never bootstraps labels.
func (*resetGitHub) CreateLabel(context.Context, github.Repository, github.Label) error {
	return errors.New("unexpected label creation")
}

// ReplaceIssueLabels records the terminal label projection.
func (g *resetGitHub) ReplaceIssueLabels(_ context.Context, _ github.Repository, _ int, labels []string) error {
	g.replacedLabels = append([]string(nil), labels...)
	g.issue.Labels = append([]string(nil), labels...)
	return nil
}

// CreateIssueComment records a created comment.
func (g *resetGitHub) CreateIssueComment(_ context.Context, _ github.Repository, _ int, body string) (github.Comment, error) {
	g.createdComments = append(g.createdComments, body)
	return github.Comment{ID: "comment-1", Body: body}, nil
}

// FindStatusComment returns the recoverable status comment identity.
func (g *resetGitHub) FindStatusComment(context.Context, github.Repository, int, string) (github.Comment, error) {
	return g.statusComment, nil
}

// EditIssueComment records the terminal status-comment projection.
func (g *resetGitHub) EditIssueComment(_ context.Context, _ github.Repository, _, body string) error {
	g.editedComments = append(g.editedComments, body)
	return nil
}

// FindPullRequest returns the configured tracked pull request.
func (g *resetGitHub) FindPullRequest(context.Context, github.Repository, string, string) (github.PullRequest, error) {
	return g.pullRequest, nil
}

// CreatePullRequest satisfies the seam; reset never creates a pull request.
func (*resetGitHub) CreatePullRequest(context.Context, github.Repository, github.PullRequestRequest) (github.PullRequest, error) {
	return github.PullRequest{}, errors.New("unexpected pull request creation")
}

// UpdatePullRequest satisfies the seam; reset never updates a pull request.
func (*resetGitHub) UpdatePullRequest(context.Context, github.Repository, int, github.PullRequestRequest) (github.PullRequest, error) {
	return github.PullRequest{}, errors.New("unexpected pull request update")
}

// SetPullRequestDraft satisfies the seam; reset never changes draft state.
func (*resetGitHub) SetPullRequestDraft(context.Context, github.Repository, int, bool) (github.PullRequest, error) {
	return github.PullRequest{}, errors.New("unexpected draft transition")
}

// resetWorkspace records Git workspace removals.
type resetWorkspace struct {
	removed   []gitadapter.Workspace
	removeErr error
}

// Create satisfies the GitWorkspace contract; reset never creates a workspace.
func (*resetWorkspace) Create(context.Context, string, string, string) (gitadapter.Workspace, error) {
	return gitadapter.Workspace{}, errors.New("unexpected workspace creation")
}

// Remove records the exact workspace selected for deletion.
func (w *resetWorkspace) Remove(_ context.Context, _ string, workspace gitadapter.Workspace) error {
	if w.removeErr != nil {
		return w.removeErr
	}
	w.removed = append(w.removed, workspace)
	if err := os.RemoveAll(workspace.Worktree); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(filepath.Dir(workspace.Worktree), ".factory-git", workspace.RunID))
}

// Inspect satisfies the GitWorkspace contract for reset tests.
func (*resetWorkspace) Inspect(context.Context, string) (gitadapter.WorktreeState, error) {
	return gitadapter.WorktreeState{}, nil
}

// CreateCheckpoint satisfies the GitWorkspace contract for reset tests.
func (*resetWorkspace) CreateCheckpoint(context.Context, gitadapter.CheckpointRequest) (gitadapter.CheckpointResult, error) {
	return gitadapter.CheckpointResult{}, nil
}

// Push satisfies the GitWorkspace contract for reset tests.
func (*resetWorkspace) Push(context.Context, gitadapter.PushRequest) error { return nil }

// SynchronizeBase satisfies the GitWorkspace contract for reset tests.
func (*resetWorkspace) SynchronizeBase(context.Context, gitadapter.BaseSyncRequest) error {
	return nil
}

// resetWorker records worker cleanup and credential-storage removal.
type resetWorker struct {
	cleaned          []worker.CleanupRequest
	credentialStores []worker.RemoveCredentialStoreRequest
	dockerErr        error
	stopCalls        int
}

// Start satisfies WorkerRuntime for reset tests.
func (*resetWorker) Start(context.Context, worker.StartRequest) error { return nil }

// Resume satisfies WorkerRuntime for reset tests.
func (*resetWorker) Resume(context.Context, worker.ResumeRequest) error { return nil }

// RunCommand satisfies WorkerRuntime for reset tests.
func (*resetWorker) RunCommand(context.Context, worker.CommandRequest) (worker.CommandResult, error) {
	return worker.CommandResult{}, nil
}

// Stop records the terminal worker stop performed by a lifecycle transition.
func (w *resetWorker) Stop(context.Context, string) error {
	w.stopCalls++
	return nil
}

// Inspect satisfies WorkerRuntime for reset tests.
func (*resetWorker) Inspect(context.Context, string) (worker.Inspection, error) {
	return worker.Inspection{}, nil
}

// Cleanup records the exact worker cleanup request and removes stored outputs.
func (w *resetWorker) Cleanup(_ context.Context, request worker.CleanupRequest) error {
	w.cleaned = append(w.cleaned, request)
	for _, path := range request.StoredOutputs {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}

// RemoveCredentialStore records the exact credential volume identity removed.
func (w *resetWorker) RemoveCredentialStore(_ context.Context, request worker.RemoveCredentialStoreRequest) error {
	w.credentialStores = append(w.credentialStores, request)
	return nil
}

// CheckDocker reports the configured worker-runtime availability.
func (w *resetWorker) CheckDocker(context.Context) error { return w.dockerErr }

// resetTerminal records terminal workspace effects for reset tests. It models
// the real adapter: creating a control workspace adds another workspace under
// the same name rather than returning the existing one, so a reset that closes
// only the first match leaves one behind.
type resetTerminal struct {
	control       terminal.Workspace
	controls      []terminal.Workspace
	closed        []string
	created       []string
	notifications []terminal.Notification
	closeErr      error
}

// EnsureControlWorkspace records a control workspace creation, which a reset
// preview must never perform.
func (t *resetTerminal) EnsureControlWorkspace(_ context.Context, request terminal.WorkspaceRequest) (terminal.Workspace, error) {
	t.created = append(t.created, request.Name)
	created := terminal.Workspace{ID: terminal.WorkspaceID(fmt.Sprintf("%s-%d", t.control.ID, len(t.created))), Name: t.control.Name}
	t.controls = append(t.controls, created)
	return created, nil
}

// FindWorkspace returns the first workspace with the requested name that is
// still open, without creating one.
func (t *resetTerminal) FindWorkspace(_ context.Context, name string) (terminal.Workspace, bool, error) {
	for _, workspace := range append([]terminal.Workspace{t.control}, t.controls...) {
		if workspace.Name != name || t.isClosed(string(workspace.ID)) {
			continue
		}
		return workspace, true, nil
	}
	return terminal.Workspace{}, false, nil
}

// isClosed reports whether a workspace handle was already closed.
func (t *resetTerminal) isClosed(workspaceID string) bool {
	for _, closed := range t.closed {
		if closed == workspaceID {
			return true
		}
	}
	return false
}

// EnsureRunWorkspace satisfies terminal.TerminalRuntime for reset tests.
func (*resetTerminal) EnsureRunWorkspace(context.Context, terminal.RunWorkspaceRequest) (terminal.RunWorkspace, error) {
	return terminal.RunWorkspace{}, errors.New("unexpected run workspace creation")
}

// CreateSurface satisfies terminal.TerminalRuntime for reset tests.
func (*resetTerminal) CreateSurface(context.Context, terminal.SurfaceRequest) (terminal.Surface, error) {
	return terminal.Surface{}, errors.New("unexpected surface creation")
}

// LaunchSurface satisfies terminal.TerminalRuntime for reset tests.
func (*resetTerminal) LaunchSurface(context.Context, terminal.SurfaceID, terminal.Command) error {
	return nil
}

// SendInput satisfies terminal.TerminalRuntime for reset tests.
func (*resetTerminal) SendInput(context.Context, terminal.SurfaceID, []byte) error { return nil }

// Notify records the terminal notification a lifecycle transition publishes.
func (t *resetTerminal) Notify(_ context.Context, notification terminal.Notification) error {
	t.notifications = append(t.notifications, notification)
	return nil
}

// CloseSurface satisfies terminal.TerminalRuntime for reset tests.
func (*resetTerminal) CloseSurface(context.Context, terminal.SurfaceID) error { return nil }

// CloseWorkspace records the exact workspace handle reset closed.
func (t *resetTerminal) CloseWorkspace(_ context.Context, workspaceID terminal.WorkspaceID) error {
	if t.closeErr != nil {
		return t.closeErr
	}
	t.closed = append(t.closed, string(workspaceID))
	return nil
}

var _ terminal.WorkspaceFinder = (*resetTerminal)(nil)
var _ worker.CredentialStoreRemover = (*resetWorker)(nil)
