// Package tracker is the factory-owned port to the work tracker: the system
// that holds the issues the factory claims, their run-state labels, the
// status and command comments, and the coordinator lease. Adapters such as
// internal/github implement these interfaces; the coordinator depends only on
// this package.
//
// Field names are part of the stored pending-effect payloads, which encode
// these types as JSON without tags. Renaming a field is a stored-data change.
package tracker

import (
	"context"
	"time"
)

// Repository identifies the registered repository that the tracker and the
// code host serve.
type Repository struct {
	Owner string
	Name  string
}

// String returns the owner/name form of the repository.
func (r Repository) String() string { return r.Owner + "/" + r.Name }

// Issue is the content-free issue snapshot needed by a claim.
type Issue struct {
	Number        int
	Title         string
	Body          string
	State         string
	Labels        []string
	IsPullRequest bool
	UpdatedAt     time.Time
}

// IssuePoller lists the repository's open, agent-authorized issue queue.
type IssuePoller interface {
	ListEligibleIssues(context.Context, Repository) ([]Issue, error)
}

// AccountReader resolves the identity of the locally authenticated tracker
// account. It is read-only: the adapter never reads or stores the credential.
type AccountReader interface {
	AuthenticatedLogin(context.Context) (string, error)
}
