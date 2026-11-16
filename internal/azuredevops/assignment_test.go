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
	if err := azuredevops.ValidateRepositoryAssignment(labels, registeredIdentity); err != nil {
		t.Fatalf("ValidateRepositoryAssignment() error = %v", err)
	}
}

// TestValidateRepositoryAssignmentRefusesEveryInvalidAssignment verifies
// that a missing, malformed, multiple, or mismatched assignment is refused
// with its problem and an actionable diagnostic that names the tag to set.
func TestValidateRepositoryAssignmentRefusesEveryInvalidAssignment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		labels  []string
		problem azuredevops.AssignmentProblem
	}{
		{name: "missing", labels: []string{"agent-ready"}, problem: azuredevops.AssignmentMissing},
		{name: "malformed", labels: []string{"factory-repo:service"}, problem: azuredevops.AssignmentMalformed},
		{name: "empty", labels: []string{"factory-repo:"}, problem: azuredevops.AssignmentMalformed},
		{name: "braced", labels: []string{"factory-repo:{" + azuredevopstest.RepositoryID + "}"}, problem: azuredevops.AssignmentMalformed},
		{name: "multiple", labels: []string{"factory-repo:" + azuredevopstest.RepositoryID, "factory-repo:" + azuredevopstest.WebRepositoryID}, problem: azuredevops.AssignmentMultiple},
		{name: "repeated", labels: []string{"factory-repo:" + azuredevopstest.RepositoryID, "factory-repo:" + azuredevopstest.RepositoryID}, problem: azuredevops.AssignmentMultiple},
		{name: "mismatched", labels: []string{"factory-repo:" + azuredevopstest.WebRepositoryID}, problem: azuredevops.AssignmentMismatched},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := azuredevops.ValidateRepositoryAssignment(tc.labels, registeredIdentity)
			var assignment *azuredevops.AssignmentError
			if !errors.As(err, &assignment) || assignment.Problem != tc.problem {
				t.Fatalf("ValidateRepositoryAssignment() error = %v, want problem %q", err, tc.problem)
			}
			if !strings.Contains(err.Error(), "factory-repo:"+azuredevopstest.RepositoryID) || !strings.Contains(err.Error(), "service") {
				t.Fatalf("error = %q, want the expected tag and repository name", err)
			}
		})
	}
}

// TestValidateRepositoryAssignmentRefusesAnUnresolvedRepository verifies
// that validation never passes against an identity without a GUID.
func TestValidateRepositoryAssignmentRefusesAnUnresolvedRepository(t *testing.T) {
	t.Parallel()

	if err := azuredevops.ValidateRepositoryAssignment([]string{"factory-repo:"}, azuredevops.RepositoryIdentity{Name: "service"}); err == nil {
		t.Fatal("ValidateRepositoryAssignment() error = nil for an unresolved repository")
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
