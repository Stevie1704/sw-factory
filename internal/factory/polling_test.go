package factory_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestPollOnceClaimsTheOldestEligibleIssue verifies deterministic queue
// ordering, eligibility filtering, and reuse of the existing claim seam.
func TestPollOnceClaimsTheOldestEligibleIssue(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	issues := []github.Issue{
		{Number: 12, Title: "newer", State: "open", Labels: []string{github.LabelAgentReady}},
		{Number: 9, Title: "pull request", State: "open", Labels: []string{github.LabelAgentReady}, IsPullRequest: true},
		{Number: 5, Title: "oldest", State: "open", Labels: []string{github.LabelAgentReady}},
		{Number: 2, Title: "closed", State: "closed", Labels: []string{github.LabelAgentReady}},
	}
	githubAdapter := &pollingGitHub{fakeGitHub: &fakeGitHub{}, issues: issues}
	runStore := &fakeRunStore{}
	service := newPollingService(root, githubAdapter, githubAdapter, nil, runStore, &fakeWorktree{workspace: gitadapter.Workspace{
		BaseSHA: "base", Branch: "factory/run-fixed", Worktree: "/worktree/run-fixed",
	}}, nil)

	result, err := service.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce() error = %v", err)
	}
	if result.Outcome != factory.PollClaimed || result.IssueNumber != 5 || result.Run.ID != "run-fixed" {
		t.Fatalf("PollOnce() result = %#v, want claimed issue 5/run-fixed", result)
	}
	if githubAdapter.claimedIssue != 5 {
		t.Fatalf("claimed issue = %d, want oldest eligible issue 5", githubAdapter.claimedIssue)
	}
	if len(githubAdapter.createdLabels) != 0 {
		t.Fatalf("polling created labels = %#v, want none", githubAdapter.createdLabels)
	}
}

// TestPollOnceDoesNotClaimWhileARunIsActive verifies a persisted active run
// suppresses issue listing and all claim effects.
func TestPollOnceDoesNotClaimWhileARunIsActive(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	active := store.Run{ID: "run-active", RepositoryPath: "/repo", IssueNumber: 42, Stage: store.StageImplementation, Status: store.StatusActive}
	runStore := &fakeRunStore{saved: []store.Run{active}}
	issues := []github.Issue{{Number: 7, State: "open", Labels: []string{github.LabelAgentReady}}}
	githubAdapter := &pollingGitHub{fakeGitHub: &fakeGitHub{}, issues: issues}
	service := newPollingService(root, githubAdapter, githubAdapter, nil, runStore, &fakeWorktree{}, nil)

	result, err := service.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce() error = %v", err)
	}
	if result.Outcome != factory.PollActiveRun || result.Run.ID != active.ID {
		t.Fatalf("PollOnce() result = %#v, want active run %q", result, active.ID)
	}
	if githubAdapter.listCalls != 0 || githubAdapter.claimedIssue != 0 {
		t.Fatalf("polling effects = list=%d claim=%d, want no issue poll or claim", githubAdapter.listCalls, githubAdapter.claimedIssue)
	}
}

// TestStartRefusesToPollWhenStartupDiagnosisIsBlocked verifies startup
// diagnosis is completed before host-lock, lease, or issue-poll effects.
func TestStartRefusesToPollWhenStartupDiagnosisIsBlocked(t *testing.T) {
	t.Parallel()

	reader := &pollingIssueReader{}
	service := factory.NewWithDependencies(filepath.Join(t.TempDir(), "config.yaml"), factory.Dependencies{
		StartupDiagnosis: func(context.Context) (factory.DoctorResult, error) {
			return factory.DoctorResult{Report: doctor.Report{Results: []doctor.Result{
				doctor.Failure("GitHub", "transport unavailable", "restore GitHub access"),
			}}}, nil
		},
		IssuePoller: reader,
	})

	err := service.Start(context.Background())
	var blocked *factory.StartupBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("Start() error = %v, want StartupBlockedError", err)
	}
	if reader.calls != 0 {
		t.Fatalf("issue list calls = %d, want zero when startup is blocked", reader.calls)
	}
}

// TestStartPollsThenStopsWithoutCancellingTheActiveRun verifies the normal
// immediate poll, the one-run claim limit, lease renewal, and context stop.
func TestStartPollsThenStopsWithoutCancellingTheActiveRun(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	issues := []github.Issue{{Number: 7, Title: "claim me", State: "open", Labels: []string{github.LabelAgentReady}}}
	githubAdapter := &pollingGitHub{fakeGitHub: &fakeGitHub{}, issues: issues}
	runStore := &fakeRunStore{}
	var cancel context.CancelFunc
	lease := &pollingLease{onRenew: func(value github.Lease) {
		if value.RunID == "run-fixed" && cancel != nil {
			cancel()
		}
	}}
	service := newPollingService(root, githubAdapter, githubAdapter, lease, runStore, &fakeWorktree{workspace: gitadapter.Workspace{
		BaseSHA: "base", Branch: "factory/run-fixed", Worktree: "/worktree/run-fixed",
	}}, nil)
	ctx, stop := context.WithCancel(context.Background())
	cancel = stop

	if err := service.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if githubAdapter.listCalls != 1 || githubAdapter.claimedIssue != 7 {
		t.Fatalf("polling calls = list=%d claim=%d, want one claim poll for issue 7", githubAdapter.listCalls, githubAdapter.claimedIssue)
	}
	if len(runStore.saved) == 0 || runStore.saved[len(runStore.saved)-1].Status != store.StatusActive {
		t.Fatalf("saved runs = %#v, want the claimed run to remain active", runStore.saved)
	}
	if len(lease.calls) < 2 || lease.calls[0].RunID != "" || lease.calls[1].RunID != "run-fixed" {
		t.Fatalf("lease renewals = %#v, want initial and claimed-run renewals", lease.calls)
	}
	if strings.Contains(githubAdapter.lastLabelMutation, github.LabelAgentCancelled) {
		t.Fatal("stopping polling cancelled the active run")
	}
}

// TestStartUsesTransportBackoffWithoutChangingRunState verifies a GitHub
// listing failure delays the next poll and consumes no run or repair budget.
func TestStartUsesTransportBackoffWithoutChangingRunState(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	var cancel context.CancelFunc
	reader := &pollingIssueReader{onList: func(call int) ([]github.Issue, error) {
		if call == 1 {
			return nil, errors.New("GitHub transport unavailable")
		}
		cancel()
		return nil, nil
	}}
	lease := &pollingLease{}
	runStore := &fakeRunStore{}
	service := newPollingService(root, &fakeGitHub{}, reader, lease, runStore, &fakeWorktree{}, nil)
	ctx, stop := context.WithCancel(context.Background())
	cancel = stop
	started := time.Now()

	if err := service.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if reader.calls != 2 {
		t.Fatalf("issue list calls = %d, want one retry after transport backoff", reader.calls)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond {
		t.Fatalf("retry elapsed = %s, want configured backoff before the second poll", elapsed)
	}
	if len(runStore.saved) != 0 {
		t.Fatalf("runs saved after queue transport failures = %#v, want none", runStore.saved)
	}
}

// TestStartAppliesQueuedIssueCommands verifies the unattended loop reads the
// structured /factory comments of the run it already reconciled, instead of
// waiting for an operator to run the `factory poll` command by hand.
func TestStartAppliesQueuedIssueCommands(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	issues := []github.Issue{{Number: 7, Title: "claim me", State: "open", Labels: []string{github.LabelAgentReady}}}
	githubAdapter := &pollingGitHub{fakeGitHub: &fakeGitHub{statusComment: github.Comment{ID: "status-1"}}, issues: issues}
	runStore := &fakeRunStore{}
	var cancel context.CancelFunc
	comments := &pollingCommentReader{
		comments: []github.Comment{{ID: "15", Author: "alice", Body: "/factory status"}},
		onList: func(call int) {
			if call >= 2 {
				cancel()
			}
		},
	}
	service := newPollingService(root, githubAdapter, githubAdapter, &pollingLease{}, runStore, &fakeWorktree{workspace: gitadapter.Workspace{
		BaseSHA: "base", Branch: "factory/run-fixed", Worktree: "/worktree/run-fixed",
	}}, comments)
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	cancel = stop
	defer stop()

	if err := service.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if comments.calls == 0 {
		t.Fatal("comment listings = 0, want the polling loop to read issue commands")
	}
	if !savedCommandWatermark(runStore.saved, "15", "status") {
		t.Fatalf("saved runs = %#v, want the status command applied with its watermark", runStore.saved)
	}
}

// TestStartBacksOffCommandTransportFailures verifies a comment-listing failure
// delays the next command poll by the configured backoff and never stops the
// coordinator.
func TestStartBacksOffCommandTransportFailures(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	issues := []github.Issue{{Number: 7, Title: "claim me", State: "open", Labels: []string{github.LabelAgentReady}}}
	githubAdapter := &pollingGitHub{fakeGitHub: &fakeGitHub{statusComment: github.Comment{ID: "status-1"}}, issues: issues}
	runStore := &fakeRunStore{}
	var cancel context.CancelFunc
	comments := &pollingCommentReader{
		listErrors: []error{errors.New("GitHub comment transport unavailable")},
		onList: func(call int) {
			if call >= 2 {
				cancel()
			}
		},
	}
	service := newPollingService(root, githubAdapter, githubAdapter, &pollingLease{}, runStore, &fakeWorktree{workspace: gitadapter.Workspace{
		BaseSHA: "base", Branch: "factory/run-fixed", Worktree: "/worktree/run-fixed",
	}}, comments)
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	cancel = stop
	defer stop()

	if err := service.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v, want the loop to survive a command transport failure", err)
	}
	if comments.calls != 2 {
		t.Fatalf("comment listings = %d, want one retry after transport backoff", comments.calls)
	}
	if gap := comments.times[1].Sub(comments.times[0]); gap < 20*time.Millisecond {
		t.Fatalf("command retry gap = %s, want the configured backoff before the second command poll", gap)
	}
	if savedCommandWatermark(runStore.saved, "15", "status") {
		t.Fatal("a queued command was applied although every comment listing failed")
	}
}

// savedCommandWatermark reports whether any persisted run recorded the named
// command against the supplied comment identifier.
func savedCommandWatermark(saved []store.Run, commentID, command string) bool {
	for _, run := range saved {
		if run.ProcessedCommentID == commentID && run.LastCommandName == command {
			return true
		}
	}
	return false
}

// pollingCommentReader supplies structured command comments to the polling
// loop with controllable transport failures.
type pollingCommentReader struct {
	comments   []github.Comment
	listErrors []error
	calls      int
	times      []time.Time
	onList     func(int)
}

// IssueComments records the listing time, runs the optional test hook, and
// then replays the next configured transport failure, or the comment set once
// the failures are exhausted.
func (r *pollingCommentReader) IssueComments(context.Context, github.Repository, int) ([]github.Comment, error) {
	r.calls++
	r.times = append(r.times, time.Now())
	if r.onList != nil {
		r.onList(r.calls)
	}
	if len(r.listErrors) > 0 {
		err := r.listErrors[0]
		r.listErrors = r.listErrors[1:]
		return nil, err
	}
	return append([]github.Comment(nil), r.comments...), nil
}

// pollingGitHub combines the existing claim fake with a deterministic issue
// listing adapter for coordinator polling tests.
type pollingGitHub struct {
	*fakeGitHub
	issues            []github.Issue
	listCalls         int
	claimedIssue      int
	lastLabelMutation string
}

// ListEligibleIssues returns the configured issue queue in its source order.
func (f *pollingGitHub) ListEligibleIssues(context.Context, github.Repository) ([]github.Issue, error) {
	f.listCalls++
	return append([]github.Issue(nil), f.issues...), nil
}

// Issue returns the requested issue so the claim path observes the selected
// queue candidate rather than a shared fixture value.
func (f *pollingGitHub) Issue(ctx context.Context, repository github.Repository, number int) (github.Issue, error) {
	for _, issue := range f.issues {
		if issue.Number == number {
			f.claimedIssue = number
			f.fakeGitHub.issueValue = issue
			return f.fakeGitHub.Issue(ctx, repository, number)
		}
	}
	return github.Issue{}, errors.New("issue not found")
}

// ReplaceIssueLabels records label mutations without changing the polling
// assertions' access to the embedded claim fake.
func (f *pollingGitHub) ReplaceIssueLabels(ctx context.Context, repository github.Repository, number int, labels []string) error {
	f.lastLabelMutation = strings.Join(labels, ",")
	return f.fakeGitHub.ReplaceIssueLabels(ctx, repository, number, labels)
}

// pollingIssueReader is a small issue-listing seam with controllable retries.
type pollingIssueReader struct {
	calls  int
	onList func(int) ([]github.Issue, error)
}

// ListEligibleIssues invokes the configured polling response.
func (f *pollingIssueReader) ListEligibleIssues(_ context.Context, _ github.Repository) ([]github.Issue, error) {
	f.calls++
	if f.onList == nil {
		return nil, nil
	}
	return f.onList(f.calls)
}

// pollingLease records visible lease renewals for start-loop assertions.
type pollingLease struct {
	calls   []github.Lease
	onRenew func(github.Lease)
}

// RenewLease records the lease and invokes its optional test hook.
func (f *pollingLease) RenewLease(_ context.Context, _ github.Repository, lease github.Lease) error {
	f.calls = append(f.calls, lease)
	if f.onRenew != nil {
		f.onRenew(lease)
	}
	return nil
}

// newPollingService constructs a service with real queue/claim behavior and
// isolated fakes for the external polling and lease adapters.
func newPollingService(root string, githubAdapter github.Client, issuePoller github.IssuePoller, lease github.LeaseClient, runStore *fakeRunStore, worktree gitadapter.WorktreeManager, comments github.CommentReader) *factory.Service {
	registration := config.RepositoryRegistration{
		Path:                 filepath.Join(root, "repository"),
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		AuthorizedUsers:      []string{"alice"},
		Polling:              config.PollingConfig{Interval: "1ms", Backoff: "20ms"},
		OperationalDataPath:  filepath.Join(root, "state", "factory.db"),
		RepositoryConfigPath: filepath.Join(root, "repository", "factory.yaml"),
	}
	return factory.NewWithDependencies(filepath.Join(root, "config.yaml"), factory.Dependencies{
		Config: &fakeConfig{value: config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{registration}}},
		OpenStore: func(context.Context, string) (factory.OperationalStore, error) {
			return runStore, nil
		},
		LoadRepository: func(string) (config.RepositoryConfig, error) { return validRepositoryConfig(), nil },
		GitHub:         githubAdapter,
		IssuePoller:    issuePoller,
		Lease:          lease,
		Worktree:       worktree,
		Comments:       comments,
		Terminal:       &lifecycleTerminal{},
		Now: func() time.Time {
			return time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
		},
		NewRunID:    func() (string, error) { return "run-fixed", nil },
		Coordinator: "coordinator-test",
		StartupDiagnosis: func(context.Context) (factory.DoctorResult, error) {
			return factory.DoctorResult{Report: doctor.Report{Results: []doctor.Result{doctor.Success("startup")}}}, nil
		},
	})
}
