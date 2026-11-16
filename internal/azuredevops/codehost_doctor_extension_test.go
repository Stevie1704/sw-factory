package azuredevops_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/azuredevops"
	"github.com/Stevie1704/sw-factory/internal/azuredevops/azuredevopstest"
	"github.com/Stevie1704/sw-factory/internal/doctor"
)

// readyRepository is the az repos show output of the registered repository.
const readyRepository = `{"id":"` + azuredevopstest.RepositoryID + `","name":"service","defaultBranch":"refs/heads/main","project":{"id":"` + azuredevopstest.ProjectID + `","name":"Factory Pilot"}}`

// readyCodeHost scripts the read-only code-host probes of a signed-in
// extension route.
func readyCodeHost(t *testing.T) *fakeAz {
	t.Helper()
	return newFakeAz(t).
		on("repos show", readyRepository).
		on("repos pr list", `[]`).
		on("repos ref list", `[{"name":"refs/heads/main","objectId":"`+strings.Repeat("d", 40)+`"}]`).
		on("devops invoke GET git/statuses", `{"count":0,"value":[]}`)
}

// TestExtensionCodeHostChecksPassReadOnlyProbesWithoutBoards verifies that
// the code-host diagnosis on the extension route proves the az devops login
// and the PR, ref, and status reads with Azure Repos commands only, never
// touches Boards or az rest, and reports the unverifiable write permissions
// as a non-blocking warning instead of a pass.
func TestExtensionCodeHostChecksPassReadOnlyProbesWithoutBoards(t *testing.T) {
	t.Parallel()

	az := readyCodeHost(t)
	report := doctor.Run(context.Background(), extensionClient(az).CodeHostStartupChecks(repository)...)
	if !report.Ready() {
		t.Fatalf("report = %#v, want ready", report.Results)
	}
	statuses := make([]doctor.Status, 0)
	for _, result := range report.Results {
		statuses = append(statuses, result.Status)
	}
	if len(statuses) != 3 || statuses[0] != doctor.StatusPassed || statuses[1] != doctor.StatusPassed || statuses[2] != doctor.StatusWarning {
		t.Fatalf("statuses = %v, want authentication and access passed and permissions warned", statuses)
	}
	if warning := report.Results[2]; !strings.Contains(warning.Problem, "not verified") || strings.Contains(warning.Action, "az login ") {
		t.Fatalf("permission result = %#v, want an unverified-permission warning without az login", warning)
	}
	if len(az.calls) != 0 {
		t.Fatalf("az rest calls = %d, want none", len(az.calls))
	}
	for _, call := range az.extensionCalls {
		if call.Command[0] == "boards" || strings.Contains(call.Route(), "wit/") {
			t.Fatalf("call %s needs Boards access", call.Route())
		}
	}
	ref := az.extensionCalls[2]
	if ref.Route() != "repos ref list" || ref.Flag("--filter") != "heads/main" || ref.Flag("--repository") != "service" {
		t.Fatalf("ref call = %+v, want the default branch of the registered repository", ref.Flags)
	}
	status := az.extensionCalls[3]
	if status.Parameters("--route-parameters")["commitId"] != strings.Repeat("d", 40) {
		t.Fatalf("status call = %+v, want the default branch commit", status.Flags)
	}
}

// loginRequiredRunner fails every command as a missing az devops login.
type loginRequiredRunner struct{}

// Run returns the extension route's sign-in error.
func (loginRequiredRunner) Run(context.Context, []string) ([]byte, error) {
	return nil, &azuredevops.LoginRequiredError{OrganizationURL: "https://dev.azure.com/contoso"}
}

// TestExtensionCodeHostChecksDirectAMissingLoginToAzDevOpsLogin verifies
// that a missing or expired PAT login blocks startup with the az devops
// login repair, never an az login one.
func TestExtensionCodeHostChecksDirectAMissingLoginToAzDevOpsLogin(t *testing.T) {
	t.Parallel()

	report := doctor.Run(context.Background(), extensionClient(loginRequiredRunner{}).CodeHostStartupChecks(repository)...)
	if report.Ready() {
		t.Fatal("report is ready without a login")
	}
	first := report.Results[0]
	if first.Status != doctor.StatusFailed || !strings.Contains(first.Action, "az devops login --organization https://dev.azure.com/contoso") || strings.Contains(first.Action, "az login ") {
		t.Fatalf("authentication = %#v, want the az devops login repair", first)
	}
}

// TestExtensionCodeHostChecksFailABlockedProbe verifies that a failed PR,
// ref, or status read blocks startup.
func TestExtensionCodeHostChecksFailABlockedProbe(t *testing.T) {
	t.Parallel()

	for _, route := range []string{"repos pr list", "repos ref list", "devops invoke GET git/statuses"} {
		az := readyCodeHost(t)
		az.routes[route] = []string{"error:ERROR: TF401027: You need the Git 'PullRequestContribute' permission"}
		report := doctor.Run(context.Background(), extensionClient(az).CodeHostStartupChecks(repository)...)
		if report.Ready() || report.Results[1].Status != doctor.StatusFailed {
			t.Fatalf("%s blocked: results = %#v, want a failed access check", route, report.Results)
		}
	}
}
