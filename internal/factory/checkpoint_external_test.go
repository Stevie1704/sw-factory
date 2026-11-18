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
	issue   tracker.Issue
	comment tracker.Comment
	markers []string
}

// Issue returns the configured issue.
func (f *readOnlyTracker) Issue(context.Context, tracker.Repository, int) (tracker.Issue, error) {
	return f.issue, nil
}

// FindStatusComment records the marker and returns the configured comment.
func (f *readOnlyTracker) FindStatusComment(_ context.Context, _ tracker.Repository, _ int, marker string) (tracker.Comment, error) {
	f.markers = append(f.markers, marker)
	return f.comment, nil
}

// readOnlyPullRequests answers FindPullRequest only.
type readOnlyPullRequests struct {
	codehost.PullRequestClient
	pullRequest codehost.PullRequest
	err         error
}

// FindPullRequest returns the configured pull request.
func (f readOnlyPullRequests) FindPullRequest(context.Context, tracker.Repository, string, string) (codehost.PullRequest, error) {
	return f.pullRequest, f.err
}

// fixedRemote returns one head for every branch.
type fixedRemote struct{ head string }

// RemoteBranchHead returns the fixed head.
func (f fixedRemote) RemoteBranchHead(context.Context, gitadapter.PushRequest) (string, error) {
	return f.head, nil
}

// TestExternalStateReaderMapsTrackerAndCodeHostState verifies that the
// adapter maps a merged pull request, a closed issue with its factory
// labels only, and the status comment that the run marker finds.
func TestExternalStateReaderMapsTrackerAndCodeHostState(t *testing.T) {
	t.Parallel()
	issues := &readOnlyTracker{
		issue:   tracker.Issue{Number: 107, State: "closed", Labels: []string{"bug", tracker.LabelAgentComplete}},
		comment: tracker.Comment{ID: "comment-9", UpdatedAt: time.Unix(500, 0).UTC()},
	}
	reader := NewExternalStateReader(config.RepositoryRegistration{Path: "/work/service"}, ExternalStateSources{
		Tracker:      issues,
		PullRequests: readOnlyPullRequests{pullRequest: codehost.PullRequest{Number: 7, State: "closed", Merged: true, HeadBranch: "factory/run-1", HeadSHA: "bbb"}},
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
	comment, err := reader.StatusComment(ctx, 107, "run-1")
	if err != nil || comment != (checkpoint.StatusCommentState{RunID: "run-1", IssueNumber: 107, ID: "comment-9", UpdatedAt: time.Unix(500, 0).UTC()}) {
		t.Fatalf("StatusComment() = %+v, %v", comment, err)
	}
	if len(issues.markers) != 1 || issues.markers[0] != statusCommentMarker("run-1") {
		t.Fatalf("status comment markers = %v, want the marker of run-1", issues.markers)
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
