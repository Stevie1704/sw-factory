package github_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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
		[]byte(`[[{"id":7,"body":"older status"}],[{"id":12345,"body":"<!-- factory-status: run-1 --> status"}]]`),
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
	if !hasArgs(runner.calls[5].args, "--paginate", "--slurp") {
		t.Fatalf("FindStatusComment args = %#v, want pagination", runner.calls[5].args)
	}

	if len(runner.calls) != 6 {
		t.Fatalf("CLI calls = %d, want six", len(runner.calls))
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
	for _, index := range []int{0, 2, 3, 4, 5} {
		if hasArgs(runner.calls[index].args, "--repo") {
			t.Errorf("gh api call %d still uses unsupported --repo: %#v", index, runner.calls[index].args)
		}
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
	if len(runner.calls) != 3 {
		t.Fatalf("GitHub calls = %d, want find/create/update", len(runner.calls))
	}
	if !containsArgs(runner.calls[0].args, "repos/example/project/pulls", "--paginate", "--slurp") {
		t.Fatalf("find args = %#v, want paginated pulls endpoint", runner.calls[0].args)
	}
	for _, index := range []int{1, 2} {
		method := "POST"
		if index == 2 {
			method = "PATCH"
		}
		if !containsArgs(runner.calls[index].args, "--method", method, "--input", "-") {
			t.Fatalf("mutation args = %#v, want JSON mutation", runner.calls[index].args)
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(runner.calls[1].input, &payload); err != nil {
		t.Fatalf("decode create payload: %v", err)
	}
	if payload["head"] != "factory/run-1" || payload["base"] != "main" || payload["draft"] != true {
		t.Fatalf("create payload = %#v, want branch/base/draft", payload)
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
