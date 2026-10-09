package factory_test

import (
	"context"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// separateCommentSurfaces is a tracker whose work items and pull requests
// have separate number spaces, as in Azure DevOps. Work item 7 and pull
// request 7 are different objects with different comments.
type separateCommentSurfaces struct {
	issueComments       map[int][]tracker.Comment
	pullRequestComments map[int][]tracker.Comment
}

// IssueComments returns the comments of one work item.
func (s separateCommentSurfaces) IssueComments(_ context.Context, _ tracker.Repository, number int) ([]tracker.Comment, error) {
	return append([]tracker.Comment(nil), s.issueComments[number]...), nil
}

// PullRequestComments returns the conversation comments of one pull request.
func (s separateCommentSurfaces) PullRequestComments(_ context.Context, _ tracker.Repository, number int) ([]tracker.Comment, error) {
	return append([]tracker.Comment(nil), s.pullRequestComments[number]...), nil
}

// TestPollCommandsReadsThePullRequestThroughTheCodeHost verifies that the
// pull-request target of a run is read through the code-host comment seam,
// so a work item with the same number as the pull request is never read.
func TestPollCommandsReadsThePullRequestThroughTheCodeHost(t *testing.T) {
	t.Parallel()

	run := commandRun(t, store.StatusActive)
	run.PullRequestNumber = 7
	githubAdapter := &commandGitHub{
		issue:         tracker.Issue{Number: 42, State: "open", Labels: []string{tracker.LabelAgentRunning}},
		statusComment: tracker.Comment{ID: "status-1"},
	}
	surfaces := separateCommentSurfaces{
		issueComments:       map[int][]tracker.Comment{7: {{ID: "90", Author: "alice", Body: "/factory status"}}},
		pullRequestComments: map[int][]tracker.Comment{7: {{ID: "80", Author: "alice", Body: "/factory status"}}},
	}
	runStore := &commandRunStore{current: &run, latest: &run}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:              commandConfig{host: commandHost()},
		OpenStore:           func(context.Context, string) (factory.OperationalStore, error) { return runStore, nil },
		Tracker:             githubAdapter,
		Comments:            surfaces,
		PullRequestComments: surfaces,
		Worker:              &agentWorker{},
	})

	results, err := service.PollCommands(context.Background(), factory.CommandPollRequest{RunID: run.ID})
	if err != nil {
		t.Fatalf("PollCommands() error = %v", err)
	}
	if len(results) != 1 || results[0].Outcome != factory.CommandAccepted {
		t.Fatalf("PollCommands() = %#v, want only the pull-request command", results)
	}
	stored, err := runStore.CurrentRun(context.Background())
	if err != nil || stored == nil || stored.ProcessedCommentID != "80" {
		t.Fatalf("CurrentRun() = %#v/%v, want watermark 80 from the pull request", stored, err)
	}
}

// TestPollCommandsKeepsATimeOrderedTextWatermark verifies that comment
// identities which are not numbers, but sort as text in creation order, are
// processed once. An earlier comment must not run again after a later one set
// the watermark.
func TestPollCommandsKeepsATimeOrderedTextWatermark(t *testing.T) {
	t.Parallel()

	run := commandRun(t, store.StatusActive)
	githubAdapter := &commandGitHub{
		issue:         tracker.Issue{Number: 42, State: "open", Labels: []string{tracker.LabelAgentRunning}},
		statusComment: tracker.Comment{ID: "status-1"},
		comments: []tracker.Comment{
			{ID: "20261009T100000.000000000Z.w42.c1", Author: "alice", Body: "/factory status"},
			{ID: "20261009T100500.000000000Z.w42.c2", Author: "alice", Body: "/factory status"},
		},
	}
	runStore := &commandRunStore{current: &run, latest: &run}
	service := newCommandService(runStore, githubAdapter, githubAdapter)

	first, err := service.PollCommands(context.Background(), factory.CommandPollRequest{RunID: run.ID})
	if err != nil || len(first) != 2 {
		t.Fatalf("first PollCommands() = %#v/%v, want two commands", first, err)
	}
	second, err := service.PollCommands(context.Background(), factory.CommandPollRequest{RunID: run.ID})
	if err != nil {
		t.Fatalf("second PollCommands() error = %v", err)
	}
	for _, result := range second {
		if result.Outcome != factory.CommandReplayed {
			t.Fatalf("second PollCommands() = %#v, want every comment replayed", second)
		}
	}
}

// pullRequestCommentHost is a command tracker whose code host keeps
// pull-request comments apart, and records where comments were posted.
type pullRequestCommentHost struct {
	*commandGitHub
	pullRequestPosts map[int][]string
}

// FindPullRequestComment reports that no coordinator comment exists yet.
func (h *pullRequestCommentHost) FindPullRequestComment(context.Context, tracker.Repository, int, string) (tracker.Comment, error) {
	return tracker.Comment{}, nil
}

// CreatePullRequestComment records one comment posted to a pull request.
func (h *pullRequestCommentHost) CreatePullRequestComment(_ context.Context, _ tracker.Repository, number int, body string) (tracker.Comment, error) {
	h.pullRequestPosts[number] = append(h.pullRequestPosts[number], body)
	return tracker.Comment{ID: "pr-comment-1", Body: body}, nil
}

// EditPullRequestComment is not reached when no comment exists yet.
func (h *pullRequestCommentHost) EditPullRequestComment(context.Context, tracker.Repository, int, string, string) error {
	return nil
}

// PullRequestComments returns no pull-request commands.
func (h *pullRequestCommentHost) PullRequestComments(context.Context, tracker.Repository, int) ([]tracker.Comment, error) {
	return nil, nil
}

// TestPollCommandsPostsClarificationQuestionsOnThePullRequest verifies that
// clarification questions for a run with a pull request reach the pull
// request through the code-host comment seam, not a work item that has the
// same number.
func TestPollCommandsPostsClarificationQuestionsOnThePullRequest(t *testing.T) {
	t.Parallel()

	run := commandRun(t, store.StatusWaitingForHuman)
	run.PullRequestNumber = 7
	run.LifecycleReason = "test agent requested clarification"
	run.PendingQuestions = []store.PendingQuestion{{ID: "format", Prompt: "Which format should be used?"}}
	host := &pullRequestCommentHost{
		commandGitHub: &commandGitHub{
			issue:         tracker.Issue{Number: 42, State: "open", Labels: []string{tracker.LabelAgentNeedsInput}},
			statusComment: tracker.Comment{ID: "status-1"},
			pullRequest:   codehost.PullRequest{Number: 7, State: "open", Draft: true},
		},
		pullRequestPosts: map[int][]string{},
	}
	runStore := &commandRunStore{current: &run, latest: &run}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config:    commandConfig{host: commandHost()},
		OpenStore: func(context.Context, string) (factory.OperationalStore, error) { return runStore, nil },
		Tracker:   host,
		Comments:  host,
		Worker:    &agentWorker{},
	})

	if _, err := service.PollCommands(context.Background(), factory.CommandPollRequest{RunID: run.ID}); err != nil {
		t.Fatalf("PollCommands() error = %v", err)
	}
	if len(host.pullRequestPosts[7]) != 1 || len(host.createdComments) != 0 || len(host.editedComments) != 0 {
		t.Fatalf("pull-request posts = %#v, issue posts = %#v, issue edits = %#v, want the questions on pull request 7 only", host.pullRequestPosts, host.createdComments, host.editedComments)
	}
	stored, err := runStore.CurrentRun(context.Background())
	if err != nil || stored == nil || stored.ClarificationCommentID != "pr-comment-1" {
		t.Fatalf("CurrentRun() = %#v/%v, want the pull-request comment identity", stored, err)
	}
}
