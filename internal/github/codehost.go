package github

import (
	"context"
	"errors"
	"fmt"

	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// CodeHostClient explicitly supplies GitHub PR conversation streams when the
// work tracker has a separate namespace. Legacy combined clients stay unchanged.
type CodeHostClient struct{ *GhClient }

var _ codehost.PullRequestCommentReader = (*CodeHostClient)(nil)
var _ codehost.PullRequestCommentClient = (*CodeHostClient)(nil)
var _ codehost.ReadinessChecker = (*GhClient)(nil)

// NewCodeHostClient creates a host-only projection of the existing gh adapter.
func NewCodeHostClient() *CodeHostClient { return &CodeHostClient{GhClient: NewClient()} }

// PullRequestComments reads the configured PR conversation, using GitHub's
// conversation endpoint shared with issues, never a local issue number lookup.
func (c *CodeHostClient) PullRequestComments(ctx context.Context, repository tracker.Repository, number int) ([]tracker.Comment, error) {
	return c.IssueComments(ctx, repository, number)
}

// FindPullRequestComment adopts a coordinator-authored PR conversation marker.
func (c *CodeHostClient) FindPullRequestComment(ctx context.Context, repository tracker.Repository, number int, marker string) (tracker.Comment, error) {
	return c.FindStatusComment(ctx, repository, number, marker)
}

// CreatePullRequestComment publishes to the independently selected PR stream.
func (c *CodeHostClient) CreatePullRequestComment(ctx context.Context, repository tracker.Repository, number int, body string) (tracker.Comment, error) {
	return c.CreateIssueComment(ctx, repository, number, body)
}

// EditPullRequestComment edits only a code-host comment identity.
func (c *CodeHostClient) EditPullRequestComment(ctx context.Context, repository tracker.Repository, _ int, id, body string) error {
	return c.EditIssueComment(ctx, repository, id, body)
}

// CodeHostStartupChecks checks gh, Git contents, PRs/reviews, and statuses without
// requiring issue triage or factory labels. It performs read-only probes only.
func (c *GhClient) CodeHostStartupChecks(repository tracker.Repository) []doctor.Check {
	return []doctor.Check{
		func(ctx context.Context) doctor.Result {
			if err := c.CheckAuthentication(ctx); err != nil {
				return doctor.Failure(doctor.Condition{Code: "github.code_host.authentication.failed", Origin: doctor.OriginEnvironment, Effect: doctor.EffectLocalMutation, Topic: "setup", Unobserved: true}, "GitHub code-host authentication", "gh cannot authenticate to the configured code host", "run gh auth login; local Markdown replaces issue access, not code-host access")
			}
			return doctor.Success("github.code_host.authentication.ready", "GitHub code-host authentication")
		},
		func(ctx context.Context) doctor.Result {
			if err := c.CheckCodeHostAccess(ctx, repository); err != nil {
				return doctor.Failure(doctor.Condition{Code: "github.code_host.access.unavailable", Origin: doctor.OriginEnvironment, Effect: doctor.EffectExternalMutation, Topic: "setup", Unobserved: true}, "GitHub PR and status access", "the code-host PR/review/status path is unavailable or its write permissions cannot be established", "verify gh network/authentication and contents, pull-request, and commit-status permissions; a local tracker alone cannot complete an issue-to-PR run")
			}
			return doctor.Success("github.code_host.access.ready", "GitHub PR and status access")
		},
	}
}

// CheckCodeHostAccess validates repository permissions and the read surfaces of
// PRs and statuses without contacting GitHub Issues or label resources.
func (c *GhClient) CheckCodeHostAccess(ctx context.Context, repository tracker.Repository) error {
	if err := validateRepository(repository); err != nil {
		return err
	}
	var response struct {
		Private       bool   `json:"private"`
		DefaultBranch string `json:"default_branch"`
		Permissions   struct {
			Pull bool `json:"pull"`
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	root := fmt.Sprintf("repos/%s", repository.String())
	if err := c.callJSON(ctx, []string{"api", root}, nil, &response); err != nil {
		return err
	}
	if !response.Permissions.Pull || !response.Permissions.Push {
		return errors.New("code-host repository read/write permission unavailable")
	}
	if err := c.checkTokenPermissions(ctx, response.Private); err != nil {
		return err
	}
	var pulls []struct {
		Number int `json:"number"`
	}
	if err := c.callJSON(ctx, []string{"api", root + "/pulls?per_page=1"}, nil, &pulls); err != nil {
		return err
	}
	if response.DefaultBranch == "" {
		return errors.New("code-host default branch unavailable")
	}
	var branch struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := c.callJSON(ctx, []string{"api", root + "/branches/" + response.DefaultBranch}, nil, &branch); err != nil {
		return err
	}
	if !codehost.ValidCommitSHA(branch.Commit.SHA) {
		return errors.New("code-host branch commit unavailable")
	}
	var statuses struct {
		Statuses []struct{} `json:"statuses"`
	}
	return c.callJSON(ctx, []string{"api", root + "/commits/" + branch.Commit.SHA + "/status"}, nil, &statuses)
}
