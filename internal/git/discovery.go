package git

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// RepositoryDiscovery is the checkout root and GitHub identity resolved from a
// directory inside an ordinary Git checkout.
type RepositoryDiscovery struct {
	// Root is the absolute checkout root reported by Git.
	Root string
	// Owner is the GitHub owner named by the discovery remote.
	Owner string
	// Repository is the GitHub repository name named by the discovery remote.
	Repository string
}

// RepositoryDiscoverer resolves registration defaults from a local checkout.
// It is read-only and never changes a Git reference or remote.
type RepositoryDiscoverer interface {
	DiscoverRepository(context.Context, string) (RepositoryDiscovery, error)
}

// DiscoverRepository resolves the checkout root and the GitHub identity of the
// default remote from any directory inside a Git checkout. It fails closed when
// the directory is outside a checkout, when the remote is missing, non-GitHub,
// or malformed, and when the fetch and push URLs disagree.
func (m *LocalWorktreeManager) DiscoverRepository(ctx context.Context, workingDirectory string) (RepositoryDiscovery, error) {
	directory := strings.TrimSpace(workingDirectory)
	if directory == "" {
		return RepositoryDiscovery{}, errors.New("a working directory is required to infer registration values")
	}
	rootOutput, err := m.runner().Run(ctx, directory, []string{"rev-parse", "--show-toplevel"})
	if err != nil {
		return RepositoryDiscovery{}, fmt.Errorf("%q is not inside a Git checkout: run factory register from a checkout or pass --repository", directory)
	}
	root := strings.TrimSpace(string(rootOutput))
	if root == "" {
		return RepositoryDiscovery{}, fmt.Errorf("%q is not inside a Git checkout: Git reported an empty checkout root", directory)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return RepositoryDiscovery{}, fmt.Errorf("resolve discovered checkout root: %w", err)
	}
	fetchOutput, err := m.runner().Run(ctx, directory, []string{"remote", "get-url", DefaultRemoteName})
	if err != nil {
		return RepositoryDiscovery{}, fmt.Errorf("the checkout at %q has no %s remote: add one or pass --github-owner and --github-repository", root, DefaultRemoteName)
	}
	owner, repository, ok := ParseGitHubRemote(string(fetchOutput))
	if !ok {
		return RepositoryDiscovery{}, fmt.Errorf("the %s remote does not identify a GitHub repository: pass --github-owner and --github-repository", DefaultRemoteName)
	}
	pushOutput, err := m.runner().Run(ctx, directory, []string{"remote", "get-url", "--push", DefaultRemoteName})
	if err != nil {
		return RepositoryDiscovery{}, fmt.Errorf("the %s remote's push URL cannot be read: pass --github-owner and --github-repository", DefaultRemoteName)
	}
	pushOwner, pushRepository, ok := ParseGitHubRemote(string(pushOutput))
	if !ok {
		return RepositoryDiscovery{}, fmt.Errorf("the %s remote's push URL does not identify a GitHub repository: pass --github-owner and --github-repository", DefaultRemoteName)
	}
	if !strings.EqualFold(owner, pushOwner) || !strings.EqualFold(repository, pushRepository) {
		return RepositoryDiscovery{}, fmt.Errorf("the %s remote's fetch and push URLs identify different GitHub repositories: pass --github-owner and --github-repository", DefaultRemoteName)
	}
	return RepositoryDiscovery{Root: root, Owner: owner, Repository: repository}, nil
}

// ParseGitHubRemote extracts the owner and repository named by one Git remote
// URL, accepting the HTTPS, ssh://, and scp-like SSH forms emitted by Git. It
// reports false for any URL that does not name a github.com repository.
func ParseGitHubRemote(value string) (string, string, bool) {
	remote := strings.TrimSpace(value)
	if remote == "" || strings.ContainsAny(remote, "\x00\r\n") {
		return "", "", false
	}
	if !strings.Contains(remote, "://") {
		at := strings.LastIndex(remote, "@")
		colon := strings.Index(remote, ":")
		if colon <= at {
			return "", "", false
		}
		remote = "ssh://" + remote[:colon] + "/" + remote[colon+1:]
	}
	parsed, err := url.Parse(remote)
	if err != nil || !strings.EqualFold(parsed.Hostname(), "github.com") {
		return "", "", false
	}
	path := strings.Trim(strings.TrimSuffix(parsed.Path, ".git"), "/")
	owner, repository, found := strings.Cut(path, "/")
	if !found || owner == "" || repository == "" || strings.Contains(repository, "/") {
		return "", "", false
	}
	return owner, repository, true
}
