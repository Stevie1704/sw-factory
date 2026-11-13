package tracker

import (
	"context"

	"github.com/Stevie1704/sw-factory/internal/localissues"
)

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
