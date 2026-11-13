package cli

import (
	"github.com/Stevie1704/sw-factory/internal/azuredevops"
	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/localtracker"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// newService is the composition root: it selects the work tracker and code
// host adapter for the coordinator from the registered repository.
func newService(configPath string) *factory.Service {
	return factory.NewWithDependencies(configPath, serviceDependencies(configPath))
}

// configuredCodeHost is the composition-time set of independently supplied host
// capabilities. Workflow consumers continue to receive individual narrow ports.
type configuredCodeHost interface {
	codehost.PullRequestClient
	codehost.PullRequestReviewReader
	codehost.CommitStatusPublisher
	codehost.PullRequestCommentReader
	codehost.PullRequestCommentClient
	codehost.ReadinessChecker
}

// serviceDependencies keeps legacy combined adapters unchanged and explicitly
// supplies every host capability when the work tracker has a separate namespace.
func serviceDependencies(configPath string) factory.Dependencies {
	dependencies := factory.Dependencies{Tracker: trackerAdapter(configPath)}
	host, err := config.LoadHost(configPath)
	if err != nil || len(host.Repositories) == 0 || host.Repositories[0].EffectiveTracker() != config.ProviderLocal {
		return dependencies
	}
	registration := host.Repositories[0]
	var client configuredCodeHost
	if registration.EffectiveCodeHost() == config.ProviderAzureDevOps {
		client = azuredevops.NewClient(registration.AzureDevOps.Organization, registration.AuthorizedUsers)
	} else {
		client = github.NewCodeHostClient()
	}
	dependencies.PullRequests = client
	dependencies.PullRequestReviews = client
	dependencies.CommitStatuses = client
	dependencies.PullRequestComments = client
	dependencies.PullRequestCommentPublisher = client
	dependencies.CodeHostReadiness = client
	return dependencies
}

// trackerAdapter selects committed Markdown or a legacy combined remote tracker.
// Azure registrations retain their plan mapping. A host configuration that
// does not exist or does not load also
// selects GitHub: init and register create it, and the command that needs it
// reports the load error.
func trackerAdapter(configPath string) tracker.Client {
	host, err := config.LoadHost(configPath)
	if err != nil || len(host.Repositories) == 0 {
		return github.NewClient()
	}
	registration := host.Repositories[0]
	if registration.EffectiveTracker() == config.ProviderLocal {
		target := ""
		if policy, err := config.LoadRepository(registration.RepositoryConfigPath); err == nil {
			target = policy.TargetBranch
		}
		return localtracker.NewClient(registration, target)
	}
	if azure := registration.AzureDevOps; !azure.IsZero() {
		client := azuredevops.NewClient(azure.Organization, registration.AuthorizedUsers)
		client.Planning = azure.Planning
		return client
	}
	return github.NewClient()
}
