package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
)

// TestLoadHostReadsTheAzureDevOpsPlanMapping verifies that an Azure DevOps
// registration can map neutral plan kinds to the project's work-item types,
// required levels, and field values.
func TestLoadHostReadsTheAzureDevOpsPlanMapping(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := `schema_version: 2
repositories:
  - path: /work/service
    azure_devops:
      organization: contoso
      project: Factory Pilot
      repository: service
      planning:
        levels:
          - type: Epic
            kinds: [spec]
          - type: Feature
            required: true
          - type: Product Backlog Item
            kinds: [task, investigation]
            fields:
              Microsoft.VSTS.Common.Priority: "2"
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
	planning := host.Repositories[0].AzureDevOps.Planning
	if planning == nil || len(planning.Levels) != 3 {
		t.Fatalf("Planning = %#v, want three levels", planning)
	}
	leaf := planning.Levels[2]
	if leaf.Type != "Product Backlog Item" || leaf.Kinds[1] != "investigation" || leaf.Fields["Microsoft.VSTS.Common.Priority"] != "2" || !planning.Levels[1].Required {
		t.Fatalf("Planning levels = %#v, want the configured PBI mapping", planning.Levels)
	}
}

// TestValidateHostRejectsAnInvalidAzureDevOpsPlanMapping verifies that a plan
// mapping that cannot be applied fails at load time, before any preview.
func TestValidateHostRejectsAnInvalidAzureDevOpsPlanMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mapping config.AzurePlanMapping
	}{
		{name: "no levels", mapping: config.AzurePlanMapping{}},
		{name: "empty type", mapping: config.AzurePlanMapping{Levels: []config.AzurePlanLevel{{Type: " ", Kinds: []string{"task"}}}}},
		{name: "repeated type", mapping: config.AzurePlanMapping{Levels: []config.AzurePlanLevel{{Type: "Epic", Kinds: []string{"spec"}}, {Type: "epic", Kinds: []string{"task"}}}}},
		{name: "repeated kind", mapping: config.AzurePlanMapping{Levels: []config.AzurePlanLevel{{Type: "Epic", Kinds: []string{"task"}}, {Type: "User Story", Kinds: []string{"task"}}}}},
		{name: "unsafe kind", mapping: config.AzurePlanMapping{Levels: []config.AzurePlanLevel{{Type: "Epic", Kinds: []string{"a kind"}}}}},
		{name: "no kind", mapping: config.AzurePlanMapping{Levels: []config.AzurePlanLevel{{Type: "Feature", Required: true}}}},
		{name: "rendered title field", mapping: config.AzurePlanMapping{Levels: []config.AzurePlanLevel{{Type: "Epic", Kinds: []string{"spec"}, Fields: map[string]string{"System.Title": "x"}}}}},
		{name: "tags field", mapping: config.AzurePlanMapping{Levels: []config.AzurePlanLevel{{Type: "Epic", Kinds: []string{"spec"}, Fields: map[string]string{"system.tags": "agent-ready"}}}}},
		{name: "empty field name", mapping: config.AzurePlanMapping{Levels: []config.AzurePlanLevel{{Type: "Epic", Kinds: []string{"spec"}, Fields: map[string]string{" ": "x"}}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			registration := validRegistration()
			registration.GitHub = config.GitHubConfig{}
			mapping := tc.mapping
			registration.AzureDevOps = config.AzureDevOpsConfig{Organization: "contoso", Project: "Factory Pilot", Repository: "service", Planning: &mapping}
			err := config.ValidateHost(config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{registration}})
			var validationErr *config.ValidationError
			if !errors.As(err, &validationErr) || validationErr.Field != "repositories[0].azure_devops.planning" {
				t.Fatalf("ValidateHost() error = %v, want a validation error for the plan mapping", err)
			}
		})
	}
}
