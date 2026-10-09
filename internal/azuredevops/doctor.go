package azuredevops

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/hostcmd"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

var _ tracker.ReadinessChecker = (*Client)(nil)

// Security namespaces whose permissions the factory needs.
const (
	gitRepositoriesNamespace = "2e9eb7ed-3c0a-47d4-87c1-0ffdd275fd87"
	areaNamespace            = "83e28ad4-2d72-4ceb-97b0-c7726d5502c3"
)

// permission is one required permission bit and how to report its absence.
type permission struct {
	namespace string
	bit       int
	missing   string
}

// gitPermissions are the repository permissions for run branches and pull
// requests.
var gitPermissions = []permission{
	{namespace: gitRepositoriesNamespace, bit: 4, missing: "the identity cannot contribute to the repository"},
	{namespace: gitRepositoriesNamespace, bit: 16, missing: "the identity cannot create branches in the repository"},
	{namespace: gitRepositoriesNamespace, bit: 16384, missing: "the identity cannot contribute to pull requests"},
}

// workItemPermission is view (16) and edit (32) of work items in the
// project's root area.
var workItemPermission = permission{namespace: areaNamespace, bit: 48, missing: "the identity cannot view and edit work items in the project's root area"}

// StartupChecks returns the Azure DevOps authentication, repository, and
// permission checks in deterministic order.
func (c *Client) StartupChecks(repository tracker.Repository) []doctor.Check {
	return []doctor.Check{
		func(ctx context.Context) doctor.Result {
			const name = "azure devops authentication"
			if _, err := c.AuthenticatedLogin(ctx); err != nil {
				if result, ok := unresponsive(name, err); ok {
					return result
				}
				return doctor.Failure(name, "the Azure CLI has no usable Azure DevOps identity", "run az login (for a service principal, az login --service-principal) for the identity that supervises the repository")
			}
			return doctor.Success(name)
		},
		func(ctx context.Context) doctor.Result {
			const name = "azure devops repository"
			if _, err := c.repositoryIdentity(ctx, repository); err != nil {
				if result, ok := unresponsive(name, err); ok {
					return result
				}
				return doctor.Failure(name, fmt.Sprintf("the repository %s could not be read", repository), "verify the organization, project, and repository names, and that the identity can read the repository")
			}
			return doctor.Success(name)
		},
		func(ctx context.Context) doctor.Result {
			const name = "azure devops permissions"
			problem, err := c.missingPermission(ctx, repository)
			if err != nil {
				if result, ok := unresponsive(name, err); ok {
					return result
				}
				return doctor.Failure(name, "the repository and work item permissions could not be read", "verify that the identity can read the repository and the project's areas, then run factory doctor again")
			}
			if problem != "" {
				return doctor.Failure(name, problem, "grant the identity Contribute, Create branch, and Contribute to pull requests on the repository, and View and Edit work items in the project's root area")
			}
			return doctor.Success(name)
		},
	}
}

// repositoryIdentityResponse is the repository and project id projection.
type repositoryIdentityResponse struct {
	ID      string `json:"id"`
	Project struct {
		ID string `json:"id"`
	} `json:"project"`
}

// repositoryIdentity reads the repository and project ids.
func (c *Client) repositoryIdentity(ctx context.Context, repository tracker.Repository) (repositoryIdentityResponse, error) {
	if err := validateRepository(repository); err != nil {
		return repositoryIdentityResponse{}, err
	}
	var response repositoryIdentityResponse
	if err := c.call(ctx, request{Method: "GET", URL: repositoryURL(repository, "", nil)}, &response); err != nil {
		return repositoryIdentityResponse{}, err
	}
	if response.ID == "" || response.Project.ID == "" {
		return repositoryIdentityResponse{}, errors.New("repository response has no repository or project id")
	}
	return response, nil
}

// missingPermission returns the first missing permission of the
// authenticated identity, or an empty text when every permission is present.
func (c *Client) missingPermission(ctx context.Context, repository tracker.Repository) (string, error) {
	identity, err := c.repositoryIdentity(ctx, repository)
	if err != nil {
		return "", err
	}
	repositoryToken := "repoV2/" + identity.Project.ID + "/" + identity.ID
	for _, required := range gitPermissions {
		granted, err := c.hasPermission(ctx, repository.Owner, required, repositoryToken)
		if err != nil || !granted {
			return required.missing, err
		}
	}
	var area struct {
		Identifier string `json:"identifier"`
	}
	if err := c.call(ctx, request{Method: "GET", URL: projectURL(repository, "wit/classificationnodes/Areas", nil, apiVersion)}, &area); err != nil {
		return "", err
	}
	if area.Identifier == "" {
		return "", errors.New("area response has no identifier")
	}
	granted, err := c.hasPermission(ctx, repository.Owner, workItemPermission, "vstfs:///Classification/Node/"+area.Identifier)
	if err != nil || !granted {
		return workItemPermission.missing, err
	}
	return "", nil
}

// hasPermission evaluates one permission of the calling identity on one
// security token.
func (c *Client) hasPermission(ctx context.Context, organization string, required permission, token string) (bool, error) {
	var response struct {
		Value []bool `json:"value"`
	}
	path := "permissions/" + required.namespace + "/" + strconv.Itoa(required.bit)
	query := url.Values{"tokens": {token}, "alwaysAllowAdministrators": {"false"}}
	if err := c.call(ctx, request{Method: "GET", URL: organizationURL(organization, path, query, apiVersion)}, &response); err != nil {
		return false, err
	}
	return len(response.Value) == 1 && response.Value[0], nil
}

// unresponsive reports a check whose az call reached its deadline.
func unresponsive(name string, err error) (doctor.Result, bool) {
	var timeout *hostcmd.TimeoutError
	if !errors.As(err, &timeout) {
		return doctor.Result{}, false
	}
	return doctor.Failure(name,
		fmt.Sprintf("Azure DevOps did not answer within %s", timeout.Timeout),
		"check network access to dev.azure.com and the az proxy settings, then run factory doctor again"), true
}
