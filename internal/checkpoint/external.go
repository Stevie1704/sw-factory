package checkpoint

import (
	"context"
	"fmt"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// ExternalState is the observed state of every external object that a
// snapshot knows. Restore compares each value and refuses on a difference.
type ExternalState struct {
	// TargetBranch is the remote head of the target branch.
	TargetBranch BranchState `json:"target_branch"`
	// Branches holds each known run branch, in run order.
	Branches []BranchState `json:"branches"`
	// PullRequests holds each known pull request, in run order.
	PullRequests []PullRequestState `json:"pull_requests"`
	// Issues holds each known issue, in run order.
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
	// PullRequest returns the pull request with number, whose head is
	// headBranch.
	PullRequest(ctx context.Context, number int, headBranch string) (PullRequestState, error)
	// Issue returns the open state and the factory state labels of issue
	// number.
	Issue(ctx context.Context, number int) (IssueState, error)
	// StatusComment returns the status comment of run runID on issue
	// issueNumber.
	StatusComment(ctx context.Context, issueNumber int, runID string) (StatusCommentState, error)
}

// ReadExternalState reads the state of the target branch and of every
// external object that runs identify. It returns one detail for each
// object that it cannot read; a checkpoint with any such detail is refused.
func ReadExternalState(ctx context.Context, reader ExternalReader, targetBranch string, runs []store.KnownRun) (ExternalState, []string) {
	observer := externalObserver{reader: reader, seenBranches: map[string]bool{}, seenIssues: map[int]bool{}}
	state := ExternalState{Branches: []BranchState{}, PullRequests: []PullRequestState{}, Issues: []IssueState{}, StatusComments: []StatusCommentState{}}
	state.TargetBranch = BranchState{Name: targetBranch}
	if head, err := reader.BranchHead(ctx, targetBranch); err != nil || head == "" {
		observer.unreadable(fmt.Sprintf("target branch %s", targetBranch), err, "the remote has no such branch")
	} else {
		state.TargetBranch.Head = head
	}
	for _, run := range runs {
		observer.observeRun(ctx, run, &state)
	}
	return state, observer.unread
}

// externalObserver collects the observed state and the unread objects.
type externalObserver struct {
	reader       ExternalReader
	seenBranches map[string]bool
	seenIssues   map[int]bool
	unread       []string
}

// observeRun reads each external object that one run identifies, once per
// object.
func (o *externalObserver) observeRun(ctx context.Context, run store.KnownRun, state *ExternalState) {
	if run.Branch != "" && !o.seenBranches[run.Branch] {
		o.seenBranches[run.Branch] = true
		head, err := o.reader.BranchHead(ctx, run.Branch)
		if err != nil {
			o.unreadable("branch "+run.Branch, err, "")
		} else {
			state.Branches = append(state.Branches, BranchState{Name: run.Branch, Head: head})
		}
	}
	if run.PullRequestNumber > 0 {
		pullRequest, err := o.reader.PullRequest(ctx, run.PullRequestNumber, run.Branch)
		if err != nil || pullRequest.Number != run.PullRequestNumber {
			o.unreadable(fmt.Sprintf("pull request #%d", run.PullRequestNumber), err, "the code host does not show it")
		} else {
			state.PullRequests = append(state.PullRequests, pullRequest)
		}
	}
	if run.IssueNumber > 0 && !o.seenIssues[run.IssueNumber] {
		o.seenIssues[run.IssueNumber] = true
		issue, err := o.reader.Issue(ctx, run.IssueNumber)
		if err != nil || issue.Number != run.IssueNumber {
			o.unreadable(fmt.Sprintf("issue #%d", run.IssueNumber), err, "the tracker does not show it")
		} else {
			state.Issues = append(state.Issues, issue)
		}
	}
	if run.StatusCommentID != "" {
		comment, err := o.reader.StatusComment(ctx, run.IssueNumber, run.RunID)
		if err != nil || comment.ID != run.StatusCommentID {
			o.unreadable(fmt.Sprintf("status comment %s of run %s", run.StatusCommentID, run.RunID), err, "the tracker does not show it")
		} else {
			state.StatusComments = append(state.StatusComments, comment)
		}
	}
}

// unreadable records one object that could not be read, with the read
// error or, without one, the reason.
func (o *externalObserver) unreadable(object string, err error, reason string) {
	if err != nil {
		reason = err.Error()
	}
	o.unread = append(o.unread, object+": "+reason)
}
