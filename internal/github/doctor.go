package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/doctor"
)

// DoctorClient is the read-only GitHub health seam used by startup diagnosis.
// It is separate from Client so diagnosing a repository cannot accidentally
// gain mutation authority.
type DoctorClient interface {
	CheckAuthentication(context.Context) error
	CheckRepositoryAccess(context.Context, Repository) error
	CheckFactoryLabels(context.Context, Repository) error
}

// StartupChecks returns the GitHub-owned authentication, permission, and
// factory-label checks in deterministic order.
func StartupChecks(client DoctorClient, repository Repository) []doctor.Check {
	return []doctor.Check{
		func(ctx context.Context) doctor.Result {
			if client == nil {
				return doctor.Failure("github authentication", "the GitHub diagnosis adapter is unavailable", "configure the authenticated gh client")
			}
			if err := client.CheckAuthentication(ctx); err != nil {
				return doctor.Failure("github authentication", "the GitHub CLI is not authenticated", "run gh auth login for the GitHub account that owns the registered repository")
			}
			return doctor.Success("github authentication")
		},
		func(ctx context.Context) doctor.Result {
			if client == nil {
				return doctor.Failure("github permissions", "the GitHub diagnosis adapter is unavailable", "configure the authenticated gh client")
			}
			if err := client.CheckRepositoryAccess(ctx, repository); err != nil {
				return doctor.Failure("github permissions", "the authenticated account cannot perform the factory's repository operations", "grant repository read, issue-label, issue-comment, contents, pull-request, and commit-status permissions")
			}
			return doctor.Success("github permissions")
		},
		func(ctx context.Context) doctor.Result {
			if client == nil {
				return doctor.Failure("github labels", "the GitHub diagnosis adapter is unavailable", "configure the authenticated gh client")
			}
			if err := client.CheckFactoryLabels(ctx, repository); err != nil {
				return doctor.Failure("github labels", "one or more factory state labels are missing", "run factory bootstrap-labels after granting label-management permission")
			}
			return doctor.Success("github labels")
		},
	}
}

// CheckAuthentication verifies that the local gh process has a usable
// GitHub authentication without reading or printing its credential.
func (c *GhClient) CheckAuthentication(ctx context.Context) error {
	if _, err := c.runner().Run(ctx, []string{"auth", "status", "--hostname", "github.com"}, nil); err != nil {
		return errors.New("gh authentication status failed")
	}
	return nil
}

// CheckRepositoryAccess verifies the registered repository exists and exposes
// the write permissions required for labels, comments, commits, and pull
// requests. The API response is discarded after this bounded validation.
func (c *GhClient) CheckRepositoryAccess(ctx context.Context, repository Repository) error {
	if err := validateDoctorRepository(repository); err != nil {
		return err
	}
	var response doctorRepositoryResponse
	if err := c.callJSON(ctx, []string{"api", fmt.Sprintf("repos/%s", repository.String())}, nil, &response); err != nil {
		return errors.New("registered GitHub repository is not readable")
	}
	if !response.Permissions.Pull {
		return errors.New("registered GitHub repository is not readable")
	}
	if !response.Permissions.Push {
		return errors.New("registered GitHub repository does not permit contents writes")
	}
	if !response.Permissions.Triage && !response.Permissions.Maintain && !response.Permissions.Admin {
		return errors.New("registered GitHub repository does not permit issue and pull-request supervision")
	}
	return nil
}

// CheckFactoryLabels verifies all factory-owned labels exist without creating
// or changing any label.
func (c *GhClient) CheckFactoryLabels(ctx context.Context, repository Repository) error {
	if err := validateDoctorRepository(repository); err != nil {
		return err
	}
	output, err := c.callBytes(ctx, []string{"api", fmt.Sprintf("repos/%s/labels", repository.String()), "--paginate", "--slurp"}, nil)
	if err != nil {
		return errors.New("factory labels are not readable")
	}
	labels, err := decodeDoctorLabels(output)
	if err != nil {
		return errors.New("factory labels response is malformed")
	}
	seen := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		seen[label.Name] = struct{}{}
	}
	for _, required := range FactoryStateLabels {
		if _, ok := seen[required]; !ok {
			return fmt.Errorf("factory label %q is missing", required)
		}
	}
	return nil
}

// validateDoctorRepository rejects values that could alter a read-only API
// endpoint assembled by the GitHub adapter.
func validateDoctorRepository(repository Repository) error {
	if strings.TrimSpace(repository.Owner) == "" || strings.TrimSpace(repository.Name) == "" {
		return errors.New("GitHub repository owner and name are required")
	}
	if strings.ContainsAny(repository.Owner+repository.Name, "\x00\r\n") || strings.Contains(repository.Owner, "/") || strings.Contains(repository.Name, "/") {
		return errors.New("GitHub repository owner and name are unsafe")
	}
	return nil
}

// doctorRepositoryResponse is the small repository API projection needed for
// permission diagnosis.
type doctorRepositoryResponse struct {
	Permissions struct {
		Pull     bool `json:"pull"`
		Push     bool `json:"push"`
		Triage   bool `json:"triage"`
		Maintain bool `json:"maintain"`
		Admin    bool `json:"admin"`
	} `json:"permissions"`
}

// doctorLabelResponse is the small label API projection needed by diagnosis.
type doctorLabelResponse struct {
	Name string `json:"name"`
}

// decodeDoctorLabels accepts gh's paginated slurp shape and a plain page for
// compatibility with controlled command-runner tests.
func decodeDoctorLabels(output []byte) ([]doctorLabelResponse, error) {
	var pages [][]doctorLabelResponse
	if err := json.Unmarshal(output, &pages); err == nil {
		labels := make([]doctorLabelResponse, 0)
		for _, page := range pages {
			labels = append(labels, page...)
		}
		return labels, nil
	}
	var page []doctorLabelResponse
	if err := json.Unmarshal(output, &page); err != nil {
		return nil, err
	}
	return page, nil
}

var _ DoctorClient = (*GhClient)(nil)
