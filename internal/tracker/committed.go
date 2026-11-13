package tracker

import (
	"context"
	"errors"

	"github.com/Stevie1704/sw-factory/internal/localissues"
)

// ErrInvalidCommittedSource distinguishes an observed malformed graph from a
// transport outage, so stale pending authority can require explicit reapproval.
var ErrInvalidCommittedSource = errors.New("invalid committed issue source")

// CommittedIssue couples accepted source to a durable tracker identity.
type CommittedIssue struct {
	Number int
	Source localissues.Issue
}

// CommittedSnapshot is a fully validated immutable source graph. No field
// represents execution authority; source and claim base must share Revision.
type CommittedSnapshot struct {
	Revision string
	Graph    localissues.Graph
	Issues   []CommittedIssue
}

// CommittedIssueReader exposes immutable Git-backed source without a provider
// dependency in the coordinator. Refresh imports content only; Read is read-only.
type CommittedIssueReader interface {
	RefreshCommittedIssues(context.Context) (CommittedSnapshot, error)
	ReadCommittedIssues(context.Context, string) (CommittedSnapshot, error)
}

// TargetCommittedIssueReader observes current repository policy rather than a
// target captured when an adapter was constructed. Only coordinator calls it.
type TargetCommittedIssueReader interface {
	RefreshCommittedIssuesAt(context.Context, string, string) (CommittedSnapshot, error)
}

// IssueProjectionReader joins a frozen Run's issue content to mutable local
// lifecycle records without consulting changed or deleted accepted source.
type IssueProjectionReader interface {
	ProjectIssue(context.Context, Repository, Issue) (Issue, error)
}
