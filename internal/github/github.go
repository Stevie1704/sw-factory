// Package github contains the host-side GitHub adapter used by the coordinator.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
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

// Label describes a factory-owned GitHub label.
type Label struct {
	Name        string
	Description string
	Color       string
}

// Comment is the identity returned after creating a GitHub issue comment.
type Comment struct {
	ID   string
	Body string
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
	// HeadBranch is the source branch name.
	HeadBranch string
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
	labels := make([]string, 0, len(response.Labels))
	for _, label := range response.Labels {
		labels = append(labels, label.Name)
	}
	return Issue{
		Number:        response.Number,
		Title:         response.Title,
		Body:          response.Body,
		State:         response.State,
		Labels:        labels,
		IsPullRequest: response.PullRequest != nil,
		UpdatedAt:     response.UpdatedAt,
	}, nil
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
	return Comment{ID: fmt.Sprint(response.ID), Body: response.Body}, nil
}

// FindStatusComment recovers a previously created status comment by its
// immutable run marker when persistence was interrupted after GitHub mutation.
func (c *GhClient) FindStatusComment(ctx context.Context, repository Repository, number int, marker string) (Comment, error) {
	if number <= 0 {
		return Comment{}, errors.New("issue number must be positive")
	}
	if strings.TrimSpace(marker) == "" {
		return Comment{}, errors.New("status comment marker is required")
	}
	var pages [][]commentResponse
	if err := c.callJSON(ctx, []string{"api", fmt.Sprintf("repos/%s/issues/%d/comments", repository.String(), number), "--paginate", "--slurp"}, nil, &pages); err != nil {
		return Comment{}, fmt.Errorf("find status comment on issue #%d: %w", number, err)
	}
	for _, page := range pages {
		for _, response := range page {
			if strings.Contains(response.Body, marker) {
				return Comment{ID: fmt.Sprint(response.ID), Body: response.Body}, nil
			}
		}
	}
	return Comment{}, nil
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

// UpdatePullRequest replaces the complete body and keeps the pull request a
// draft. The caller supplies a body with its human-authored portion preserved.
func (c *GhClient) UpdatePullRequest(ctx context.Context, repository Repository, number int, request PullRequestRequest) (PullRequest, error) {
	if number <= 0 {
		return PullRequest{}, errors.New("pull request number must be positive")
	}
	if err := validatePullRequestRequest(request); err != nil {
		return PullRequest{}, err
	}
	var response pullRequestResponse
	payload := pullRequestUpdatePayload{Title: request.Title, Body: request.Body, Draft: request.Draft, State: "open"}
	if err := c.callJSON(ctx, []string{"api", fmt.Sprintf("repos/%s/pulls/%d", repository.String(), number), "--method", "PATCH"}, payload, &response); err != nil {
		return PullRequest{}, fmt.Errorf("update draft pull request #%d: %w", number, err)
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

// commentResponse is the subset of a GitHub comment response needed to retain
// the editable comment identity.
type commentResponse struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

// pullRequestResponse is the GitHub API subset used by the factory.
type pullRequestResponse struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	State   string `json:"state"`
	Draft   bool   `json:"draft"`
	Head    struct {
		Ref string `json:"ref"`
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
	Draft bool   `json:"draft"`
	State string `json:"state"`
}

// pullRequest converts an API response into the adapter-neutral model.
func (r pullRequestResponse) pullRequest() PullRequest {
	return PullRequest{
		Number: r.Number, URL: r.HTMLURL, Title: r.Title, Body: r.Body,
		State: r.State, Draft: r.Draft, HeadBranch: r.Head.Ref, BaseBranch: r.Base.Ref,
	}
}

// decodePullRequestList accepts both gh --slurp pagination output and a plain
// single-page array, which keeps the adapter tolerant of test and CLI modes.
func decodePullRequestList(output []byte) ([]pullRequestResponse, error) {
	var pages [][]pullRequestResponse
	if err := json.Unmarshal(output, &pages); err == nil {
		result := make([]pullRequestResponse, 0)
		for _, page := range pages {
			result = append(result, page...)
		}
		return result, nil
	}
	var singlePage []pullRequestResponse
	if err := json.Unmarshal(output, &singlePage); err != nil {
		return nil, err
	}
	return singlePage, nil
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
