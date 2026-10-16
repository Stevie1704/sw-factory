package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/azuredevops"
)

// TestTrackerAdapterPassesTheAzurePlanMappingToTheClient verifies that the
// composition root gives the Azure DevOps adapter the registration's
// configured plan mapping, so publication does not fall back to the Agile
// default.
func TestTrackerAdapterPassesTheAzurePlanMappingToTheClient(t *testing.T) {
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
            kinds: [parent]
          - type: Product Backlog Item
            kinds: [task, investigation]
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
	client, ok := trackerAdapter(path).(*azuredevops.Client)
	if !ok {
		t.Fatalf("trackerAdapter() = %T, want the Azure DevOps adapter", trackerAdapter(path))
	}
	if client.Planning == nil || len(client.Planning.Levels) != 2 || client.Planning.Levels[1].Type != "Product Backlog Item" {
		t.Fatalf("Planning = %#v, want the configured PBI mapping", client.Planning)
	}
}
