package azuredevops

// This file ports the Azure Repos code-host operations to the CLI extension
// route (ADR 0030): az repos pr for pull requests, az repos ref for refs,
// and verified az devops invoke routes for threads and commit statuses.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// extensionPullRequestResponse is the az repos pr projection of one pull
// request: the REST projection plus the repository it belongs to, which
// the organization-scoped commands require the adapter to check.
type extensionPullRequestResponse struct {
	pullRequestResponse
	Repository struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Project struct {
			Name string `json:"name"`
		} `json:"project"`
	} `json:"repository"`
}

// belongsTo refuses a pull request outside the registered project and
// repository, and outside the frozen repository GUID when repositoryID is
// not empty.
func (r extensionPullRequestResponse) belongsTo(repository tracker.Repository, repositoryID string) error {
	if !strings.EqualFold(r.Repository.Project.Name, repository.Project) || !strings.EqualFold(r.Repository.Name, repository.Name) {
		return fmt.Errorf("pull request %d belongs to another repository than %s", r.PullRequestID, repository)
	}
	if repositoryID != "" && !strings.EqualFold(r.Repository.ID, repositoryID) {
		return fmt.Errorf("pull request %d belongs to another repository than the frozen repository %s", r.PullRequestID, repositoryID)
	}
	return nil
}

// cliText renders flag and a free-text value as one "--flag=value"
// argument. One argument keeps a value that starts with '-' or holds lines
// from being parsed as options, and a multi-value flag such as
// --description then receives exactly one value, so its newline join
// returns the text unchanged. Azure CLI replaces a value that starts with
// '@' by a file's content, so such a value is refused before any call.
func cliText(flag, value string) (string, error) {
	if strings.HasPrefix(value, "@") {
		return "", fmt.Errorf("%s cannot start with '@' on the devops_extension route, because Azure CLI reads such a value from a file", strings.TrimPrefix(flag, "--"))
	}
	return flag + "=" + value, nil
}

// extensionPullRequestIDs lists, in host order, the pull requests of the
// registered repository in every status whose refs match the exact branch
// pair.
func (c *Client) extensionPullRequestIDs(ctx context.Context, repository tracker.Repository, headBranch, baseBranch string) ([]int, error) {
	source, err := cliText("--source-branch", headBranch)
	if err != nil {
		return nil, err
	}
	target, err := cliText("--target-branch", baseBranch)
	if err != nil {
		return nil, err
	}
	var response []extensionPullRequestResponse
	args := []string{"repos", "pr", "list", "--project", repository.Project, "--repository", repository.Name, source, target, "--status", "all", "--top", "100"}
	if err := c.extension(ctx, repository.Owner, args, &response); err != nil {
		return nil, fmt.Errorf("find pull request for %q: %w", headBranch, err)
	}
	var ids []int
	for _, candidate := range response {
		if candidate.SourceRefName != branchRef(headBranch) || candidate.TargetRefName != branchRef(baseBranch) {
			continue
		}
		if err := candidate.belongsTo(repository, ""); err != nil {
			return nil, fmt.Errorf("find pull request for %q: %w", headBranch, err)
		}
		ids = append(ids, candidate.PullRequestID)
	}
	return ids, nil
}

// extensionPullRequest reads one pull request with the organization-scoped
// az repos pr show and refuses one of another repository.
func (c *Client) extensionPullRequest(ctx context.Context, repository tracker.Repository, number int, repositoryID string) (extensionPullRequestResponse, error) {
	var response extensionPullRequestResponse
	if err := c.extension(ctx, repository.Owner, []string{"repos", "pr", "show", "--id", strconv.Itoa(number)}, &response); err != nil {
		return extensionPullRequestResponse{}, fmt.Errorf("read pull request %d: %w", number, err)
	}
	if response.PullRequestID != number {
		return extensionPullRequestResponse{}, fmt.Errorf("read pull request %d: the response names pull request %d", number, response.PullRequestID)
	}
	if err := response.belongsTo(repository, repositoryID); err != nil {
		return extensionPullRequestResponse{}, err
	}
	return response, nil
}

// pullRequestTextArgs renders the title and the description as one
// argument each.
func pullRequestTextArgs(request codehost.PullRequestRequest) ([]string, error) {
	title, err := cliText("--title", request.Title)
	if err != nil {
		return nil, err
	}
	description, err := cliText("--description", request.Body)
	if err != nil {
		return nil, err
	}
	return []string{title, description}, nil
}

// extensionCreatePullRequest creates one pull request with az repos pr
// create in the registered project and repository.
func (c *Client) extensionCreatePullRequest(ctx context.Context, repository tracker.Repository, request codehost.PullRequestRequest) (codehost.PullRequest, error) {
	text, err := pullRequestTextArgs(request)
	if err != nil {
		return codehost.PullRequest{}, err
	}
	source, err := cliText("--source-branch", request.HeadBranch)
	if err != nil {
		return codehost.PullRequest{}, err
	}
	targetBranch, err := cliText("--target-branch", request.BaseBranch)
	if err != nil {
		return codehost.PullRequest{}, err
	}
	target, frozen, workItems, err := c.pullRequestTarget(ctx, repository, request.WorkItem)
	if err != nil {
		return codehost.PullRequest{}, fmt.Errorf("create pull request: %w", err)
	}
	args := append([]string{"repos", "pr", "create", "--project", repository.Project, "--repository", target, source, targetBranch}, text...)
	args = append(append(args, "--draft", strconv.FormatBool(request.Draft)), workItems...)
	var response extensionPullRequestResponse
	if err := c.extension(ctx, repository.Owner, args, &response); err != nil {
		return codehost.PullRequest{}, fmt.Errorf("create pull request: %w", err)
	}
	if err := response.belongsTo(repository, frozen); err != nil {
		return codehost.PullRequest{}, fmt.Errorf("create pull request: %w", err)
	}
	return response.pullRequest(repository), nil
}

// pullRequestTarget returns the --repository value of a new pull request,
// the frozen repository GUID its response must name, and the --work-items
// arguments. Without a work item it targets the registered repository
// name. With one, the registered repository must still resolve to the
// GUID frozen with the Run; the pull request is then created in that GUID
// with the source work item, so a renamed or replaced repository receives
// nothing.
func (c *Client) pullRequestTarget(ctx context.Context, repository tracker.Repository, link *codehost.WorkItemLink) (string, string, []string, error) {
	if link == nil {
		return repository.Name, "", nil, nil
	}
	if link.ID <= 0 || !canonicalGUID.MatchString(link.RepositoryID) {
		return "", "", nil, errors.New("the source work item needs a positive ID and a frozen repository GUID")
	}
	identity, err := c.ResolveRepository(ctx, repository)
	if err != nil {
		return "", "", nil, err
	}
	if !strings.EqualFold(identity.ID, link.RepositoryID) {
		return "", "", nil, fmt.Errorf("registered repository %s resolves to %s, not the frozen repository %s of the Run", repository, identity.ID, strings.ToLower(link.RepositoryID))
	}
	return identity.ID, identity.ID, []string{"--work-items", strconv.Itoa(link.ID)}, nil
}

// EnsurePullRequestWorkItem verifies that pull request number of the
// frozen repository links the source work item natively, and adds the
// association through az repos pr work-item add when a lost response or an
// adopted pull request left it out. It never creates a pull request, and it
// fails unless the association list confirms the link.
func (c *Client) EnsurePullRequestWorkItem(ctx context.Context, repository tracker.Repository, number int, link codehost.WorkItemLink) error {
	if !c.extensionRoute() {
		return errNoWorkItemRoute
	}
	if err := validatePullRequestTarget(repository, number); err != nil {
		return err
	}
	if link.ID <= 0 || !canonicalGUID.MatchString(link.RepositoryID) {
		return errors.New("the source work item needs a positive ID and a frozen repository GUID")
	}
	if _, err := c.extensionPullRequest(ctx, repository, number, link.RepositoryID); err != nil {
		return fmt.Errorf("link work item %d: %w", link.ID, err)
	}
	linked, err := c.pullRequestLinksWorkItem(ctx, repository, number, link.ID)
	if err != nil || linked {
		return err
	}
	args := []string{"repos", "pr", "work-item", "add", "--id", strconv.Itoa(number), "--work-items", strconv.Itoa(link.ID)}
	if err := c.extension(ctx, repository.Owner, args, nil); err != nil {
		return fmt.Errorf("link work item %d to pull request %d: %w", link.ID, number, err)
	}
	linked, err = c.pullRequestLinksWorkItem(ctx, repository, number, link.ID)
	if err != nil {
		return err
	}
	if !linked {
		return fmt.Errorf("pull request %d does not list work item %d after linking it", number, link.ID)
	}
	return nil
}

// pullRequestLinksWorkItem reports whether az repos pr work-item list names
// work item id for pull request number.
func (c *Client) pullRequestLinksWorkItem(ctx context.Context, repository tracker.Repository, number, id int) (bool, error) {
	var response []struct {
		ID int `json:"id"`
	}
	if err := c.extension(ctx, repository.Owner, []string{"repos", "pr", "work-item", "list", "--id", strconv.Itoa(number)}, &response); err != nil {
		return false, fmt.Errorf("list work items of pull request %d: %w", number, err)
	}
	for _, item := range response {
		if item.ID == id {
			return true, nil
		}
	}
	return false, nil
}

// extensionUpdatePullRequest checks that pull request number belongs to the
// registered repository and then runs az repos pr update with the
// command-specific arguments.
func (c *Client) extensionUpdatePullRequest(ctx context.Context, repository tracker.Repository, number int, changes []string, action string) (codehost.PullRequest, error) {
	if _, err := c.extensionPullRequest(ctx, repository, number, ""); err != nil {
		return codehost.PullRequest{}, fmt.Errorf("%s: %w", action, err)
	}
	var response extensionPullRequestResponse
	args := append([]string{"repos", "pr", "update", "--id", strconv.Itoa(number)}, changes...)
	if err := c.extension(ctx, repository.Owner, args, &response); err != nil {
		return codehost.PullRequest{}, fmt.Errorf("%s: %w", action, err)
	}
	if err := response.belongsTo(repository, ""); err != nil {
		return codehost.PullRequest{}, fmt.Errorf("%s: %w", action, err)
	}
	return response.pullRequest(repository), nil
}

// gitInvoke runs one verified git-area az devops invoke route below the
// registered repository: pullRequestThreads, pullRequestThreadComments, or
// statuses (ADR 0030). The project and repository are always route
// parameters, so the call never depends on an organization-scoped lookup.
func (c *Client) gitInvoke(ctx context.Context, repository tracker.Repository, method, resource string, route, query map[string]string, body, destination any) error {
	parameters := map[string]string{"project": repository.Project, "repositoryId": repository.Name}
	for key, value := range route {
		parameters[key] = value
	}
	return c.invoke(ctx, repository.Owner, invokeRequest{Area: "git", Resource: resource, Method: method, APIVersion: apiVersion, Route: parameters, Query: query, Body: body}, destination)
}

// extensionReviewerLogins checks that pull request number belongs to the
// registered repository, because az repos pr reviewer list is
// organization-scoped and its output names no repository, and then reads
// the reviewers.
func (c *Client) extensionReviewerLogins(ctx context.Context, repository tracker.Repository, number int) (map[string]string, error) {
	if _, err := c.extensionPullRequest(ctx, repository, number, ""); err != nil {
		return nil, fmt.Errorf("list reviewers of pull request %d: %w", number, err)
	}
	var response []struct {
		ID         string `json:"id"`
		UniqueName string `json:"uniqueName"`
	}
	if err := c.extension(ctx, repository.Owner, []string{"repos", "pr", "reviewer", "list", "--id", strconv.Itoa(number)}, &response); err != nil {
		return nil, fmt.Errorf("list reviewers of pull request %d: %w", number, err)
	}
	logins := make(map[string]string, len(response))
	for _, reviewer := range response {
		logins[identityKey(reviewer.ID)] = reviewer.UniqueName
	}
	return logins, nil
}

// errNoWorkItemRoute reports a work-item association on the az rest route,
// where no Run carries a frozen Azure Boards repository.
var errNoWorkItemRoute = errors.New("native pull-request work-item association requires the devops_extension route")
