package factory

import (
	"context"
	"errors"

	"github.com/Stevie1704/sw-factory/internal/config"
)

// registration loads the one configured repository registration.
func (s *Service) registration() (config.RepositoryRegistration, error) {
	if s.configPath == "" {
		return config.RepositoryRegistration{}, errors.New("host configuration path is required")
	}
	host, err := s.deps.Config.Load(s.configPath)
	if err != nil {
		return config.RepositoryRegistration{}, err
	}
	if len(host.Repositories) == 0 {
		return config.RepositoryRegistration{}, errors.New("no repository is registered")
	}
	return host.Repositories[0], nil
}

// openRunStore loads the registration and opens the claim-capable store once.
func (s *Service) openRunStore(ctx context.Context) (config.RepositoryRegistration, RunStore, error) {
	registration, err := s.registration()
	if err != nil {
		return config.RepositoryRegistration{}, nil, err
	}
	opened, err := s.deps.OpenStore(ctx, registration.OperationalDataPath)
	if err != nil {
		return config.RepositoryRegistration{}, nil, err
	}
	runStore, ok := opened.(RunStore)
	if !ok {
		_ = opened.Close()
		return config.RepositoryRegistration{}, nil, errors.New("operational store does not support run coordination")
	}
	return registration, runStore, nil
}
