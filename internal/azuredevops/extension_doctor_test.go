package azuredevops_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/azuredevops"
	"github.com/Stevie1704/sw-factory/internal/azuredevops/azuredevopstest"
	"github.com/Stevie1704/sw-factory/internal/doctor"
)

// failingRunner fails every command with one error, as the production
// extension runner would.
type failingRunner struct{ err error }

// Run returns the configured error.
func (r failingRunner) Run(context.Context, []string) ([]byte, error) { return nil, r.err }

// extensionChecks runs the tracker startup checks of the extension route.
func extensionChecks(runner azuredevops.CommandRunner) doctor.Report {
	return doctor.Run(context.Background(), extensionClient(runner).StartupChecks(repository)...)
}

// TestExtensionStartupChecksValidateIdentityAndPermissionsReadOnly
// verifies that a working az devops login passes the read-only checks with
// az repos show and az boards query only, and that the identity and
// permission checks are reported as not available on this route instead of
// passing.
func TestExtensionStartupChecksValidateIdentityAndPermissionsReadOnly(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).
		on("repos show", repositoryShow("service", azuredevopstest.RepositoryID)).
		on(routeQuery, "").
		on("factory-devops identity", `{"login":"factory@contoso.com"}`).
		on("factory-devops permission", `{"granted":true}`).
		on("devops invoke GET wit/classificationNodes", `{"identifier":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}`)
	report := extensionChecks(az)
	names := []string{}
	for _, result := range report.Results {
		names = append(names, result.Name+"="+string(result.Status))
	}
	want := "azure devops authentication=passed,azure devops repository=passed,azure devops boards=passed,azure devops identity=passed,azure devops permissions=passed"
	if strings.Join(names, ",") != want {
		t.Fatalf("results = %v, want %s", names, want)
	}
	if !report.Ready() || !report.Complete() {
		t.Fatalf("report ready=%t complete=%t, want ready and complete", report.Ready(), report.Complete())
	}
	if len(az.calls) != 0 {
		t.Fatalf("az rest calls = %d, want none", len(az.calls))
	}
	for _, call := range az.extensionCalls {
		if route := call.Route(); route != "repos show" && route != routeQuery && route != "factory-devops identity" && route != "factory-devops permission" && route != "devops invoke GET wit/classificationNodes" {
			t.Fatalf("doctor ran %s, want only read-only commands", route)
		}
	}
}

// TestExtensionStartupChecksDiagnoseLoginAndExtensionDistinctly verifies
// the authentication diagnoses: a missing or expired login asks for az
// devops login, a missing extension asks for its installation, and neither
// ever asks for az login.
func TestExtensionStartupChecksDiagnoseLoginAndExtensionDistinctly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		err    error
		code   string
		action string
	}{
		{name: "login", err: &azuredevops.LoginRequiredError{OrganizationURL: "https://dev.azure.com/contoso"}, code: "azure_devops.authentication.failed", action: "az devops login --organization https://dev.azure.com/contoso"},
		{name: "extension", err: &azuredevops.ExtensionMissingError{}, code: "azure_devops.extension.missing", action: "az extension add --name azure-devops"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			report := extensionChecks(failingRunner{err: tc.err})
			first := report.Results[0]
			if first.Status != doctor.StatusFailed || first.Code != tc.code || !strings.Contains(first.Action, tc.action) || strings.Contains(first.Action, "az login ") {
				t.Fatalf("authentication = %#v, want %s with %q", first, tc.code, tc.action)
			}
			if report.Ready() {
				t.Fatal("report is ready without a usable sign-in")
			}
		})
	}
}

// TestExtensionStartupChecksWarnAboutUnassignedAgentReadyWorkItems verifies
// that agent-ready PBIs without a usable assignment, which the queue never
// lists, are named with the tag to add, including PBIs whose tag names an
// unknown GUID, while PBIs assigned to another repository of the project
// are not. The project's repositories are listed once per check.
func TestExtensionStartupChecksWarnAboutUnassignedAgentReadyWorkItems(t *testing.T) {
	t.Parallel()

	webTag := "factory-repo:" + azuredevopstest.WebRepositoryID
	unknownTag := "factory-repo:" + unknownRepositoryID
	az := newFakeAz(t).
		on("repos show", repositoryShow("service", azuredevopstest.RepositoryID)).
		on(routeReposList, projectRepositoryList).
		on(routeQuery, `[{"id":3,"fields":{"System.Id":3,"System.Tags":"agent-ready; `+assignedTag+`"}},
			{"id":5,"fields":{"System.Id":5,"System.Tags":"agent-ready; `+webTag+`"}},
			{"id":7,"fields":{"System.Id":7,"System.Tags":"agent-ready"}},
			{"id":9,"fields":{"System.Id":9,"System.Tags":"agent-ready; factory-repo:service"}},
			{"id":11,"fields":{"System.Id":11,"System.Tags":"agent-ready; `+unknownTag+`"}},
			{"id":13,"fields":{"System.Id":13,"System.Tags":"agent-ready; `+unknownTag+`"}}]`)
	boards := extensionChecks(az).Results[2]
	if boards.Status != doctor.StatusWarning || !strings.Contains(boards.Problem, "7, 9, 11, 13") || strings.Contains(boards.Problem, "5") || !strings.Contains(boards.Action, assignedTag) {
		t.Fatalf("boards = %#v, want a warning naming work items 7, 9, 11, and 13 and the tag to add", boards)
	}
	if listed := az.extensionCalled(routeReposList); len(listed) != 1 || listed[0].Flag("--project") != "Factory Pilot" {
		t.Fatalf("repos list calls = %+v, want one for project Factory Pilot", listed)
	}
	if wiql := az.extensionCalled(routeQuery)[0].Flag("--wiql"); !strings.Contains(wiql, "[System.Tags]") || !strings.HasPrefix(wiql, "SELECT [System.Id], [System.Tags]") {
		t.Fatalf("doctor WIQL = %q, want the tags selected", wiql)
	}
}

// TestExtensionStartupChecksDoNotGuessForeignAssignmentsWithoutARepositoryList
// verifies that, when the project's repositories cannot be listed, the
// doctor still warns about missing and malformed assignments but does not
// report a PBI whose tag names another GUID, because that GUID may be
// another repository of the project.
func TestExtensionStartupChecksDoNotGuessForeignAssignmentsWithoutARepositoryList(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).
		on("repos show", repositoryShow("service", azuredevopstest.RepositoryID)).
		on(routeReposList, "error:az repos list: timeout").
		on(routeQuery, `[{"id":7,"fields":{"System.Id":7,"System.Tags":"agent-ready"}},
			{"id":11,"fields":{"System.Id":11,"System.Tags":"agent-ready; factory-repo:`+unknownRepositoryID+`"}}]`)
	boards := extensionChecks(az).Results[2]
	if boards.Status != doctor.StatusWarning || !strings.Contains(boards.Problem, "items 7 have") || strings.Contains(boards.Problem, "11") {
		t.Fatalf("boards = %#v, want a warning naming only work item 7", boards)
	}
}

// TestExtensionStartupChecksReportAMissingRepositoryAfterASignIn verifies
// that an Azure DevOps refusal for the repository proves a working sign-in
// and fails only the repository check.
func TestExtensionStartupChecksReportAMissingRepositoryAfterASignIn(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).
		on("repos show", "error:az repos show: ERROR: TF401019: The Git repository with name or identifier service does not exist or you do not have permissions for the operation you are attempting.").
		on(routeQuery, "")
	report := extensionChecks(az)
	if report.Results[0].Status != doctor.StatusPassed || report.Results[1].Status != doctor.StatusFailed || !strings.Contains(report.Results[1].Problem, "contoso/Factory Pilot/service") {
		t.Fatalf("results = %#v", report.Results[:2])
	}
}
