package checkpoint

import (
	"context"
	"fmt"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// ExternalState is the observed state of every external object that a
// snapshot knows, through its runs or its publication ledger. Restore compares each value and refuses on a difference.
type ExternalState struct {
	// TargetBranch is the remote head of the target branch.
	TargetBranch BranchState `json:"target_branch"`
	// Branches holds each known run or publication branch, in run order
	// and then in publication ledger order.
	Branches []BranchState `json:"branches"`
	// PullRequests holds each known run or publication pull request, in
	// run order and then in publication ledger order.
	PullRequests []PullRequestState `json:"pull_requests"`
	// Issues holds each known run issue or published tracker item, in run
	// order and then in publication ledger order.
	Issues []IssueState `json:"issues"`
	// StatusComments holds each status comment that the store identifies,
	// in run order.
	StatusComments []StatusCommentState `json:"status_comments"`
}

// BranchState is the remote head of one branch. An empty head means that
// the remote has no such branch, which is an observed state too.
type BranchState struct {
	Name string `json:"name"`
	Head string `json:"head"`
}

// PullRequestState is the observed state of one pull request.
type PullRequestState struct {
	Number     int    `json:"number"`
	HeadBranch string `json:"head_branch"`
	HeadSHA    string `json:"head_sha"`
	// State is open, closed, or merged.
	State string `json:"state"`
	Draft bool   `json:"draft"`
}

// IssueState is the open state and the factory state labels of one issue.
type IssueState struct {
	Number        int      `json:"number"`
	Open          bool     `json:"open"`
	FactoryLabels []string `json:"factory_labels"`
}

// StatusCommentState identifies one status comment and its update time.
type StatusCommentState struct {
	RunID       string    `json:"run_id"`
	IssueNumber int       `json:"issue_number"`
	ID          string    `json:"id"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ExternalReader reads the observable state of one external object from
// the work tracker, the code host, or the Git remote. It only reads. An
// object that it cannot read is an error, never an empty success.
type ExternalReader interface {
	// BranchHead returns the remote head of branch, or "" when the remote
	// has no such branch.
	BranchHead(ctx context.Context, branch string) (string, error)
	// PullRequest reads the pull request with number by that number, so a
	// newer or retargeted pull request of the same branch cannot hide it.
	// A pull request whose head is not headBranch is an error.
	PullRequest(ctx context.Context, number int, headBranch string) (PullRequestState, error)
	// Issue returns the open state and the factory state labels of issue
	// number.
	Issue(ctx context.Context, number int) (IssueState, error)
	// StatusComment reads the status comment with the persisted commentID
	// of run runID on issue issueNumber, whatever login wrote it.
	StatusComment(ctx context.Context, issueNumber int, runID, commentID string) (StatusCommentState, error)
}

// ReadExternalState reads the state of the target branch and of every
// external object that runs and the publication ledger identify. It reads
// each object once. It returns one detail for each object that it cannot
// read; a checkpoint with any such detail is refused.
func ReadExternalState(ctx context.Context, reader ExternalReader, targetBranch string, runs []store.KnownRun, publications []store.KnownPublication) (ExternalState, []string) {
	observer := externalObserver{reader: reader, seenBranches: map[string]bool{}, seenPullRequests: map[int]bool{}, seenIssues: map[int]bool{}}
	state := ExternalState{Branches: []BranchState{}, PullRequests: []PullRequestState{}, Issues: []IssueState{}, StatusComments: []StatusCommentState{}}
	state.TargetBranch = BranchState{Name: targetBranch}
	if head, err := reader.BranchHead(ctx, targetBranch); err != nil || head == "" {
		observer.unreadable(fmt.Sprintf("target branch %s", targetBranch), err, "the remote has no such branch")
	} else {
		state.TargetBranch.Head = head
	}
	for _, run := range runs {
		observer.observeBranch(ctx, run.Branch, &state)
		observer.observePullRequest(ctx, run.PullRequestNumber, run.Branch, &state)
		observer.observeIssue(ctx, run.IssueNumber, &state)
		observer.observeStatusComment(ctx, run, &state)
	}
	for _, publication := range publications {
		observer.observeBranch(ctx, publication.Branch, &state)
		observer.observePullRequest(ctx, publication.PullRequestNumber, publication.Branch, &state)
		observer.observeIssue(ctx, publication.IssueNumber, &state)
	}
	return state, observer.unread
}

// externalObserver collects the observed state and the unread objects.
type externalObserver struct {
	reader           ExternalReader
	seenBranches     map[string]bool
	seenPullRequests map[int]bool
	seenIssues       map[int]bool
	unread           []string
}

// observeBranch reads the remote head of a named branch once.
func (o *externalObserver) observeBranch(ctx context.Context, branch string, state *ExternalState) {
	if branch == "" || o.seenBranches[branch] {
		return
	}
	o.seenBranches[branch] = true
	head, err := o.reader.BranchHead(ctx, branch)
	if err != nil {
		o.unreadable("branch "+branch, err, "")
		return
	}
	state.Branches = append(state.Branches, BranchState{Name: branch, Head: head})
}

// observePullRequest reads a numbered pull request from headBranch once.
func (o *externalObserver) observePullRequest(ctx context.Context, number int, headBranch string, state *ExternalState) {
	if number <= 0 || o.seenPullRequests[number] {
		return
	}
	o.seenPullRequests[number] = true
	pullRequest, err := o.reader.PullRequest(ctx, number, headBranch)
	if err != nil || pullRequest.Number != number {
		o.unreadable(fmt.Sprintf("pull request #%d", number), err, "the code host does not show it")
		return
	}
	state.PullRequests = append(state.PullRequests, pullRequest)
}

// observeIssue reads a numbered issue once.
func (o *externalObserver) observeIssue(ctx context.Context, number int, state *ExternalState) {
	if number <= 0 || o.seenIssues[number] {
		return
	}
	o.seenIssues[number] = true
	issue, err := o.reader.Issue(ctx, number)
	if err != nil || issue.Number != number {
		o.unreadable(fmt.Sprintf("issue #%d", number), err, "the tracker does not show it")
		return
	}
	state.Issues = append(state.Issues, issue)
}

// observeStatusComment reads the status comment that a run identifies.
func (o *externalObserver) observeStatusComment(ctx context.Context, run store.KnownRun, state *ExternalState) {
	if run.StatusCommentID == "" {
		return
	}
	comment, err := o.reader.StatusComment(ctx, run.IssueNumber, run.RunID, run.StatusCommentID)
	if err != nil || comment.ID != run.StatusCommentID {
		o.unreadable(fmt.Sprintf("status comment %s of run %s", run.StatusCommentID, run.RunID), err, "the tracker does not show it")
		return
	}
	state.StatusComments = append(state.StatusComments, comment)
}

// unreadable records one object that could not be read, with the read
// error or, without one, the reason.
func (o *externalObserver) unreadable(object string, err error, reason string) {
	if err != nil {
		reason = err.Error()
	}
	o.unread = append(o.unread, object+": "+reason)
}
