package azuredevops

import (
	"context"
	"net/url"

	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// backend is the route-specific half of the adapter (ADR 0030). Client
// keeps the route-neutral validation, mapping, and caches, and composes the
// backend that its Route selects for every operation whose commands differ
// by route. A new route implements backend and adds one case to
// Client.backend; no public method changes.
type backend interface {
	identityBackend
	boardsBackend
	commentBackend
	planBackend
	assignmentBackend
	pullRequestBackend
}

// pullRequestBackend holds the route's Azure Repos operations.
type pullRequestBackend interface {
	// pullRequestIDs lists the pull requests of the exact branch pair.
	pullRequestIDs(ctx context.Context, repository tracker.Repository, headBranch, baseBranch string) ([]int, error)
	// readPullRequest reads one pull request with its full description.
	readPullRequest(ctx context.Context, repository tracker.Repository, number int) (codehost.PullRequest, error)
	// createPullRequest creates one pull request.
	createPullRequest(ctx context.Context, repository tracker.Repository, request codehost.PullRequestRequest) (codehost.PullRequest, error)
	// updatePullRequest replaces the title and description.
	updatePullRequest(ctx context.Context, repository tracker.Repository, number int, request codehost.PullRequestRequest) (codehost.PullRequest, error)
	// setPullRequestDraft sets or clears the draft flag.
	setPullRequestDraft(ctx context.Context, repository tracker.Repository, number int, draft bool) (codehost.PullRequest, error)
	// reviewerLogins maps reviewer identity keys to logins.
	reviewerLogins(ctx context.Context, repository tracker.Repository, number int) (map[string]string, error)
	// postCommitStatus posts one commit status payload.
	postCommitStatus(ctx context.Context, repository tracker.Repository, sha string, payload map[string]any) error
	// readCommitStatuses decodes one page of commit statuses, starting at
	// skip, into destination.
	readCommitStatuses(ctx context.Context, repository tracker.Repository, sha string, skip int, destination any) error
	// readThreads decodes the threads of one pull request into destination.
	readThreads(ctx context.Context, repository tracker.Repository, number int, destination any) error
	// createThread posts one thread and decodes it into destination.
	createThread(ctx context.Context, repository tracker.Repository, number int, payload map[string]any, destination any) error
	// editThreadComment replaces the content of one thread comment.
	editThreadComment(ctx context.Context, repository tracker.Repository, number, thread, id int, body string) error
}

// assignmentBackend holds the repository-assignment checks of ADR 0030.
// The az rest route has no assignment: it accepts every claim and plan item
// and refuses a native pull-request association.
type assignmentBackend interface {
	// claimRepositoryAssignment validates the assignment of issue right
	// before a claim, and returns the identity to freeze, or nil.
	claimRepositoryAssignment(ctx context.Context, repository tracker.Repository, issue tracker.Issue) (*tracker.RepositoryAssignment, error)
	// verifyPlanItem checks an item found by correlation against the
	// approved item.
	verifyPlanItem(ctx context.Context, repository tracker.Repository, item tracker.RenderedPlanItem, ref tracker.PlanItemRef) error
	// ensurePullRequestWorkItem verifies, and adds when missing, the
	// native link of a pull request to its source work item.
	ensurePullRequestWorkItem(ctx context.Context, repository tracker.Repository, number int, link codehost.WorkItemLink) error
}

// planBackend holds the route's plan preflight reads and plan writes.
type planBackend interface {
	// checkPlanSupport refuses, before any call, a graph that the route
	// cannot publish.
	checkPlanSupport(graph tracker.PlanGraph) error
	// preparePlan checks permissions and, on the extension route, assigns
	// repository targets to preview.
	preparePlan(ctx context.Context, repository tracker.Repository, mapping config.AzurePlanMapping, graph tracker.PlanGraph, preview *tracker.PlanPreview) error
	// planRead decodes one project resource for preflight: the az rest
	// path, or the az devops invoke pair read, into destination.
	planRead(ctx context.Context, repository tracker.Repository, path string, query url.Values, read invokeRequest, destination any) error
	// listLinkTypes reads the work item link types of the organization.
	listLinkTypes(ctx context.Context, repository tracker.Repository) ([]linkType, error)
	// checkLinkSelectable refuses an enabled link type that the route
	// cannot select.
	checkLinkSelectable(types []linkType, link string) error
	// createPlanItem creates one rendered plan item.
	createPlanItem(ctx context.Context, repository tracker.Repository, item tracker.RenderedPlanItem) (tracker.PlanItemRef, error)
	// findPlanItems returns the work items that carry the correlation tag.
	findPlanItems(ctx context.Context, repository tracker.Repository, correlation string) ([]tracker.PlanItemRef, error)
	// createPlanRelationship adds the link reference from work item from
	// to work item to.
	createPlanRelationship(ctx context.Context, repository tracker.Repository, from, to int, link string) error
	// findPlanRelationship reports whether that link exists.
	findPlanRelationship(ctx context.Context, repository tracker.Repository, from, to int, link string) (bool, error)
}

// identityBackend holds the route's process and identity operations.
type identityBackend interface {
	// defaultRunner is the production az runner of the route.
	defaultRunner() CommandRunner
	// readLogin reads the account name of the signed-in identity.
	readLogin(ctx context.Context) (string, error)
	// resolveRepository resolves the registered repository to its stable
	// identity.
	resolveRepository(ctx context.Context, repository tracker.Repository) (RepositoryIdentity, error)
	// startupChecks are the tracker checks of factory doctor.
	startupChecks(repository tracker.Repository) []doctor.Check
	// codeHostStartupChecks are the code-host checks of factory doctor.
	codeHostStartupChecks(repository tracker.Repository) []doctor.Check
}

// boardsBackend holds the route's work item reads and tag writes.
type boardsBackend interface {
	// workItem reads one work item of the project.
	workItem(ctx context.Context, repository tracker.Repository, number int) (workItemResponse, error)
	// workItems reads one batch of work items of the project.
	workItems(ctx context.Context, repository tracker.Repository, ids []int) ([]workItemResponse, error)
	// eligibleIssues returns the open, authorized agent-ready issues.
	eligibleIssues(ctx context.Context, repository tracker.Repository) ([]tracker.Issue, error)
	// readTypeStates decodes the states of one work item type into
	// destination.
	readTypeStates(ctx context.Context, repository tracker.Repository, workItemType string, destination any) error
	// readUpdates decodes one page of the update history of a work item,
	// starting at skip, into destination.
	readUpdates(ctx context.Context, repository tracker.Repository, number, skip int, destination any) error
	// replaceIssueLabels replaces the tag list of one work item.
	replaceIssueLabels(ctx context.Context, repository tracker.Repository, number int, labels []string) error
}

// commentBackend holds the route's work item comment operations.
type commentBackend interface {
	// readComments decodes one page of the comments of a work item into
	// destination.
	readComments(ctx context.Context, repository tracker.Repository, number int, query url.Values, destination any) error
	// createComment posts one Markdown comment on a work item.
	createComment(ctx context.Context, repository tracker.Repository, number int, body string) (tracker.Comment, error)
	// editComment replaces the text of one work item comment.
	editComment(ctx context.Context, repository tracker.Repository, number, id int, body string) error
	// findStatusComment returns the status comment that holds marker, or a
	// zero comment.
	findStatusComment(ctx context.Context, repository tracker.Repository, number int, marker string) (tracker.Comment, error)
}

// restBackend is the az rest route: every call is one az rest command
// authenticated by az login. It embeds the client for its runner, caches,
// and shared helpers.
type restBackend struct{ *Client }

// extensionBackend is the Azure DevOps CLI extension route: az boards, az
// repos, and az devops invoke, authenticated by az devops login. It embeds
// the client for its runner, caches, and shared helpers.
type extensionBackend struct{ *Client }

var (
	_ backend = restBackend{}
	_ backend = extensionBackend{}
)

// backend returns the backend of the client's route. It reads Route on each
// call, so a client built as a literal or changed after construction
// selects the route it names.
func (c *Client) backend() backend {
	if c.Route == config.AzureRouteDevOpsExtension {
		return extensionBackend{c}
	}
	return restBackend{c}
}
