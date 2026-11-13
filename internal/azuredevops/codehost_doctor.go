package azuredevops

import (
	"context"
	"net/url"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

var _ codehost.ReadinessChecker = (*Client)(nil)

// CodeHostStartupChecks checks the existing Azure Repos az route without any
// Boards areas, tags, or work-item API. A blocked az route stays a startup error.
func (c *Client) CodeHostStartupChecks(repository tracker.Repository) []doctor.Check {
	return []doctor.Check{
		func(ctx context.Context) doctor.Result {
			if _, err := c.AuthenticatedLogin(ctx); err != nil {
				return doctor.Failure(doctor.Condition{Code: "azure_devops.code_host.authentication.failed", Origin: doctor.OriginEnvironment, Effect: doctor.EffectLocalMutation, Topic: "setup", Unobserved: true}, "Azure Repos authentication", "az cannot establish the configured Azure Repos identity", "verify az login and company policy; local Markdown removes Boards access only, and no alternative Azure authentication route is implemented")
			}
			return doctor.Success("azure_devops.code_host.authentication.ready", "Azure Repos authentication")
		},
		func(ctx context.Context) doctor.Result {
			identity, err := c.repositoryIdentity(ctx, repository)
			if err != nil {
				return azureCodeHostUnavailable()
			}
			token := "repoV2/" + identity.Project.ID + "/" + identity.ID
			for _, required := range gitPermissions {
				granted, err := c.hasPermission(ctx, repository.Owner, required, token)
				if err != nil || !granted {
					return azureCodeHostUnavailable()
				}
			}
			var pulls struct {
				Value []struct{} `json:"value"`
			}
			if err := c.call(ctx, request{Method: "GET", URL: repositoryURL(repository, "/pullrequests", url.Values{"searchCriteria.status": {"active"}, "$top": {"1"}})}, &pulls); err != nil {
				return azureCodeHostUnavailable()
			}
			if !strings.HasPrefix(identity.DefaultBranch, "refs/heads/") {
				return azureCodeHostUnavailable()
			}
			var refs struct {
				Value []struct {
					Name     string `json:"name"`
					ObjectID string `json:"objectId"`
				} `json:"value"`
			}
			if err := c.call(ctx, request{Method: "GET", URL: repositoryURL(repository, "/refs", url.Values{"filter": {strings.TrimPrefix(identity.DefaultBranch, "refs/")}})}, &refs); err != nil {
				return azureCodeHostUnavailable()
			}
			sha := ""
			for _, ref := range refs.Value {
				if ref.Name == identity.DefaultBranch {
					sha = ref.ObjectID
				}
			}
			if !codehost.ValidCommitSHA(sha) {
				return azureCodeHostUnavailable()
			}
			var statuses struct {
				Value []struct{} `json:"value"`
			}
			if err := c.call(ctx, request{Method: "GET", URL: repositoryURL(repository, "/commits/"+sha+"/statuses", url.Values{"top": {"1"}})}, &statuses); err != nil {
				return azureCodeHostUnavailable()
			}
			return doctor.Success("azure_devops.code_host.access.ready", "Azure Repos PR and status access")
		},
	}
}

// azureCodeHostUnavailable preserves an explicit unsupported full-path result.
func azureCodeHostUnavailable() doctor.Result {
	return doctor.Failure(doctor.Condition{Code: "azure_devops.code_host.access.unavailable", Origin: doctor.OriginEnvironment, Effect: doctor.EffectExternalMutation, Topic: "setup", Unobserved: true}, "Azure Repos PR and status access", "the az-based Azure Repos PR/review/status route is unavailable or lacks repository Contribute, Create branch, or Contribute to pull requests permissions", "verify az and company policy for Azure Repos APIs; a local tracker cannot bypass blocked PR/status access, and fully local/manual completion is unsupported")
}
