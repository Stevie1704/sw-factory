package factory_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/hostcmd"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestClaimKeepsARunWhoseLabelTransitionTimedOut verifies that a claim whose
// GitHub label transition reaches its host command deadline stays a pending
// claim: the run stays active with its reserved effect and its workspace, a
// replay after a coordinator restart that times out again changes nothing,
// and the next replay after GitHub answers completes the claim.
func TestClaimKeepsARunWhoseLabelTransitionTimedOut(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	timeout := &hostcmd.TimeoutError{Operation: "gh api", Timeout: github.CommandTimeout}
	githubAdapter := &labelTimeoutGitHub{
		pollingGitHub: &pollingGitHub{
			fakeGitHub: &fakeGitHub{statusComment: github.Comment{ID: "status-1"}},
			issues:     []github.Issue{{Number: 7, Title: "claim me", State: "open", Labels: []string{github.LabelAgentReady}}},
		},
		labelErrors: []error{timeout, timeout},
	}
	worktreePath := filepath.Join(root, "worktree")
	if err := os.Mkdir(worktreePath, 0o700); err != nil {
		t.Fatal(err)
	}
	worktree := &recoveryWorktree{
		fakeWorktree: fakeWorktree{workspace: gitadapter.Workspace{BaseSHA: "base", Branch: "factory/run-fixed", Worktree: worktreePath}},
		state:        gitadapter.WorktreeState{RepositoryPath: filepath.Join(root, "repository"), Branch: "factory/run-fixed", HeadSHA: "base"},
	}
	databasePath := filepath.Join(root, "state", "factory.db")
	service := newSQLitePollingService(root, databasePath, githubAdapter, worktree)

	if _, err := service.ClaimIssue(ctx, 7); !isHostCommandTimeout(err) {
		t.Fatalf("ClaimIssue() error = %v, want the host command timeout", err)
	}
	assertPendingClaim(t, ctx, databasePath, "after the timed-out claim")
	if worktree.removed {
		t.Fatal("the timed-out claim removed the run workspace")
	}

	// A restarted coordinator replays the pending claim in its one-time
	// startup reconciliation. A timeout there must not be cached for the life
	// of the process.
	restarted := newSQLitePollingService(root, databasePath, githubAdapter, worktree)
	if _, err := restarted.PollCommands(ctx, factory.CommandPollRequest{}); !isHostCommandTimeout(err) {
		t.Fatalf("first PollCommands() error = %v, want the replay timeout", err)
	}
	assertPendingClaim(t, ctx, databasePath, "after the timed-out replay")

	if _, err := restarted.PollCommands(ctx, factory.CommandPollRequest{}); err != nil {
		t.Fatalf("second PollCommands() error = %v, want the replay to complete the claim", err)
	}
	opened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	run, err := opened.LatestRun(ctx)
	if err != nil || run == nil || run.Status != store.StatusActive {
		t.Fatalf("run after replay = %#v, %v, want an active claimed run", run, err)
	}
	if pending, err := opened.PendingEffect(ctx, run.ID); err != nil || pending != nil {
		t.Fatalf("pending effect after replay = %#v, %v, want none", pending, err)
	}
	if !slices.Contains(githubAdapter.issueValue.Labels, github.LabelAgentRunning) {
		t.Fatalf("issue labels after replay = %q, want %q", githubAdapter.issueValue.Labels, github.LabelAgentRunning)
	}
	for _, labels := range githubAdapter.replacedHistory {
		if slices.Contains(labels, github.LabelAgentFailed) {
			t.Fatalf("label history = %q, want no %q transition", githubAdapter.replacedHistory, github.LabelAgentFailed)
		}
	}
}

// assertPendingClaim fails unless the latest run is still active and its
// claim transition is still reserved in the effect journal.
func assertPendingClaim(t *testing.T, ctx context.Context, databasePath, moment string) {
	t.Helper()
	opened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	run, err := opened.LatestRun(ctx)
	if err != nil || run == nil {
		t.Fatalf("latest run %s = %v, %v, want the claimed run", moment, run, err)
	}
	if run.Status != store.StatusActive || run.LifecycleReason != "" {
		t.Fatalf("run %s = %s (%q), want it active and unpaused", moment, run.Status, run.LifecycleReason)
	}
	pending, err := opened.PendingEffect(ctx, run.ID)
	if err != nil || pending == nil {
		t.Fatalf("pending effect %s = %v, %v, want the reserved claim transition", moment, pending, err)
	}
}

// isHostCommandTimeout reports whether err carries a host command timeout.
func isHostCommandTimeout(err error) bool {
	var timeout *hostcmd.TimeoutError
	return errors.As(err, &timeout)
}

// labelTimeoutGitHub fails the next label mutations with the configured
// errors before it records them, like a gh call that never reached GitHub.
type labelTimeoutGitHub struct {
	*pollingGitHub
	labelErrors []error
}

// ReplaceIssueLabels returns the next configured failure, then records the
// mutation once the failures are used.
func (f *labelTimeoutGitHub) ReplaceIssueLabels(ctx context.Context, repository github.Repository, number int, labels []string) error {
	if len(f.labelErrors) > 0 {
		err := f.labelErrors[0]
		f.labelErrors = f.labelErrors[1:]
		return err
	}
	if err := f.pollingGitHub.ReplaceIssueLabels(ctx, repository, number, labels); err != nil {
		return err
	}
	// pollingGitHub serves Issue from its queue, so the queue must show the
	// new labels to the reconciliation that reads them back.
	for index := range f.issues {
		if f.issues[index].Number == number {
			f.issues[index].Labels = append([]string(nil), labels...)
		}
	}
	return nil
}

// newSQLitePollingService builds the polling service on the production
// SQLite store, so the effect journal behaves as it does in production.
func newSQLitePollingService(root, databasePath string, githubAdapter *labelTimeoutGitHub, worktree gitadapter.WorktreeManager) *factory.Service {
	registration := config.RepositoryRegistration{
		Path:                 filepath.Join(root, "repository"),
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		AuthorizedUsers:      []string{"alice"},
		Polling:              config.PollingConfig{Interval: "1ms", Backoff: "20ms"},
		OperationalDataPath:  databasePath,
		RepositoryConfigPath: filepath.Join(root, "repository", "factory.yaml"),
	}
	return factory.NewWithDependencies(filepath.Join(root, "config.yaml"), factory.Dependencies{
		Config:         &fakeConfig{value: config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{registration}}},
		OpenStore:      func(ctx context.Context, path string) (factory.OperationalStore, error) { return store.Open(ctx, path) },
		LoadRepository: func(string) (config.RepositoryConfig, error) { return validRepositoryConfig(), nil },
		GitHub:         githubAdapter,
		IssuePoller:    githubAdapter,
		Lease:          &pollingLease{},
		Worktree:       worktree,
		Comments:       &pollingCommentReader{},
		Now:            func() time.Time { return time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC) },
		NewRunID:       func() (string, error) { return "run-fixed", nil },
		Coordinator:    "coordinator-test",
	})
}
