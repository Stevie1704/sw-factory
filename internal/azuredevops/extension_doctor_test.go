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

// TestExtensionStartupChecksPassReadOnlyChecksAndReportTheRestUnavailable
// verifies that a working az devops login passes the read-only checks with
// az repos show and az boards query only, and that the identity and
// permission checks are reported as not available on this route instead of
// passing.
func TestExtensionStartupChecksPassReadOnlyChecksAndReportTheRestUnavailable(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).
		on("repos show", repositoryShow("service", azuredevopstest.RepositoryID)).
		on(routeQuery, "")
	report := extensionChecks(az)
	names := []string{}
	for _, result := range report.Results {
		names = append(names, result.Name+"="+string(result.Status))
	}
	want := "azure devops authentication=passed,azure devops repository=passed,azure devops boards=passed,azure devops identity=skipped,azure devops permissions=skipped"
	if strings.Join(names, ",") != want {
		t.Fatalf("results = %v, want %s", names, want)
	}
	if !report.Ready() || report.Complete() {
		t.Fatalf("report ready=%t complete=%t, want ready and incomplete", report.Ready(), report.Complete())
	}
	for _, result := range report.Results[3:] {
		if !strings.Contains(result.Problem, "devops_extension") {
			t.Fatalf("%s problem = %q, want the route named", result.Name, result.Problem)
		}
	}
	if len(az.calls) != 0 {
		t.Fatalf("az rest calls = %d, want none", len(az.calls))
	}
	for _, call := range az.extensionCalls {
		if route := call.Route(); route != "repos show" && route != routeQuery {
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
