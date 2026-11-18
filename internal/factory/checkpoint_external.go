package factory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/checkpoint"
	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/config"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// RemoteBranchReader reads the head of one branch on the Git remote.
type RemoteBranchReader interface {
	RemoteBranchHead(context.Context, gitadapter.PushRequest) (string, error)
}

// ExternalStateSources are the read seams of the external state of a
// checkpoint. A nil source makes each read of it fail.
type ExternalStateSources struct {
	// Tracker reads issues and status comments.
	Tracker tracker.Client
	// PullRequests reads pull requests.
	PullRequests codehost.PullRequestClient
	// Remote reads branch heads on the Git remote of the checkout.
	Remote RemoteBranchReader
	// TargetBranch is the base branch of every factory pull request.
	TargetBranch string
}

// externalStateReader reads the external state of one registration through
// the tracker, code host, and Git seams. It only reads.
type externalStateReader struct {
	registration config.RepositoryRegistration
	repository   tracker.Repository
	sources      ExternalStateSources
}

// NewExternalStateReader returns the checkpoint.ExternalReader of one
// registration. A checkpoint records what it reads, and a restore compares
// the same reads with that record.
func NewExternalStateReader(registration config.RepositoryRegistration, sources ExternalStateSources) checkpoint.ExternalReader {
	return externalStateReader{registration: registration, repository: registeredRepository(registration), sources: sources}
}

// BranchHead reads the remote head of branch from the registered checkout.
func (r externalStateReader) BranchHead(ctx context.Context, branch string) (string, error) {
	if r.sources.Remote == nil {
		return "", errors.New("no Git remote reader is configured")
	}
	return r.sources.Remote.RemoteBranchHead(ctx, gitadapter.PushRequest{WorktreePath: r.registration.Path, Branch: branch})
}

// PullRequest finds the pull request of headBranch into the target branch
// and returns it when it has number.
func (r externalStateReader) PullRequest(ctx context.Context, number int, headBranch string) (checkpoint.PullRequestState, error) {
	if r.sources.PullRequests == nil {
		return checkpoint.PullRequestState{}, errors.New("no code host is configured")
	}
	found, err := r.sources.PullRequests.FindPullRequest(ctx, r.repository, headBranch, r.sources.TargetBranch)
	if err != nil {
		return checkpoint.PullRequestState{}, err
	}
	if found.Number != number {
		return checkpoint.PullRequestState{}, fmt.Errorf("the code host shows no pull request #%d from %s", number, headBranch)
	}
	state := strings.ToLower(found.State)
	if found.Merged {
		state = "merged"
	}
	return checkpoint.PullRequestState{Number: found.Number, HeadBranch: found.HeadBranch, HeadSHA: found.HeadSHA, State: state, Draft: found.Draft}, nil
}

// Issue reads the open state and the factory state labels of one issue.
func (r externalStateReader) Issue(ctx context.Context, number int) (checkpoint.IssueState, error) {
	if r.sources.Tracker == nil {
		return checkpoint.IssueState{}, errors.New("no work tracker is configured")
	}
	issue, err := r.sources.Tracker.Issue(ctx, r.repository, number)
	if err != nil {
		return checkpoint.IssueState{}, err
	}
	labels := []string{}
	for _, label := range issue.Labels {
		if slices.Contains(tracker.FactoryStateLabels, label) {
			labels = append(labels, label)
		}
	}
	slices.Sort(labels)
	return checkpoint.IssueState{Number: issue.Number, Open: strings.EqualFold(issue.State, "open"), FactoryLabels: labels}, nil
}

// StatusComment finds the status comment of a run by its marker.
func (r externalStateReader) StatusComment(ctx context.Context, issueNumber int, runID string) (checkpoint.StatusCommentState, error) {
	if r.sources.Tracker == nil {
		return checkpoint.StatusCommentState{}, errors.New("no work tracker is configured")
	}
	comment, err := r.sources.Tracker.FindStatusComment(ctx, r.repository, issueNumber, statusCommentMarker(runID))
	if err != nil {
		return checkpoint.StatusCommentState{}, err
	}
	if comment.ID == "" {
		return checkpoint.StatusCommentState{}, fmt.Errorf("the tracker shows no status comment of run %s", runID)
	}
	return checkpoint.StatusCommentState{RunID: runID, IssueNumber: issueNumber, ID: comment.ID, UpdatedAt: comment.UpdatedAt.UTC()}, nil
}
