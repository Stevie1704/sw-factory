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
