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

// PullRequestNumberReader reads one pull request by its number, in every
// state and whatever its base branch is.
type PullRequestNumberReader interface {
	PullRequest(ctx context.Context, repository tracker.Repository, number int) (codehost.PullRequest, error)
}

// PullRequest reads the pull request with the persisted number. It never
// searches by branch pair, so a newer or retargeted pull request of the same
// branch cannot hide it. A pull request from another head branch is an
// error, because a head branch never changes.
func (r externalStateReader) PullRequest(ctx context.Context, number int, headBranch string) (checkpoint.PullRequestState, error) {
	reader, ok := r.sources.PullRequests.(PullRequestNumberReader)
	if !ok {
		return checkpoint.PullRequestState{}, errors.New("the code host cannot read a pull request by number")
	}
	found, err := reader.PullRequest(ctx, r.repository, number)
	if err != nil {
		return checkpoint.PullRequestState{}, err
	}
	if found.Number != number || found.HeadBranch != headBranch {
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

// StatusComment reads the comments of the issue and returns the one with
// the persisted commentID. It does not filter by the current login, so an
// account rotation cannot hide a status comment that the store knows.
func (r externalStateReader) StatusComment(ctx context.Context, issueNumber int, runID, commentID string) (checkpoint.StatusCommentState, error) {
	reader, ok := r.sources.Tracker.(tracker.CommentReader)
	if !ok {
		return checkpoint.StatusCommentState{}, errors.New("the work tracker cannot list issue comments")
	}
	comments, err := reader.IssueComments(ctx, r.repository, issueNumber)
	if err != nil {
		return checkpoint.StatusCommentState{}, err
	}
	for _, comment := range comments {
		if comment.ID == commentID {
			return checkpoint.StatusCommentState{RunID: runID, IssueNumber: issueNumber, ID: comment.ID, UpdatedAt: comment.UpdatedAt.UTC()}, nil
		}
	}
	return checkpoint.StatusCommentState{}, fmt.Errorf("the tracker shows no status comment %s of run %s", commentID, runID)
}

// factoryBranchPrefix is the prefix of every factory-owned branch.
const factoryBranchPrefix = "factory/"

// RemoteBranchLister lists the branches on the Git remote below a prefix.
type RemoteBranchLister interface {
	RemoteBranches(ctx context.Context, worktreePath, prefix string) ([]string, error)
}

// PullRequestHeadLister lists the pull requests, in every state, whose head
// branch starts with a prefix.
type PullRequestHeadLister interface {
	ListPullRequestsByHeadPrefix(ctx context.Context, repository tracker.Repository, prefix string) ([]codehost.PullRequest, error)
}

// LabeledIssueLister lists the issues, in every state, that carry any of
// the labels.
type LabeledIssueLister interface {
	ListIssuesWithLabels(ctx context.Context, repository tracker.Repository, labels []string) ([]tracker.Issue, error)
}

// NewOwnedObjectLister returns the checkpoint.OwnedObjectLister of one
// registration. A source that cannot list its factory-owned objects makes
// that listing fail, so a restore refuses as unverified.
func NewOwnedObjectLister(registration config.RepositoryRegistration, sources ExternalStateSources) checkpoint.OwnedObjectLister {
	return externalStateReader{registration: registration, repository: registeredRepository(registration), sources: sources}
}

// runStateLabels are the factory state labels that only a run sets. The
// ready label is a human authorization, not a factory effect, so an issue
// that carries only it has no run to know.
func runStateLabels() []string {
	var labels []string
	for _, label := range tracker.FactoryStateLabels {
		if label != tracker.LabelAgentReady {
			labels = append(labels, label)
		}
	}
	return labels
}

// FactoryBranches lists the remote branches below factory/.
func (r externalStateReader) FactoryBranches(ctx context.Context) ([]string, error) {
	lister, ok := r.sources.Remote.(RemoteBranchLister)
	if !ok {
		return nil, errors.New("the Git remote reader cannot list branches")
	}
	return lister.RemoteBranches(ctx, r.registration.Path, factoryBranchPrefix)
}

// FactoryPullRequests lists the pull requests from factory/ branches.
func (r externalStateReader) FactoryPullRequests(ctx context.Context) ([]checkpoint.PullRequestState, error) {
	lister, ok := r.sources.PullRequests.(PullRequestHeadLister)
	if !ok {
		return nil, errors.New("the code host cannot list pull requests by head branch")
	}
	found, err := lister.ListPullRequestsByHeadPrefix(ctx, r.repository, factoryBranchPrefix)
	if err != nil {
		return nil, err
	}
	states := make([]checkpoint.PullRequestState, 0, len(found))
	for _, pullRequest := range found {
		states = append(states, checkpoint.PullRequestState{Number: pullRequest.Number, HeadBranch: pullRequest.HeadBranch, HeadSHA: pullRequest.HeadSHA, State: pullRequest.State, Draft: pullRequest.Draft})
	}
	return states, nil
}

// FactoryLabeledIssues lists the issues with a factory run-state label.
func (r externalStateReader) FactoryLabeledIssues(ctx context.Context) ([]checkpoint.IssueState, error) {
	lister, ok := r.sources.Tracker.(LabeledIssueLister)
	if !ok {
		return nil, errors.New("the work tracker cannot list issues by label")
	}
	labels := runStateLabels()
	found, err := lister.ListIssuesWithLabels(ctx, r.repository, labels)
	if err != nil {
		return nil, err
	}
	states := make([]checkpoint.IssueState, 0, len(found))
	for _, issue := range found {
		var factoryLabels []string
		for _, label := range issue.Labels {
			if slices.Contains(labels, label) {
				factoryLabels = append(factoryLabels, label)
			}
		}
		states = append(states, checkpoint.IssueState{Number: issue.Number, Open: strings.EqualFold(issue.State, "open"), FactoryLabels: factoryLabels})
	}
	return states, nil
}
