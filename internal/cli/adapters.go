package cli

import (
	"github.com/Stevie1704/sw-factory/internal/azuredevops"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// newService is the composition root: it selects the work tracker and code
// host adapter for the coordinator from the registered repository.
func newService(configPath string) *factory.Service {
	return factory.NewWithDependencies(configPath, factory.Dependencies{Tracker: trackerAdapter(configPath)})
}

// trackerAdapter returns the Azure DevOps adapter for an Azure DevOps
// registration and the GitHub adapter otherwise. A host configuration that
// does not exist or does not load also selects GitHub: init and register
// create it, and the command that needs it reports the load error.
func trackerAdapter(configPath string) tracker.Client {
	host, err := config.LoadHost(configPath)
	if err != nil || len(host.Repositories) == 0 {
		return github.NewClient()
	}
	registration := host.Repositories[0]
	if azure := registration.AzureDevOps; !azure.IsZero() {
		return azuredevops.NewClient(azure.Organization, registration.AuthorizedUsers)
	}
	return github.NewClient()
}
