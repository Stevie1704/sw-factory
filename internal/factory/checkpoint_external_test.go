package factory

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/checkpoint"
	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/config"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// readOnlyTracker answers the two tracker reads of a checkpoint. Any other
// call panics through the nil embedded client, so the reader cannot write.
type readOnlyTracker struct {
	tracker.Client
	issue    tracker.Issue
	comments []tracker.Comment
}

// Issue returns the configured issue.
func (f *readOnlyTracker) Issue(context.Context, tracker.Repository, int) (tracker.Issue, error) {
	return f.issue, nil
}

// IssueComments returns the configured comments of every author.
func (f *readOnlyTracker) IssueComments(context.Context, tracker.Repository, int) ([]tracker.Comment, error) {
	return f.comments, nil
}

// FindStatusComment finds nothing, as GitHub does for a comment of another
// login. A checkpoint must not depend on it.
func (f *readOnlyTracker) FindStatusComment(context.Context, tracker.Repository, int, string) (tracker.Comment, error) {
	return tracker.Comment{}, nil
}

// readOnlyPullRequests answers the pull request reads. FindPullRequest
// returns the newest pull request of a branch pair; PullRequest reads one
// pull request by number.
type readOnlyPullRequests struct {
	codehost.PullRequestClient
	newest   codehost.PullRequest
	byNumber map[int]codehost.PullRequest
	err      error
}

// FindPullRequest returns the newest pull request of the branch pair.
func (f readOnlyPullRequests) FindPullRequest(context.Context, tracker.Repository, string, string) (codehost.PullRequest, error) {
	return f.newest, f.err
}

// PullRequest returns the pull request with number, or an error when the
// code host shows none.
func (f readOnlyPullRequests) PullRequest(_ context.Context, _ tracker.Repository, number int) (codehost.PullRequest, error) {
	if f.err != nil {
		return codehost.PullRequest{}, f.err
	}
	pullRequest, ok := f.byNumber[number]
	if !ok {
		return codehost.PullRequest{}, errors.New("not found")
	}
	return pullRequest, nil
}

// PullRequest reads one fixture pull request by number.
func (f *restoreCodeHost) PullRequest(_ context.Context, _ tracker.Repository, number int) (codehost.PullRequest, error) {
	for _, pullRequest := range f.pullRequests {
		if pullRequest.Number == number {
			return pullRequest, f.err
		}
	}
	return codehost.PullRequest{}, errors.Join(f.err, errors.New("not found"))
}

// IssueComments returns the fixture status comment.
func (f *restoreTracker) IssueComments(context.Context, tracker.Repository, int) ([]tracker.Comment, error) {
	return []tracker.Comment{f.comment}, f.err
}

// fixedRemote returns one head for every branch.
type fixedRemote struct{ head string }

// RemoteBranchHead returns the fixed head.
func (f fixedRemote) RemoteBranchHead(context.Context, gitadapter.PushRequest) (string, error) {
	return f.head, nil
}

// TestExternalStateReaderMapsTrackerAndCodeHostState verifies that the
// adapter maps a merged pull request, a closed issue with its factory
// labels only, and the status comment with the persisted ID.
func TestExternalStateReaderMapsTrackerAndCodeHostState(t *testing.T) {
	t.Parallel()
	merged := codehost.PullRequest{Number: 7, State: "closed", Merged: true, HeadBranch: "factory/run-1", HeadSHA: "bbb"}
	issues := &readOnlyTracker{
		issue:    tracker.Issue{Number: 107, State: "closed", Labels: []string{"bug", tracker.LabelAgentComplete}},
		comments: []tracker.Comment{{ID: "comment-9", UpdatedAt: time.Unix(500, 0).UTC()}},
	}
	reader := NewExternalStateReader(config.RepositoryRegistration{Path: "/work/service"}, ExternalStateSources{
		Tracker:      issues,
		PullRequests: readOnlyPullRequests{newest: merged, byNumber: map[int]codehost.PullRequest{7: merged}},
		Remote:       fixedRemote{head: "aaa"},
		TargetBranch: "main",
	})
	ctx := context.Background()

	if head, err := reader.BranchHead(ctx, "main"); err != nil || head != "aaa" {
		t.Fatalf("BranchHead() = %q, %v", head, err)
	}
	pullRequest, err := reader.PullRequest(ctx, 7, "factory/run-1")
	if err != nil || pullRequest != (checkpoint.PullRequestState{Number: 7, HeadBranch: "factory/run-1", HeadSHA: "bbb", State: "merged"}) {
		t.Fatalf("PullRequest() = %+v, %v", pullRequest, err)
	}
	issue, err := reader.Issue(ctx, 107)
	if err != nil || !reflect.DeepEqual(issue, checkpoint.IssueState{Number: 107, Open: false, FactoryLabels: []string{tracker.LabelAgentComplete}}) {
		t.Fatalf("Issue() = %+v, %v", issue, err)
	}
	comment, err := reader.StatusComment(ctx, 107, "run-1", "comment-9")
	if err != nil || comment != (checkpoint.StatusCommentState{RunID: "run-1", IssueNumber: 107, ID: "comment-9", UpdatedAt: time.Unix(500, 0).UTC()}) {
		t.Fatalf("StatusComment() = %+v, %v", comment, err)
	}
}

// TestExternalStateReaderReadsAKnownPullRequestByNumber verifies that a
// newer pull request of the same branch pair, or a retargeted one, does not
// hide the persisted pull request number.
func TestExternalStateReaderReadsAKnownPullRequestByNumber(t *testing.T) {
	t.Parallel()
	known := codehost.PullRequest{Number: 7, State: "closed", HeadBranch: "factory/run-1", HeadSHA: "bbb"}
	newer := codehost.PullRequest{Number: 9, State: "open", HeadBranch: "factory/run-1", HeadSHA: "ccc"}
	reader := NewExternalStateReader(config.RepositoryRegistration{}, ExternalStateSources{
		PullRequests: readOnlyPullRequests{newest: newer, byNumber: map[int]codehost.PullRequest{7: known, 9: newer}},
		TargetBranch: "main",
	})

	pullRequest, err := reader.PullRequest(context.Background(), 7, "factory/run-1")
	if err != nil || pullRequest != (checkpoint.PullRequestState{Number: 7, HeadBranch: "factory/run-1", HeadSHA: "bbb", State: "closed"}) {
		t.Fatalf("PullRequest() = %+v, %v; want #7 by its number", pullRequest, err)
	}
	if _, err := reader.PullRequest(context.Background(), 9, "factory/run-2"); err == nil {
		t.Fatal("PullRequest() accepted a pull request from another head branch")
	}
}

// TestExternalStateReaderReadsAKnownStatusCommentByID verifies that a
// status comment of another login, such as one from before an account
// rotation, is read by its persisted ID, and that a missing ID is an error.
func TestExternalStateReaderReadsAKnownStatusCommentByID(t *testing.T) {
	t.Parallel()
	issues := &readOnlyTracker{comments: []tracker.Comment{
		{ID: "comment-8", Author: "new-bot", UpdatedAt: time.Unix(400, 0).UTC()},
		{ID: "comment-9", Author: "old-bot", UpdatedAt: time.Unix(500, 0).UTC()},
	}}
	reader := NewExternalStateReader(config.RepositoryRegistration{}, ExternalStateSources{Tracker: issues, TargetBranch: "main"})

	comment, err := reader.StatusComment(context.Background(), 107, "run-1", "comment-9")
	if err != nil || comment != (checkpoint.StatusCommentState{RunID: "run-1", IssueNumber: 107, ID: "comment-9", UpdatedAt: time.Unix(500, 0).UTC()}) {
		t.Fatalf("StatusComment() = %+v, %v; want comment-9 by its ID", comment, err)
	}
	if _, err := reader.StatusComment(context.Background(), 107, "run-1", "comment-10"); err == nil {
		t.Fatal("StatusComment() succeeded for an ID that the tracker does not show")
	}
}

// TestExternalStateReaderRefusesAMissingSource verifies that a missing
// code host and a failed read are errors, never an empty state.
func TestExternalStateReaderRefusesAMissingSource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	missing := NewExternalStateReader(config.RepositoryRegistration{}, ExternalStateSources{TargetBranch: "main"})
	if _, err := missing.PullRequest(ctx, 7, "factory/run-1"); err == nil {
		t.Fatal("PullRequest() without a code host succeeded")
	}
	if _, err := missing.Issue(ctx, 107); err == nil {
		t.Fatal("Issue() without a tracker succeeded")
	}
	if _, err := missing.BranchHead(ctx, "main"); err == nil {
		t.Fatal("BranchHead() without a Git remote succeeded")
	}
	failing := NewExternalStateReader(config.RepositoryRegistration{}, ExternalStateSources{PullRequests: readOnlyPullRequests{err: errors.New("unreachable")}})
	if _, err := failing.PullRequest(ctx, 7, "factory/run-1"); err == nil {
		t.Fatal("PullRequest() with a failing code host succeeded")
	}
}
