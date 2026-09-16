package factory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
)

// InferredValue is one registration value that inference supplied because the
// operator did not. Flag names the register option that overrides it.
type InferredValue struct {
	// Flag is the register command option this value was resolved for.
	Flag string
	// Value is the resolved value, in the form the registration stores it.
	Value string
}

// inferRegistration fills the registration values an operator did not supply
// from the local Git checkout and the authenticated GitHub account. Explicit
// values are never replaced, every probe is read-only, and an unresolvable
// value fails closed before any registration is written.
func (s *Service) inferRegistration(ctx context.Context, request RegisterRequest) (RegisterRequest, []InferredValue, error) {
	needsRepository := strings.TrimSpace(request.RepositoryPath) == ""
	needsOwner := strings.TrimSpace(request.GitHubOwner) == ""
	needsName := strings.TrimSpace(request.GitHubRepository) == ""
	needsUsers := len(request.AuthorizedUsers) == 0
	inferred := make([]InferredValue, 0, 4)
	if needsRepository || needsOwner || needsName {
		discovery, err := s.discoverRegistrationRepository(ctx, request)
		if err != nil {
			return RegisterRequest{}, nil, err
		}
		if needsRepository {
			request.RepositoryPath = discovery.Root
			inferred = append(inferred, InferredValue{Flag: "repository", Value: discovery.Root})
		}
		if needsOwner {
			request.GitHubOwner = discovery.Owner
			inferred = append(inferred, InferredValue{Flag: "github-owner", Value: discovery.Owner})
		}
		if needsName {
			request.GitHubRepository = discovery.Repository
			inferred = append(inferred, InferredValue{Flag: "github-repository", Value: discovery.Repository})
		}
	}
	if needsUsers {
		if s.deps.GitHubAccount == nil {
			return RegisterRequest{}, nil, errors.New("register cannot infer an authorized user: no GitHub account adapter is configured; pass --authorized-user")
		}
		login, err := s.deps.GitHubAccount.AuthenticatedLogin(ctx)
		if err != nil {
			return RegisterRequest{}, nil, fmt.Errorf("infer the authorized user: %w", err)
		}
		request.AuthorizedUsers = []string{login}
		inferred = append(inferred, InferredValue{Flag: "authorized-user", Value: login})
	}
	return request, inferred, nil
}

// discoverRegistrationRepository resolves the checkout root and GitHub identity
// from the explicit repository path when one was supplied, and otherwise from
// the directory the register command was invoked in.
func (s *Service) discoverRegistrationRepository(ctx context.Context, request RegisterRequest) (gitadapter.RepositoryDiscovery, error) {
	directory := strings.TrimSpace(request.RepositoryPath)
	if directory == "" {
		directory = strings.TrimSpace(request.WorkingDirectory)
	}
	if directory == "" {
		return gitadapter.RepositoryDiscovery{}, errors.New("register cannot infer the repository and GitHub identity without a working directory: run it inside a checkout or pass --repository, --github-owner, and --github-repository")
	}
	if s.deps.RepositoryDiscoverer == nil {
		return gitadapter.RepositoryDiscovery{}, errors.New("register cannot infer the repository and GitHub identity: no Git discovery adapter is configured; pass --repository, --github-owner, and --github-repository")
	}
	discovery, err := s.deps.RepositoryDiscoverer.DiscoverRepository(ctx, directory)
	if err != nil {
		return gitadapter.RepositoryDiscovery{}, fmt.Errorf("infer the repository and GitHub identity: %w; pass --repository, --github-owner, and --github-repository to register explicitly", err)
	}
	return discovery, nil
}
