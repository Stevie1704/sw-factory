// Package github contains the host-side GitHub adapter used by the coordinator.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/ref"
)

const (
	// LabelAgentReady authorizes an issue for a factory claim.
	LabelAgentReady = "agent-ready"
	// LabelAgentRunning marks an active factory run.
	LabelAgentRunning = "agent-running"
	// LabelAgentNeedsInput marks a run waiting for human input.
	LabelAgentNeedsInput = "agent-needs-input"
	// LabelAgentFailed marks a failed factory run.
	LabelAgentFailed = "agent-failed"
	// LabelAgentCancelled marks a cancelled factory run.
	LabelAgentCancelled = "agent-cancelled"
	// LabelAgentComplete marks a completed factory run.
	LabelAgentComplete = "agent-complete"
)

// FactoryStateLabels is the complete set of labels owned by the factory.
// Claiming and transitioning only replace labels from this set; ordinary issue
// labels are preserved.
var FactoryStateLabels = []string{
	LabelAgentReady,
	LabelAgentRunning,
	LabelAgentNeedsInput,
	LabelAgentFailed,
	LabelAgentCancelled,
	LabelAgentComplete,
}

// Repository identifies a GitHub repository.
type Repository struct {
	Owner string
	Name  string
}

// String returns the owner/name form accepted by the GitHub CLI.
func (r Repository) String() string { return r.Owner + "/" + r.Name }

// Issue is the content-free GitHub issue snapshot needed by a claim.
type Issue struct {
	Number        int
	Title         string
	Body          string
	State         string
	Labels        []string
	IsPullRequest bool
	UpdatedAt     time.Time
}

// LeaseStatusContext is the stable Commit Status context used to expose the
// coordinator's renewable GitHub lease on the configured target branch.
const LeaseStatusContext = "factory/lease"

// Lease describes one visible coordinator ownership heartbeat.
type Lease struct {
	// TargetBranch is the branch whose current commit receives the status.
	TargetBranch string
	// Coordinator identifies the host holding the lease.
	Coordinator string
	// RunID identifies the active run, when one has been claimed.
	RunID string
	// RenewedAt is the coordinator's latest heartbeat time.
	RenewedAt time.Time
	// ExpiresAt is the time after which the GitHub projection is stale.
	ExpiresAt time.Time
}

// Label describes a factory-owned GitHub label.
type Label struct {
	Name        string
	Description string
	Color       string
}

// Comment is the coordinator-neutral identity and revision of a GitHub issue
// or pull-request comment.
type Comment struct {
	// ID is the immutable GitHub comment identity used as a replay watermark.
	ID string
	// Body is the complete user-authored comment text.
	Body string
	// Author is the GitHub login that authored the comment.
	Author string
	// UpdatedAt is the current edit revision of the comment.
	UpdatedAt time.Time
}

// CommentReader lists comments for an issue or pull request through the
// shared GitHub issue-comments endpoint.
type CommentReader interface {
	IssueComments(context.Context, Repository, int) ([]Comment, error)
}

// IssuePoller lists the repository's open, agent-authorized issue queue.
type IssuePoller interface {
	ListEligibleIssues(context.Context, Repository) ([]Issue, error)
}

// LeaseClient publishes a renewable, operator-visible coordinator lease.
type LeaseClient interface {
	RenewLease(context.Context, Repository, Lease) error
}

// PullRequest is the pull-request identity and body returned to the
// coordinator after a host-side GitHub operation.
type PullRequest struct {
	// Number is the repository-local pull-request number.
	Number int
	// URL is the browser URL for operator supervision.
	URL string
	// Title is the pull-request title.
	Title string
	// Body is the current complete pull-request body.
	Body string
	// State is the GitHub lifecycle state.
	State string
	// Draft reports whether GitHub marks the pull request as a draft.
	Draft bool
	// Merged reports whether GitHub has completed the pull request merge.
	Merged bool
	// MergeCommitSHA is the immutable commit created by a successful merge.
	MergeCommitSHA string
	// HeadBranch is the source branch name.
	HeadBranch string
	// HeadSHA is the exact commit currently referenced by the source branch.
	HeadSHA string
	// BaseBranch is the target branch name.
	BaseBranch string
}

// PullRequestRequest contains the stable fields used to create or update a
// draft pull request.
type PullRequestRequest struct {
	// Title is the pull-request title.
	Title string
	// Body is the complete body, including any preserved human-authored text.
	Body string
	// HeadBranch is the source branch to publish.
	HeadBranch string
	// BaseBranch is the target branch to merge into.
	BaseBranch string
	// Draft requests a draft pull request on creation and update.
	Draft bool
}

// CommitStatusState is the GitHub state vocabulary used for deterministic
// checkpoint results.
type CommitStatusState string

const (
	// CommitStatusPending is an in-progress status state.
	CommitStatusPending CommitStatusState = "pending"
	// CommitStatusSuccess marks a successful checkpoint result.
	CommitStatusSuccess CommitStatusState = "success"
	// CommitStatusFailure marks a declared command that exited unsuccessfully.
	CommitStatusFailure CommitStatusState = "failure"
	// CommitStatusError marks setup or runtime infrastructure failure.
	CommitStatusError CommitStatusState = "error"
)

// CommitStatus identifies one status attached to one exact commit SHA.
type CommitStatus struct {
	// SHA is the immutable commit being reported.
	SHA string
	// State is the GitHub status state.
	State CommitStatusState
	// Context is the stable status context used for repeated reports.
	Context string
	// Description is a content-free human-readable summary.
	Description string
	// TargetURL is an optional operator-facing evidence URL.
	TargetURL string
}

// CommitStatusPublisher is the host-side seam for publishing exact-SHA
// Commit Statuses without exposing GitHub credentials to workflow code.
type CommitStatusPublisher interface {
	CreateCommitStatus(context.Context, Repository, CommitStatus) error
}

// CommitStatusReader is the read-only projection used to recognize a status
// that GitHub accepted before the coordinator process stopped.
type CommitStatusReader interface {
	ListCommitStatuses(context.Context, Repository, string) ([]CommitStatus, error)
}

// Client is the small host-side seam used by the claim coordinator. It keeps
// GitHub credentials inside the local gh process and returns only workflow
// data to the coordinator.
type Client interface {
	Issue(context.Context, Repository, int) (Issue, error)
	CreateLabel(context.Context, Repository, Label) error
	ReplaceIssueLabels(context.Context, Repository, int, []string) error
	CreateIssueComment(context.Context, Repository, int, string) (Comment, error)
	FindStatusComment(context.Context, Repository, int, string) (Comment, error)
	EditIssueComment(context.Context, Repository, string, string) error
}

// PullRequestClient is the host-side seam for idempotent draft pull-request
// discovery and mutation. It is separate from Client so existing issue/state
// adapters do not gain GitHub pull-request authority accidentally.
type PullRequestClient interface {
	FindPullRequest(context.Context, Repository, string, string) (PullRequest, error)
	CreatePullRequest(context.Context, Repository, PullRequestRequest) (PullRequest, error)
	UpdatePullRequest(context.Context, Repository, int, PullRequestRequest) (PullRequest, error)
}

// PullRequestReviewState is the bounded GitHub review decision vocabulary.
// Only a submitted review carries one; an unsubmitted draft is `PENDING`.
type PullRequestReviewState string

const (
	// PullRequestReviewPending is an unsubmitted review draft. It is visible
	// only to its author and must never start factory work.
	PullRequestReviewPending PullRequestReviewState = "PENDING"
	// PullRequestReviewCommented is a submitted review without a decision.
	PullRequestReviewCommented PullRequestReviewState = "COMMENTED"
	// PullRequestReviewApproved is a submitted approval.
	PullRequestReviewApproved PullRequestReviewState = "APPROVED"
	// PullRequestReviewChangesRequested is the only review decision the
	// coordinator treats as a progression event.
	PullRequestReviewChangesRequested PullRequestReviewState = "CHANGES_REQUESTED"
	// PullRequestReviewDismissed is a decision a maintainer later withdrew.
	PullRequestReviewDismissed PullRequestReviewState = "DISMISSED"
)

// PullRequestReviewComment is one inline, file-anchored review finding.
type PullRequestReviewComment struct {
	// Path is the repository-relative file the reviewer annotated.
	Path string
	// Line is the annotated line in the file, or zero when GitHub reports none.
	Line int
	// Body is the reviewer's complete comment text.
	Body string
}

// PullRequestReview is one completed human review of a tracked pull request.
type PullRequestReview struct {
	// ID is the immutable GitHub review identity used as a replay watermark.
	ID string
	// Author is the GitHub login that submitted the review.
	Author string
	// State is the submitted review decision.
	State PullRequestReviewState
	// Body is the completed review summary text.
	Body string
	// URL is the browser URL for operator supervision.
	URL string
	// SubmittedAt is the submission time. It is the zero value while GitHub
	// still holds the review as an unsubmitted draft.
	SubmittedAt time.Time
	// Comments contains the review's inline findings in GitHub order.
	Comments []PullRequestReviewComment
}

// PullRequestReviewReader is the read-only seam for observing completed human
// reviews. It is separate from the mutation clients because the factory never
// submits, approves, dismisses, or merges a review.
type PullRequestReviewReader interface {
	PullRequestReviews(context.Context, Repository, int) ([]PullRequestReview, error)
}

// PullRequestDraftClient owns the explicit draft/readiness mutation for an
// existing pull request. Keeping it separate prevents body updates from
// accidentally changing merge readiness.
type PullRequestDraftClient interface {
	SetPullRequestDraft(context.Context, Repository, int, bool) (PullRequest, error)
}

// CommandRunner is the executable seam for the local GitHub CLI adapter.
type CommandRunner interface {
	Run(context.Context, []string, []byte) ([]byte, error)
}

// commandRunner executes the host gh binary.
type commandRunner struct{}

// Run executes gh with optional JSON input supplied through stdin.
func (commandRunner) Run(ctx context.Context, args []string, input []byte) ([]byte, error) {
	command := exec.CommandContext(ctx, "gh", args...)
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			return nil, err
		}
		return nil, fmt.Errorf("gh %s: %s: %w", strings.Join(args, " "), message, err)
	}
	return stdout.Bytes(), nil
}

// GhClient invokes the locally authenticated GitHub CLI. The adapter never
// reads or stores the CLI credential; gh resolves it for each command.
// GhClient implements Client through the locally authenticated gh executable.
type GhClient struct {
	Runner CommandRunner
}

var _ CommentReader = (*GhClient)(nil)
var _ IssuePoller = (*GhClient)(nil)
var _ LeaseClient = (*GhClient)(nil)
var _ CommitStatusReader = (*GhClient)(nil)
var _ PullRequestDraftClient = (*GhClient)(nil)

// NewClient returns a GitHub CLI-backed client.
func NewClient() *GhClient { return &GhClient{Runner: commandRunner{}} }

// runner returns the injected command runner or the production CLI runner.
func (c *GhClient) runner() CommandRunner {
	if c.Runner == nil {
		return commandRunner{}
	}
	return c.Runner
}

// Issue fetches one issue snapshot from GitHub.
func (c *GhClient) Issue(ctx context.Context, repository Repository, number int) (Issue, error) {
	if number <= 0 {
		return Issue{}, errors.New("issue number must be positive")
	}
	var response issueResponse
	if err := c.callJSON(ctx, []string{"api", fmt.Sprintf("repos/%s/issues/%d", repository.String(), number)}, nil, &response); err != nil {
		return Issue{}, fmt.Errorf("fetch issue #%d: %w", number, err)
	}
	return response.issue(), nil
}

// ListEligibleIssues reads open issues labeled agent-ready and returns them in
// ascending issue-number order. GitHub's issues endpoint also returns pull
// requests, so those are filtered before the result crosses the adapter seam.
func (c *GhClient) ListEligibleIssues(ctx context.Context, repository Repository) ([]Issue, error) {
	args := []string{
		"api", fmt.Sprintf("repos/%s/issues", repository.String()),
		"--paginate", "--slurp", "--method", "GET",
		"-f", "state=open", "-f", "labels=" + LabelAgentReady,
	}
	output, err := c.callBytes(ctx, args, nil)
	if err != nil {
		return nil, fmt.Errorf("list eligible issues: %w", err)
	}
	responses, err := decodeIssueList(output)
	if err != nil {
		return nil, fmt.Errorf("decode eligible issues: %w", err)
	}
	issues := make([]Issue, 0, len(responses))
	for _, response := range responses {
		issue := response.issue()
		if issue.Number <= 0 || issue.IsPullRequest || !strings.EqualFold(strings.TrimSpace(issue.State), "open") || !containsLabel(issue.Labels, LabelAgentReady) {
			continue
		}
		issues = append(issues, issue)
	}
	sort.SliceStable(issues, func(left, right int) bool {
		return issues[left].Number < issues[right].Number
	})
	return issues, nil
}

// RenewLease publishes the coordinator heartbeat as a pending Commit Status
// on the current target-branch commit. A stale heartbeat remains visible in
// GitHub with its expiry timestamp after a host process disappears.
func (c *GhClient) RenewLease(ctx context.Context, repository Repository, lease Lease) error {
	if strings.TrimSpace(lease.TargetBranch) == "" {
		return errors.New("lease target branch is required and must be safe")
	}
	if err := ref.ValidatePart(lease.TargetBranch); err != nil {
		return fmt.Errorf("lease target branch: %w", err)
	}
	if strings.TrimSpace(lease.Coordinator) == "" || strings.ContainsAny(lease.Coordinator, "\x00\r\n") {
		return errors.New("lease coordinator is required and must be a single line")
	}
	if lease.RenewedAt.IsZero() || lease.ExpiresAt.IsZero() || !lease.ExpiresAt.After(lease.RenewedAt) {
		return errors.New("lease heartbeat and expiry must be valid")
	}
	var response commitResponse
	if err := c.callJSON(ctx, []string{"api", fmt.Sprintf("repos/%s/commits/%s", repository.String(), lease.TargetBranch), "--method", "GET"}, nil, &response); err != nil {
		return fmt.Errorf("read target branch for coordinator lease: %w", err)
	}
	if !ValidCommitSHA(response.SHA) {
		return errors.New("target branch response did not contain a valid commit SHA")
	}
	description := fmt.Sprintf("coordinator=%s run=%s renewed=%s expires=%s", safeStatusValue(lease.Coordinator), safeStatusValue(lease.RunID), lease.RenewedAt.UTC().Format(time.RFC3339), lease.ExpiresAt.UTC().Format(time.RFC3339))
	description = truncateStatusDescription(description, 140)
	if err := c.CreateCommitStatus(ctx, repository, CommitStatus{
		SHA:         response.SHA,
		State:       CommitStatusPending,
		Context:     LeaseStatusContext,
		Description: description,
	}); err != nil {
		return fmt.Errorf("publish coordinator lease: %w", err)
	}
	return nil
}

// CreateLabel creates or updates one factory label through gh.
func (c *GhClient) CreateLabel(ctx context.Context, repository Repository, label Label) error {
	if strings.TrimSpace(label.Name) == "" {
		return errors.New("GitHub label name is required")
	}
	args := []string{
		"label", "create", label.Name,
		"--repo", repository.String(),
		"--color", strings.TrimPrefix(label.Color, "#"),
		"--description", label.Description,
		"--force",
	}
	if _, err := c.runner().Run(ctx, args, nil); err != nil {
		return fmt.Errorf("create GitHub label %q: %w", label.Name, err)
	}
	return nil
}

// ReplaceIssueLabels replaces an issue's labels with the supplied complete set.
func (c *GhClient) ReplaceIssueLabels(ctx context.Context, repository Repository, number int, labels []string) error {
	if number <= 0 {
		return errors.New("issue number must be positive")
	}
	if err := c.call(ctx, []string{"api", fmt.Sprintf("repos/%s/issues/%d/labels", repository.String(), number), "--method", "PUT"}, map[string][]string{"labels": labels}); err != nil {
		return fmt.Errorf("replace labels on issue #%d: %w", number, err)
	}
	return nil
}

// CreateIssueComment creates one issue comment and returns its immutable id.
func (c *GhClient) CreateIssueComment(ctx context.Context, repository Repository, number int, body string) (Comment, error) {
	var response commentResponse
	if err := c.callJSON(ctx, []string{"api", fmt.Sprintf("repos/%s/issues/%d/comments", repository.String(), number), "--method", "POST"}, map[string]string{"body": body}, &response); err != nil {
		return Comment{}, fmt.Errorf("create status comment on issue #%d: %w", number, err)
	}
	return response.comment(), nil
}

// IssueComments lists all comments for an issue or pull request in GitHub's
// stable API order. The caller uses comment IDs as the exactly-once watermark.
func (c *GhClient) IssueComments(ctx context.Context, repository Repository, number int) ([]Comment, error) {
	if number <= 0 {
		return nil, errors.New("issue number must be positive")
	}
	var pages [][]commentResponse
	if err := c.callJSON(ctx, []string{"api", fmt.Sprintf("repos/%s/issues/%d/comments", repository.String(), number), "--paginate", "--slurp"}, nil, &pages); err != nil {
		return nil, fmt.Errorf("list comments on issue #%d: %w", number, err)
	}
	comments := make([]Comment, 0)
	for _, page := range pages {
		for _, response := range page {
			comments = append(comments, response.comment())
		}
	}
	return comments, nil
}

// FindStatusComment recovers a previously created status comment by its
// immutable run marker when persistence was interrupted after GitHub mutation.
// It only returns a marker match authored by the authenticated coordinator.
func (c *GhClient) FindStatusComment(ctx context.Context, repository Repository, number int, marker string) (Comment, error) {
	if number <= 0 {
		return Comment{}, errors.New("issue number must be positive")
	}
	if strings.TrimSpace(marker) == "" {
		return Comment{}, errors.New("status comment marker is required")
	}
	coordinator, err := c.authenticatedUser(ctx)
	if err != nil {
		return Comment{}, err
	}
	var pages [][]commentResponse
	if err := c.callJSON(ctx, []string{"api", fmt.Sprintf("repos/%s/issues/%d/comments", repository.String(), number), "--paginate", "--slurp"}, nil, &pages); err != nil {
		return Comment{}, fmt.Errorf("find status comment on issue #%d: %w", number, err)
	}
	for _, page := range pages {
		for _, response := range page {
			comment := response.comment()
			if strings.Contains(comment.Body, marker) && strings.EqualFold(strings.TrimSpace(comment.Author), coordinator) {
				return comment, nil
			}
		}
	}
	return Comment{}, nil
}

// authenticatedUser returns the GitHub login attached to the local gh
// credential, which is the only reliable coordinator identity available to the
// adapter when it recovers a status comment.
func (c *GhClient) authenticatedUser(ctx context.Context) (string, error) {
	var response userResponse
	if err := c.callJSON(ctx, []string{"api", "user"}, nil, &response); err != nil {
		return "", fmt.Errorf("identify authenticated GitHub user: %w", err)
	}
	login := strings.TrimSpace(response.Login)
	if login == "" {
		return "", errors.New("authenticated GitHub user has no login")
	}
	return login, nil
}

// EditIssueComment edits an existing issue comment by id.
func (c *GhClient) EditIssueComment(ctx context.Context, repository Repository, commentID string, body string) error {
	if strings.TrimSpace(commentID) == "" {
		return errors.New("status comment id is required")
	}
	if err := c.call(ctx, []string{"api", fmt.Sprintf("repos/%s/issues/comments/%s", repository.String(), commentID), "--method", "PATCH"}, map[string]string{"body": body}); err != nil {
		return fmt.Errorf("edit status comment %s: %w", commentID, err)
	}
	return nil
}

// FindPullRequest finds the pull request for one exact source/target branch
// pair, including closed pull requests so a retry cannot create a duplicate.
func (c *GhClient) FindPullRequest(ctx context.Context, repository Repository, headBranch, baseBranch string) (PullRequest, error) {
	if err := validatePullRequestBranches(headBranch, baseBranch); err != nil {
		return PullRequest{}, err
	}
	args := []string{
		"api", fmt.Sprintf("repos/%s/pulls", repository.String()),
		"--paginate", "--slurp",
		"--method", "GET",
		"-f", "state=all",
		"-f", "head=" + repository.Owner + ":" + headBranch,
		"-f", "base=" + baseBranch,
	}
	output, err := c.callBytes(ctx, args, nil)
	if err != nil {
		return PullRequest{}, fmt.Errorf("find pull request for %q: %w", headBranch, err)
	}
	responses, err := decodePullRequestList(output)
	if err != nil {
		return PullRequest{}, fmt.Errorf("decode pull requests for %q: %w", headBranch, err)
	}
	for _, response := range responses {
		pullRequest := response.pullRequest()
		if pullRequest.HeadBranch == headBranch && pullRequest.BaseBranch == baseBranch {
			return pullRequest, nil
		}
	}
	return PullRequest{}, nil
}

// CreatePullRequest creates one draft pull request from a pushed run branch.
func (c *GhClient) CreatePullRequest(ctx context.Context, repository Repository, request PullRequestRequest) (PullRequest, error) {
	if err := validatePullRequestRequest(request); err != nil {
		return PullRequest{}, err
	}
	var response pullRequestResponse
	payload := pullRequestCreatePayload{
		Title: request.Title, Body: request.Body, Head: request.HeadBranch,
		Base: request.BaseBranch, Draft: request.Draft,
	}
	if err := c.callJSON(ctx, []string{"api", fmt.Sprintf("repos/%s/pulls", repository.String()), "--method", "POST"}, payload, &response); err != nil {
		return PullRequest{}, fmt.Errorf("create draft pull request: %w", err)
	}
	return response.pullRequest(), nil
}

// UpdatePullRequest replaces the complete body and title.
// It preserves the current state of closed pull requests and does not include
// the Draft field in PATCH payloads to avoid state conflicts.
func (c *GhClient) UpdatePullRequest(ctx context.Context, repository Repository, number int, request PullRequestRequest) (PullRequest, error) {
	if number <= 0 {
		return PullRequest{}, errors.New("pull request number must be positive")
	}
	if err := validatePullRequestRequest(request); err != nil {
		return PullRequest{}, err
	}
	// Fetch current PR state to detect closed pull requests
	var currentResponse pullRequestResponse
	if err := c.callJSON(ctx, []string{"api", fmt.Sprintf("repos/%s/pulls/%d", repository.String(), number)}, nil, &currentResponse); err != nil {
		return PullRequest{}, fmt.Errorf("fetch pull request #%d state: %w", number, err)
	}
	var response pullRequestResponse
	payload := pullRequestUpdatePayload{Title: request.Title, Body: request.Body}
	// Preserve closed state; only PATCH open PRs to open state
	if currentResponse.State != "closed" {
		payload.State = "open"
	}
	if err := c.callJSON(ctx, []string{"api", fmt.Sprintf("repos/%s/pulls/%d", repository.String(), number), "--method", "PATCH"}, payload, &response); err != nil {
		return PullRequest{}, fmt.Errorf("update draft pull request #%d: %w", number, err)
	}
	return response.pullRequest(), nil
}

// SetPullRequestDraft explicitly toggles GitHub readiness through the host CLI
// and then reads the resulting pull-request projection.
func (c *GhClient) SetPullRequestDraft(ctx context.Context, repository Repository, number int, draft bool) (PullRequest, error) {
	if number <= 0 {
		return PullRequest{}, errors.New("pull request number must be positive")
	}
	args := []string{"pr", "ready", strconv.Itoa(number), "--repo", repository.String()}
	if draft {
		args = append(args, "--undo")
	}
	if _, err := c.runner().Run(ctx, args, nil); err != nil {
		return PullRequest{}, fmt.Errorf("set pull request #%d draft=%t: %w", number, draft, err)
	}
	var response pullRequestResponse
	if err := c.callJSON(ctx, []string{"api", fmt.Sprintf("repos/%s/pulls/%d", repository.String(), number)}, nil, &response); err != nil {
		return PullRequest{}, fmt.Errorf("read pull request #%d after readiness change: %w", number, err)
	}
	return response.pullRequest(), nil
}

// CreateCommitStatus publishes one deterministic result for an exact commit.
// The status context is caller-defined but must be stable and single-line.
func (c *GhClient) CreateCommitStatus(ctx context.Context, repository Repository, status CommitStatus) error {
	if !ValidCommitSHA(status.SHA) {
		return errors.New("commit status SHA must contain exactly 40 or 64 lowercase hexadecimal characters")
	}
	if status.State != CommitStatusPending && status.State != CommitStatusSuccess && status.State != CommitStatusFailure && status.State != CommitStatusError {
		return fmt.Errorf("unsupported commit status state %q", status.State)
	}
	if strings.TrimSpace(status.Context) == "" || strings.ContainsAny(status.Context, "\r\n") {
		return errors.New("commit status context must be a nonempty single line")
	}
	if len(status.Description) > 140 {
		return errors.New("commit status description must be at most 140 characters")
	}
	payload := map[string]string{
		"state":       string(status.State),
		"context":     status.Context,
		"description": status.Description,
	}
	if status.TargetURL != "" {
		payload["target_url"] = status.TargetURL
	}
	if err := c.call(ctx, []string{"api", fmt.Sprintf("repos/%s/statuses/%s", repository.String(), status.SHA), "--method", "POST"}, payload); err != nil {
		return fmt.Errorf("publish commit status for %s: %w", status.SHA, err)
	}
	return nil
}

// ListCommitStatuses reads all statuses attached to one exact commit so a
// pending status effect can be recognized without publishing a duplicate.
func (c *GhClient) ListCommitStatuses(ctx context.Context, repository Repository, sha string) ([]CommitStatus, error) {
	if !ValidCommitSHA(sha) {
		return nil, errors.New("commit status SHA must contain exactly 40 or 64 lowercase hexadecimal characters")
	}
	output, err := c.callBytes(ctx, []string{"api", fmt.Sprintf("repos/%s/commits/%s/statuses", repository.String(), sha), "--method", "GET", "--paginate", "--slurp"}, nil)
	if err != nil {
		return nil, fmt.Errorf("list commit statuses for %s: %w", sha, err)
	}
	responses, err := decodeJSONPages[commitStatusResponse](output)
	if err != nil {
		return nil, fmt.Errorf("decode commit statuses for %s: %w", sha, err)
	}
	statuses := make([]CommitStatus, 0, len(responses))
	for _, response := range responses {
		statuses = append(statuses, CommitStatus{
			SHA:         sha,
			State:       CommitStatusState(response.State),
			Context:     response.Context,
			Description: response.Description,
			TargetURL:   response.TargetURL,
		})
	}
	return statuses, nil
}

// callJSON executes a GitHub CLI request and decodes its JSON response.
func (c *GhClient) callJSON(ctx context.Context, args []string, payload any, destination any) error {
	output, err := c.callBytes(ctx, args, payload)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(output, destination); err != nil {
		return fmt.Errorf("decode GitHub response: %w", err)
	}
	return nil
}

// call executes a GitHub CLI request whose response body is not needed.
func (c *GhClient) call(ctx context.Context, args []string, payload any) error {
	_, err := c.callBytes(ctx, args, payload)
	return err
}

// callBytes encodes a request payload in memory and sends it through stdin.
func (c *GhClient) callBytes(ctx context.Context, args []string, payload any) ([]byte, error) {
	var input []byte
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode GitHub request: %w", err)
		}
		input = encoded
		args = append(args, "--input", "-")
	}
	return c.runner().Run(ctx, args, input)
}

// ValidCommitSHA reports whether value is a full 40- or 64-character lowercase
// hexadecimal commit SHA while rejecting values that could alter a GitHub API path.
func ValidCommitSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') {
			continue
		}
		return false
	}
	return true
}

// issueResponse is the subset of the GitHub issue API response needed by a
// specification packet.
type issueResponse struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	PullRequest *struct{} `json:"pull_request"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
	UpdatedAt time.Time `json:"updated_at"`
}

// issue converts the GitHub issue response into the adapter-neutral model.
func (r issueResponse) issue() Issue {
	labels := make([]string, 0, len(r.Labels))
	for _, label := range r.Labels {
		labels = append(labels, label.Name)
	}
	return Issue{
		Number:        r.Number,
		Title:         r.Title,
		Body:          r.Body,
		State:         r.State,
		Labels:        labels,
		IsPullRequest: r.PullRequest != nil,
		UpdatedAt:     r.UpdatedAt,
	}
}

// decodeIssueList accepts both gh's slurped pagination array and one page of
// issue responses, keeping the adapter tolerant of test and CLI modes.
func decodeIssueList(output []byte) ([]issueResponse, error) {
	var pages [][]issueResponse
	if err := json.Unmarshal(output, &pages); err == nil {
		issues := make([]issueResponse, 0)
		for _, page := range pages {
			issues = append(issues, page...)
		}
		return issues, nil
	}
	var singlePage []issueResponse
	if err := json.Unmarshal(output, &singlePage); err != nil {
		return nil, err
	}
	return singlePage, nil
}

// containsLabel reports whether one issue projection contains a named label.
func containsLabel(labels []string, wanted string) bool {
	for _, label := range labels {
		if label == wanted {
			return true
		}
	}
	return false
}

// safeStatusValue bounds untrusted lease values to one status-comment line.
func safeStatusValue(value string) string {
	return strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ", "\x00", " ").Replace(value))
}

// truncateStatusDescription limits a status description by Unicode code
// points so an unusually long coordinator identity cannot create invalid UTF-8.
func truncateStatusDescription(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

// commitResponse is the branch-head projection needed by lease publishing.
type commitResponse struct {
	SHA string `json:"sha"`
}

// commitStatusResponse is the GitHub status projection needed for replay
// recognition; GitHub's numeric status id is intentionally not retained.
type commitStatusResponse struct {
	State       string `json:"state"`
	Context     string `json:"context"`
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
}

// commentResponse is the subset of a GitHub comment response needed to retain
// the editable comment identity.
type commentResponse struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	UpdatedAt time.Time `json:"updated_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

// userResponse is the authenticated GitHub user projection used for ownership
// checks on coordinator-created comments.
type userResponse struct {
	Login string `json:"login"`
}

// comment converts the GitHub response shape into the coordinator-neutral
// comment model.
func (r commentResponse) comment() Comment {
	return Comment{ID: fmt.Sprint(r.ID), Body: r.Body, Author: r.User.Login, UpdatedAt: r.UpdatedAt}
}

// pullRequestResponse is the GitHub API subset used by the factory.
type pullRequestResponse struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	State   string `json:"state"`
	Draft   bool   `json:"draft"`
	// Merged is supplied by pull-specific GitHub responses when available.
	Merged bool `json:"merged"`
	// MergedAt is the timestamp GitHub supplies for a successful merge.
	MergedAt *time.Time `json:"merged_at"`
	// MergeCommitSHA is the immutable commit recorded by GitHub for the merge.
	MergeCommitSHA string `json:"merge_commit_sha"`
	Head           struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// pullRequestCreatePayload is the GitHub API create request shape.
type pullRequestCreatePayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	Draft bool   `json:"draft"`
}

// pullRequestUpdatePayload is the GitHub API update request shape.
type pullRequestUpdatePayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Draft bool   `json:"draft,omitempty"`
	State string `json:"state,omitempty"`
}

// pullRequest converts an API response into the adapter-neutral model.
func (r pullRequestResponse) pullRequest() PullRequest {
	return PullRequest{
		Number: r.Number, URL: r.HTMLURL, Title: r.Title, Body: r.Body,
		State: r.State, Draft: r.Draft, Merged: r.Merged || r.MergedAt != nil,
		MergeCommitSHA: r.MergeCommitSHA, HeadBranch: r.Head.Ref, HeadSHA: r.Head.SHA, BaseBranch: r.Base.Ref,
	}
}

// decodePullRequestList accepts both gh --slurp pagination output and a plain
// single-page array, which keeps the adapter tolerant of test and CLI modes.
func decodePullRequestList(output []byte) ([]pullRequestResponse, error) {
	return decodeJSONPages[pullRequestResponse](output)
}

// validatePullRequestBranches rejects values that could alter an API request
// while permitting normal Git branch slashes.
func validatePullRequestBranches(headBranch, baseBranch string) error {
	for field, value := range map[string]string{"head branch": headBranch, "base branch": baseBranch} {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("pull request %s is required and must be a single line", field)
		}
	}
	return nil
}

// validatePullRequestRequest validates the fields used by a create or update
// mutation before invoking the authenticated GitHub CLI.
func validatePullRequestRequest(request PullRequestRequest) error {
	if strings.TrimSpace(request.Title) == "" || strings.ContainsAny(request.Title, "\x00\r\n") {
		return errors.New("pull request title is required and must be a single line")
	}
	if err := validatePullRequestBranches(request.HeadBranch, request.BaseBranch); err != nil {
		return err
	}
	if strings.ContainsRune(request.Body, '\x00') {
		return errors.New("pull request body contains a NUL byte")
	}
	return nil
}

// pullRequestReviewResponse is the GitHub pull-request review subset the
// coordinator needs to recognize one completed human review.
type pullRequestReviewResponse struct {
	ID          int64      `json:"id"`
	Body        string     `json:"body"`
	State       string     `json:"state"`
	SubmittedAt *time.Time `json:"submitted_at"`
	HTMLURL     string     `json:"html_url"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
}

// review converts the GitHub response shape into the coordinator-neutral
// review model. A review GitHub has not submitted has no submission time, and
// the coordinator uses that absence to ignore an incomplete draft.
func (r pullRequestReviewResponse) review() PullRequestReview {
	value := PullRequestReview{
		ID:       fmt.Sprint(r.ID),
		Author:   r.User.Login,
		State:    PullRequestReviewState(strings.ToUpper(strings.TrimSpace(r.State))),
		Body:     r.Body,
		URL:      r.HTMLURL,
		Comments: []PullRequestReviewComment{},
	}
	if r.SubmittedAt != nil {
		value.SubmittedAt = *r.SubmittedAt
	}
	return value
}

// pullRequestReviewCommentResponse is the inline review-comment subset used to
// carry a human reviewer's file-anchored findings into a repair packet.
type pullRequestReviewCommentResponse struct {
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Position int    `json:"position"`
	Body     string `json:"body"`
}

// comment converts one inline review comment into the neutral model, keeping
// the newer `line` anchor and falling back to the legacy diff position.
func (r pullRequestReviewCommentResponse) comment() PullRequestReviewComment {
	line := r.Line
	if line == 0 {
		line = r.Position
	}
	return PullRequestReviewComment{Path: r.Path, Line: line, Body: r.Body}
}

// PullRequestReviews lists every submitted review for one pull request,
// including the inline comments of the reviews that carry them. It is
// read-only: the factory never submits, approves, or dismisses a review.
func (c *GhClient) PullRequestReviews(ctx context.Context, repository Repository, number int) ([]PullRequestReview, error) {
	if number <= 0 {
		return nil, errors.New("pull request number must be positive")
	}
	output, err := c.callBytes(ctx, []string{
		"api", fmt.Sprintf("repos/%s/pulls/%d/reviews", repository.String(), number),
		"--method", "GET", "--paginate", "--slurp",
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("list reviews for pull request #%d: %w", number, err)
	}
	responses, err := decodeJSONPages[pullRequestReviewResponse](output)
	if err != nil {
		return nil, fmt.Errorf("decode reviews for pull request #%d: %w", number, err)
	}
	reviews := make([]PullRequestReview, 0, len(responses))
	for _, response := range responses {
		review := response.review()
		if review.State == PullRequestReviewChangesRequested {
			comments, commentErr := c.pullRequestReviewComments(ctx, repository, number, response.ID)
			if commentErr != nil {
				return nil, commentErr
			}
			review.Comments = comments
		}
		reviews = append(reviews, review)
	}
	return reviews, nil
}

// pullRequestReviewComments lists the inline findings of one review.
func (c *GhClient) pullRequestReviewComments(ctx context.Context, repository Repository, number int, reviewID int64) ([]PullRequestReviewComment, error) {
	output, err := c.callBytes(ctx, []string{
		"api", fmt.Sprintf("repos/%s/pulls/%d/reviews/%d/comments", repository.String(), number, reviewID),
		"--method", "GET", "--paginate", "--slurp",
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("list inline comments for review %d: %w", reviewID, err)
	}
	responses, err := decodeJSONPages[pullRequestReviewCommentResponse](output)
	if err != nil {
		return nil, fmt.Errorf("decode inline comments for review %d: %w", reviewID, err)
	}
	comments := make([]PullRequestReviewComment, 0, len(responses))
	for _, response := range responses {
		comments = append(comments, response.comment())
	}
	return comments, nil
}

// decodeJSONPages accepts both gh --slurp pagination output and a plain
// single-page array, which keeps the adapter tolerant of test and CLI modes.
func decodeJSONPages[T any](output []byte) ([]T, error) {
	var pages [][]T
	if err := json.Unmarshal(output, &pages); err == nil {
		result := make([]T, 0)
		for _, page := range pages {
			result = append(result, page...)
		}
		return result, nil
	}
	var singlePage []T
	if err := json.Unmarshal(output, &singlePage); err != nil {
		return nil, err
	}
	return singlePage, nil
}
