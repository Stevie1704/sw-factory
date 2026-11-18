package github_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// TestGhClientListsPullRequestsByHeadPrefix verifies that the listing reads
// every pull request in every state and keeps those from the prefix only.
func TestGhClientListsPullRequestsByHeadPrefix(t *testing.T) {
	t.Parallel()
	runner := &fakeCommandRunner{outputs: [][]byte{[]byte(`[[
		{"number":7,"state":"closed","merged_at":"2026-11-01T00:00:00Z","head":{"ref":"factory/run-1","sha":"bbb"},"base":{"ref":"main"}},
		{"number":8,"state":"open","head":{"ref":"feature/manual","sha":"ccc"},"base":{"ref":"main"}}
	]]`)}}
	client := &github.GhClient{Runner: runner}
	pullRequests, err := client.ListPullRequestsByHeadPrefix(context.Background(), tracker.Repository{Owner: "example", Name: "project"}, "factory/")
	if err != nil || len(pullRequests) != 1 || pullRequests[0].Number != 7 || pullRequests[0].HeadBranch != "factory/run-1" {
		t.Fatalf("ListPullRequestsByHeadPrefix() = %+v, %v; want only #7", pullRequests, err)
	}
	if len(runner.calls) != 1 || !containsArgs(runner.calls[0].args, "repos/example/project/pulls", "--paginate", "--slurp") || !hasArgs(runner.calls[0].args, "-f", "state=all") {
		t.Fatalf("GitHub call = %#v, want a paginated listing in every state", runner.calls)
	}
}

// TestGhClientListsIssuesWithLabels verifies that the listing reads each
// label in every state, skips pull requests, and returns each issue once.
func TestGhClientListsIssuesWithLabels(t *testing.T) {
	t.Parallel()
	runner := &fakeCommandRunner{outputs: [][]byte{
		[]byte(`[[{"number":107,"state":"closed","labels":[{"name":"agent-complete"},{"name":"agent-failed"}]},{"number":9,"state":"open","labels":[{"name":"agent-complete"}],"pull_request":{"url":"x"}}]]`),
		[]byte(`[[{"number":107,"state":"closed","labels":[{"name":"agent-complete"},{"name":"agent-failed"}]},{"number":108,"state":"open","labels":[{"name":"agent-failed"}]}]]`),
	}}
	client := &github.GhClient{Runner: runner}
	issues, err := client.ListIssuesWithLabels(context.Background(), tracker.Repository{Owner: "example", Name: "project"}, []string{"agent-complete", "agent-failed"})
	if err != nil || len(issues) != 2 || issues[0].Number != 107 || issues[1].Number != 108 {
		t.Fatalf("ListIssuesWithLabels() = %+v, %v; want #107 and #108 once each", issues, err)
	}
	if len(runner.calls) != 2 || !hasArgs(runner.calls[0].args, "-f", "state=all") || !hasArgs(runner.calls[0].args, "-f", "labels=agent-complete") || !hasArgs(runner.calls[1].args, "-f", "labels=agent-failed") {
		t.Fatalf("GitHub calls = %#v, want one listing in every state per label", runner.calls)
	}
}

// TestGhClientListingFailureIsAnError verifies that a failed listing is an
// error, never an empty success.
func TestGhClientListingFailureIsAnError(t *testing.T) {
	t.Parallel()
	client := &github.GhClient{Runner: &fakeCommandRunner{outputs: [][]byte{nil}, errs: []error{errors.New("gh unreachable")}}}
	if _, err := client.ListIssuesWithLabels(context.Background(), tracker.Repository{Owner: "example", Name: "project"}, []string{"agent-failed"}); err == nil {
		t.Fatal("ListIssuesWithLabels() of an unreachable tracker succeeded")
	}
}
