package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/azuredevops"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/localpublication"
	"github.com/Stevie1704/sw-factory/internal/localtracker"
	"github.com/Stevie1704/sw-factory/internal/tracker"
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

// TestLocalCompositionSuppliesIndependentHostCapabilities exercises both
// supported registrations without executing any remote tracker command.
func TestLocalCompositionSuppliesIndependentHostCapabilities(t *testing.T) {
	for _, provider := range []config.Provider{config.ProviderGitHub, config.ProviderAzureDevOps} {
		t.Run(string(provider), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "host", "config.yaml")
			registration := config.RepositoryRegistration{Path: "/work/service", Tracker: config.ProviderLocal, CodeHost: provider, LocalOperator: config.LocalOperatorConfig{InstallationID: "install-11111111111111111111111111111111", Name: "operator"}, AuthorizedUsers: []string{"reviewer"}, OperationalDataPath: "/private/factory/store.db", RepositoryConfigPath: "/work/service/factory.yaml", Polling: config.PollingConfig{Interval: "30s", Backoff: "5m"}}
			if provider == config.ProviderAzureDevOps {
				registration.AzureDevOps = config.AzureDevOpsConfig{Organization: "company", Project: "project", Repository: "service"}
			} else {
				registration.GitHub = config.GitHubConfig{Owner: "example", Repository: "service"}
			}
			if err := config.SaveHost(path, config.HostConfig{SchemaVersion: 2, Repositories: []config.RepositoryRegistration{registration}}); err != nil {
				t.Fatal(err)
			}
			dependencies := serviceDependencies(path)
			adapter, ok := dependencies.Tracker.(*localpublication.Adapter)
			if !ok {
				t.Fatalf("tracker = %T", dependencies.Tracker)
			}
			if _, ok := adapter.Client.(*localtracker.Client); !ok {
				t.Fatalf("committed tracker = %T", adapter.Client)
			}
			if _, ok := dependencies.Tracker.(tracker.PlanBatchPublisher); !ok {
				t.Fatal("atomic batch publication missing from local composition")
			}
			if _, ok := dependencies.Tracker.(tracker.CommittedIssueReader); !ok {
				t.Fatal("committed source capability lost during composition")
			}
			if separate, ok := dependencies.Tracker.(tracker.SeparateCommentStreams); !ok || !separate.SeparateIssueCommentStream() {
				t.Fatal("local and PR comment identities share a watermark")
			}
			if dependencies.PullRequestComments == nil || dependencies.PullRequestCommentPublisher == nil || dependencies.PullRequestReviews == nil || dependencies.CommitStatuses == nil || dependencies.CodeHostReadiness == nil {
				t.Fatal("independent code-host capabilities are missing")
			}
			if provider == config.ProviderGitHub {
				if _, ok := dependencies.PullRequests.(*github.CodeHostClient); !ok {
					t.Fatalf("GitHub code host = %T", dependencies.PullRequests)
				}
			} else {
				if _, ok := dependencies.PullRequests.(*azuredevops.Client); !ok {
					t.Fatalf("Azure code host = %T", dependencies.PullRequests)
				}
			}
		})
	}
}
