package factory

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Registration field names reported by inference. They match the register
// command's flags so an operator can override exactly one inferred value.
const (
	inferredRepository       = "repository"
	inferredGitHubOwner      = "github-owner"
	inferredGitHubRepository = "github-repository"
	inferredAuthorizedUser   = "authorized-user"
)

// inferRegistration fills the registration values an operator did not supply
// from the local Git checkout and the authenticated GitHub account. Explicit
// values are never replaced, every probe is read-only, and an unresolvable
// value fails closed before any registration is written.
func (s *Service) inferRegistration(ctx context.Context, request RegisterRequest) (RegisterRequest, []string, error) {
	needsRepository := strings.TrimSpace(request.RepositoryPath) == ""
	needsOwner := strings.TrimSpace(request.GitHubOwner) == ""
	needsName := strings.TrimSpace(request.GitHubRepository) == ""
	needsUsers := len(request.AuthorizedUsers) == 0
	inferred := make([]string, 0, 4)
	if needsRepository || needsOwner || needsName {
		discovery, err := s.discoverRegistrationRepository(ctx, request)
		if err != nil {
			return RegisterRequest{}, nil, err
		}
		if needsRepository {
			request.RepositoryPath = discovery.Root
			inferred = append(inferred, inferredRepository)
		}
		if needsOwner {
			request.GitHubOwner = discovery.Owner
			inferred = append(inferred, inferredGitHubOwner)
		}
		if needsName {
			request.GitHubRepository = discovery.Repository
			inferred = append(inferred, inferredGitHubRepository)
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
		inferred = append(inferred, inferredAuthorizedUser)
	}
	return request, inferred, nil
}

// discoverRegistrationRepository resolves the checkout root and GitHub identity
// from the explicit repository path when one was supplied, and otherwise from
// the directory the register command was invoked in.
func (s *Service) discoverRegistrationRepository(ctx context.Context, request RegisterRequest) (registrationDiscovery, error) {
	directory := strings.TrimSpace(request.RepositoryPath)
	if directory == "" {
		directory = strings.TrimSpace(request.WorkingDirectory)
	}
	if directory == "" {
		return registrationDiscovery{}, errors.New("register cannot infer the repository and GitHub identity without a working directory: run it inside a checkout or pass --repository, --github-owner, and --github-repository")
	}
	if s.deps.RepositoryDiscoverer == nil {
		return registrationDiscovery{}, errors.New("register cannot infer the repository and GitHub identity: no Git discovery adapter is configured; pass --repository, --github-owner, and --github-repository")
	}
	discovery, err := s.deps.RepositoryDiscoverer.DiscoverRepository(ctx, directory)
	if err != nil {
		return registrationDiscovery{}, fmt.Errorf("infer the repository and GitHub identity: %w", err)
	}
	return registrationDiscovery{Root: discovery.Root, Owner: discovery.Owner, Repository: discovery.Repository}, nil
}

// registrationDiscovery is the registration-facing projection of one Git
// checkout discovery.
type registrationDiscovery struct {
	Root       string
	Owner      string
	Repository string
}
