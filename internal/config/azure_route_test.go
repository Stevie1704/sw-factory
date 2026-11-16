package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
)

// azureRegistration returns a complete Azure DevOps registration with the
// given connection settings.
func azureRegistration(azure config.AzureDevOpsConfig) config.RepositoryRegistration {
	registration := validRegistration()
	registration.GitHub = config.GitHubConfig{}
	registration.AzureDevOps = azure
	return registration
}

// validateAzure validates one host with the given Azure DevOps settings.
func validateAzure(azure config.AzureDevOpsConfig) error {
	return config.ValidateHost(config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{azureRegistration(azure)}})
}

// TestAnOmittedAzureRouteKeepsTheAzRestRoute verifies that a registration
// written before route selection existed keeps the az rest route.
func TestAnOmittedAzureRouteKeepsTheAzRestRoute(t *testing.T) {
	t.Parallel()

	azure := config.AzureDevOpsConfig{Organization: "contoso", Project: "Factory Pilot", Repository: "service"}
	if err := validateAzure(azure); err != nil {
		t.Fatalf("ValidateHost() error = %v", err)
	}
	if got := azure.EffectiveRoute(); got != config.AzureRouteAzRest {
		t.Fatalf("EffectiveRoute() = %q, want %q", got, config.AzureRouteAzRest)
	}
	azure.Route = config.AzureRouteAzRest
	if err := validateAzure(azure); err != nil {
		t.Fatalf("ValidateHost() with an explicit az_rest route error = %v", err)
	}
}

// TestLoadHostSelectsTheDevOpsExtensionRoute verifies that the host
// configuration selects the Azure DevOps CLI extension route explicitly and
// names the other project repositories that plans may assign PBIs to.
func TestLoadHostSelectsTheDevOpsExtensionRoute(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := `schema_version: 2
repositories:
  - path: /work/service
    azure_devops:
      organization: contoso
      project: Factory Pilot
      repository: service
      route: devops_extension
      plan_repositories: [web]
    authorized_users: [alice@contoso.com]
    polling:
      interval: 30s
      backoff: 5m
    operational_data_path: /var/lib/factory/factory.db
    repository_config_path: /work/service/factory.yaml
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	host, err := config.LoadHost(path)
	if err != nil {
		t.Fatalf("LoadHost() error = %v", err)
	}
	azure := host.Repositories[0].AzureDevOps
	if azure.EffectiveRoute() != config.AzureRouteDevOpsExtension {
		t.Fatalf("EffectiveRoute() = %q, want %q", azure.EffectiveRoute(), config.AzureRouteDevOpsExtension)
	}
	if len(azure.PlanRepositories) != 1 || azure.PlanRepositories[0] != "web" {
		t.Fatalf("PlanRepositories = %v, want [web]", azure.PlanRepositories)
	}
	if host.Repositories[0].EffectiveTracker() != config.ProviderAzureDevOps {
		t.Fatalf("EffectiveTracker() = %q, want azure_devops", host.Repositories[0].EffectiveTracker())
	}
}

// TestValidateHostRejectsAnInvalidAzureRouteSelection verifies that an
// unknown route, a route without an Azure identity, and plan repositories
// outside the extension route fail with a field-specific error.
func TestValidateHostRejectsAnInvalidAzureRouteSelection(t *testing.T) {
	t.Parallel()

	base := config.AzureDevOpsConfig{Organization: "contoso", Project: "Factory Pilot", Repository: "service", Route: config.AzureRouteDevOpsExtension}
	tests := []struct {
		name  string
		azure func(config.AzureDevOpsConfig) config.AzureDevOpsConfig
		field string
	}{
		{name: "unknown route", azure: func(a config.AzureDevOpsConfig) config.AzureDevOpsConfig { a.Route = "pat"; return a }, field: "repositories[0].azure_devops.route"},
		{name: "route without identity", azure: func(a config.AzureDevOpsConfig) config.AzureDevOpsConfig {
			return config.AzureDevOpsConfig{Route: a.Route}
		}, field: "repositories[0].azure_devops.organization"},
		{name: "plan repositories on az rest", azure: func(a config.AzureDevOpsConfig) config.AzureDevOpsConfig {
			a.Route = ""
			a.PlanRepositories = []string{"web"}
			return a
		}, field: "repositories[0].azure_devops.plan_repositories"},
		{name: "plan repository repeats the registered one", azure: func(a config.AzureDevOpsConfig) config.AzureDevOpsConfig {
			a.PlanRepositories = []string{"Service"}
			return a
		}, field: "repositories[0].azure_devops.plan_repositories"},
		{name: "duplicate plan repository", azure: func(a config.AzureDevOpsConfig) config.AzureDevOpsConfig {
			a.PlanRepositories = []string{"web", "WEB"}
			return a
		}, field: "repositories[0].azure_devops.plan_repositories"},
		{name: "unsafe plan repository", azure: func(a config.AzureDevOpsConfig) config.AzureDevOpsConfig {
			a.PlanRepositories = []string{"web/other"}
			return a
		}, field: "repositories[0].azure_devops.plan_repositories"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := validateAzure(tc.azure(base))
			var validationErr *config.ValidationError
			if !errors.As(err, &validationErr) || validationErr.Field != tc.field {
				t.Fatalf("ValidateHost() error = %v, want a validation error for %q", err, tc.field)
			}
		})
	}
}

// TestPlanTargetRepositoriesListTheRegisteredRepositoryFirst verifies that a
// plan can always target the registered repository and, on the extension
// route, the configured plan repositories of the same project.
func TestPlanTargetRepositoriesListTheRegisteredRepositoryFirst(t *testing.T) {
	t.Parallel()

	azure := config.AzureDevOpsConfig{Organization: "contoso", Project: "Factory Pilot", Repository: "service", Route: config.AzureRouteDevOpsExtension, PlanRepositories: []string{"web", "api"}}
	got := azure.PlanTargetRepositories()
	want := []string{"service", "web", "api"}
	if len(got) != len(want) {
		t.Fatalf("PlanTargetRepositories() = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("PlanTargetRepositories() = %v, want %v", got, want)
		}
	}
}
