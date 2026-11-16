package azuredevops

import (
	"errors"
	"fmt"
	"strings"
)

// repositoryTagPrefix starts the Factory-managed repository assignment tag
// factory-repo:<repository GUID> of an executable Azure PBI (ADR 0030).
// Azure Boards work items belong to a project, so the tag is a Factory
// convention, not native repository ownership.
const repositoryTagPrefix = "factory-repo:"

// RepositoryAssignmentTag returns the assignment tag for one repository
// GUID, in lowercase.
func RepositoryAssignmentTag(repositoryID string) string {
	return repositoryTagPrefix + strings.ToLower(strings.TrimSpace(repositoryID))
}

// IsRepositoryAssignmentTag reports whether tag is a repository assignment
// tag, well formed or not. Tag updates must preserve such tags.
func IsRepositoryAssignmentTag(tag string) bool {
	return len(tag) >= len(repositoryTagPrefix) && strings.EqualFold(tag[:len(repositoryTagPrefix)], repositoryTagPrefix)
}

// AssignmentProblem classifies a refused repository assignment.
type AssignmentProblem string

const (
	// AssignmentMissing means the work item has no assignment tag.
	AssignmentMissing AssignmentProblem = "missing"
	// AssignmentMalformed means an assignment tag does not name a GUID.
	AssignmentMalformed AssignmentProblem = "malformed"
	// AssignmentMultiple means the work item has more than one assignment
	// tag.
	AssignmentMultiple AssignmentProblem = "multiple"
	// AssignmentMismatched means the tag names a repository other than the
	// registered one, for example a repository of another installation or
	// an unknown GUID.
	AssignmentMismatched AssignmentProblem = "mismatched"
)

// AssignmentError is a refused repository assignment. Its message tells the
// operator which tag assigns the work item to the registered repository.
type AssignmentError struct {
	// Problem is the refusal class.
	Problem AssignmentProblem
	// Expected is the registered repository.
	Expected RepositoryIdentity
	// Found lists the assignment tags of the work item.
	Found []string
}

// Error describes the refusal and the repair.
func (e *AssignmentError) Error() string {
	want := RepositoryAssignmentTag(e.Expected.ID)
	switch e.Problem {
	case AssignmentMissing:
		return fmt.Sprintf("the work item has no repository assignment; add the tag %s to assign it to repository %s", want, e.Expected.Name)
	case AssignmentMultiple:
		return fmt.Sprintf("the work item has %d repository assignment tags (%s); keep exactly one, %s for repository %s", len(e.Found), strings.Join(e.Found, ", "), want, e.Expected.Name)
	case AssignmentMalformed:
		return fmt.Sprintf("the repository assignment tag %s does not name a repository GUID; use %s for repository %s", strings.Join(e.Found, ", "), want, e.Expected.Name)
	default:
		return fmt.Sprintf("the work item is assigned by %s to another repository, not to %s (%s); another installation owns it", strings.Join(e.Found, ", "), e.Expected.Name, want)
	}
}

// ValidateRepositoryAssignment accepts labels only when they hold exactly
// one well-formed assignment tag that names the expected repository GUID.
// It is the authoritative check before a claim; query filtering is only an
// optimization.
func ValidateRepositoryAssignment(labels []string, expected RepositoryIdentity) error {
	if !canonicalGUID.MatchString(expected.ID) {
		return errors.New("the registered Azure Repos repository has no resolved GUID")
	}
	found := []string{}
	for _, label := range labels {
		if IsRepositoryAssignmentTag(strings.TrimSpace(label)) {
			found = append(found, strings.TrimSpace(label))
		}
	}
	refuse := func(problem AssignmentProblem) error {
		return &AssignmentError{Problem: problem, Expected: expected, Found: found}
	}
	switch {
	case len(found) == 0:
		return refuse(AssignmentMissing)
	case len(found) > 1:
		return refuse(AssignmentMultiple)
	}
	id := found[0][len(repositoryTagPrefix):]
	if !canonicalGUID.MatchString(id) {
		return refuse(AssignmentMalformed)
	}
	if !strings.EqualFold(id, expected.ID) {
		return refuse(AssignmentMismatched)
	}
	return nil
}
