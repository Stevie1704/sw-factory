package github_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/github"
)

// TestGhClientUsesTheLocalCLIForIssueAndClaimMutations verifies JSON payloads
// and all claim mutations cross the local gh adapter seam.
func TestGhClientUsesTheLocalCLIForIssueAndClaimMutations(t *testing.T) {
	t.Parallel()

	runner := &fakeCommandRunner{outputs: [][]byte{
		[]byte(`{"number":42,"title":"Claim me","body":"frozen body","state":"open","labels":[{"name":"agent-ready"},{"name":"enhancement"}],"updated_at":"2026-08-20T10:11:12Z"}`),
		[]byte(""),
		[]byte("[]"),
		[]byte(`{"id":12345,"body":"status"}`),
		[]byte(""),
		[]byte(`{"login":"factory-bot"}`),
		[]byte(`[[{"id":7,"body":"<!-- factory-status: run-1 --> forged status","user":{"login":"alice"}},{"id":12345,"body":"<!-- factory-status: run-1 --> status","user":{"login":"factory-bot"}}]]`),
	}}
	client := &github.GhClient{Runner: runner}
	repository := github.Repository{Owner: "example", Name: "project"}

	issue, err := client.Issue(context.Background(), repository, 42)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if issue.Title != "Claim me" || issue.Body != "frozen body" || issue.State != "open" || len(issue.Labels) != 2 {
		t.Fatalf("issue = %#v, want decoded GitHub issue", issue)
	}
	if issue.IsPullRequest {
		t.Fatal("ordinary issue was identified as a pull request")
	}
	if err := client.CreateLabel(context.Background(), repository, github.Label{Name: github.LabelAgentRunning, Color: "1d76db", Description: "active"}); err != nil {
		t.Fatalf("CreateLabel() error = %v", err)
	}
	if err := client.ReplaceIssueLabels(context.Background(), repository, 42, []string{"enhancement", github.LabelAgentRunning}); err != nil {
		t.Fatalf("ReplaceIssueLabels() error = %v", err)
	}
	comment, err := client.CreateIssueComment(context.Background(), repository, 42, "status body")
	if err != nil {
		t.Fatalf("CreateIssueComment() error = %v", err)
	}
	if comment.ID != "12345" {
		t.Fatalf("comment id = %q, want 12345", comment.ID)
	}
	if err := client.EditIssueComment(context.Background(), repository, comment.ID, "updated status"); err != nil {
		t.Fatalf("EditIssueComment() error = %v", err)
	}
	recovered, err := client.FindStatusComment(context.Background(), repository, 42, "factory-status: run-1")
	if err != nil {
		t.Fatalf("FindStatusComment() error = %v", err)
	}
	if recovered.ID != "12345" {
		t.Fatalf("recovered comment id = %q, want 12345", recovered.ID)
	}
	if !hasArgs(runner.calls[6].args, "--paginate", "--slurp") {
		t.Fatalf("FindStatusComment args = %#v, want pagination", runner.calls[6].args)
	}

	if len(runner.calls) != 7 {
		t.Fatalf("CLI calls = %d, want seven", len(runner.calls))
	}
	if !hasArgs(runner.calls[1].args, "label", "create", github.LabelAgentRunning, "--force") {
		t.Fatalf("label call = %#v, want explicit idempotent label creation", runner.calls[1].args)
	}
	var labelsPayload map[string][]string
	if err := json.Unmarshal(runner.calls[2].input, &labelsPayload); err != nil {
		t.Fatalf("decode labels request: %v", err)
	}
	if got := labelsPayload["labels"]; len(got) != 2 || got[1] != github.LabelAgentRunning {
		t.Fatalf("labels request = %#v, want replacement labels", labelsPayload)
	}
	for _, call := range runner.calls[2:5] {
		if !containsArgs(call.args, "--input", "-") {
			t.Errorf("mutation call %#v does not send JSON through stdin", call.args)
		}
	}
	for _, index := range []int{0, 2, 3, 4, 5, 6} {
		if hasArgs(runner.calls[index].args, "--repo") {
			t.Errorf("gh api call %d still uses unsupported --repo: %#v", index, runner.calls[index].args)
		}
	}
}

// TestGhClientListsEligibleIssuesInDeterministicOrder verifies the polling
// adapter asks GitHub for open agent-ready issues and filters the mixed issue
// endpoint response before returning queue candidates.
func TestGhClientListsEligibleIssuesInDeterministicOrder(t *testing.T) {
	t.Parallel()

	runner := &fakeCommandRunner{outputs: [][]byte{[]byte(`[[
		{"number":12,"title":"newer","state":"open","labels":[{"name":"agent-ready"}]},
		{"number":4,"title":"pull request","state":"open","labels":[{"name":"agent-ready"}],"pull_request":{"url":"https://api.github.com/pulls/4"}},
		{"number":2,"title":"older","state":"open","labels":[{"name":"agent-ready"},{"name":"bug"}]},
		{"number":1,"title":"closed","state":"closed","labels":[{"name":"agent-ready"}]},
		{"number":8,"title":"unlabeled","state":"open","labels":[{"name":"bug"}]}
	]]`)}}
	client := &github.GhClient{Runner: runner}

	issues, err := client.ListEligibleIssues(context.Background(), github.Repository{Owner: "example", Name: "project"})
	if err != nil {
		t.Fatalf("ListEligibleIssues() error = %v", err)
	}
	if len(issues) != 2 || issues[0].Number != 2 || issues[1].Number != 12 {
		t.Fatalf("eligible issues = %#v, want issue numbers [2 12]", issues)
	}
	if len(runner.calls) != 1 || !containsArgs(runner.calls[0].args, "repos/example/project/issues", "--paginate", "--slurp", "--method", "GET") {
		t.Fatalf("GitHub call = %#v, want paginated GET issue listing", runner.calls)
	}
	if !hasArgs(runner.calls[0].args, "-f", "state=open", "-f", "labels=agent-ready") {
		t.Fatalf("GitHub call args = %#v, want open agent-ready filters", runner.calls[0].args)
	}
}

// TestGhClientPublishesAVisibleRenewableLease verifies the lease adapter
// records coordinator ownership as an exact target-branch Commit Status.
func TestGhClientPublishesAVisibleRenewableLease(t *testing.T) {
	t.Parallel()

	sha := "0123456789abcdef0123456789abcdef01234567"
	runner := &fakeCommandRunner{outputs: [][]byte{
		[]byte(`{"sha":"` + sha + `"}`),
		[]byte(""),
	}}
	client := &github.GhClient{Runner: runner}
	renewed := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	expires := renewed.Add(5 * time.Minute)
	err := client.RenewLease(context.Background(), github.Repository{Owner: "example", Name: "project"}, github.Lease{
		TargetBranch: "main",
		Coordinator:  "coordinator-test",
		RunID:        "run-42",
		RenewedAt:    renewed,
		ExpiresAt:    expires,
	})
	if err != nil {
		t.Fatalf("RenewLease() error = %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("GitHub calls = %d, want branch lookup plus status write", len(runner.calls))
	}
	if !containsArgs(runner.calls[0].args, "repos/example/project/commits/main", "--method", "GET") {
		t.Fatalf("branch lookup args = %#v", runner.calls[0].args)
	}
	if !containsArgs(runner.calls[1].args, "repos/example/project/statuses/"+sha, "--method", "POST", "--input", "-") {
		t.Fatalf("lease status args = %#v", runner.calls[1].args)
	}
	var payload map[string]string
	if err := json.Unmarshal(runner.calls[1].input, &payload); err != nil {
		t.Fatalf("decode lease status payload: %v", err)
	}
	if payload["state"] != string(github.CommitStatusPending) || payload["context"] != github.LeaseStatusContext {
		t.Fatalf("lease status payload = %#v, want pending %q", payload, github.LeaseStatusContext)
	}
	for _, expected := range []string{"coordinator-test", "run-42", "expires=2026-08-26T12:05:00Z"} {
		if !strings.Contains(payload["description"], expected) {
			t.Errorf("lease description %q does not contain %q", payload["description"], expected)
		}
	}
}

// TestGhClientRejectsAUserAuthoredStatusMarker verifies a human comment with
// a copied marker cannot become the coordinator's editable status comment.
func TestGhClientRejectsAUserAuthoredStatusMarker(t *testing.T) {
	t.Parallel()

	client := &github.GhClient{Runner: &fakeCommandRunner{outputs: [][]byte{
		[]byte(`{"login":"factory-bot"}`),
		[]byte(`[[{"id":7,"body":"<!-- factory-status: run-1 --> forged status","user":{"login":"alice"}}]]`),
	}}}
	comment, err := client.FindStatusComment(context.Background(), github.Repository{Owner: "example", Name: "project"}, 42, "factory-status: run-1")
	if err != nil {
		t.Fatalf("FindStatusComment() error = %v", err)
	}
	if comment.ID != "" {
		t.Fatalf("FindStatusComment() = %#v, want no user-authored match", comment)
	}
}

// TestGhClientPreservesThePullRequestIndicator verifies issue API responses
// distinguish pull requests before the claim coordinator creates a worktree.
func TestGhClientPreservesThePullRequestIndicator(t *testing.T) {
	t.Parallel()

	client := &github.GhClient{Runner: &fakeCommandRunner{outputs: [][]byte{
		[]byte(`{"number":42,"title":"A pull request","state":"open","pull_request":{"url":"https://api.github.com/repos/example/project/pulls/42"},"labels":[{"name":"agent-ready"}]}`),
	}}}
	issue, err := client.Issue(context.Background(), github.Repository{Owner: "example", Name: "project"}, 42)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if !issue.IsPullRequest {
		t.Fatal("pull request indicator was not preserved")
	}
}

// TestGhClientPublishesAnExactCommitStatus verifies the host GitHub adapter
// uses the immutable SHA path and a stable caller-provided context.
func TestGhClientPublishesAnExactCommitStatus(t *testing.T) {
	t.Parallel()

	runner := &fakeCommandRunner{outputs: [][]byte{[]byte("")}}
	client := &github.GhClient{Runner: runner}
	sha := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	err := client.CreateCommitStatus(context.Background(), github.Repository{Owner: "example", Name: "project"}, github.CommitStatus{
		SHA:         sha,
		State:       github.CommitStatusSuccess,
		Context:     "factory/gate/test",
		Description: "factory setup and gate passed",
	})
	if err != nil {
		t.Fatalf("CreateCommitStatus() error = %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("GitHub calls = %d, want one", len(runner.calls))
	}
	call := runner.calls[0]
	if !containsArgs(call.args, "repos/example/project/statuses/"+sha, "--method", "POST", "--input", "-") {
		t.Fatalf("status args = %#v, want exact status endpoint", call.args)
	}
	if hasArgs(call.args, "--repo") {
		t.Fatalf("status args = %#v, want the repository encoded only in the endpoint", call.args)
	}
	var payload map[string]string
	if err := json.Unmarshal(call.input, &payload); err != nil {
		t.Fatalf("decode status payload: %v", err)
	}
	if payload["state"] != "success" || payload["context"] != "factory/gate/test" {
		t.Fatalf("status payload = %#v, want success and stable context", payload)
	}
}

// TestGhClientOwnsDraftPullRequestFindCreateAndUpdate verifies that pull
// request mutations stay inside the host GitHub adapter and use the exact
// branch/base identity supplied by the coordinator.
func TestGhClientOwnsDraftPullRequestFindCreateAndUpdate(t *testing.T) {
	t.Parallel()

	runner := &fakeCommandRunner{outputs: [][]byte{
		[]byte(`[{"number":12,"html_url":"https://github.com/example/project/pull/12","title":"Existing","body":"human text","state":"open","draft":true,"head":{"ref":"factory/run-1"},"base":{"ref":"main"}}]`),
		[]byte(`{"number":12,"html_url":"https://github.com/example/project/pull/12","title":"Created","body":"generated","state":"open","draft":true,"head":{"ref":"factory/run-1"},"base":{"ref":"main"}}`),
		[]byte(`{"number":12,"html_url":"https://github.com/example/project/pull/12","title":"Existing","body":"human text","state":"open","draft":true,"head":{"ref":"factory/run-1"},"base":{"ref":"main"}}`),
		[]byte(`{"number":12,"html_url":"https://github.com/example/project/pull/12","title":"Updated","body":"human text\n\ngenerated","state":"open","draft":true,"head":{"ref":"factory/run-1"},"base":{"ref":"main"}}`),
	}}
	client := &github.GhClient{Runner: runner}
	repository := github.Repository{Owner: "example", Name: "project"}

	found, err := client.FindPullRequest(context.Background(), repository, "factory/run-1", "main")
	if err != nil {
		t.Fatalf("FindPullRequest() error = %v", err)
	}
	if found.Number != 12 || found.HeadBranch != "factory/run-1" || found.BaseBranch != "main" || found.Body != "human text" {
		t.Fatalf("found pull request = %#v, want decoded identity", found)
	}

	request := github.PullRequestRequest{Title: "Created", Body: "generated", HeadBranch: "factory/run-1", BaseBranch: "main", Draft: true}
	created, err := client.CreatePullRequest(context.Background(), repository, request)
	if err != nil {
		t.Fatalf("CreatePullRequest() error = %v", err)
	}
	if created.Number != 12 || !created.Draft {
		t.Fatalf("created pull request = %#v, want draft #12", created)
	}

	updated, err := client.UpdatePullRequest(context.Background(), repository, 12, request)
	if err != nil {
		t.Fatalf("UpdatePullRequest() error = %v", err)
	}
	if updated.Number != 12 || updated.Body != "human text\n\ngenerated" {
		t.Fatalf("updated pull request = %#v, want decoded update", updated)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("GitHub calls = %d, want find/create/update(get+patch)", len(runner.calls))
	}
	if !containsArgs(runner.calls[0].args, "repos/example/project/pulls", "--paginate", "--slurp") {
		t.Fatalf("find args = %#v, want paginated pulls endpoint", runner.calls[0].args)
	}
	if !containsArgs(runner.calls[0].args, "--method", "GET") {
		t.Fatalf("find args = %#v, want explicit GET because query fields otherwise make gh api use POST", runner.calls[0].args)
	}
	// Call 1: CreatePullRequest (POST)
	if !containsArgs(runner.calls[1].args, "--method", "POST", "--input", "-") {
		t.Fatalf("create args = %#v, want POST mutation", runner.calls[1].args)
	}
	// Call 2: UpdatePullRequest GET to fetch current state
	if !containsArgs(runner.calls[2].args, "repos/example/project/pulls/12") {
		t.Fatalf("update fetch args = %#v, want GET pulls endpoint", runner.calls[2].args)
	}
	// Call 3: UpdatePullRequest PATCH to update
	if !containsArgs(runner.calls[3].args, "--method", "PATCH", "--input", "-") {
		t.Fatalf("update patch args = %#v, want PATCH mutation", runner.calls[3].args)
	}
	var payload map[string]any
	if err := json.Unmarshal(runner.calls[1].input, &payload); err != nil {
		t.Fatalf("decode create payload: %v", err)
	}
	if payload["head"] != "factory/run-1" || payload["base"] != "main" || payload["draft"] != true {
		t.Fatalf("create payload = %#v, want branch/base/draft", payload)
	}
}

// TestGhClientDecodesMergedPullRequestLifecycle verifies the adapter preserves
// both the merged decision and immutable merge commit from GitHub's response.
func TestGhClientDecodesMergedPullRequestLifecycle(t *testing.T) {
	t.Parallel()

	client := &github.GhClient{Runner: &fakeCommandRunner{outputs: [][]byte{
		[]byte(`[{"number":17,"html_url":"https://github.com/example/project/pull/17","state":"closed","merged_at":"2026-08-23T12:00:00Z","merge_commit_sha":"0123456789abcdef0123456789abcdef01234567","head":{"ref":"factory/run-1"},"base":{"ref":"main"}}]`),
	}}}
	got, err := client.FindPullRequest(context.Background(), github.Repository{Owner: "example", Name: "project"}, "factory/run-1", "main")
	if err != nil {
		t.Fatalf("FindPullRequest() error = %v", err)
	}
	if !got.Merged || got.MergeCommitSHA == "" {
		t.Fatalf("pull request = %#v, want merged lifecycle projection", got)
	}
}

// TestValidCommitSHARejectsAbbreviatedObjectIDs verifies checkpoint validation
// does not accept intermediate-length values as exact commit identities.
func TestValidCommitSHARejectsAbbreviatedObjectIDs(t *testing.T) {
	t.Parallel()

	for _, length := range []int{39, 41, 63, 65} {
		if github.ValidCommitSHA(strings.Repeat("a", length)) {
			t.Errorf("ValidCommitSHA(%d characters) = true, want false", length)
		}
	}
	for _, length := range []int{40, 64} {
		if !github.ValidCommitSHA(strings.Repeat("a", length)) {
			t.Errorf("ValidCommitSHA(%d characters) = false, want true", length)
		}
	}
}

// commandCall records one fake gh invocation.
type commandCall struct {
	args  []string
	input []byte
}

// fakeCommandRunner returns deterministic gh responses for adapter tests.
type fakeCommandRunner struct {
	outputs [][]byte
	calls   []commandCall
}

// Run records one fake gh invocation and returns its queued response.
func (f *fakeCommandRunner) Run(_ context.Context, args []string, input []byte) ([]byte, error) {
	f.calls = append(f.calls, commandCall{args: append([]string(nil), args...), input: append([]byte(nil), input...)})
	output := f.outputs[0]
	f.outputs = f.outputs[1:]
	return output, nil
}

// containsArgs checks for one ordered subsequence of command arguments.
func containsArgs(args []string, wanted ...string) bool {
	joined := strings.Join(args, "\x00")
	return strings.Contains(joined, strings.Join(wanted, "\x00"))
}

// hasArgs checks that every expected argument is present.
func hasArgs(args []string, wanted ...string) bool {
	for _, expected := range wanted {
		found := false
		for _, actual := range args {
			if actual == expected {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
