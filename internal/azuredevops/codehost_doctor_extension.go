package azuredevops

import (
	"context"
	"errors"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// codeHostStartupChecks diagnoses Azure Repos on the extension
// route with read-only az repos and az devops invoke probes. It needs
// neither Boards nor az login. The CLI helper reads the current identity
// and evaluates effective repository permissions without any mutation.
func (c extensionBackend) codeHostStartupChecks(repository tracker.Repository) []doctor.Check {
	defaultBranch := ""
	return []doctor.Check{
		func(ctx context.Context) doctor.Result {
			_, branch, err := c.showRepository(ctx, repository)
			if err != nil {
				return extensionCodeHostSignInFailure(repository, err)
			}
			defaultBranch = branch
			if _, err := c.AuthenticatedLogin(ctx); err != nil {
				return extensionCodeHostSignInFailure(repository, err)
			}
			return doctor.Success("azure_devops.code_host.authentication.ready", "Azure Repos authentication")
		},
		func(ctx context.Context) doctor.Result {
			if err := c.probeExtensionCodeHost(ctx, repository, defaultBranch); err != nil {
				return extensionCodeHostUnavailable()
			}
			return doctor.Success("azure_devops.code_host.access.ready", "Azure Repos PR and status access")
		},
		func(ctx context.Context) doctor.Result {
			if err := c.extensionGitPermissions(ctx, repository); err != nil {
				return doctor.Failure(permissionsInsufficient, "Azure Repos write permissions", err.Error(), "grant the az devops login identity Contribute, Create branch, and Contribute to pull requests on "+repository.String())
			}
			return doctor.Success("azure_devops.code_host.permissions.ready", "Azure Repos write permissions")
		},
	}
}

// probeExtensionCodeHost reads one active pull request, the default branch
// ref, and the statuses of its commit.
func (c *Client) probeExtensionCodeHost(ctx context.Context, repository tracker.Repository, defaultBranch string) error {
	if !strings.HasPrefix(defaultBranch, branchPrefix) {
		return errors.New("the repository has no default branch")
	}
	var pulls []struct{}
	if err := c.extension(ctx, repository.Owner, append(repositoryCommand(repository, "repos", "pr", "list"), "--status", "active", "--top", "1"), &pulls); err != nil {
		return err
	}
	var refs []struct {
		Name     string `json:"name"`
		ObjectID string `json:"objectId"`
	}
	filter := strings.TrimPrefix(defaultBranch, "refs/")
	if err := c.extension(ctx, repository.Owner, append(repositoryCommand(repository, "repos", "ref", "list"), "--filter", filter), &refs); err != nil {
		return err
	}
	sha := ""
	for _, ref := range refs {
		if ref.Name == defaultBranch {
			sha = ref.ObjectID
		}
	}
	if !codehost.ValidCommitSHA(sha) {
		return errors.New("the default branch has no commit")
	}
	var statuses struct {
		Value []struct{} `json:"value"`
	}
	return c.gitInvoke(ctx, repository, "GET", "statuses", map[string]string{"commitId": sha}, map[string]string{"top": "1"}, nil, &statuses)
}

// extensionCodeHostSignInFailure reports a missing az devops login or a
// registered repository the signed-in identity cannot read. The repair
// never points to az login.
func extensionCodeHostSignInFailure(repository tracker.Repository, err error) doctor.Result {
	condition := doctor.Condition{Code: "azure_devops.code_host.authentication.failed", Origin: doctor.OriginEnvironment, Effect: doctor.EffectLocalMutation, Topic: "setup", Unobserved: true}
	action := "run az devops login --organization " + extensionOrganizationURL(repository.Owner)
	var login *LoginRequiredError
	if errors.As(err, &login) {
		return doctor.Failure(condition, "Azure Repos authentication", "the Azure DevOps CLI sign-in is missing or expired", action)
	}
	return doctor.Failure(condition, "Azure Repos authentication", "the az devops login identity cannot read the registered Azure Repos repository", action+" with an identity that can read "+repository.String()+", and check the registered organization, project, and repository")
}

// extensionCodeHostUnavailable reports a blocked PR, ref, or status read on
// the extension route.
func extensionCodeHostUnavailable() doctor.Result {
	return doctor.Failure(doctor.Condition{Code: "azure_devops.code_host.access.unavailable", Origin: doctor.OriginEnvironment, Effect: doctor.EffectExternalMutation, Topic: "setup", Unobserved: true},
		"Azure Repos PR and status access",
		"az repos cannot read pull requests, the default branch, or commit statuses of the registered repository",
		"verify the azure-devops CLI extension, the az devops login identity's repository access, and company policy; a local tracker cannot bypass blocked PR/status access")
}
