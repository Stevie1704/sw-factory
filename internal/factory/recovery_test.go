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
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestProgressionRefusesAnAgreeingInterruptedRun verifies the fail-closed
// boundary even when every observed projection agrees with the store.
func TestProgressionRefusesAnAgreeingInterruptedRun(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	worktreePath := filepath.Join(root, "worktree")
	if err := mkdir(worktreePath); err != nil {
		t.Fatal(err)
	}
	run := recoveryRun(worktreePath)
	githubAdapter := &fakeGitHub{
		issueValue:    github.Issue{Number: run.IssueNumber, State: "open", Labels: []string{github.LabelAgentRunning}},
		statusComment: github.Comment{ID: run.StatusCommentID, Body: factory.StatusCommentBody(run)},
	}
	worktree := &recoveryWorktree{state: gitadapter.WorktreeState{RepositoryPath: run.RepositoryPath, Branch: run.Branch, HeadSHA: run.CheckpointSHA}}
	storeAdapter := &recoveryRunStore{run: run}
	service := newRecoveryService(t, storeAdapter, githubAdapter, worktree, nil)

	_, err := service.Transition(context.Background(), factory.TransitionRequest{RunID: run.ID, Stage: store.StageImplementation, Status: store.StatusActive})
	var recoveryErr *factory.RecoveryRequiredError
	if !errors.As(err, &recoveryErr) {
		t.Fatalf("Transition() error = %v, want RecoveryRequiredError", err)
	}
	if !recoveryErr.Diagnosis.SourcesAgree {
		t.Fatalf("diagnosis = %#v, want agreeing sources", recoveryErr.Diagnosis)
	}
	if len(recoveryErr.Diagnosis.Discrepancies) != 0 {
		t.Fatalf("discrepancies = %#v, want none", recoveryErr.Diagnosis.Discrepancies)
	}
	if recoveryErr.Diagnosis.Code != factory.RecoveryRequiredCode || !strings.Contains(recoveryErr.Error(), "no recovery occurred") {
		t.Fatalf("recovery error = %q, want typed refusal without recovery", recoveryErr.Error())
	}
	if len(storeAdapter.saved) != 0 || len(githubAdapter.replacedHistory) != 0 || len(githubAdapter.editedComments) != 0 {
		t.Fatalf("progression effects = saved=%d labels=%d comments=%d, want none", len(storeAdapter.saved), len(githubAdapter.replacedHistory), len(githubAdapter.editedComments))
	}
}

// TestStatusReportsEveryRecoveryDiscrepancy verifies status remains a
// read-only diagnosis for the source failures called out by issue 24.
func TestStatusReportsEveryRecoveryDiscrepancy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		configure  func(*store.Run, *recoveryWorktree, *fakeGitHub, *fakePullRequests)
		wantFields []string
	}{
		{
			name: "missing worktree",
			configure: func(run *store.Run, _ *recoveryWorktree, _ *fakeGitHub, _ *fakePullRequests) {
				run.Worktree = filepath.Join(t.TempDir(), "missing")
			},
			wantFields: []string{"worktree.path"},
		},
		{
			name: "wrong branch and head",
			configure: func(run *store.Run, worktree *recoveryWorktree, _ *fakeGitHub, _ *fakePullRequests) {
				worktree.state.Branch = "factory/other-run"
				worktree.state.HeadSHA = "different-checkpoint"
				_ = run
			},
			wantFields: []string{"worktree.branch", "worktree.checkpoint"},
		},
		{
			name: "worktree belongs to another repository",
			configure: func(_ *store.Run, worktree *recoveryWorktree, _ *fakeGitHub, _ *fakePullRequests) {
				worktree.state.RepositoryPath = "/another-repository"
			},
			wantFields: []string{"worktree.repository"},
		},
		{
			name: "stale GitHub label",
			configure: func(run *store.Run, _ *recoveryWorktree, githubAdapter *fakeGitHub, _ *fakePullRequests) {
				run.Status = store.StatusWaitingForHuman
				githubAdapter.issueValue.Labels = []string{github.LabelAgentRunning}
			},
			wantFields: []string{"github.state label"},
		},
		{
			name: "missing status comment",
			configure: func(_ *store.Run, _ *recoveryWorktree, githubAdapter *fakeGitHub, _ *fakePullRequests) {
				githubAdapter.statusComment = github.Comment{}
			},
			wantFields: []string{"github.status comment"},
		},
		{
			name: "mismatched status comment",
			configure: func(run *store.Run, _ *recoveryWorktree, githubAdapter *fakeGitHub, _ *fakePullRequests) {
				githubAdapter.statusComment = github.Comment{ID: "comment-other", Body: factory.StatusCommentBody(*run)}
			},
			wantFields: []string{"github.status comment"},
		},
		{
			name: "stale status comment body",
			configure: func(run *store.Run, _ *recoveryWorktree, githubAdapter *fakeGitHub, _ *fakePullRequests) {
				githubAdapter.statusComment.Body = factory.StatusCommentBody(*run) + "\nstale projection"
			},
			wantFields: []string{"github.status comment"},
		},
		{
			name: "missing pull request",
			configure: func(run *store.Run, _ *recoveryWorktree, _ *fakeGitHub, _ *fakePullRequests) {
				run.PullRequestNumber = 17
				run.PullRequestURL = "https://github.com/example/project/pull/17"
			},
			wantFields: []string{"github.pull request"},
		},
		{
			name: "mismatched pull request",
			configure: func(run *store.Run, _ *recoveryWorktree, _ *fakeGitHub, pullRequests *fakePullRequests) {
				run.PullRequestNumber = 17
				run.PullRequestURL = "https://github.com/example/project/pull/17"
				pullRequests.existing = github.PullRequest{Number: 18, URL: "https://github.com/example/project/pull/18", HeadBranch: run.Branch, BaseBranch: "main"}
			},
			wantFields: []string{"github.pull request number", "github.pull request URL"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			worktreePath := filepath.Join(root, "worktree")
			if err := mkdir(worktreePath); err != nil {
				t.Fatal(err)
			}
			run := recoveryRun(worktreePath)
			githubAdapter := &fakeGitHub{
				issueValue:    github.Issue{Number: run.IssueNumber, State: "open", Labels: []string{github.LabelAgentRunning}},
				statusComment: github.Comment{ID: run.StatusCommentID, Body: factory.StatusCommentBody(run)},
			}
			worktree := &recoveryWorktree{state: gitadapter.WorktreeState{RepositoryPath: run.RepositoryPath, Branch: run.Branch, HeadSHA: run.CheckpointSHA}}
			pullRequests := &fakePullRequests{}
			test.configure(&run, worktree, githubAdapter, pullRequests)
			storeAdapter := &recoveryRunStore{run: run}
			service := newRecoveryService(t, storeAdapter, githubAdapter, worktree, pullRequests)

			status, err := service.Status(context.Background())
			if err != nil {
				t.Fatalf("Status() error = %v", err)
			}
			if status.LatestRun == nil || status.Recovery == nil {
				t.Fatalf("status = %#v, want run and recovery diagnosis", status)
			}
			if status.Recovery.SourcesAgree || len(status.Recovery.Discrepancies) == 0 {
				t.Fatalf("recovery = %#v, want disagreement", status.Recovery)
			}
			fields := make(map[string]bool)
			for _, discrepancy := range status.Recovery.Discrepancies {
				fields[discrepancy.Source+"."+discrepancy.Field] = true
			}
			for _, field := range test.wantFields {
				if !fields[field] {
					t.Errorf("recovery fields = %#v, want %q", fields, field)
				}
			}
			if len(storeAdapter.saved) != 0 || len(githubAdapter.replacedHistory) != 0 || len(githubAdapter.editedComments) != 0 {
				t.Fatalf("status effects = saved=%d labels=%d comments=%d, want none", len(storeAdapter.saved), len(githubAdapter.replacedHistory), len(githubAdapter.editedComments))
			}
		})
	}
}

// TestAbandonPendingEffectLeavesTheRunWaitingForHuman verifies that an
// explicitly abandoned ambiguous mutation is removed from the journal while
// the workflow remains paused instead of silently continuing.
func TestAbandonPendingEffectLeavesTheRunWaitingForHuman(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	worktreePath := filepath.Join(root, "worktree")
	if err := mkdir(worktreePath); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "state", "factory.db")
	ctx := context.Background()
	opened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	run := recoveryRun(worktreePath)
	run.RepositoryPath = root
	run.Coordinator = "coordinator-test"
	if err := opened.SaveRun(ctx, run); err != nil {
		_ = opened.Close()
		t.Fatal(err)
	}
	effect := store.PendingEffect{RunID: run.ID, ID: "effect-abandon", Kind: store.PendingEffectKindPush, Payload: "{}"}
	if err := opened.SavePendingEffect(ctx, effect); err != nil {
		_ = opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
		Path: root, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
		OperationalDataPath: databasePath, RepositoryConfigPath: filepath.Join(root, "factory.yaml"),
	}}}
	githubAdapter := &fakeGitHub{
		issueValue:    github.Issue{Number: run.IssueNumber, State: "open", Labels: []string{github.LabelAgentRunning}},
		statusComment: github.Comment{ID: run.StatusCommentID, Body: factory.StatusCommentBody(run)},
	}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:    &fakeConfig{value: host},
		OpenStore: func(ctx context.Context, path string) (factory.OperationalStore, error) { return store.Open(ctx, path) },
		GitHub:    githubAdapter,
		Worker:    &recoveryWorker{},
		Now:       func() time.Time { return time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC) },
	})
	result, err := service.AbandonPendingEffect(ctx, factory.AbandonPendingEffectRequest{RunID: run.ID, EffectID: effect.ID, Reason: "operator verified the remote branch manually"})
	if err != nil {
		t.Fatalf("AbandonPendingEffect() error = %v", err)
	}
	if result.Run == nil || result.Run.Status != store.StatusWaitingForHuman || result.Outcome != factory.RecoveryOutcomeWaitingForHuman {
		t.Fatalf("abandon result = %#v, want waiting-for-human run", result)
	}
	reopened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	pending, err := reopened.PendingEffect(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending != nil {
		t.Fatalf("pending effect after abandonment = %#v, want nil", pending)
	}
	latest, err := reopened.LatestRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || latest.Status != store.StatusWaitingForHuman {
		t.Fatalf("latest run after abandonment = %#v, want waiting-for-human", latest)
	}
}

// recoveryRun returns a complete persisted run fixture for startup diagnosis.
func recoveryRun(worktreePath string) store.Run {
	packet, err := json.Marshal(factory.SpecificationPacket{Version: 1, Issue: github.Issue{Number: 42, Title: "Recovery", Body: "diagnose"}, RepositoryConfig: validRepositoryConfig()})
	if err != nil {
		panic(err)
	}
	return store.Run{
		ID: "run-recovery", RepositoryPath: "REPOSITORY", IssueNumber: 42,
		Stage: store.StageImplementation, Status: store.StatusActive,
		Branch: "factory/run-recovery", Worktree: worktreePath, CheckpointSHA: "checkpoint",
		StatusCommentID: "comment-1", SpecificationPacket: string(packet),
		CreatedAt: time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC),
	}
}

// newRecoveryService creates a high-level service with only read-only
// recovery projections and mutation-recording test doubles.
func newRecoveryService(t *testing.T, runStore *recoveryRunStore, githubAdapter *fakeGitHub, worktree *recoveryWorktree, pullRequests *fakePullRequests) *factory.Service {
	t.Helper()
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
		Path: runStore.run.RepositoryPath, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
		OperationalDataPath: filepath.Join(t.TempDir(), "factory.db"), RepositoryConfigPath: filepath.Join(runStore.run.RepositoryPath, "factory.yaml"),
	}}}
	dependencies := factory.Dependencies{
		Config:    &fakeConfig{value: host},
		OpenStore: func(context.Context, string) (factory.OperationalStore, error) { return runStore, nil },
		GitHub:    githubAdapter, Worktree: worktree,
		Worker: &recoveryWorker{},
	}
	if pullRequests != nil {
		dependencies.PullRequests = pullRequests
	}
	return factory.NewWithDependencies("/host/config.yaml", dependencies)
}

// recoveryWorktree supplies a read-only Git projection for recovery tests.
type recoveryWorktree struct {
	fakeWorktree
	state gitadapter.WorktreeState
	err   error
}

// Inspect returns the configured branch and checkpoint projection.
func (w *recoveryWorktree) Inspect(context.Context, string) (gitadapter.WorktreeState, error) {
	return w.state, w.err
}

// recoveryRunStore supplies one persisted run to status and progression seams.
type recoveryRunStore struct {
	run             store.Run
	saved           []store.Run
	lifecycleClaims map[string]map[store.Status]bool
}

// CurrentRun returns the persisted non-terminal run.
func (s *recoveryRunStore) CurrentRun(context.Context) (*store.Run, error) {
	run := s.run
	return &run, nil
}

// LatestRun returns the same run for the read-only status projection.
func (s *recoveryRunStore) LatestRun(context.Context) (*store.Run, error) {
	run := s.run
	return &run, nil
}

// SaveRun records unexpected progression persistence.
func (s *recoveryRunStore) SaveRun(_ context.Context, run store.Run) error {
	s.saved = append(s.saved, run)
	return nil
}

// ClaimLifecycleNotification atomically claims notification delivery.
func (s *recoveryRunStore) ClaimLifecycleNotification(_ context.Context, runID string, terminalStatus store.Status) (bool, error) {
	if s.lifecycleClaims == nil {
		s.lifecycleClaims = make(map[string]map[store.Status]bool)
	}
	if s.lifecycleClaims[runID] == nil {
		s.lifecycleClaims[runID] = make(map[store.Status]bool)
	}
	if s.lifecycleClaims[runID][terminalStatus] {
		return false, nil
	}
	s.lifecycleClaims[runID][terminalStatus] = true
	return true, nil
}

// ReleaseLifecycleNotification removes a notification claim.
func (s *recoveryRunStore) ReleaseLifecycleNotification(_ context.Context, runID string, terminalStatus store.Status) error {
	if s.lifecycleClaims != nil && s.lifecycleClaims[runID] != nil {
		delete(s.lifecycleClaims[runID], terminalStatus)
	}
	return nil
}

// Close satisfies the operational-store seam.
func (*recoveryRunStore) Close() error { return nil }

// mkdir creates a recovery fixture directory without hiding setup errors.
func mkdir(path string) error {
	return os.MkdirAll(path, 0o755)
}

// recoveryWorker makes human-waiting recovery tests explicit about the worker
// stop seam without requiring Docker on the host running the test suite.
type recoveryWorker struct{}

// Start satisfies the worker runtime for read-only recovery tests.
func (*recoveryWorker) Start(context.Context, worker.StartRequest) error { return nil }

// Resume satisfies the worker runtime for read-only recovery tests.
func (*recoveryWorker) Resume(context.Context, worker.ResumeRequest) error { return nil }

// RunCommand satisfies the worker runtime for read-only recovery tests.
func (*recoveryWorker) RunCommand(context.Context, worker.CommandRequest) (worker.CommandResult, error) {
	return worker.CommandResult{}, nil
}

// Stop satisfies the worker runtime for read-only recovery tests.
func (*recoveryWorker) Stop(context.Context, string) error { return nil }

// Inspect satisfies the worker runtime for read-only recovery tests.
func (*recoveryWorker) Inspect(context.Context, string) (worker.Inspection, error) {
	return worker.Inspection{}, nil
}

var _ worker.WorkerRuntime = (*recoveryWorker)(nil)
