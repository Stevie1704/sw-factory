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

	"github.com/Stevie1704/sw-factory/internal/doctor"
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

const (
	// LabelAgentReady authorizes an issue for a factory claim.
	LabelAgentReady = "agent-ready"
	// LabelAgentRunning marks an active factory run.
	LabelAgentRunning = "agent-running"
	// LabelAgentNeedsInput marks a run waiting for human input.
	LabelAgentNeedsInput = "agent-needs-input"
	// LabelAgentFailed marks a failed factory run.
	LabelAgentFailed = "agent-failed"
	// LabelAgentCancelled marks a cancelled factory run.
	LabelAgentCancelled = "agent-cancelled"
	// LabelAgentComplete marks a completed factory run.
	LabelAgentComplete = "agent-complete"
)

// FactoryStateLabels is the complete set of labels owned by the factory.
// Claiming and transitioning only replace labels from this set; ordinary issue
// labels are preserved.
var FactoryStateLabels = []string{
	LabelAgentReady,
	LabelAgentRunning,
	LabelAgentNeedsInput,
	LabelAgentFailed,
	LabelAgentCancelled,
	LabelAgentComplete,
}

// Label describes a factory-owned run-state label.
type Label struct {
	Name        string
	Description string
	Color       string
}

// Comment is the identity and revision of an issue or pull-request comment.
type Comment struct {
	// ID is the immutable comment identity used as a replay watermark.
	ID string
	// Body is the complete user-authored comment text.
	Body string
	// Author is the tracker login that authored the comment.
	Author string
	// UpdatedAt is the current edit revision of the comment.
	UpdatedAt time.Time
}

// CommentReader lists the comments of an issue or pull request. The command
// stream of a run is read through it.
type CommentReader interface {
	IssueComments(context.Context, Repository, int) ([]Comment, error)
}

// Client is the issue, label, and comment seam used by the claim coordinator.
// It keeps tracker credentials inside the adapter and returns only workflow
// data to the coordinator.
type Client interface {
	Issue(context.Context, Repository, int) (Issue, error)
	CreateLabel(context.Context, Repository, Label) error
	ReplaceIssueLabels(context.Context, Repository, int, []string) error
	CreateIssueComment(context.Context, Repository, int, string) (Comment, error)
	FindStatusComment(context.Context, Repository, int, string) (Comment, error)
	EditIssueComment(context.Context, Repository, string, string) error
}

// Lease describes one visible coordinator ownership heartbeat.
type Lease struct {
	// Coordinator identifies the host holding the lease.
	Coordinator string
	// RunID identifies the active run, when one has been claimed.
	RunID string
	// RenewedAt is the coordinator's latest heartbeat time.
	RenewedAt time.Time
	// ExpiresAt is the time after which the lease projection is stale.
	ExpiresAt time.Time
}

// LeaseClient publishes a renewable, operator-visible coordinator lease. It is
// an optional adapter capability: the host lock owns the repository, and the
// lease is only a diagnostic projection (ADR 0018).
type LeaseClient interface {
	RenewLease(context.Context, Repository, Lease) error
}

// ReadinessChecker reports whether an adapter can serve the registered
// repository, for example its authentication and permissions. Each adapter
// owns its own checks, so doctor needs no provider knowledge.
type ReadinessChecker interface {
	StartupChecks(Repository) []doctor.Check
}
