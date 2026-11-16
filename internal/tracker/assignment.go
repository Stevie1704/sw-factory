package tracker

import "context"

// RepositoryAssignment is the stable identity of the code repository that a
// work item is explicitly assigned to, validated at claim (ADR 0030). The
// coordinator freezes it with the run, so a later tag edit cannot redirect
// active work.
type RepositoryAssignment struct {
	// Organization names the tracker organization.
	Organization string
	// ProjectID and Project identify the project of the repository.
	ProjectID string
	Project   string
	// RepositoryID is the stable repository identity the assignment names,
	// and Repository is its name at claim time.
	RepositoryID string
	Repository   string
}

// RepositoryAssignmentValidator is the optional claim port of a tracker
// whose work items must name their target repository explicitly.
type RepositoryAssignmentValidator interface {
	// ClaimRepositoryAssignment validates the assignment of issue to the
	// registered repository right before a claim. It returns nil without an
	// error when the tracker's route needs no assignment, and an error that
	// names the repair for a missing, malformed, multiple, or mismatched
	// assignment. It never grants agent-ready authorization.
	ClaimRepositoryAssignment(context.Context, Repository, Issue) (*RepositoryAssignment, error)
}
