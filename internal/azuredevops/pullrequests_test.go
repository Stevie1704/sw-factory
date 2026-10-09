package azuredevops_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/codehost"
)

// activePullRequest is an open draft pull request of the run branch.
const activePullRequest = `{"pullRequestId":17,"status":"active","isDraft":true,"title":"Add export","description":"body",
	"sourceRefName":"refs/heads/factory/run-1","targetRefName":"refs/heads/main",
	"lastMergeSourceCommit":{"commitId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`

// TestFindPullRequestSearchesTheExactBranchPairInEveryStatus verifies the
// search criteria and the conversion to the neutral pull request.
func TestFindPullRequestSearchesTheExactBranchPairInEveryStatus(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).on("GET "+repositoryPath+"/pullrequests", `{"value":[`+activePullRequest+`]}`)

	pullRequest, err := newClient(az).FindPullRequest(context.Background(), repository, "factory/run-1", "main")
	if err != nil {
		t.Fatalf("FindPullRequest() error = %v", err)
	}
	want := codehost.PullRequest{
		Number: 17, URL: "https://dev.azure.com/contoso/Factory%20Pilot/_git/service/pullrequest/17",
		Title: "Add export", Body: "body", State: "open", Draft: true,
		HeadBranch: "factory/run-1", HeadSHA: strings.Repeat("a", 40), BaseBranch: "main",
	}
	if pullRequest != want {
		t.Fatalf("FindPullRequest() = %#v, want %#v", pullRequest, want)
	}
	query := az.calls[0].url.Query()
	if query.Get("searchCriteria.sourceRefName") != "refs/heads/factory/run-1" || query.Get("searchCriteria.targetRefName") != "refs/heads/main" || query.Get("searchCriteria.status") != "all" {
		t.Fatalf("search = %v, want the exact branch pair in every status", query)
	}
}

// TestPullRequestStatusMapsCompletedToMergedAndAbandonedToClosed verifies
// the lifecycle mapping that the coordinator relies on.
func TestPullRequestStatusMapsCompletedToMergedAndAbandonedToClosed(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).on("GET "+repositoryPath+"/pullrequests", `{"value":[
		{"pullRequestId":20,"status":"abandoned","sourceRefName":"refs/heads/other","targetRefName":"refs/heads/main"},
		{"pullRequestId":17,"status":"completed","sourceRefName":"refs/heads/factory/run-1","targetRefName":"refs/heads/main",
		 "lastMergeCommit":{"commitId":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}]}`,
		`{"value":[{"pullRequestId":18,"status":"abandoned","sourceRefName":"refs/heads/factory/run-2","targetRefName":"refs/heads/main"}]}`)
	client := newClient(az)

	merged, err := client.FindPullRequest(context.Background(), repository, "factory/run-1", "main")
	if err != nil || merged.Number != 17 || !merged.Merged || merged.State != "closed" || merged.MergeCommitSHA != strings.Repeat("b", 40) {
		t.Fatalf("FindPullRequest() = %#v/%v, want the merged pull request 17", merged, err)
	}
	abandoned, err := client.FindPullRequest(context.Background(), repository, "factory/run-2", "main")
	if err != nil || abandoned.Number != 18 || abandoned.Merged || abandoned.State != "closed" {
		t.Fatalf("FindPullRequest() = %#v/%v, want the closed, unmerged pull request 18", abandoned, err)
	}
}

// TestCreateUpdateAndReadinessUseThePullRequestEndpoints verifies the
// payloads of the three pull-request mutations.
func TestCreateUpdateAndReadinessUseThePullRequestEndpoints(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).
		on("POST "+repositoryPath+"/pullrequests", activePullRequest).
		on("PATCH "+repositoryPath+"/pullrequests/17", activePullRequest, strings.Replace(activePullRequest, `"isDraft":true`, `"isDraft":false`, 1))
	client := newClient(az)
	request := codehost.PullRequestRequest{Title: "Add export", Body: "body", HeadBranch: "factory/run-1", BaseBranch: "main", Draft: true}

	created, err := client.CreatePullRequest(context.Background(), repository, request)
	if err != nil || created.Number != 17 {
		t.Fatalf("CreatePullRequest() = %#v/%v", created, err)
	}
	var payload map[string]any
	decodeBody(t, az.called("POST " + repositoryPath + "/pullrequests")[0], &payload)
	if payload["sourceRefName"] != "refs/heads/factory/run-1" || payload["targetRefName"] != "refs/heads/main" || payload["title"] != "Add export" || payload["description"] != "body" || payload["isDraft"] != true {
		t.Fatalf("create payload = %#v", payload)
	}
	if _, err := client.UpdatePullRequest(context.Background(), repository, 17, request); err != nil {
		t.Fatalf("UpdatePullRequest() error = %v", err)
	}
	ready, err := client.SetPullRequestDraft(context.Background(), repository, 17, false)
	if err != nil || ready.Draft {
		t.Fatalf("SetPullRequestDraft() = %#v/%v, want a ready pull request", ready, err)
	}
	patches := az.called("PATCH " + repositoryPath + "/pullrequests/17")
	var update, readiness map[string]any
	decodeBody(t, patches[0], &update)
	decodeBody(t, patches[1], &readiness)
	if len(update) != 2 || update["title"] != "Add export" || update["description"] != "body" {
		t.Fatalf("update payload = %#v, want only title and description", update)
	}
	if len(readiness) != 1 || readiness["isDraft"] != false {
		t.Fatalf("readiness payload = %#v, want only isDraft", readiness)
	}
}

// TestCreatePullRequestRejectsADescriptionAboveTheAzureLimit verifies that
// the adapter fails before the call instead of truncating the body.
func TestCreatePullRequestRejectsADescriptionAboveTheAzureLimit(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t)
	_, err := newClient(az).CreatePullRequest(context.Background(), repository, codehost.PullRequestRequest{Title: "t", Body: strings.Repeat("x", 4001), HeadBranch: "factory/run-1", BaseBranch: "main"})
	if err == nil || !strings.Contains(err.Error(), "4000") || len(az.calls) != 0 {
		t.Fatalf("CreatePullRequest() error = %v, calls = %d, want a rejection before the call", err, len(az.calls))
	}
}

// TestCommitStatusesUseTheGitStatusAPI verifies the state and context
// mapping in both directions.
func TestCommitStatusesUseTheGitStatusAPI(t *testing.T) {
	t.Parallel()

	sha := strings.Repeat("c", 40)
	az := newFakeAz(t).
		on("POST "+repositoryPath+"/commits/"+sha+"/statuses", `{}`).
		on("GET "+repositoryPath+"/commits/"+sha+"/statuses", `{"value":[
			{"state":"succeeded","description":"ok","context":{"name":"factory/gate/test"},"targetUrl":"https://x"},
			{"state":"failed","description":"no","context":{"genre":"factory","name":"review/spec"}}]}`)
	client := newClient(az)

	err := client.CreateCommitStatus(context.Background(), repository, codehost.CommitStatus{SHA: sha, State: codehost.CommitStatusSuccess, Context: "factory/gate/test", Description: "ok", TargetURL: "https://x"})
	if err != nil {
		t.Fatalf("CreateCommitStatus() error = %v", err)
	}
	var payload struct {
		State       string `json:"state"`
		Description string `json:"description"`
		TargetURL   string `json:"targetUrl"`
		Context     struct {
			Name  string `json:"name"`
			Genre string `json:"genre"`
		} `json:"context"`
	}
	decodeBody(t, az.called("POST " + repositoryPath + "/commits/" + sha + "/statuses")[0], &payload)
	if payload.State != "succeeded" || payload.Context.Name != "factory/gate/test" || payload.Context.Genre != "" || payload.TargetURL != "https://x" {
		t.Fatalf("status payload = %#v", payload)
	}
	statuses, err := client.ListCommitStatuses(context.Background(), repository, sha)
	if err != nil || len(statuses) != 2 {
		t.Fatalf("ListCommitStatuses() = %#v/%v", statuses, err)
	}
	if statuses[0].State != codehost.CommitStatusSuccess || statuses[0].Context != "factory/gate/test" || statuses[1].State != codehost.CommitStatusFailure || statuses[1].Context != "factory/review/spec" {
		t.Fatalf("ListCommitStatuses() = %#v, want neutral states and contexts", statuses)
	}
}

// reviewThreads is a pull request with a rejecting vote by alice, her file
// and general findings, a resolved thread, a later approval by bob, and a
// conversation comment.
const reviewThreads = `{"value":[
	{"id":1,"publishedDate":"2026-10-09T10:00:00Z","status":"active",
	 "threadContext":{"filePath":"/internal/export.go","rightFileStart":{"line":12}},
	 "comments":[{"id":1,"author":{"uniqueName":"alice@contoso.com"},"content":"Handle the empty case.","commentType":"text","publishedDate":"2026-10-09T10:00:00Z"}]},
	{"id":2,"publishedDate":"2026-10-09T10:01:00Z","status":"active",
	 "comments":[{"id":1,"author":{"uniqueName":"alice@contoso.com"},"content":"Add a test for CSV quoting.","commentType":"text","publishedDate":"2026-10-09T10:01:00Z"}]},
	{"id":3,"publishedDate":"2026-10-09T10:01:30Z","status":"fixed",
	 "threadContext":{"filePath":"/README.md","rightFileStart":{"line":1}},
	 "comments":[{"id":1,"author":{"uniqueName":"alice@contoso.com"},"content":"Resolved already.","commentType":"text","publishedDate":"2026-10-09T10:01:30Z"}]},
	{"id":4,"publishedDate":"2026-10-09T10:02:00Z",
	 "properties":{"CodeReviewThreadType":{"$value":"VoteUpdate"},"CodeReviewVoteResult":{"$value":"-5"},"CodeReviewVotedByIdentity":{"$value":"1"}},
	 "identities":{"1":{"uniqueName":"alice@contoso.com"}},
	 "comments":[{"id":1,"author":{"uniqueName":"alice@contoso.com"},"content":"Alice voted -5","commentType":"system","publishedDate":"2026-10-09T10:02:00Z"}]},
	{"id":5,"publishedDate":"2026-10-09T11:00:00Z",
	 "properties":{"CodeReviewThreadType":{"$value":"VoteUpdate"},"CodeReviewVoteResult":{"$value":"10"},"CodeReviewVotedByIdentity":{"$value":"2"}},
	 "identities":{"2":{"uniqueName":"bob@contoso.com"}},
	 "comments":[{"id":1,"author":{"uniqueName":"bob@contoso.com"},"content":"Bob voted 10","commentType":"system","publishedDate":"2026-10-09T11:00:00Z"}]},
	{"id":6,"publishedDate":"2026-10-09T11:05:00Z","status":"active",
	 "comments":[
	  {"id":1,"author":{"uniqueName":"bob@contoso.com"},"content":"/factory status","commentType":"text","publishedDate":"2026-10-09T11:05:00Z","lastUpdatedDate":"2026-10-09T11:06:00Z"},
	  {"id":2,"author":{"uniqueName":"bob@contoso.com"},"content":"deleted","isDeleted":true,"commentType":"text","publishedDate":"2026-10-09T11:07:00Z"}]}]}`

// TestPullRequestReviewsTurnVoteThreadsIntoReviews verifies that a vote is
// a review with a stable identity, that waiting-for-author maps to changes
// requested, and that the voter's open findings become the review content.
func TestPullRequestReviewsTurnVoteThreadsIntoReviews(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).on("GET "+repositoryPath+"/pullRequests/17/threads", reviewThreads)

	reviews, err := newClient(az).PullRequestReviews(context.Background(), repository, 17)
	if err != nil {
		t.Fatalf("PullRequestReviews() error = %v", err)
	}
	if len(reviews) != 2 {
		t.Fatalf("PullRequestReviews() = %#v, want two vote reviews", reviews)
	}
	changes := reviews[0]
	if changes.ID != "20261009T100200.000000000Z.p17.t4" || changes.Author != "alice@contoso.com" || changes.State != codehost.PullRequestReviewChangesRequested || !changes.SubmittedAt.Equal(time.Date(2026, 10, 9, 10, 2, 0, 0, time.UTC)) {
		t.Fatalf("review = %#v, want alice's changes-requested vote", changes)
	}
	if changes.Body != "Add a test for CSV quoting." || len(changes.Comments) != 1 || changes.Comments[0] != (codehost.PullRequestReviewComment{Path: "internal/export.go", Line: 12, Body: "Handle the empty case."}) {
		t.Fatalf("review content = %q %#v, want the open general and file findings", changes.Body, changes.Comments)
	}
	if reviews[1].State != codehost.PullRequestReviewApproved || reviews[1].Author != "bob@contoso.com" {
		t.Fatalf("review = %#v, want bob's approval", reviews[1])
	}
}

// TestPullRequestCommentsListTheConversationWithoutVotes verifies the
// conversation comments, their identities, and that system comments and
// deleted comments are left out.
func TestPullRequestCommentsListTheConversationWithoutVotes(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).on("GET "+repositoryPath+"/pullRequests/17/threads", reviewThreads)

	comments, err := newClient(az).PullRequestComments(context.Background(), repository, 17)
	if err != nil {
		t.Fatalf("PullRequestComments() error = %v", err)
	}
	if len(comments) != 4 {
		t.Fatalf("PullRequestComments() = %#v, want four conversation comments", comments)
	}
	last := comments[3]
	if last.ID != "20261009T110500.000000000Z.p17.t6.c1" || last.Body != "/factory status" || last.Author != "bob@contoso.com" || !last.UpdatedAt.Equal(time.Date(2026, 10, 9, 11, 6, 0, 0, time.UTC)) {
		t.Fatalf("comment = %#v, want bob's command with a time-ordered identity", last)
	}
}

// TestPullRequestCommentIsCreatedFoundAndEdited verifies that a coordinator
// comment opens a closed thread, is recovered by its marker, and is edited
// through the thread and comment in its identity.
func TestPullRequestCommentIsCreatedFoundAndEdited(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).
		on("POST "+repositoryPath+"/pullRequests/17/threads", `{"id":9,"publishedDate":"2026-10-09T12:00:00Z","comments":[{"id":1,"author":{"uniqueName":"alice@contoso.com"},"content":"q","commentType":"text","publishedDate":"2026-10-09T12:00:00Z"}]}`).
		on("GET /contoso/_apis/connectionData", connectionData).
		on("GET "+repositoryPath+"/pullRequests/17/threads", `{"value":[{"id":9,"publishedDate":"2026-10-09T12:00:00Z","comments":[{"id":1,"author":{"uniqueName":"alice@contoso.com"},"content":"q <!-- factory-clarification: run-1 v1 -->","commentType":"text","publishedDate":"2026-10-09T12:00:00Z"}]}]}`).
		on("PATCH "+repositoryPath+"/pullRequests/17/threads/9/comments/1", `{}`)
	client := newClient(az)

	created, err := client.CreatePullRequestComment(context.Background(), repository, 17, "q <!-- factory-clarification: run-1 v1 -->")
	if err != nil || created.ID != "20261009T120000.000000000Z.p17.t9.c1" {
		t.Fatalf("CreatePullRequestComment() = %#v/%v", created, err)
	}
	var thread struct {
		Status   string `json:"status"`
		Comments []struct {
			Content     string `json:"content"`
			CommentType string `json:"commentType"`
		} `json:"comments"`
	}
	decodeBody(t, az.called("POST " + repositoryPath + "/pullRequests/17/threads")[0], &thread)
	if thread.Status != "closed" || len(thread.Comments) != 1 || thread.Comments[0].CommentType != "text" {
		t.Fatalf("thread = %#v, want one text comment in a closed thread", thread)
	}
	found, err := client.FindPullRequestComment(context.Background(), repository, 17, "<!-- factory-clarification: run-1 v1 -->")
	if err != nil || found.ID != created.ID {
		t.Fatalf("FindPullRequestComment() = %#v/%v", found, err)
	}
	if err := client.EditPullRequestComment(context.Background(), repository, 17, found.ID, "new"); err != nil {
		t.Fatalf("EditPullRequestComment() error = %v", err)
	}
	var edit map[string]string
	decodeBody(t, az.called("PATCH " + repositoryPath + "/pullRequests/17/threads/9/comments/1")[0], &edit)
	if edit["content"] != "new" {
		t.Fatalf("edit = %#v", edit)
	}
}
