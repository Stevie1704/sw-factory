package cli

import (
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/github"
)

// newService is the composition root: it selects the GitHub adapter as the
// work tracker and the code host for the coordinator.
func newService(configPath string) *factory.Service {
	return factory.NewWithDependencies(configPath, factory.Dependencies{Tracker: github.NewClient()})
}
