package azuredevops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

var (
	_ codehost.PullRequestClient        = (*Client)(nil)
	_ codehost.PullRequestDraftClient   = (*Client)(nil)
	_ codehost.PullRequestReviewReader  = (*Client)(nil)
	_ codehost.PullRequestCommentReader = (*Client)(nil)
	_ codehost.PullRequestCommentClient = (*Client)(nil)
	_ codehost.CommitStatusPublisher    = (*Client)(nil)
	_ codehost.CommitStatusReader       = (*Client)(nil)
)

// maxDescriptionLength is the Azure Repos limit of a pull-request
// description.
const maxDescriptionLength = 4000

// branchPrefix starts the full reference name of a branch.
const branchPrefix = "refs/heads/"

// branchRef is the full reference name of a branch.
func branchRef(branch string) string {
	return branchPrefix + branch
}

// branchName is the branch name of a full reference name.
func branchName(ref string) string {
	return strings.TrimPrefix(ref, branchPrefix)
}

// maxStatusDescriptionLength is the longest commit status description the
// factory publishes, the same bound as on GitHub.
const maxStatusDescriptionLength = 140

// validatePullRequestTarget rejects an incomplete repository or a
// non-positive pull request number before any call.
func validatePullRequestTarget(repository tracker.Repository, number int) error {
	if err := validateRepository(repository); err != nil {
		return err
	}
	if number <= 0 {
		return errors.New("pull request number must be positive")
	}
	return nil
}

// pullRequestResponse is the Azure Repos pull-request projection.
type pullRequestResponse struct {
	PullRequestID         int    `json:"pullRequestId"`
	Status                string `json:"status"`
	IsDraft               bool   `json:"isDraft"`
	Title                 string `json:"title"`
	Description           string `json:"description"`
	SourceRefName         string `json:"sourceRefName"`
	TargetRefName         string `json:"targetRefName"`
	LastMergeSourceCommit struct {
		CommitID string `json:"commitId"`
	} `json:"lastMergeSourceCommit"`
	LastMergeCommit struct {
		CommitID string `json:"commitId"`
	} `json:"lastMergeCommit"`
}

// pullRequest converts the response into the neutral model. An active pull
// request is open; a completed one is merged and closed; an abandoned one is
// closed without a merge.
func (r pullRequestResponse) pullRequest(repository tracker.Repository) codehost.PullRequest {
	value := codehost.PullRequest{
		Number:     r.PullRequestID,
		URL:        pullRequestURL(repository, r.PullRequestID),
		Title:      r.Title,
		Body:       r.Description,
		State:      "closed",
		Draft:      r.IsDraft,
		HeadBranch: branchName(r.SourceRefName),
		HeadSHA:    r.LastMergeSourceCommit.CommitID,
		BaseBranch: branchName(r.TargetRefName),
	}
	switch r.Status {
	case "active":
		value.State = "open"
	case "completed":
		value.Merged = true
		value.MergeCommitSHA = r.LastMergeCommit.CommitID
	}
	return value
}

// pullRequestURL is the browser URL of one pull request.
func pullRequestURL(repository tracker.Repository, number int) string {
	return fmt.Sprintf("%s%s/%s/_git/%s/pullrequest/%d", serviceURL, url.PathEscape(repository.Owner), url.PathEscape(repository.Project), url.PathEscape(repository.Name), number)
}

// FindPullRequest returns the newest pull request of one exact source and
// target branch pair in any status, so a retry cannot create a duplicate. It
// reads the match by id to get the complete description.
func (c *Client) FindPullRequest(ctx context.Context, repository tracker.Repository, headBranch, baseBranch string) (codehost.PullRequest, error) {
	if err := validateRepository(repository); err != nil {
		return codehost.PullRequest{}, err
	}
	if err := validateBranches(headBranch, baseBranch); err != nil {
		return codehost.PullRequest{}, err
	}
	query := url.Values{
		"searchCriteria.sourceRefName": {branchRef(headBranch)},
		"searchCriteria.targetRefName": {branchRef(baseBranch)},
		"searchCriteria.status":        {"all"},
		"$top":                         {"100"},
	}
	var response struct {
		Value []pullRequestResponse `json:"value"`
	}
	if err := c.call(ctx, request{Method: "GET", URL: repositoryURL(repository, "/pullrequests", query)}, &response); err != nil {
		return codehost.PullRequest{}, fmt.Errorf("find pull request for %q: %w", headBranch, err)
	}
	for _, candidate := range response.Value {
		if candidate.SourceRefName == branchRef(headBranch) && candidate.TargetRefName == branchRef(baseBranch) {
			return c.pullRequestByID(ctx, repository, candidate.PullRequestID)
		}
	}
	return codehost.PullRequest{}, nil
}

// pullRequestByID reads one pull request. The list endpoint truncates the
// description to 400 characters, so a body that regeneration preserves must
// come from this read.
func (c *Client) pullRequestByID(ctx context.Context, repository tracker.Repository, number int) (codehost.PullRequest, error) {
	var response pullRequestResponse
	if err := c.call(ctx, request{Method: "GET", URL: repositoryURL(repository, fmt.Sprintf("/pullrequests/%d", number), nil)}, &response); err != nil {
		return codehost.PullRequest{}, fmt.Errorf("read pull request %d: %w", number, err)
	}
	return response.pullRequest(repository), nil
}

// CreatePullRequest creates one pull request from a pushed run branch.
func (c *Client) CreatePullRequest(ctx context.Context, repository tracker.Repository, request codehost.PullRequestRequest) (codehost.PullRequest, error) {
	if err := validatePullRequestRequest(repository, request); err != nil {
		return codehost.PullRequest{}, err
	}
	payload := map[string]any{
		"sourceRefName": branchRef(request.HeadBranch),
		"targetRefName": branchRef(request.BaseBranch),
		"title":         request.Title,
		"description":   request.Body,
		"isDraft":       request.Draft,
	}
	return c.sendPullRequest(ctx, repository, "POST", "/pullrequests", payload, "create pull request")
}

// UpdatePullRequest replaces the title and description. It leaves the status
// and the draft flag unchanged.
func (c *Client) UpdatePullRequest(ctx context.Context, repository tracker.Repository, number int, request codehost.PullRequestRequest) (codehost.PullRequest, error) {
	if err := validatePullRequestTarget(repository, number); err != nil {
		return codehost.PullRequest{}, err
	}
	if err := validatePullRequestRequest(repository, request); err != nil {
		return codehost.PullRequest{}, err
	}
	payload := map[string]any{"title": request.Title, "description": request.Body}
	return c.sendPullRequest(ctx, repository, "PATCH", fmt.Sprintf("/pullrequests/%d", number), payload, fmt.Sprintf("update pull request %d", number))
}

// SetPullRequestDraft sets or clears the draft flag of one pull request.
func (c *Client) SetPullRequestDraft(ctx context.Context, repository tracker.Repository, number int, draft bool) (codehost.PullRequest, error) {
	if err := validatePullRequestTarget(repository, number); err != nil {
		return codehost.PullRequest{}, err
	}
	return c.sendPullRequest(ctx, repository, "PATCH", fmt.Sprintf("/pullrequests/%d", number), map[string]any{"isDraft": draft}, fmt.Sprintf("set pull request %d draft=%t", number, draft))
}

// sendPullRequest sends one pull-request mutation and converts the result.
func (c *Client) sendPullRequest(ctx context.Context, repository tracker.Repository, method, path string, payload map[string]any, action string) (codehost.PullRequest, error) {
	var response pullRequestResponse
	if err := c.call(ctx, request{Method: method, URL: repositoryURL(repository, path, nil), Body: payload}, &response); err != nil {
		return codehost.PullRequest{}, fmt.Errorf("%s: %w", action, err)
	}
	return response.pullRequest(repository), nil
}

// validateBranches rejects branch names that could alter a request.
func validateBranches(headBranch, baseBranch string) error {
	for field, value := range map[string]string{"head branch": headBranch, "base branch": baseBranch} {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("pull request %s is required and must be a single line", field)
		}
	}
	return nil
}

// validatePullRequestRequest validates a create or update request before any
// call. Azure Repos rejects a description above 4,000 characters, so the
// adapter fails here instead of truncating text that can contain markers.
func validatePullRequestRequest(repository tracker.Repository, request codehost.PullRequestRequest) error {
	if err := validateRepository(repository); err != nil {
		return err
	}
	if strings.TrimSpace(request.Title) == "" || strings.ContainsAny(request.Title, "\x00\r\n") {
		return errors.New("pull request title is required and must be a single line")
	}
	if err := validateBranches(request.HeadBranch, request.BaseBranch); err != nil {
		return err
	}
	if strings.ContainsRune(request.Body, '\x00') {
		return errors.New("pull request body contains a NUL byte")
	}
	if length := len([]rune(request.Body)); length > maxDescriptionLength {
		return fmt.Errorf("pull request body has %d characters; Azure Repos allows at most %d", length, maxDescriptionLength)
	}
	return nil
}

// commitStatusStates maps the neutral status states to Azure Repos states.
var commitStatusStates = map[codehost.CommitStatusState]string{
	codehost.CommitStatusPending: "pending",
	codehost.CommitStatusSuccess: "succeeded",
	codehost.CommitStatusFailure: "failed",
	codehost.CommitStatusError:   "error",
}

// statusContext is the Azure Repos status context. The factory writes the
// complete neutral context as the name; genre stays empty.
type statusContext struct {
	Name  string `json:"name"`
	Genre string `json:"genre,omitempty"`
}

// CreateCommitStatus publishes one result for an exact commit.
func (c *Client) CreateCommitStatus(ctx context.Context, repository tracker.Repository, status codehost.CommitStatus) error {
	if err := validateRepository(repository); err != nil {
		return err
	}
	if !codehost.ValidCommitSHA(status.SHA) {
		return errors.New("commit status SHA must contain exactly 40 or 64 lowercase hexadecimal characters")
	}
	state, ok := commitStatusStates[status.State]
	if !ok {
		return fmt.Errorf("unsupported commit status state %q", status.State)
	}
	if strings.TrimSpace(status.Context) == "" || strings.ContainsAny(status.Context, "\r\n") {
		return errors.New("commit status context must be a nonempty single line")
	}
	if len(status.Description) > maxStatusDescriptionLength {
		return errors.New("commit status description must be at most 140 characters")
	}
	payload := map[string]any{"state": state, "description": status.Description, "context": statusContext{Name: status.Context}}
	if status.TargetURL != "" {
		payload["targetUrl"] = status.TargetURL
	}
	if err := c.call(ctx, request{Method: "POST", URL: repositoryURL(repository, "/commits/"+status.SHA+"/statuses", nil), Body: payload}, nil); err != nil {
		return fmt.Errorf("publish commit status for %s: %w", status.SHA, err)
	}
	return nil
}

// statusPageSize is the page size of the commit status list.
const statusPageSize = 1000

// ListCommitStatuses reads every status of one exact commit. A context with
// a genre reads as "genre/name".
func (c *Client) ListCommitStatuses(ctx context.Context, repository tracker.Repository, sha string) ([]codehost.CommitStatus, error) {
	if err := validateRepository(repository); err != nil {
		return nil, err
	}
	if !codehost.ValidCommitSHA(sha) {
		return nil, errors.New("commit status SHA must contain exactly 40 or 64 lowercase hexadecimal characters")
	}
	neutral := make(map[string]codehost.CommitStatusState, len(commitStatusStates))
	for key, value := range commitStatusStates {
		neutral[value] = key
	}
	statuses := make([]codehost.CommitStatus, 0)
	for skip := 0; ; skip += statusPageSize {
		var response struct {
			Value []struct {
				State       string        `json:"state"`
				Description string        `json:"description"`
				TargetURL   string        `json:"targetUrl"`
				Context     statusContext `json:"context"`
			} `json:"value"`
		}
		query := url.Values{"latestOnly": {"false"}, "top": {strconv.Itoa(statusPageSize)}, "skip": {strconv.Itoa(skip)}}
		if err := c.call(ctx, request{Method: "GET", URL: repositoryURL(repository, "/commits/"+sha+"/statuses", query)}, &response); err != nil {
			return nil, fmt.Errorf("list commit statuses for %s: %w", sha, err)
		}
		for _, value := range response.Value {
			name := value.Context.Name
			if value.Context.Genre != "" {
				name = value.Context.Genre + "/" + name
			}
			statuses = append(statuses, codehost.CommitStatus{SHA: sha, State: neutral[value.State], Context: name, Description: value.Description, TargetURL: value.TargetURL})
		}
		if len(response.Value) < statusPageSize {
			return statuses, nil
		}
	}
}

// threadProperty is one typed thread property value. A value keeps its type
// in the response, for example the System.Int32 SupportsMarkdown flag, so it
// is kept raw and read as text only where the factory needs it.
type threadProperty struct {
	Value json.RawMessage `json:"$value"`
}

// text returns a string value unquoted and any other value as its JSON text.
func (p threadProperty) text() string {
	var value string
	if err := json.Unmarshal(p.Value, &value); err == nil {
		return value
	}
	return string(p.Value)
}

// threadCommentResponse is one comment of a pull-request thread.
type threadCommentResponse struct {
	ID              int         `json:"id"`
	Author          identityRef `json:"author"`
	Content         string      `json:"content"`
	CommentType     string      `json:"commentType"`
	IsDeleted       bool        `json:"isDeleted"`
	PublishedDate   time.Time   `json:"publishedDate"`
	LastUpdatedDate time.Time   `json:"lastUpdatedDate"`
}

// threadResponse is one pull-request thread. A vote creates a system thread
// whose properties name the vote and the voter's identity id; the service
// account authors its comment.
type threadResponse struct {
	ID            int                       `json:"id"`
	PublishedDate time.Time                 `json:"publishedDate"`
	Status        string                    `json:"status"`
	IsDeleted     bool                      `json:"isDeleted"`
	Properties    map[string]threadProperty `json:"properties"`
	ThreadContext *struct {
		FilePath       string `json:"filePath"`
		RightFileStart *struct {
			Line int `json:"line"`
		} `json:"rightFileStart"`
	} `json:"threadContext"`
	Comments []threadCommentResponse `json:"comments"`
}

// isVote reports whether the thread records a reviewer vote.
func (t threadResponse) isVote() bool {
	return t.Properties["CodeReviewThreadType"].text() == "VoteUpdate"
}

// voterID returns the normalized identity id of the voter of a vote thread.
func (t threadResponse) voterID() string {
	return identityKey(t.Properties["CodeReviewVotedByTfId"].text())
}

// identityKey normalizes an identity id. Thread properties write it with or
// without dashes and in either case.
func identityKey(id string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(id), "-", ""))
}

// isOpenFinding reports whether a text thread still asks for a change.
func (t threadResponse) isOpenFinding() bool {
	return !t.IsDeleted && (t.Status == "active" || t.Status == "pending") && len(t.Comments) > 0 && !t.Comments[0].IsDeleted && t.Comments[0].CommentType == "text"
}

// threads reads every thread of one pull request.
func (c *Client) threads(ctx context.Context, repository tracker.Repository, number int) ([]threadResponse, error) {
	if err := validatePullRequestTarget(repository, number); err != nil {
		return nil, err
	}
	var response struct {
		Value []threadResponse `json:"value"`
	}
	if err := c.call(ctx, request{Method: "GET", URL: repositoryURL(repository, fmt.Sprintf("/pullRequests/%d/threads", number), nil)}, &response); err != nil {
		return nil, fmt.Errorf("list threads of pull request %d: %w", number, err)
	}
	return response.Value, nil
}

// voteStates maps reviewer votes to the neutral review vocabulary. "Waiting
// for author" (-5) and "rejected" (-10) both ask for changes; a reset vote
// (0) carries no decision.
var voteStates = map[string]codehost.PullRequestReviewState{
	"10":  codehost.PullRequestReviewApproved,
	"5":   codehost.PullRequestReviewApproved,
	"0":   codehost.PullRequestReviewCommented,
	"-5":  codehost.PullRequestReviewChangesRequested,
	"-10": codehost.PullRequestReviewChangesRequested,
}

// PullRequestReviews returns one review for each reviewer vote. A vote is
// not an event in Azure Repos, but every vote change creates a system thread,
// so the thread gives the review a stable identity. The thread names the
// voter by identity id only, so the reviewer list resolves the login. The
// voter's open threads published up to the vote are the review content:
// general threads form the body and file threads the inline comments.
func (c *Client) PullRequestReviews(ctx context.Context, repository tracker.Repository, number int) ([]codehost.PullRequestReview, error) {
	threads, err := c.threads(ctx, repository, number)
	if err != nil {
		return nil, err
	}
	var logins map[string]string
	reviews := make([]codehost.PullRequestReview, 0)
	for _, thread := range threads {
		if !thread.isVote() || thread.IsDeleted {
			continue
		}
		state, ok := voteStates[thread.Properties["CodeReviewVoteResult"].text()]
		if !ok {
			continue
		}
		if logins == nil {
			if logins, err = c.reviewerLogins(ctx, repository, number); err != nil {
				return nil, err
			}
		}
		review := codehost.PullRequestReview{
			ID:          eventID(thread.PublishedDate, idPart{"p", number}, idPart{"t", thread.ID}),
			Author:      logins[thread.voterID()],
			State:       state,
			URL:         pullRequestURL(repository, number),
			SubmittedAt: thread.PublishedDate,
			Comments:    []codehost.PullRequestReviewComment{},
		}
		if state == codehost.PullRequestReviewChangesRequested && review.Author != "" {
			review.Body, review.Comments = reviewFindings(threads, review.Author, thread.PublishedDate)
		}
		reviews = append(reviews, review)
	}
	return reviews, nil
}

// reviewerLogins maps the normalized identity id of every reviewer of a pull
// request to its login. A vote adds the voter as a reviewer.
func (c *Client) reviewerLogins(ctx context.Context, repository tracker.Repository, number int) (map[string]string, error) {
	var response struct {
		Value []struct {
			ID         string `json:"id"`
			UniqueName string `json:"uniqueName"`
		} `json:"value"`
	}
	if err := c.call(ctx, request{Method: "GET", URL: repositoryURL(repository, fmt.Sprintf("/pullRequests/%d/reviewers", number), nil)}, &response); err != nil {
		return nil, fmt.Errorf("list reviewers of pull request %d: %w", number, err)
	}
	logins := make(map[string]string, len(response.Value))
	for _, reviewer := range response.Value {
		logins[identityKey(reviewer.ID)] = reviewer.UniqueName
	}
	return logins, nil
}

// reviewFindings collects the open threads that author started up to the
// vote time.
func reviewFindings(threads []threadResponse, author string, votedAt time.Time) (string, []codehost.PullRequestReviewComment) {
	general := make([]string, 0)
	inline := make([]codehost.PullRequestReviewComment, 0)
	for _, thread := range threads {
		if thread.isVote() || !thread.isOpenFinding() || thread.PublishedDate.After(votedAt) {
			continue
		}
		first := thread.Comments[0]
		if !strings.EqualFold(first.Author.UniqueName, author) {
			continue
		}
		if thread.ThreadContext == nil || thread.ThreadContext.FilePath == "" {
			general = append(general, first.Content)
			continue
		}
		line := 0
		if thread.ThreadContext.RightFileStart != nil {
			line = thread.ThreadContext.RightFileStart.Line
		}
		inline = append(inline, codehost.PullRequestReviewComment{Path: strings.TrimPrefix(thread.ThreadContext.FilePath, "/"), Line: line, Body: first.Content})
	}
	return strings.Join(general, "\n\n"), inline
}

// threadComment converts one thread comment into the neutral model. The
// identity starts with the publication time, so it sorts with work item
// comment identities (ADR 0019).
func threadComment(number, thread int, comment threadCommentResponse) tracker.Comment {
	updated := comment.LastUpdatedDate
	if updated.IsZero() {
		updated = comment.PublishedDate
	}
	return tracker.Comment{ID: eventID(comment.PublishedDate, idPart{"p", number}, idPart{"t", thread}, idPart{"c", comment.ID}), Body: comment.Content, Author: comment.Author.UniqueName, UpdatedAt: updated}
}

// PullRequestComments lists the text comments of every thread of a pull
// request, without votes and other system comments.
func (c *Client) PullRequestComments(ctx context.Context, repository tracker.Repository, number int) ([]tracker.Comment, error) {
	threads, err := c.threads(ctx, repository, number)
	if err != nil {
		return nil, err
	}
	comments := make([]tracker.Comment, 0)
	for _, thread := range threads {
		if thread.isVote() || thread.IsDeleted {
			continue
		}
		for _, comment := range thread.Comments {
			if comment.IsDeleted || comment.CommentType != "text" {
				continue
			}
			comments = append(comments, threadComment(number, thread.ID, comment))
		}
	}
	return comments, nil
}

// FindPullRequestComment returns the comment of the authenticated identity
// that contains marker, or a zero comment.
func (c *Client) FindPullRequestComment(ctx context.Context, repository tracker.Repository, number int, marker string) (tracker.Comment, error) {
	if strings.TrimSpace(marker) == "" {
		return tracker.Comment{}, errors.New("pull request comment marker is required")
	}
	coordinator, err := c.AuthenticatedLogin(ctx)
	if err != nil {
		return tracker.Comment{}, err
	}
	comments, err := c.PullRequestComments(ctx, repository, number)
	if err != nil {
		return tracker.Comment{}, err
	}
	for _, comment := range comments {
		if strings.Contains(comment.Body, marker) && strings.EqualFold(strings.TrimSpace(comment.Author), coordinator) {
			return comment, nil
		}
	}
	return tracker.Comment{}, nil
}

// CreatePullRequestComment posts one comment in a new thread. The thread is
// closed, so a branch policy that requires resolved comments does not block
// the merge on a factory message.
func (c *Client) CreatePullRequestComment(ctx context.Context, repository tracker.Repository, number int, body string) (tracker.Comment, error) {
	if err := validatePullRequestTarget(repository, number); err != nil {
		return tracker.Comment{}, err
	}
	payload := map[string]any{
		"comments": []map[string]any{{"parentCommentId": 0, "content": body, "commentType": "text"}},
		"status":   "closed",
	}
	var response threadResponse
	if err := c.call(ctx, request{Method: "POST", URL: repositoryURL(repository, fmt.Sprintf("/pullRequests/%d/threads", number), nil), Body: payload}, &response); err != nil {
		return tracker.Comment{}, fmt.Errorf("create comment on pull request %d: %w", number, err)
	}
	if len(response.Comments) == 0 {
		return tracker.Comment{}, fmt.Errorf("create comment on pull request %d returned no comment", number)
	}
	comment := threadComment(number, response.ID, response.Comments[0])
	comment.Body = body
	return comment, nil
}

// EditPullRequestComment replaces the content of one thread comment. The
// identity names the thread, because comment ids are unique only per thread.
func (c *Client) EditPullRequestComment(ctx context.Context, repository tracker.Repository, number int, commentID, body string) error {
	if err := validateRepository(repository); err != nil {
		return err
	}
	pullRequest, okPullRequest := eventIDPart(commentID, "p")
	thread, okThread := eventIDPart(commentID, "t")
	id, okID := eventIDPart(commentID, "c")
	if !okPullRequest || !okThread || !okID || pullRequest != number {
		return fmt.Errorf("%q is not a comment identity of pull request %d", commentID, number)
	}
	path := fmt.Sprintf("/pullRequests/%d/threads/%d/comments/%d", number, thread, id)
	if err := c.call(ctx, request{Method: "PATCH", URL: repositoryURL(repository, path, nil), Body: map[string]string{"content": body}}, nil); err != nil {
		return fmt.Errorf("edit comment %d of thread %d on pull request %d: %w", id, thread, number, err)
	}
	return nil
}
