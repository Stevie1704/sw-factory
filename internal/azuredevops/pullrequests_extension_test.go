package azuredevops_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/azuredevops"
	"github.com/Stevie1704/sw-factory/internal/azuredevops/azuredevopstest"
	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/effect"
)

// serviceRepository is the az repos projection of the registered
// repository inside a pull-request response.
const serviceRepository = `"repository":{"id":"` + azuredevopstest.RepositoryID + `","name":"service","project":{"id":"` + azuredevopstest.ProjectID + `","name":"Factory Pilot"}}`

// webRepository is the second repository of the same project.
const webRepository = `"repository":{"id":"` + azuredevopstest.WebRepositoryID + `","name":"web","project":{"id":"` + azuredevopstest.ProjectID + `","name":"Factory Pilot"}}`

// extensionPullRequest is an open draft pull request of the run branch as
// az repos pr show prints it.
const extensionPullRequest = `{"pullRequestId":17,"status":"active","isDraft":true,"title":"Add export","description":"body",
	"sourceRefName":"refs/heads/factory/run-1","targetRefName":"refs/heads/main",
	"lastMergeSourceCommit":{"commitId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},` + serviceRepository + `}`

// TestExtensionFindPullRequestListsTheBranchPairAndReadsTheFullBody verifies
// that the extension route searches one repository with az repos pr list,
// explicitly targeted, and reads the match with az repos pr show because
// the list truncates descriptions.
func TestExtensionFindPullRequestListsTheBranchPairAndReadsTheFullBody(t *testing.T) {
	t.Parallel()

	truncated := strings.Replace(extensionPullRequest, `"description":"body"`, `"description":"bo"`, 1)
	other := `{"pullRequestId":20,"status":"active","sourceRefName":"refs/heads/other","targetRefName":"refs/heads/main",` + serviceRepository + `}`
	az := newFakeAz(t).
		on("repos pr list", `[`+other+`,`+truncated+`]`).
		on("repos pr show", extensionPullRequest)

	pullRequest, err := extensionClient(az).FindPullRequest(context.Background(), repository, "factory/run-1", "main")
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
	if len(az.calls) != 0 || len(az.extensionCalls) != 2 {
		t.Fatalf("calls = %d az rest, %d extension; want 0 and 2", len(az.calls), len(az.extensionCalls))
	}
	list := az.extensionCalls[0]
	if list.Flag("--project") != "Factory Pilot" || list.Flag("--repository") != "service" || list.Flag("--source-branch") != "factory/run-1" ||
		list.Flag("--target-branch") != "main" || list.Flag("--status") != "all" || list.Flag("--top") != "100" {
		t.Fatalf("list = %+v, want the exact branch pair of the registered repository in every status", list.Flags)
	}
	if show := az.extensionCalls[1]; show.Route() != "repos pr show" || show.Flag("--id") != "17" {
		t.Fatalf("show = %+v, want az repos pr show --id 17", show)
	}
}

// TestExtensionPullRequestReadsRefuseAnotherRepository verifies that an
// organization-scoped pull-request read for a pull request of another
// repository in the same project is refused, not adopted.
func TestExtensionPullRequestReadsRefuseAnotherRepository(t *testing.T) {
	t.Parallel()

	foreign := strings.Replace(extensionPullRequest, serviceRepository, webRepository, 1)
	az := newFakeAz(t).
		on("repos pr list", `[`+foreign+`]`).
		on("repos pr show", foreign)

	if _, err := extensionClient(az).FindPullRequest(context.Background(), repository, "factory/run-1", "main"); err == nil || !strings.Contains(err.Error(), "another repository") {
		t.Fatalf("FindPullRequest() error = %v, want a refused foreign pull request", err)
	}
}

// TestExtensionCreateUpdateAndReadinessUseDedicatedCommands verifies that
// create, update, and readiness run az repos pr create and update, that the
// title and the whole description each travel as one argument so Markdown
// lines that start with '-' or contain '=@' reach Azure Repos unchanged,
// and that update and readiness first check the repository of the
// organization-scoped pull request.
func TestExtensionCreateUpdateAndReadinessUseDedicatedCommands(t *testing.T) {
	t.Parallel()

	body := "---\n- item\n--title\na=@b\n"
	az := newFakeAz(t).
		on("repos pr create", extensionPullRequest).
		on("repos pr show", extensionPullRequest).
		on("repos pr update", extensionPullRequest)
	client := extensionClient(az)
	request := codehost.PullRequestRequest{Title: "-Add export", Body: body, HeadBranch: "factory/run-1", BaseBranch: "main", Draft: true}

	created, err := client.CreatePullRequest(context.Background(), repository, request)
	if err != nil || created.Number != 17 {
		t.Fatalf("CreatePullRequest() = %#v/%v, want pull request 17", created, err)
	}
	create := az.extensionCalls[0]
	if create.Route() != "repos pr create" || create.Flag("--project") != "Factory Pilot" || create.Flag("--repository") != "service" ||
		create.Flag("--source-branch") != "factory/run-1" || create.Flag("--target-branch") != "main" || create.Flag("--draft") != "true" {
		t.Fatalf("create = %+v, want an explicitly targeted draft pull request", create.Flags)
	}
	if got := create.Flags["--description"]; len(got) != 1 || got[0] != body || create.Flag("--title") != "-Add export" {
		t.Fatalf("create title/description = %q/%q, want each as one unchanged value", create.Flag("--title"), got)
	}
	if _, ok := create.Flags["--work-items"]; ok {
		t.Fatal("create links a work item without a source work item")
	}

	if _, err := client.UpdatePullRequest(context.Background(), repository, 17, request); err != nil {
		t.Fatalf("UpdatePullRequest() error = %v", err)
	}
	if _, err := client.SetPullRequestDraft(context.Background(), repository, 17, false); err != nil {
		t.Fatalf("SetPullRequestDraft() error = %v", err)
	}
	routes := make([]string, 0)
	for _, call := range az.extensionCalls[1:] {
		routes = append(routes, call.Route())
	}
	if strings.Join(routes, ",") != "repos pr show,repos pr update,repos pr show,repos pr update" {
		t.Fatalf("routes = %v, want a repository check before each update", routes)
	}
	update, ready := az.extensionCalls[2], az.extensionCalls[4]
	if update.Flag("--id") != "17" || update.Flags["--description"][0] != body || update.Flag("--title") != "-Add export" {
		t.Fatalf("update = %+v, want title and description of pull request 17", update.Flags)
	}
	if _, ok := update.Flags["--draft"]; ok {
		t.Fatal("update changes the draft flag")
	}
	if ready.Flag("--id") != "17" || ready.Flag("--draft") != "false" || len(ready.Flags["--title"]) != 0 {
		t.Fatalf("readiness = %+v, want only --draft false", ready.Flags)
	}
}

// TestExtensionPullRequestTextIsCheckedBeforeAnyCall verifies that a
// description above the Azure limit and text that Azure CLI would read from
// a file fail before any command runs.
func TestExtensionPullRequestTextIsCheckedBeforeAnyCall(t *testing.T) {
	t.Parallel()

	for name, request := range map[string]codehost.PullRequestRequest{
		"long body":  {Title: "t", Body: strings.Repeat("x", 4001), HeadBranch: "factory/run-1", BaseBranch: "main"},
		"file body":  {Title: "t", Body: "@notes.md", HeadBranch: "factory/run-1", BaseBranch: "main"},
		"file title": {Title: "@t", Body: "b", HeadBranch: "factory/run-1", BaseBranch: "main"},
	} {
		az := newFakeAz(t)
		if _, err := extensionClient(az).CreatePullRequest(context.Background(), repository, request); err == nil {
			t.Fatalf("%s: CreatePullRequest() error = nil, want a refused request", name)
		}
		if _, err := extensionClient(az).UpdatePullRequest(context.Background(), repository, 17, request); err == nil {
			t.Fatalf("%s: UpdatePullRequest() error = nil, want a refused request", name)
		}
		if len(az.extensionCalls) != 0 {
			t.Fatalf("%s: calls = %d, want none", name, len(az.extensionCalls))
		}
	}
}

// sourceItem is PBI 42 of a Run frozen to the registered repository.
var sourceItem = codehost.WorkItemLink{ID: 42, RepositoryID: azuredevopstest.RepositoryID}

// TestExtensionCreateLinksTheSourceWorkItemInTheFrozenRepository verifies
// that a pull request of an Azure Boards-backed Run is created in the
// frozen repository GUID with the source PBI through --work-items.
func TestExtensionCreateLinksTheSourceWorkItemInTheFrozenRepository(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).
		on("repos show", `{"id":"`+azuredevopstest.RepositoryID+`","name":"service","project":{"id":"`+azuredevopstest.ProjectID+`","name":"Factory Pilot"}}`).
		on("repos pr create", extensionPullRequest)
	request := codehost.PullRequestRequest{Title: "Add export", Body: "body", HeadBranch: "factory/run-1", BaseBranch: "main", Draft: true, WorkItem: &sourceItem}

	created, err := extensionClient(az).CreatePullRequest(context.Background(), repository, request)
	if err != nil || created.Number != 17 {
		t.Fatalf("CreatePullRequest() = %#v/%v, want pull request 17", created, err)
	}
	create := az.extensionCalls[1]
	if az.extensionCalls[0].Route() != "repos show" || create.Route() != "repos pr create" {
		t.Fatalf("calls = %v, want the repository resolved before create", az.extensionCalls)
	}
	if create.Flag("--repository") != azuredevopstest.RepositoryID || create.Flag("--project") != "Factory Pilot" || create.Flag("--work-items") != "42" {
		t.Fatalf("create = %+v, want the frozen repository GUID and PBI 42", create.Flags)
	}
}

// TestExtensionCreateRefusesARegistrationThatNoLongerNamesTheFrozenRepository
// verifies that a registered name that now resolves to another GUID than
// the Run's frozen repository creates nothing.
func TestExtensionCreateRefusesARegistrationThatNoLongerNamesTheFrozenRepository(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).on("repos show", `{"id":"`+azuredevopstest.WebRepositoryID+`","name":"service","project":{"id":"`+azuredevopstest.ProjectID+`","name":"Factory Pilot"}}`)
	request := codehost.PullRequestRequest{Title: "Add export", Body: "body", HeadBranch: "factory/run-1", BaseBranch: "main", Draft: true, WorkItem: &sourceItem}

	if _, err := extensionClient(az).CreatePullRequest(context.Background(), repository, request); err == nil || !strings.Contains(err.Error(), "frozen") {
		t.Fatalf("CreatePullRequest() error = %v, want a refused repository", err)
	}
	for _, call := range az.extensionCalls {
		if call.Route() == "repos pr create" {
			t.Fatal("CreatePullRequest() created a pull request in another repository")
		}
	}
}

// TestEnsurePullRequestWorkItemRecoversAMissingAssociation verifies that a
// pull request without its source PBI gains it through az repos pr
// work-item add, and that the association is read back.
func TestEnsurePullRequestWorkItemRecoversAMissingAssociation(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).
		on("repos pr show", extensionPullRequest).
		on("repos pr work-item list", `[{"id":7}]`, `[{"id":7},{"id":42}]`).
		on("repos pr work-item add", `[{"id":7},{"id":42}]`)

	if err := extensionClient(az).EnsurePullRequestWorkItem(context.Background(), repository, 17, sourceItem); err != nil {
		t.Fatalf("EnsurePullRequestWorkItem() error = %v", err)
	}
	routes := make([]string, 0)
	for _, call := range az.extensionCalls {
		routes = append(routes, call.Route())
	}
	if strings.Join(routes, ",") != "repos pr show,repos pr work-item list,repos pr work-item add,repos pr work-item list" {
		t.Fatalf("routes = %v, want check, list, add, and verify", routes)
	}
	if add := az.extensionCalls[2]; add.Flag("--id") != "17" || add.Flag("--work-items") != "42" {
		t.Fatalf("add = %+v, want PBI 42 on pull request 17", add.Flags)
	}
}

// TestEnsurePullRequestWorkItemKeepsAnExistingAssociation verifies that a
// linked pull request is verified without another write.
func TestEnsurePullRequestWorkItemKeepsAnExistingAssociation(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).
		on("repos pr show", extensionPullRequest).
		on("repos pr work-item list", `[{"id":42}]`)

	if err := extensionClient(az).EnsurePullRequestWorkItem(context.Background(), repository, 17, sourceItem); err != nil {
		t.Fatalf("EnsurePullRequestWorkItem() error = %v", err)
	}
	if len(az.extensionCalls) != 2 {
		t.Fatalf("calls = %v, want only the check and the list", az.extensionCalls)
	}
}

// TestEnsurePullRequestWorkItemRefusesAWrongRepositoryOrAnUnverifiedLink
// verifies that a pull request of another repository is never linked, and
// that an add the association list does not confirm fails.
func TestEnsurePullRequestWorkItemRefusesAWrongRepositoryOrAnUnverifiedLink(t *testing.T) {
	t.Parallel()

	foreign := newFakeAz(t).on("repos pr show", strings.Replace(extensionPullRequest, serviceRepository, webRepository, 1))
	if err := extensionClient(foreign).EnsurePullRequestWorkItem(context.Background(), repository, 17, sourceItem); err == nil {
		t.Fatal("EnsurePullRequestWorkItem() error = nil, want a refused foreign pull request")
	}
	if len(foreign.extensionCalls) != 1 {
		t.Fatalf("calls = %v, want only the repository check", foreign.extensionCalls)
	}

	frozenElsewhere := codehost.WorkItemLink{ID: 42, RepositoryID: azuredevopstest.WebRepositoryID}
	az := newFakeAz(t).on("repos pr show", extensionPullRequest)
	if err := extensionClient(az).EnsurePullRequestWorkItem(context.Background(), repository, 17, frozenElsewhere); err == nil {
		t.Fatal("EnsurePullRequestWorkItem() error = nil, want a refused frozen repository")
	}

	unverified := newFakeAz(t).
		on("repos pr show", extensionPullRequest).
		on("repos pr work-item list", `[]`).
		on("repos pr work-item add", `[]`)
	if err := extensionClient(unverified).EnsurePullRequestWorkItem(context.Background(), repository, 17, sourceItem); err == nil || !strings.Contains(err.Error(), "42") {
		t.Fatalf("EnsurePullRequestWorkItem() error = %v, want an unverified association naming PBI 42", err)
	}
}

// TestExtensionLostCreateResponseRecoversThePullRequestAndItsLink verifies
// the retry after a create whose response was lost: the branch search
// adopts the pull request that the lost call created, and the missing PBI
// association is added without a second create.
func TestExtensionLostCreateResponseRecoversThePullRequestAndItsLink(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).
		on("repos show", `{"id":"`+azuredevopstest.RepositoryID+`","name":"service","project":{"id":"`+azuredevopstest.ProjectID+`","name":"Factory Pilot"}}`).
		on("repos pr list", `[]`, `[`+extensionPullRequest+`]`).
		on("repos pr create", "error:the operation timed out").
		on("repos pr show", extensionPullRequest).
		on("repos pr work-item list", `[]`, `[{"id":42}]`).
		on("repos pr work-item add", `[{"id":42}]`)
	client := extensionClient(az)
	request := codehost.PullRequestRequest{Title: "Add export", Body: "body", HeadBranch: "factory/run-1", BaseBranch: "main", Draft: true, WorkItem: &sourceItem}

	if _, err := effect.UpsertPullRequest(context.Background(), client, repository, 0, request); err == nil {
		t.Fatal("first UpsertPullRequest() error = nil, want the lost response")
	}
	recovered, err := effect.UpsertPullRequest(context.Background(), client, repository, 0, request)
	if err != nil || recovered.Number != 17 {
		t.Fatalf("retry UpsertPullRequest() = %#v/%v, want the adopted pull request 17", recovered, err)
	}
	creates, adds := 0, 0
	for _, call := range az.extensionCalls {
		switch call.Route() {
		case "repos pr create":
			creates++
		case "repos pr work-item add":
			adds++
		}
	}
	if creates != 1 || adds != 1 {
		t.Fatalf("creates = %d, adds = %d; want one create and one recovered association", creates, adds)
	}
}

// TestExtensionPullRequestOperationsReportAMissingLogin verifies that an
// expired or missing az devops login surfaces as LoginRequiredError from
// the pull-request operations, with the az devops login repair.
func TestExtensionPullRequestOperationsReportAMissingLogin(t *testing.T) {
	t.Parallel()

	client := extensionClient(loginRequiredRunner{})
	_, err := client.FindPullRequest(context.Background(), repository, "factory/run-1", "main")
	var login *azuredevops.LoginRequiredError
	if !errors.As(err, &login) || !strings.Contains(err.Error(), "az devops login --organization https://dev.azure.com/contoso") {
		t.Fatalf("FindPullRequest() error = %v, want LoginRequiredError", err)
	}
	if err := client.CreateCommitStatus(context.Background(), repository, codehost.CommitStatus{SHA: strings.Repeat("c", 40), State: codehost.CommitStatusPending, Context: "factory/test"}); !errors.As(err, &login) {
		t.Fatalf("CreateCommitStatus() error = %v, want LoginRequiredError", err)
	}
}

// wantInvoke fails the test unless call is the az devops invoke of area,
// resource, and method at API version 7.1 with exactly the route parameters.
func wantInvoke(t *testing.T, call azuredevopstest.ExtensionCall, route string, parameters map[string]string) {
	t.Helper()
	if call.Route() != route || call.Flag("--api-version") != "7.1" {
		t.Fatalf("call = %s at %q, want %s at 7.1", call.Route(), call.Flag("--api-version"), route)
	}
	got := call.Parameters("--route-parameters")
	if len(got) != len(parameters) {
		t.Fatalf("route parameters = %v, want %v", got, parameters)
	}
	for key, value := range parameters {
		if got[key] != value {
			t.Fatalf("route parameters = %v, want %v", got, parameters)
		}
	}
}

// TestExtensionCommitStatusesUseTheGitStatusesInvokeRoute verifies that
// commit statuses are written and read through az devops invoke git/statuses
// with the registered project, repository, and commit as route parameters.
func TestExtensionCommitStatusesUseTheGitStatusesInvokeRoute(t *testing.T) {
	t.Parallel()

	sha := strings.Repeat("c", 40)
	az := newFakeAz(t).
		on("devops invoke POST git/statuses", `{}`).
		on("devops invoke GET git/statuses", `{"count":2,"value":[
			{"state":"succeeded","description":"ok","context":{"name":"factory/gate/test"},"targetUrl":"https://x"},
			{"state":"failed","description":"no","context":{"genre":"factory","name":"review/spec"}}],"continuation_token":null}`)
	client := extensionClient(az)

	err := client.CreateCommitStatus(context.Background(), repository, codehost.CommitStatus{SHA: sha, State: codehost.CommitStatusSuccess, Context: "factory/gate/test", Description: "ok", TargetURL: "https://x"})
	if err != nil {
		t.Fatalf("CreateCommitStatus() error = %v", err)
	}
	route := map[string]string{"project": "Factory Pilot", "repositoryId": "service", "commitId": sha}
	post := az.extensionCalls[0]
	wantInvoke(t, post, "devops invoke POST git/statuses", route)
	var payload struct {
		State   string `json:"state"`
		Context struct {
			Name string `json:"name"`
		} `json:"context"`
		TargetURL string `json:"targetUrl"`
	}
	if err := json.Unmarshal([]byte(post.Body), &payload); err != nil || payload.State != "succeeded" || payload.Context.Name != "factory/gate/test" || payload.TargetURL != "https://x" {
		t.Fatalf("status body = %q/%v", post.Body, err)
	}
	statuses, err := client.ListCommitStatuses(context.Background(), repository, sha)
	if err != nil || len(statuses) != 2 || statuses[1].Context != "factory/review/spec" || statuses[1].State != codehost.CommitStatusFailure {
		t.Fatalf("ListCommitStatuses() = %#v/%v", statuses, err)
	}
	list := az.extensionCalls[1]
	wantInvoke(t, list, "devops invoke GET git/statuses", route)
	if query := list.Parameters("--query-parameters"); query["latestOnly"] != "false" || query["top"] != "1000" || query["skip"] != "0" {
		t.Fatalf("status query = %v, want every status from the first page", query)
	}
}

// extensionReviewers is the az repos pr reviewer list output that resolves
// the voters' identity ids.
const extensionReviewers = `[
	{"id":"a1a1a1a1-0000-0000-0000-000000000001","uniqueName":"alice@contoso.com","vote":-5},
	{"id":"b2b2b2b2-0000-0000-0000-000000000002","uniqueName":"bob@contoso.com","vote":10}]`

// TestExtensionReviewsAndCommentsReadThreadsThroughInvoke verifies that
// reviews and conversation comments come from the pullRequestThreads invoke
// route, and that the reviewer list runs only after the pull request was
// checked to belong to the registered repository.
func TestExtensionReviewsAndCommentsReadThreadsThroughInvoke(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).
		on("devops invoke GET git/pullRequestThreads", reviewThreads).
		on("repos pr show", extensionPullRequest).
		on("repos pr reviewer list", extensionReviewers)
	client := extensionClient(az)

	reviews, err := client.PullRequestReviews(context.Background(), repository, 17)
	if err != nil || len(reviews) != 2 {
		t.Fatalf("PullRequestReviews() = %#v/%v, want two vote reviews", reviews, err)
	}
	if reviews[0].Author != "alice@contoso.com" || reviews[0].State != codehost.PullRequestReviewChangesRequested || reviews[0].Body != "Add a test for CSV quoting." || len(reviews[0].Comments) != 1 {
		t.Fatalf("review = %#v, want alice's changes-requested vote with her findings", reviews[0])
	}
	if reviews[1].Author != "bob@contoso.com" || reviews[1].State != codehost.PullRequestReviewApproved {
		t.Fatalf("review = %#v, want bob's approval", reviews[1])
	}
	wantInvoke(t, az.extensionCalls[0], "devops invoke GET git/pullRequestThreads", map[string]string{"project": "Factory Pilot", "repositoryId": "service", "pullRequestId": "17"})
	if az.extensionCalls[1].Route() != "repos pr show" || az.extensionCalls[2].Route() != "repos pr reviewer list" || az.extensionCalls[2].Flag("--id") != "17" {
		t.Fatalf("calls = %v, want a repository check before az repos pr reviewer list --id 17", az.extensionCalls[1:])
	}

	comments, err := client.PullRequestComments(context.Background(), repository, 17)
	if err != nil || len(comments) != 4 || comments[3].ID != "20261009T110500.000000000Z.p17.t6.c1" {
		t.Fatalf("PullRequestComments() = %#v/%v, want four time-ordered conversation comments", comments, err)
	}
}

// TestExtensionReviewsRefuseAReviewerListOfAnotherRepository verifies that
// a pull request number of another repository never reads that pull
// request's reviewers.
func TestExtensionReviewsRefuseAReviewerListOfAnotherRepository(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).
		on("devops invoke GET git/pullRequestThreads", reviewThreads).
		on("repos pr show", strings.Replace(extensionPullRequest, serviceRepository, webRepository, 1))

	if _, err := extensionClient(az).PullRequestReviews(context.Background(), repository, 17); err == nil {
		t.Fatal("PullRequestReviews() error = nil, want a refused foreign pull request")
	}
	for _, call := range az.extensionCalls {
		if call.Route() == "repos pr reviewer list" {
			t.Fatal("PullRequestReviews() read the reviewers of a foreign pull request")
		}
	}
}

// TestExtensionCommentIsCreatedInAClosedThreadAndEdited verifies that a
// coordinator comment opens a closed thread through invoke, keeps its
// thread-scoped identity, and is edited through pullRequestThreadComments.
func TestExtensionCommentIsCreatedInAClosedThreadAndEdited(t *testing.T) {
	t.Parallel()

	az := newFakeAz(t).
		on("devops invoke POST git/pullRequestThreads", `{"id":9,"publishedDate":"2026-10-09T12:00:00Z","comments":[{"id":1,"author":{"uniqueName":"alice@contoso.com"},"content":"q","commentType":"text","publishedDate":"2026-10-09T12:00:00Z"}]}`).
		on("devops invoke PATCH git/pullRequestThreadComments", `{}`)
	client := extensionClient(az)

	created, err := client.CreatePullRequestComment(context.Background(), repository, 17, "q <!-- marker -->")
	if err != nil || created.ID != "20261009T120000.000000000Z.p17.t9.c1" || created.Body != "q <!-- marker -->" {
		t.Fatalf("CreatePullRequestComment() = %#v/%v", created, err)
	}
	post := az.extensionCalls[0]
	wantInvoke(t, post, "devops invoke POST git/pullRequestThreads", map[string]string{"project": "Factory Pilot", "repositoryId": "service", "pullRequestId": "17"})
	var thread struct {
		Status   string `json:"status"`
		Comments []struct {
			Content     string `json:"content"`
			CommentType string `json:"commentType"`
		} `json:"comments"`
	}
	if err := json.Unmarshal([]byte(post.Body), &thread); err != nil || thread.Status != "closed" || len(thread.Comments) != 1 || thread.Comments[0].Content != "q <!-- marker -->" {
		t.Fatalf("thread body = %q/%v, want one text comment in a closed thread", post.Body, err)
	}
	if err := client.EditPullRequestComment(context.Background(), repository, 17, created.ID, "new"); err != nil {
		t.Fatalf("EditPullRequestComment() error = %v", err)
	}
	edit := az.extensionCalls[1]
	wantInvoke(t, edit, "devops invoke PATCH git/pullRequestThreadComments", map[string]string{"project": "Factory Pilot", "repositoryId": "service", "pullRequestId": "17", "threadId": "9", "commentId": "1"})
	if edit.Body != `{"content":"new"}` {
		t.Fatalf("edit body = %q", edit.Body)
	}
}
