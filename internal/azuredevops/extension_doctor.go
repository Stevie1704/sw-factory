package azuredevops

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// Extension-route diagnosis conditions (ADR 0030). Codes are stable;
// wording may change.
var (
	extensionMissingCondition = doctor.Condition{Code: "azure_devops.extension.missing", Origin: doctor.OriginEnvironment, Effect: doctor.EffectLocalMutation, Topic: "setup"}
	serviceUnreachable        = doctor.Condition{Code: "azure_devops.service.unreachable", Origin: doctor.OriginEnvironment, Effect: doctor.EffectInspection, Topic: "setup", Unobserved: true}
	boardsUnreadable          = doctor.Condition{Code: "azure_devops.boards.unreadable", Origin: doctor.OriginEnvironment, Effect: doctor.EffectExternalMutation, Topic: "setup"}
	boardsUnassigned          = doctor.Condition{Code: "azure_devops.boards.unassigned", Origin: doctor.OriginEnvironment, Effect: doctor.EffectExternalMutation, Topic: "setup"}
)

// Reasons for the checks that the extension route cannot perform. They are
// reported as skipped, never as passed.
const (
	identityNotAvailable = "not available on the devops_extension route: no verified az devops command or invoke resource returns the signed-in identity (connection data belongs to the Location area, which az devops invoke refuses); status comments are recovered by their marker alone"

	permissionsNotAvailable = "not available on the devops_extension route: permission evaluation belongs to the Security area, which az devops invoke refuses, and az devops security permission show needs the signed-in identity; verify by hand that the az devops login identity has Contribute, Create branch, and Contribute to pull requests on the repository, View and Edit work items in the project, and Create tag definition"
)

// signedInRefusals are Azure DevOps error codes that only an authenticated
// caller receives: a missing or unreadable project or repository.
var signedInRefusals = []string{"TF401019", "TF200016"}

// extensionStartupChecks returns the read-only startup checks of the
// extension route. They run only az repos show, az boards query, and, for an
// assignment tag that names another GUID, az repos list; they never write
// and never ask for az login. Identity and permission checks have no
// verified command on this route and report themselves as skipped.
func (c *Client) extensionStartupChecks(repository tracker.Repository) []doctor.Check {
	organizationURL := extensionOrganizationURL(repository.Owner)
	return []doctor.Check{
		func(ctx context.Context) doctor.Result {
			const name = "azure devops authentication"
			_, err := c.ResolveRepository(ctx, repository)
			return extensionAuthenticationResult(name, organizationURL, err)
		},
		func(ctx context.Context) doctor.Result {
			const name = "azure devops repository"
			if _, err := c.ResolveRepository(ctx, repository); err != nil {
				if result, ok := unresponsive("azure_devops.repository.timeout", name, err); ok {
					return result
				}
				return doctor.Failure(repositoryUnreadable, name, fmt.Sprintf("the repository %s could not be read", repository), "verify the organization, project, and repository names, and that the az devops login identity can read the repository")
			}
			return doctor.Success("azure_devops.repository.ready", name)
		},
		func(ctx context.Context) doctor.Result {
			return c.extensionBoardsResult(ctx, repository)
		},
		func(context.Context) doctor.Result {
			return doctor.Skipped("azure_devops.identity.not_available", "azure devops identity", identityNotAvailable)
		},
		func(context.Context) doctor.Result {
			return doctor.Skipped("azure_devops.permissions.not_available", "azure devops permissions", permissionsNotAvailable)
		},
	}
}

// extensionBoardsResult queries the project's agent-ready work items, which
// proves Boards read access, and warns about those without a usable
// repository assignment: the queue never lists them, so the operator would
// otherwise learn of them only from a direct claim. A tag that names an
// unknown GUID is unusable too. Work items assigned to another repository
// of the project belong to another installation and are not reported; when
// az repos list fails, a tag naming another GUID is not reported either,
// because the doctor cannot tell which kind it is. The project's
// repositories are listed at most once per check.
func (c *Client) extensionBoardsResult(ctx context.Context, repository tracker.Repository) doctor.Result {
	const name = "azure devops boards"
	wiql := "SELECT [System.Id], [System.Tags] FROM WorkItems WHERE [System.TeamProject] = " + wiqlString(repository.Project) +
		" AND [System.Tags] CONTAINS " + wiqlString(tracker.LabelAgentReady) + " ORDER BY [System.Id]"
	var found []struct {
		ID     int `json:"id"`
		Fields struct {
			Tags string `json:"System.Tags"`
		} `json:"fields"`
	}
	if err := c.extension(ctx, repository.Owner, []string{"boards", "query", "--wiql", wiql, "--project", repository.Project}, &found); err != nil {
		if result, ok := unresponsive("azure_devops.boards.timeout", name, err); ok {
			return result
		}
		return doctor.Failure(boardsUnreadable, name, fmt.Sprintf("the work items of project %s could not be queried", repository.Project), "grant the az devops login identity View work items in the project, then run factory doctor again")
	}
	identity, err := c.ResolveRepository(ctx, repository)
	if err != nil {
		// The repository check reports why the assignment cannot be checked.
		return doctor.Success("azure_devops.boards.ready", name)
	}
	projectRepositories := sync.OnceValues(func() ([]RepositoryIdentity, error) {
		return c.projectRepositories(ctx, repository)
	})
	unassigned := []string{}
	for _, item := range found {
		var refused *AssignmentError
		if errors.As(ValidateRepositoryAssignment(splitTags(item.Fields.Tags), identity, projectRepositories), &refused) && refused.Problem != AssignmentMismatched {
			unassigned = append(unassigned, strconv.Itoa(item.ID))
		}
	}
	if len(unassigned) > 0 {
		return doctor.Warning(boardsUnassigned, name,
			fmt.Sprintf("agent-ready work items %s have no usable repository assignment, so no installation claims them", strings.Join(unassigned, ", ")),
			fmt.Sprintf("set the tag %s on each one that repository %s implements, replacing a malformed or unknown factory-repo tag, and keep exactly one factory-repo tag per work item", RepositoryAssignmentTag(identity.ID), identity.Name))
	}
	return doctor.Success("azure_devops.boards.ready", name)
}

// extensionAuthenticationResult classifies the first authenticated command
// of the extension route: a missing or expired login, a missing extension,
// a timeout, and an unreachable service are distinct diagnoses. A refusal
// that only an authenticated caller receives proves a working sign-in.
func extensionAuthenticationResult(name, organizationURL string, err error) doctor.Result {
	var login *LoginRequiredError
	var missing *ExtensionMissingError
	switch {
	case err == nil || signedInRefusal(err):
		return doctor.Success("azure_devops.authentication.ready", name)
	case errors.As(err, &login):
		return doctor.Failure(authenticationFailed, name, "the Azure DevOps CLI sign-in for "+organizationURL+" is missing, expired, or revoked", "run az devops login --organization "+organizationURL+" with a PAT of the identity that supervises the repository")
	case errors.As(err, &missing):
		return doctor.Failure(extensionMissingCondition, name, "the azure-devops Azure CLI extension is not installed", "run az extension add --name azure-devops")
	}
	if result, ok := unresponsive("azure_devops.authentication.timeout", name, err); ok {
		return result
	}
	return doctor.Failure(serviceUnreachable, name, "Azure DevOps could not be reached or did not accept the request", "check network access to dev.azure.com and the az proxy settings; if the sign-in expired, run az devops login --organization "+organizationURL)
}

// signedInRefusal reports whether err is an Azure DevOps refusal that only
// an authenticated caller receives.
func signedInRefusal(err error) bool {
	for _, code := range signedInRefusals {
		if strings.Contains(err.Error(), code) {
			return true
		}
	}
	return false
}
