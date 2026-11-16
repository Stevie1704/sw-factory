package azuredevops_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/azuredevops"
	"github.com/Stevie1704/sw-factory/internal/azuredevops/azuredevopstest"
)

// registeredIdentity is the stable identity of the scripted registered
// repository.
var registeredIdentity = azuredevops.RepositoryIdentity{Organization: "contoso", ProjectID: azuredevopstest.ProjectID, Project: "Factory Pilot", ID: azuredevopstest.RepositoryID, Name: "service"}

// webIdentity is the other scripted repository of the registered project.
var webIdentity = azuredevops.RepositoryIdentity{Organization: "contoso", ProjectID: azuredevopstest.ProjectID, Project: "Factory Pilot", ID: azuredevopstest.WebRepositoryID, Name: "web"}

// unknownRepositoryID is a well-formed GUID of no repository of the
// project.
const unknownRepositoryID = "0f0e0d0c-0b0a-4908-8706-050403020100"

// projectRepositories lists the two scripted repositories of the project.
func projectRepositories() ([]azuredevops.RepositoryIdentity, error) {
	return []azuredevops.RepositoryIdentity{registeredIdentity, webIdentity}, nil
}

// unlistedRepositories fails the test when validation lists the project's
// repositories although the tags do not need it.
func unlistedRepositories(t *testing.T) func() ([]azuredevops.RepositoryIdentity, error) {
	return func() ([]azuredevops.RepositoryIdentity, error) {
		t.Error("the project's repositories were listed, want no list")
		return nil, errors.New("not listed")
	}
}

// TestRepositoryAssignmentTagNamesTheLowercaseGUID verifies the tag form.
func TestRepositoryAssignmentTagNamesTheLowercaseGUID(t *testing.T) {
	t.Parallel()

	got := azuredevops.RepositoryAssignmentTag(strings.ToUpper(azuredevopstest.RepositoryID))
	if got != "factory-repo:"+azuredevopstest.RepositoryID {
		t.Fatalf("RepositoryAssignmentTag() = %q", got)
	}
}

// TestValidateRepositoryAssignmentAcceptsExactlyTheRegisteredRepository
// verifies that one assignment tag for the registered repository is
// accepted beside other tags, in any letter case.
func TestValidateRepositoryAssignmentAcceptsExactlyTheRegisteredRepository(t *testing.T) {
	t.Parallel()

	labels := []string{"agent-ready", "factory-plan:session/1/task-a", "Factory-Repo:" + strings.ToUpper(azuredevopstest.RepositoryID), "team-x"}
	if err := azuredevops.ValidateRepositoryAssignment(labels, registeredIdentity, unlistedRepositories(t)); err != nil {
		t.Fatalf("ValidateRepositoryAssignment() error = %v", err)
	}
}

// TestValidateRepositoryAssignmentRefusesEveryInvalidAssignment verifies
// that a missing, malformed, multiple, unknown, or mismatched assignment is
// refused with its problem and an actionable diagnostic that names the tag
// to set and, for a well-formed foreign GUID, what that GUID is.
func TestValidateRepositoryAssignmentRefusesEveryInvalidAssignment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		labels  []string
		problem azuredevops.AssignmentProblem
		// names is a further text that the diagnostic must contain.
		names string
	}{
		{name: "missing", labels: []string{"agent-ready"}, problem: azuredevops.AssignmentMissing},
		{name: "malformed", labels: []string{"factory-repo:service"}, problem: azuredevops.AssignmentMalformed},
		{name: "empty", labels: []string{"factory-repo:"}, problem: azuredevops.AssignmentMalformed},
		{name: "braced", labels: []string{"factory-repo:{" + azuredevopstest.RepositoryID + "}"}, problem: azuredevops.AssignmentMalformed},
		{name: "multiple", labels: []string{"factory-repo:" + azuredevopstest.RepositoryID, "factory-repo:" + azuredevopstest.WebRepositoryID}, problem: azuredevops.AssignmentMultiple},
		{name: "repeated", labels: []string{"factory-repo:" + azuredevopstest.RepositoryID, "factory-repo:" + azuredevopstest.RepositoryID}, problem: azuredevops.AssignmentMultiple},
		{name: "mismatched", labels: []string{"factory-repo:" + azuredevopstest.WebRepositoryID}, problem: azuredevops.AssignmentMismatched, names: "repository web of project Factory Pilot"},
		{name: "unknown", labels: []string{"factory-repo:" + unknownRepositoryID}, problem: azuredevops.AssignmentUnknown, names: "no repository of project Factory Pilot"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := azuredevops.ValidateRepositoryAssignment(tc.labels, registeredIdentity, projectRepositories)
			var assignment *azuredevops.AssignmentError
			if !errors.As(err, &assignment) || assignment.Problem != tc.problem {
				t.Fatalf("ValidateRepositoryAssignment() error = %v, want problem %q", err, tc.problem)
			}
			if !strings.Contains(err.Error(), "factory-repo:"+azuredevopstest.RepositoryID) || !strings.Contains(err.Error(), "service") {
				t.Fatalf("error = %q, want the expected tag and repository name", err)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Fatalf("error = %q, want %q", err, tc.names)
			}
		})
	}
}

// TestValidateRepositoryAssignmentRefusesAnUnresolvedRepository verifies
// that validation never passes against an identity without a GUID.
func TestValidateRepositoryAssignmentRefusesAnUnresolvedRepository(t *testing.T) {
	t.Parallel()

	if err := azuredevops.ValidateRepositoryAssignment([]string{"factory-repo:"}, azuredevops.RepositoryIdentity{Name: "service"}, projectRepositories); err == nil {
		t.Fatal("ValidateRepositoryAssignment() error = nil for an unresolved repository")
	}
}

// TestValidateRepositoryAssignmentFailsClosedWithoutAUsableRepositoryList
// verifies that a foreign GUID is still refused when the project's
// repositories cannot be listed, or when the list does not hold the
// registered repository, but is then classified neither unknown nor
// mismatched.
func TestValidateRepositoryAssignmentFailsClosedWithoutAUsableRepositoryList(t *testing.T) {
	t.Parallel()

	for name, list := range map[string]func() ([]azuredevops.RepositoryIdentity, error){
		"list failure": func() ([]azuredevops.RepositoryIdentity, error) { return nil, errors.New("az repos list: timeout") },
		"registered unlisted": func() ([]azuredevops.RepositoryIdentity, error) {
			return []azuredevops.RepositoryIdentity{webIdentity}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := azuredevops.ValidateRepositoryAssignment([]string{"factory-repo:" + unknownRepositoryID}, registeredIdentity, list)
			var assignment *azuredevops.AssignmentError
			if err == nil || errors.As(err, &assignment) {
				t.Fatalf("ValidateRepositoryAssignment() error = %v, want an unclassified refusal", err)
			}
			if !strings.Contains(err.Error(), "factory-repo:"+azuredevopstest.RepositoryID) {
				t.Fatalf("error = %q, want the expected tag", err)
			}
		})
	}
}

// TestRepositoryAssignmentTagIsNotAFactoryStateLabel verifies that the
// assignment tag is metadata: IsRepositoryAssignmentTag recognizes it so
// state-tag updates can preserve it.
func TestRepositoryAssignmentTagIsNotAFactoryStateLabel(t *testing.T) {
	t.Parallel()

	if !azuredevops.IsRepositoryAssignmentTag("FACTORY-REPO:x") || azuredevops.IsRepositoryAssignmentTag("factory-plan:x") {
		t.Fatal("IsRepositoryAssignmentTag() does not recognize exactly the factory-repo prefix")
	}
}
