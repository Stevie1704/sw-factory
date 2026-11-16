package azuredevops_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/azuredevops"
	"github.com/Stevie1704/sw-factory/internal/azuredevops/azuredevopstest"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// extensionClient returns an extension-route adapter over the fake CLI.
func extensionClient(az azuredevops.CommandRunner) *azuredevops.Client {
	client := azuredevops.NewClientForRoute("contoso", []string{"alice@contoso.com"}, config.AzureRouteDevOpsExtension)
	client.Runner = az
	return client
}

// TestResolveRepositoryReadsTheStableIDWithExplicitTargeting verifies that
// the extension route resolves a registered repository name to its stable
// GUID with az repos show, naming the organization, project, and repository
// explicitly instead of relying on CLI defaults.
func TestResolveRepositoryReadsTheStableIDWithExplicitTargeting(t *testing.T) {
	t.Parallel()

	boards := azuredevopstest.NewBoards(t, azuredevopstest.ScrumProcess())
	client := extensionClient(boards)
	for _, want := range []struct{ name, id string }{{azuredevopstest.Repository, azuredevopstest.RepositoryID}, {azuredevopstest.WebRepository, azuredevopstest.WebRepositoryID}} {
		name, id := want.name, want.id
		identity, err := client.ResolveRepository(context.Background(), tracker.Repository{Owner: "contoso", Project: "Factory Pilot", Name: name})
		if err != nil {
			t.Fatalf("ResolveRepository(%s) error = %v", name, err)
		}
		want := azuredevops.RepositoryIdentity{Organization: "contoso", ProjectID: azuredevopstest.ProjectID, Project: "Factory Pilot", ID: id, Name: name}
		if identity != want {
			t.Fatalf("ResolveRepository(%s) = %+v, want %+v", name, identity, want)
		}
	}
	calls := boards.ExtensionCalls()
	if len(calls) != 2 || len(boards.Calls()) != 0 {
		t.Fatalf("calls = %d extension, %d az rest; want 2 and 0", len(calls), len(boards.Calls()))
	}
	call := calls[0]
	if call.Route() != "repos show" || call.Flag("--organization") != "https://dev.azure.com/contoso" || call.Flag("--project") != "Factory Pilot" || call.Flag("--repository") != "service" {
		t.Fatalf("call = %+v, want an explicitly targeted az repos show", call)
	}
}

// TestResolveRepositoryRefusesAnIdentityOutsideTheRegisteredProject verifies
// that a response for another project, another repository name, or without
// a GUID is never accepted as the registered repository identity.
func TestResolveRepositoryRefusesAnIdentityOutsideTheRegisteredProject(t *testing.T) {
	t.Parallel()

	for name, response := range map[string]string{
		"other project":   `{"id":"3b1f6a2e-8c4d-4e5f-9a6b-7c8d9e0f1a2b","name":"service","project":{"id":"6f9b0c5e-2a41-4d8e-9a43-1c2f5e7d8a90","name":"Other"}}`,
		"other name":      `{"id":"3b1f6a2e-8c4d-4e5f-9a6b-7c8d9e0f1a2b","name":"web","project":{"id":"6f9b0c5e-2a41-4d8e-9a43-1c2f5e7d8a90","name":"Factory Pilot"}}`,
		"malformed ID":    `{"id":"service","name":"service","project":{"id":"6f9b0c5e-2a41-4d8e-9a43-1c2f5e7d8a90","name":"Factory Pilot"}}`,
		"missing project": `{"id":"3b1f6a2e-8c4d-4e5f-9a6b-7c8d9e0f1a2b","name":"service"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			az := newFakeAz(t).on("repos show", response)
			if _, err := extensionClient(az).ResolveRepository(context.Background(), repository); err == nil {
				t.Fatal("ResolveRepository() error = nil, want a refused identity")
			}
		})
	}
}

// TestResolveRepositoryReportsAMissingRepository verifies that an unknown
// repository fails with an error that names it.
func TestResolveRepositoryReportsAMissingRepository(t *testing.T) {
	t.Parallel()

	boards := azuredevopstest.NewBoards(t, azuredevopstest.ScrumProcess())
	_, err := extensionClient(boards).ResolveRepository(context.Background(), tracker.Repository{Owner: "contoso", Project: "Factory Pilot", Name: "missing"})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("ResolveRepository() error = %v, want an error naming the repository", err)
	}
}

// TestExtensionRouteNeverFallsBackToAzRest verifies that an operation not
// yet available on the extension route fails closed without running az rest
// or requesting an Entra ID token.
func TestExtensionRouteNeverFallsBackToAzRest(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t)
	client := extensionClient(az)
	_, err := client.AuthenticatedLogin(context.Background())
	if !errors.Is(err, azuredevops.ErrExtensionOperationUnavailable) {
		t.Fatalf("AuthenticatedLogin() error = %v, want ErrExtensionOperationUnavailable", err)
	}
	if len(az.calls) != 0 || len(az.extensionCalls) != 0 {
		t.Fatalf("calls = %d az rest, %d extension; want none", len(az.calls), len(az.extensionCalls))
	}
}

// TestExtensionRouteStartupChecksNeverDirectToAzLogin verifies that the
// tracker and code-host startup checks on the extension route fail without
// telling the operator to run az login and without running az rest.
func TestExtensionRouteStartupChecksNeverDirectToAzLogin(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t)
	client := extensionClient(az)
	for name, checks := range map[string][]doctor.Check{"tracker": client.StartupChecks(repository), "code host": client.CodeHostStartupChecks(repository)} {
		report := doctor.Run(context.Background(), checks...)
		first := report.Results[0]
		if first.Status != doctor.StatusFailed || strings.Contains(first.Action, "az login") || !strings.Contains(first.Action, "az devops login") {
			t.Fatalf("%s authentication = %#v, want a failure that never asks for az login", name, first)
		}
	}
	if len(az.calls) != 0 {
		t.Fatalf("az rest calls = %d, want none", len(az.calls))
	}
}

// TestExtensionRoutePlanWritesFailWithoutAWrite verifies that a plan write on
// the extension route before #305 reports a no-change error, so publication
// never treats it as an unknown outcome.
func TestExtensionRoutePlanWritesFailWithoutAWrite(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t)
	item := tracker.RenderedPlanItem{NodeID: "a", Type: "Epic", Title: "A", Body: "B", Labels: []string{"factory-plan:s/1/a"}, Correlation: "s/1/a"}
	_, err := extensionClient(az).CreatePlanItem(context.Background(), repository, item)
	if !tracker.WriteNotApplied(err) {
		t.Fatalf("CreatePlanItem() error = %v, want a no-change plan error", err)
	}
	if len(az.calls) != 0 {
		t.Fatalf("az rest calls = %d, want none", len(az.calls))
	}
}

// TestExtensionRouteLoginFailureIsANoChangePlanError verifies that a
// missing or expired az devops login proves that no plan write happened.
func TestExtensionRouteLoginFailureIsANoChangePlanError(t *testing.T) {
	t.Parallel()

	err := azuredevops.ClassifyPlanErrorForTest(&azuredevops.LoginRequiredError{OrganizationURL: "https://dev.azure.com/contoso"})
	if kind, _ := tracker.PlanErrorKindOf(err); kind != tracker.PlanErrorPermission {
		t.Fatalf("classified error = %v, want a permission plan error", err)
	}
}

// TestAzRestRouteDoesNotRunExtensionCommands verifies that a registration
// without a route keeps az rest and cannot resolve repository IDs through the
// extension.
func TestAzRestRouteDoesNotRunExtensionCommands(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).on("GET "+organizationConnectionData, connectionData)
	client := newClient(az)
	if login, err := client.AuthenticatedLogin(context.Background()); err != nil || login != "alice@contoso.com" {
		t.Fatalf("AuthenticatedLogin() = %q, %v", login, err)
	}
	if _, err := client.ResolveRepository(context.Background(), repository); err == nil {
		t.Fatal("ResolveRepository() on the az rest route error = nil")
	}
	if len(az.extensionCalls) != 0 {
		t.Fatalf("extension calls = %d, want none", len(az.extensionCalls))
	}
}

// organizationConnectionData is the az rest identity route of the test
// organization.
const organizationConnectionData = "/contoso/_apis/connectionData"
