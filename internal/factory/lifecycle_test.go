package factory_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestPollLifecycleCompletesAMergedPullRequest verifies the public lifecycle
// seam records merge completion, releases the active slot, and retains the
// recoverable run identity.
func TestPollLifecycleCompletesAMergedPullRequest(t *testing.T) {
	t.Parallel()

	run := commandRun(t, store.StatusActive)
	run.Branch = "factory/run-command"
	run.PullRequestNumber = 17
	run.PullRequestURL = "https://github.com/example/project/pull/17"
	githubAdapter := &lifecycleGitHub{
		commandGitHub: commandGitHub{issue: github.Issue{Number: run.IssueNumber, State: "open", Labels: []string{github.LabelAgentRunning}}, statusComment: github.Comment{ID: run.StatusCommentID}},
		pullRequest: github.PullRequest{
			Number:         run.PullRequestNumber,
			URL:            run.PullRequestURL,
			State:          "closed",
			Merged:         true,
			MergeCommitSHA: strings.Repeat("a", 40),
			HeadBranch:     run.Branch,
			BaseBranch:     "main",
		},
	}
	runStore := &commandRunStore{current: &run, latest: &run}
	workerRuntime := &lifecycleWorker{}
	terminalRuntime := &lifecycleTerminal{}
	service := newLifecycleService(runStore, githubAdapter, workerRuntime, terminalRuntime)

	result, err := service.PollLifecycle(context.Background(), factoryLifecycleRequest(run.ID))
	if err != nil {
		t.Fatalf("PollLifecycle() error = %v", err)
	}
	if result.Outcome != factory.LifecycleCompleted || result.Run.Status != store.StatusComplete || result.Run.Stage != store.StageReady {
		t.Fatalf("lifecycle result = %#v, want completed ready run", result)
	}
	if result.Run.MergeCommitSHA != githubAdapter.pullRequest.MergeCommitSHA {
		t.Fatalf("merge commit = %q, want %q", result.Run.MergeCommitSHA, githubAdapter.pullRequest.MergeCommitSHA)
	}
	if runStore.current != nil {
		t.Fatalf("current run = %#v, want active slot released", runStore.current)
	}
	if len(githubAdapter.replacedLabels) != 1 || !equalStrings(githubAdapter.replacedLabels, []string{github.LabelAgentComplete}) {
		t.Fatalf("state labels = %#v, want exactly agent-complete", githubAdapter.replacedLabels)
	}
	if len(githubAdapter.editedComments) != 1 {
		t.Fatalf("status comment edits = %d, want one", len(githubAdapter.editedComments))
	}
	for _, expected := range []string{"status: `complete`", "pull request: #17", "merge commit: `" + githubAdapter.pullRequest.MergeCommitSHA + "`"} {
		if !strings.Contains(githubAdapter.editedComments[0].body, expected) {
			t.Errorf("status comment %q does not contain %q", githubAdapter.editedComments[0].body, expected)
		}
	}
	if workerRuntime.stopCalls != 1 {
		t.Fatalf("worker stop calls = %d, want one", workerRuntime.stopCalls)
	}
	if len(terminalRuntime.notifications) != 1 {
		t.Fatalf("notifications = %d, want one completion notification", len(terminalRuntime.notifications))
	}

	repeated, err := service.PollLifecycle(context.Background(), factoryLifecycleRequest(run.ID))
	if err != nil {
		t.Fatalf("repeated PollLifecycle() error = %v", err)
	}
	if repeated.Run.Status != store.StatusComplete || repeated.Outcome != factory.LifecycleUnchanged {
		t.Fatalf("repeated lifecycle result = %#v, want unchanged completed run", repeated)
	}
	if len(githubAdapter.editedComments) != 1 || workerRuntime.stopCalls != 1 || len(terminalRuntime.notifications) != 1 {
		t.Fatalf("repeated lifecycle effects = edits %d/stops %d/notifications %d, want no duplicates", len(githubAdapter.editedComments), workerRuntime.stopCalls, len(terminalRuntime.notifications))
	}
}

// TestPollLifecycleRetriesAFailedTerminalNotification verifies a delivery
// failure remains retryable after the terminal state itself is persisted.
func TestPollLifecycleRetriesAFailedTerminalNotification(t *testing.T) {
	t.Parallel()

	run := commandRun(t, store.StatusActive)
	run.Branch = "factory/run-command"
	run.PullRequestNumber = 17
	run.PullRequestURL = "https://github.com/example/project/pull/17"
	githubAdapter := &lifecycleGitHub{
		commandGitHub: commandGitHub{issue: github.Issue{Number: run.IssueNumber, State: "open", Labels: []string{github.LabelAgentRunning}}, statusComment: github.Comment{ID: run.StatusCommentID}},
		pullRequest:   github.PullRequest{Number: 17, State: "closed", Merged: true, MergeCommitSHA: strings.Repeat("d", 40), HeadBranch: run.Branch, BaseBranch: "main"},
	}
	runStore := &commandRunStore{current: &run, latest: &run}
	terminalRuntime := &lifecycleTerminal{notifyErrors: []error{errors.New("cmux unavailable")}}
	service := newLifecycleService(runStore, githubAdapter, &lifecycleWorker{}, terminalRuntime)

	first, err := service.PollLifecycle(context.Background(), factoryLifecycleRequest(run.ID))
	if err == nil {
		t.Fatal("first PollLifecycle() error = nil, want notification failure")
	}
	if first.Run.Status != store.StatusComplete || first.Run.LifecycleNotificationSent {
		t.Fatalf("first lifecycle result = %#v, want completed run with pending notification", first)
	}

	second, err := service.PollLifecycle(context.Background(), factoryLifecycleRequest(run.ID))
	if err != nil {
		t.Fatalf("second PollLifecycle() error = %v", err)
	}
	if second.Run.Status != store.StatusComplete || !second.Run.LifecycleNotificationSent {
		t.Fatalf("second lifecycle result = %#v, want completed run with delivered notification", second)
	}
	if len(terminalRuntime.notifications) != 2 {
		t.Fatalf("notification attempts = %d, want one retry", len(terminalRuntime.notifications))
	}
}

// TestAuthorizedCancelStopsAndRetainsAnActiveRun verifies cancellation is a
// policy-controlled terminal transition rather than workspace cleanup.
func TestAuthorizedCancelStopsAndRetainsAnActiveRun(t *testing.T) {
	t.Parallel()

	run := commandRun(t, store.StatusActive)
	run.Branch = "factory/run-command"
	run.Worktree = "/worktrees/run-command"
	githubAdapter := &lifecycleGitHub{commandGitHub: commandGitHub{issue: github.Issue{Number: run.IssueNumber, State: "open", Labels: []string{github.LabelAgentRunning}}, statusComment: github.Comment{ID: run.StatusCommentID}}}
	runStore := &commandRunStore{current: &run, latest: &run}
	workerRuntime := &lifecycleWorker{}
	terminalRuntime := &lifecycleTerminal{}
	service := newLifecycleService(runStore, githubAdapter, workerRuntime, terminalRuntime)

	result, err := service.HandleCommand(context.Background(), factory.CommandRequest{IssueNumber: run.IssueNumber, Comment: github.Comment{ID: "99", Author: "alice", Body: "/factory cancel"}})
	if err != nil {
		t.Fatalf("HandleCommand() error = %v", err)
	}
	if result.Outcome != factory.CommandAccepted || result.Run.Status != store.StatusCancelled {
		t.Fatalf("cancel result = %#v, want accepted cancelled run", result)
	}
	if runStore.current != nil || workerRuntime.stopCalls != 1 {
		t.Fatalf("cancel retention state = current %#v/stops %d, want released slot and one stop", runStore.current, workerRuntime.stopCalls)
	}
	if len(githubAdapter.replacedLabels) != 1 || !equalStrings(githubAdapter.replacedLabels, []string{github.LabelAgentCancelled}) {
		t.Fatalf("cancel labels = %#v, want exactly agent-cancelled", githubAdapter.replacedLabels)
	}
	if len(githubAdapter.createdComments) != 0 || len(githubAdapter.editedComments) != 1 {
		t.Fatalf("cancel comments = created %d/edited %d, want one edit and no new comment", len(githubAdapter.createdComments), len(githubAdapter.editedComments))
	}
	if !strings.Contains(githubAdapter.editedComments[0].body, "status: `cancelled`") {
		t.Fatalf("cancel status comment = %q, want cancelled status", githubAdapter.editedComments[0].body)
	}
	if len(terminalRuntime.notifications) != 1 {
		t.Fatalf("cancel notifications = %d, want one", len(terminalRuntime.notifications))
	}

	replayed, err := service.HandleCommand(context.Background(), factory.CommandRequest{IssueNumber: run.IssueNumber, Comment: github.Comment{ID: "99", Author: "alice", Body: "/factory cancel"}})
	if err != nil {
		t.Fatalf("replayed HandleCommand() error = %v", err)
	}
	if replayed.Outcome != factory.CommandReplayed || len(githubAdapter.editedComments) != 1 || workerRuntime.stopCalls != 1 || len(terminalRuntime.notifications) != 1 {
		t.Fatalf("replayed cancel = %#v, effects edits %d/stops %d/notifications %d, want no duplicate effects", replayed, len(githubAdapter.editedComments), workerRuntime.stopCalls, len(terminalRuntime.notifications))
	}
}

// TestPollLifecycleCancelsAnUnmergedClosedPullRequest verifies closed PRs are
// cancellation signals and are checked after the merged state.
func TestPollLifecycleCancelsAnUnmergedClosedPullRequest(t *testing.T) {
	t.Parallel()

	run := commandRun(t, store.StatusActive)
	run.Branch = "factory/run-command"
	run.PullRequestNumber = 17
	run.PullRequestURL = "https://github.com/example/project/pull/17"
	githubAdapter := &lifecycleGitHub{
		commandGitHub: commandGitHub{issue: github.Issue{Number: run.IssueNumber, State: "open", Labels: []string{github.LabelAgentRunning}}, statusComment: github.Comment{ID: run.StatusCommentID}},
		pullRequest:   github.PullRequest{Number: run.PullRequestNumber, URL: run.PullRequestURL, State: "closed", HeadBranch: run.Branch, BaseBranch: "main"},
	}
	runStore := &commandRunStore{current: &run, latest: &run}
	workerRuntime := &lifecycleWorker{}
	terminalRuntime := &lifecycleTerminal{}
	service := newLifecycleService(runStore, githubAdapter, workerRuntime, terminalRuntime)

	result, err := service.PollLifecycle(context.Background(), factoryLifecycleRequest(run.ID))
	if err != nil {
		t.Fatalf("PollLifecycle() error = %v", err)
	}
	if result.Outcome != factory.LifecycleCancelled || result.Run.Status != store.StatusCancelled {
		t.Fatalf("lifecycle result = %#v, want cancelled run", result)
	}
	if result.Run.MergeCommitSHA != "" {
		t.Fatalf("cancelled merge commit = %q, want empty", result.Run.MergeCommitSHA)
	}
}

// TestPollLifecycleCancelsAClosedIssueWithoutAPullRequest verifies closure is
// respected even before a draft pull request exists.
func TestPollLifecycleCancelsAClosedIssueWithoutAPullRequest(t *testing.T) {
	t.Parallel()

	run := commandRun(t, store.StatusActive)
	run.Branch = "factory/run-command"
	githubAdapter := &lifecycleGitHub{commandGitHub: commandGitHub{issue: github.Issue{Number: run.IssueNumber, State: "closed", Labels: []string{github.LabelAgentRunning}}, statusComment: github.Comment{ID: run.StatusCommentID}}}
	runStore := &commandRunStore{current: &run, latest: &run}
	workerRuntime := &lifecycleWorker{}
	terminalRuntime := &lifecycleTerminal{}
	service := newLifecycleService(runStore, githubAdapter, workerRuntime, terminalRuntime)

	result, err := service.PollLifecycle(context.Background(), factoryLifecycleRequest(run.ID))
	if err != nil {
		t.Fatalf("PollLifecycle() error = %v", err)
	}
	if result.Outcome != factory.LifecycleCancelled || result.Run.Status != store.StatusCancelled || result.Reason != "issue #42 closed" {
		t.Fatalf("lifecycle result = %#v, want issue-closure cancellation", result)
	}
}

// TestPollLifecyclePrefersMergeOverClosedIssue verifies a merged PR remains a
// successful completion even when GitHub has already closed the issue.
func TestPollLifecyclePrefersMergeOverClosedIssue(t *testing.T) {
	t.Parallel()

	run := commandRun(t, store.StatusActive)
	run.Branch = "factory/run-command"
	run.PullRequestNumber = 17
	run.PullRequestURL = "https://github.com/example/project/pull/17"
	githubAdapter := &lifecycleGitHub{
		commandGitHub: commandGitHub{issue: github.Issue{Number: run.IssueNumber, State: "closed", Labels: []string{github.LabelAgentRunning}}, statusComment: github.Comment{ID: run.StatusCommentID}},
		pullRequest:   github.PullRequest{Number: 17, State: "closed", Merged: true, MergeCommitSHA: strings.Repeat("b", 40), HeadBranch: run.Branch, BaseBranch: "main"},
	}
	runStore := &commandRunStore{current: &run, latest: &run}
	service := newLifecycleService(runStore, githubAdapter, &lifecycleWorker{}, &lifecycleTerminal{})

	result, err := service.PollLifecycle(context.Background(), factoryLifecycleRequest(run.ID))
	if err != nil {
		t.Fatalf("PollLifecycle() error = %v", err)
	}
	if result.Outcome != factory.LifecycleCompleted || result.Run.Status != store.StatusComplete {
		t.Fatalf("lifecycle result = %#v, want merged completion", result)
	}
}

// TestPollCommandsObservesLifecycleBeforeReadingComments verifies the normal
// production poll path finalizes a run even when no command comment exists.
func TestPollCommandsObservesLifecycleBeforeReadingComments(t *testing.T) {
	t.Parallel()

	run := commandRun(t, store.StatusActive)
	run.Branch = "factory/run-command"
	run.PullRequestNumber = 17
	run.PullRequestURL = "https://github.com/example/project/pull/17"
	githubAdapter := &lifecycleGitHub{
		commandGitHub: commandGitHub{issue: github.Issue{Number: run.IssueNumber, State: "open", Labels: []string{github.LabelAgentRunning}}, statusComment: github.Comment{ID: run.StatusCommentID}},
		pullRequest:   github.PullRequest{Number: 17, State: "closed", Merged: true, MergeCommitSHA: strings.Repeat("c", 40), HeadBranch: run.Branch, BaseBranch: "main"},
	}
	runStore := &commandRunStore{current: &run, latest: &run}
	service := newLifecycleService(runStore, githubAdapter, &lifecycleWorker{}, &lifecycleTerminal{})

	results, err := service.PollCommands(context.Background(), factory.CommandPollRequest{RunID: run.ID})
	if err != nil {
		t.Fatalf("PollCommands() error = %v", err)
	}
	if len(results) != 0 || runStore.current != nil || runStore.latest.Status != store.StatusComplete {
		t.Fatalf("poll results = %#v, current = %#v, latest = %#v; want lifecycle completion before comments", results, runStore.current, runStore.latest)
	}
}

// TestRetryExplicitlyReopensACancelledRun verifies reopening alone is not a
// transition, while an authorized retry command is.
func TestRetryExplicitlyReopensACancelledRun(t *testing.T) {
	t.Parallel()

	run := commandRun(t, store.StatusCancelled)
	run.Branch = "factory/run-command"
	githubAdapter := &lifecycleGitHub{commandGitHub: commandGitHub{issue: github.Issue{Number: run.IssueNumber, State: "open", Labels: []string{github.LabelAgentCancelled}}, statusComment: github.Comment{ID: run.StatusCommentID}}}
	runStore := &commandRunStore{latest: &run}
	service := newLifecycleService(runStore, githubAdapter, &lifecycleWorker{}, &lifecycleTerminal{})

	result, err := service.HandleCommand(context.Background(), factory.CommandRequest{IssueNumber: run.IssueNumber, Comment: github.Comment{ID: "100", Author: "alice", Body: "/factory retry"}})
	if err != nil {
		t.Fatalf("HandleCommand() error = %v", err)
	}
	if result.Outcome != factory.CommandAccepted || result.Run.Status != store.StatusActive {
		t.Fatalf("retry result = %#v, want active retried run", result)
	}
	if !equalStrings(githubAdapter.replacedLabels, []string{github.LabelAgentRunning}) {
		t.Fatalf("retry labels = %#v, want agent-running", githubAdapter.replacedLabels)
	}
}

// TestRetryRejectsAClosedPullRequest verifies a PR closure cannot be bypassed
// by reopening only the parent issue before retrying.
func TestRetryRejectsAClosedPullRequest(t *testing.T) {
	t.Parallel()

	run := commandRun(t, store.StatusCancelled)
	run.Branch = "factory/run-command"
	run.PullRequestNumber = 17
	run.PullRequestURL = "https://github.com/example/project/pull/17"
	run.LifecycleReason = "pull request #17 closed without merging"
	run.LifecycleNotificationSent = true
	githubAdapter := &lifecycleGitHub{
		commandGitHub: commandGitHub{issue: github.Issue{Number: run.IssueNumber, State: "open", Labels: []string{github.LabelAgentCancelled}}, statusComment: github.Comment{ID: run.StatusCommentID}},
		pullRequest:   github.PullRequest{Number: 17, State: "closed", HeadBranch: run.Branch, BaseBranch: "main"},
	}
	runStore := &commandRunStore{latest: &run}
	service := newLifecycleService(runStore, githubAdapter, &lifecycleWorker{}, &lifecycleTerminal{})

	result, err := service.HandleCommand(context.Background(), factory.CommandRequest{IssueNumber: run.IssueNumber, Comment: github.Comment{ID: "101", Author: "alice", Body: "/factory retry"}})
	if err == nil {
		t.Fatal("HandleCommand() error = nil, want retry policy rejection")
	}
	var rejection *factory.PolicyRejection
	if !errors.As(err, &rejection) || rejection.Code != factory.PolicyRejectionRetryState {
		t.Fatalf("HandleCommand() error = %v, want retry_state rejection", err)
	}
	if result.Outcome != factory.CommandRejected || result.Run.Status != store.StatusCancelled {
		t.Fatalf("retry result = %#v, want rejected cancelled run", result)
	}
	if len(githubAdapter.replacedLabels) != 0 {
		t.Fatalf("retry labels = %#v, want no state transition", githubAdapter.replacedLabels)
	}
}

// factoryLifecycleRequest keeps lifecycle tests independent of the request's
// concrete field layout while still exercising explicit run selection.
func factoryLifecycleRequest(runID string) factory.LifecycleRequest {
	return factory.LifecycleRequest{RunID: runID}
}

// newLifecycleService builds a high-level factory with only lifecycle
// collaborators replaced by deterministic fakes.
func newLifecycleService(runStore *commandRunStore, githubAdapter *lifecycleGitHub, workerRuntime worker.WorkerRuntime, terminalRuntime terminal.TerminalRuntime) *factory.Service {
	return factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config: &commandConfig{host: commandHost()},
		OpenStore: func(context.Context, string) (factory.OperationalStore, error) {
			return runStore, nil
		},
		GitHub:       githubAdapter,
		PullRequests: githubAdapter,
		Worker:       workerRuntime,
		Terminal:     terminalRuntime,
	})
}

// lifecycleGitHub combines the existing command fake with tracked PR state.
type lifecycleGitHub struct {
	commandGitHub
	pullRequest github.PullRequest
}

// FindPullRequest returns the one tracked PR projection used by lifecycle
// observation.
func (g *lifecycleGitHub) FindPullRequest(context.Context, github.Repository, string, string) (github.PullRequest, error) {
	return g.pullRequest, nil
}

// CreatePullRequest satisfies github.PullRequestClient for lifecycle tests.
func (g *lifecycleGitHub) CreatePullRequest(context.Context, github.Repository, github.PullRequestRequest) (github.PullRequest, error) {
	return g.pullRequest, nil
}

// UpdatePullRequest satisfies github.PullRequestClient for lifecycle tests.
func (g *lifecycleGitHub) UpdatePullRequest(context.Context, github.Repository, int, github.PullRequestRequest) (github.PullRequest, error) {
	return g.pullRequest, nil
}

// lifecycleWorker records the retention-preserving worker stop effect.
type lifecycleWorker struct {
	stopCalls int
}

// Start satisfies worker.WorkerRuntime for lifecycle tests.
func (*lifecycleWorker) Start(context.Context, worker.StartRequest) error { return nil }

// Resume satisfies worker.WorkerRuntime for lifecycle tests.
func (*lifecycleWorker) Resume(context.Context, worker.ResumeRequest) error { return nil }

// RunCommand satisfies worker.WorkerRuntime for lifecycle tests.
func (*lifecycleWorker) RunCommand(context.Context, worker.CommandRequest) (worker.CommandResult, error) {
	return worker.CommandResult{}, nil
}

// Stop records a stop without deleting the worker's retained state.
func (w *lifecycleWorker) Stop(context.Context, string) error {
	w.stopCalls++
	return nil
}

// Inspect satisfies worker.WorkerRuntime for lifecycle tests.
func (*lifecycleWorker) Inspect(context.Context, string) (worker.Inspection, error) {
	return worker.Inspection{Exists: true, Running: true}, nil
}

// lifecycleTerminal records operator notifications without talking to cmux.
type lifecycleTerminal struct {
	notifications []terminal.Notification
	notifyErrors  []error
}

// EnsureControlWorkspace returns a stable control workspace for notification.
func (*lifecycleTerminal) EnsureControlWorkspace(context.Context, terminal.WorkspaceRequest) (terminal.Workspace, error) {
	return terminal.Workspace{ID: "control"}, nil
}

// EnsureRunWorkspace satisfies terminal.TerminalRuntime for lifecycle tests.
func (*lifecycleTerminal) EnsureRunWorkspace(context.Context, terminal.RunWorkspaceRequest) (terminal.RunWorkspace, error) {
	return terminal.RunWorkspace{Workspace: terminal.Workspace{ID: "run"}}, nil
}

// CreateSurface satisfies terminal.TerminalRuntime for lifecycle tests.
func (*lifecycleTerminal) CreateSurface(context.Context, terminal.SurfaceRequest) (terminal.Surface, error) {
	return terminal.Surface{ID: "surface"}, nil
}

// LaunchSurface satisfies terminal.TerminalRuntime for lifecycle tests.
func (*lifecycleTerminal) LaunchSurface(context.Context, terminal.SurfaceID, terminal.Command) error {
	return nil
}

// SendInput satisfies terminal.TerminalRuntime for lifecycle tests.
func (*lifecycleTerminal) SendInput(context.Context, terminal.SurfaceID, []byte) error { return nil }

// Notify records one lifecycle notification.
func (t *lifecycleTerminal) Notify(_ context.Context, notification terminal.Notification) error {
	t.notifications = append(t.notifications, notification)
	if len(t.notifyErrors) > 0 {
		err := t.notifyErrors[0]
		t.notifyErrors = t.notifyErrors[1:]
		return err
	}
	return nil
}

// CloseSurface satisfies terminal.TerminalRuntime for lifecycle tests.
func (*lifecycleTerminal) CloseSurface(context.Context, terminal.SurfaceID) error { return nil }

// CloseWorkspace satisfies terminal.TerminalRuntime for lifecycle tests.
func (*lifecycleTerminal) CloseWorkspace(context.Context, terminal.WorkspaceID) error { return nil }
