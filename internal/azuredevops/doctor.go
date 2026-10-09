package azuredevops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/hostcmd"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

var _ tracker.ReadinessChecker = (*Client)(nil)

// Security namespaces whose permissions the factory needs.
const (
	gitRepositoriesNamespace = "2e9eb7ed-3c0a-47d4-87c1-0ffdd275fd87"
	areaNamespace            = "83e28ad4-2d72-4ceb-97b0-c7726d5502c3"
	taggingNamespace         = "bb50f182-8e5e-40b8-bc21-e8752a1e7ae2"
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

// tagCreatePermission is Create tag definition in the project. The first use
// of a tag creates it, so a missing factory tag needs this permission.
var tagCreatePermission = permission{namespace: taggingNamespace, bit: 2}

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
				return doctor.Failure(name, problem, "grant the identity Contribute, Create branch, and Contribute to pull requests on the repository, View and Edit work items in the project's root area, and Create tag definition in the project, or add the missing tags once by hand")
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
	return c.missingTagPermission(ctx, repository, identity.Project.ID)
}

// tagList is the project tag list response.
type tagList struct {
	Value []struct {
		Name string `json:"name"`
	} `json:"value"`
}

// projectTags returns the lower-case names of the project's tags. The API
// reference shows the list wrapped in an array, unlike other list endpoints,
// so both shapes are accepted.
func (c *Client) projectTags(ctx context.Context, repository tracker.Repository) (map[string]bool, error) {
	var raw json.RawMessage
	if err := c.call(ctx, request{Method: "GET", URL: projectURL(repository, "wit/tags", nil, apiVersion)}, &raw); err != nil {
		return nil, err
	}
	lists := []tagList{}
	if err := json.Unmarshal(raw, &lists); err != nil {
		var single tagList
		if err := json.Unmarshal(raw, &single); err != nil {
			return nil, fmt.Errorf("decode project tags: %w", err)
		}
		lists = append(lists, single)
	}
	existing := map[string]bool{}
	for _, list := range lists {
		for _, tag := range list.Value {
			existing[strings.ToLower(tag.Name)] = true
		}
	}
	return existing, nil
}

// missingTagPermission returns a problem when a factory tag does not exist in
// the project and the identity cannot create it on first use.
func (c *Client) missingTagPermission(ctx context.Context, repository tracker.Repository, projectID string) (string, error) {
	existing, err := c.projectTags(ctx, repository)
	if err != nil {
		return "", err
	}
	missing := make([]string, 0)
	for _, label := range tracker.FactoryStateLabels {
		if !existing[label] {
			missing = append(missing, label)
		}
	}
	if len(missing) == 0 {
		return "", nil
	}
	granted, err := c.hasPermission(ctx, repository.Owner, tagCreatePermission, "/"+projectID)
	if err != nil || granted {
		return "", err
	}
	return "the identity cannot create the missing factory tags " + strings.Join(missing, ", "), nil
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
