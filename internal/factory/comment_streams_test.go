package factory_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// separateCommandTracker represents a tracker with identities unrelated to PRs.
type separateCommandTracker struct{ *commandGitHub }

// SeparateIssueCommentStream opts into independently scoped command watermarks.
func (*separateCommandTracker) SeparateIssueCommentStream() bool { return true }

// separatePRComments supplies the code-host stream without a tracker lookup.
type separatePRComments struct{ comments []tracker.Comment }

// PullRequestComments returns only the comments of the code-host PR.
func (c *separatePRComments) PullRequestComments(context.Context, tracker.Repository, int) ([]tracker.Comment, error) {
	return c.comments, nil
}

// TestSeparateCommentStreamsNeverSuppressEachOther exercises real command polling
// when a local issue and a PR have equal numbers and independent ID ranges.
func TestSeparateCommentStreamsNeverSuppressEachOther(t *testing.T) {
	run := commandRun(t, store.StatusActive)
	run.PullRequestNumber = run.IssueNumber
	issue := &separateCommandTracker{commandGitHub: &commandGitHub{issue: tracker.Issue{Number: run.IssueNumber, State: "open", Labels: []string{tracker.LabelAgentRunning}}, statusComment: tracker.Comment{ID: "local-status-1"}, pullRequest: codehost.PullRequest{Number: run.IssueNumber, State: "open", HeadBranch: run.Branch}, comments: []tracker.Comment{{ID: "local-comment-999", Author: "alice", Body: "/factory status"}}}}
	pr := &separatePRComments{comments: []tracker.Comment{{ID: "1", Author: "alice", Body: "/factory status"}}}
	runStore := &commandRunStore{current: &run, latest: &run}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{Config: commandConfig{host: commandHost()}, OpenStore: func(context.Context, string) (factory.OperationalStore, error) { return runStore, nil }, Tracker: issue, Comments: issue, PullRequestComments: pr, Now: func() time.Time { return time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC) }})
	results, err := service.PollCommands(t.Context(), factory.CommandPollRequest{})
	if err != nil || len(results) != 2 || results[0].Outcome != factory.CommandAccepted || results[1].Outcome != factory.CommandAccepted {
		t.Fatalf("first polling = %#v, %v", results, err)
	}
	pr.comments = append(pr.comments, tracker.Comment{ID: "2", Author: "alice", Body: "/factory status"})
	results, err = service.PollCommands(t.Context(), factory.CommandPollRequest{})
	accepted := 0
	for _, result := range results {
		if result.Outcome == factory.CommandAccepted {
			accepted++
		}
	}
	if err != nil || accepted != 1 {
		t.Fatalf("new PR command suppressed by local history: %#v, %v", results, err)
	}
	results, err = service.PollCommands(t.Context(), factory.CommandPollRequest{})
	for _, result := range results {
		if result.Outcome != factory.CommandReplayed {
			t.Fatalf("command replayed its effect: %#v", result)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
}
