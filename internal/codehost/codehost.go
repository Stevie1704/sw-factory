// Package codehost is the factory-owned port to the code host: the system
// that holds the run branch's pull request, its human reviews, and the
// checkpoint commit statuses. Adapters such as internal/github implement these
// interfaces; the coordinator depends only on this package.
//
// Field names are part of the stored pending-effect payloads, which encode
// these types as JSON without tags. Renaming a field is a stored-data change.
package codehost

import (
	"context"
	"time"

	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// PullRequest is the pull-request identity and body returned to the
// coordinator after a host-side code-host operation.
type PullRequest struct {
	// Number is the repository-local pull-request number.
	Number int
	// URL is the browser URL for operator supervision.
	URL string
	// Title is the pull-request title.
	Title string
	// Body is the current complete pull-request body.
	Body string
	// State is the code host's lifecycle state of the pull request.
	State string
	// Draft reports whether the code host marks the pull request as a draft.
	Draft bool
	// Merged reports whether the code host has completed the pull request merge.
	Merged bool
	// MergeCommitSHA is the immutable commit created by a successful merge.
	MergeCommitSHA string
	// HeadBranch is the source branch name.
	HeadBranch string
	// HeadSHA is the exact commit currently referenced by the source branch.
	HeadSHA string
	// BaseBranch is the target branch name.
	BaseBranch string
}

// PullRequestRequest contains the stable fields used to create or update a
// draft pull request.
type PullRequestRequest struct {
	// Title is the pull-request title.
	Title string
	// Body is the complete body, including any preserved human-authored text.
	Body string
	// HeadBranch is the source branch to publish.
	HeadBranch string
	// BaseBranch is the target branch to merge into.
	BaseBranch string
	// Draft requests a draft pull request on creation and update.
	Draft bool
}

// PullRequestClient is the host-side seam for idempotent draft pull-request
// discovery and mutation. It is separate from tracker.Client so issue and
// state adapters do not gain pull-request authority accidentally.
type PullRequestClient interface {
	FindPullRequest(context.Context, tracker.Repository, string, string) (PullRequest, error)
	CreatePullRequest(context.Context, tracker.Repository, PullRequestRequest) (PullRequest, error)
	UpdatePullRequest(context.Context, tracker.Repository, int, PullRequestRequest) (PullRequest, error)
}

// PullRequestReviewState is the bounded review decision vocabulary. Each
// adapter maps its provider's review values to it. Only a submitted review
// carries a decision; an unsubmitted draft is `PENDING`.
type PullRequestReviewState string

const (
	// PullRequestReviewPending is an unsubmitted review draft. It is visible
	// only to its author and must never start factory work.
	PullRequestReviewPending PullRequestReviewState = "PENDING"
	// PullRequestReviewCommented is a submitted review without a decision.
	PullRequestReviewCommented PullRequestReviewState = "COMMENTED"
	// PullRequestReviewApproved is a submitted approval.
	PullRequestReviewApproved PullRequestReviewState = "APPROVED"
	// PullRequestReviewChangesRequested is the only review decision the
	// coordinator treats as a progression event.
	PullRequestReviewChangesRequested PullRequestReviewState = "CHANGES_REQUESTED"
	// PullRequestReviewDismissed is a decision a maintainer later withdrew.
	PullRequestReviewDismissed PullRequestReviewState = "DISMISSED"
)

// PullRequestReviewComment is one inline, file-anchored review finding.
type PullRequestReviewComment struct {
	// Path is the repository-relative file the reviewer annotated.
	Path string
	// Line is the annotated line in the file, or zero when the code host reports none.
	Line int
	// Body is the reviewer's complete comment text.
	Body string
}

// PullRequestReview is one completed human review of a tracked pull request.
type PullRequestReview struct {
	// ID is the immutable review identity used as a replay watermark.
	ID string
	// Author is the code-host login that submitted the review.
	Author string
	// State is the submitted review decision.
	State PullRequestReviewState
	// Body is the completed review summary text.
	Body string
	// URL is the browser URL for operator supervision.
	URL string
	// SubmittedAt is the submission time. It is the zero value while the
	// code host still holds the review as an unsubmitted draft.
	SubmittedAt time.Time
	// Comments contains the review's inline findings in code-host order.
	Comments []PullRequestReviewComment
}

// PullRequestReviewReader is the read-only seam for observing completed human
// reviews. It is separate from the mutation clients because the factory never
// submits, approves, dismisses, or merges a review.
type PullRequestReviewReader interface {
	PullRequestReviews(context.Context, tracker.Repository, int) ([]PullRequestReview, error)
}

// PullRequestCommentReader lists the conversation comments of a pull request.
// An adapter whose pull requests share the issue number space and comment
// stream, such as GitHub, does not need it: the coordinator then reads the
// pull request through tracker.CommentReader. Comment identities of both
// surfaces must sort in creation order with the same comparison (ADR 0019).
type PullRequestCommentReader interface {
	PullRequestComments(context.Context, tracker.Repository, int) ([]tracker.Comment, error)
}

// PullRequestCommentClient finds, creates, and edits a coordinator-authored
// pull-request comment, such as clarification questions. It has the same
// optional status as PullRequestCommentReader.
type PullRequestCommentClient interface {
	FindPullRequestComment(context.Context, tracker.Repository, int, string) (tracker.Comment, error)
	CreatePullRequestComment(context.Context, tracker.Repository, int, string) (tracker.Comment, error)
	EditPullRequestComment(context.Context, tracker.Repository, int, string, string) error
}

// PullRequestDraftClient owns the explicit draft/readiness mutation for an
// existing pull request. Keeping it separate prevents body updates from
// accidentally changing merge readiness.
type PullRequestDraftClient interface {
	SetPullRequestDraft(context.Context, tracker.Repository, int, bool) (PullRequest, error)
}

// CommitStatusState is the state vocabulary used for deterministic checkpoint
// results. Each adapter maps it to its provider's status values.
type CommitStatusState string

const (
	// CommitStatusPending is an in-progress status state.
	CommitStatusPending CommitStatusState = "pending"
	// CommitStatusSuccess marks a successful checkpoint result.
	CommitStatusSuccess CommitStatusState = "success"
	// CommitStatusFailure marks a declared command that exited unsuccessfully.
	CommitStatusFailure CommitStatusState = "failure"
	// CommitStatusError marks setup or runtime infrastructure failure.
	CommitStatusError CommitStatusState = "error"
)

// CommitStatus identifies one status attached to one exact commit SHA.
type CommitStatus struct {
	// SHA is the immutable commit being reported.
	SHA string
	// State is the status state.
	State CommitStatusState
	// Context is the stable status context used for repeated reports.
	Context string
	// Description is a content-free human-readable summary.
	Description string
	// TargetURL is an optional operator-facing evidence URL.
	TargetURL string
}

// CommitStatusPublisher is the host-side seam for publishing exact-SHA
// Commit Statuses without exposing code-host credentials to workflow code.
type CommitStatusPublisher interface {
	CreateCommitStatus(context.Context, tracker.Repository, CommitStatus) error
}

// CommitStatusReader is the read-only projection used to recognize a status
// that the code host accepted before the coordinator process stopped.
type CommitStatusReader interface {
	ListCommitStatuses(context.Context, tracker.Repository, string) ([]CommitStatus, error)
}

// ValidCommitSHA reports whether value is a full 40- or 64-character lowercase
// hexadecimal commit SHA while rejecting values that could alter a code-host API path.
func ValidCommitSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') {
			continue
		}
		return false
	}
	return true
}
