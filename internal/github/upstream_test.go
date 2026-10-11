package github_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/bugreport"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/hostcmd"
)

// upstreamRunner records gh calls and answers each with a scripted result.
type upstreamRunner struct {
	output []byte
	err    error
	calls  [][]string
	inputs [][]byte
}

// Run records the call and returns the scripted result.
func (r *upstreamRunner) Run(_ context.Context, args []string, input []byte) ([]byte, error) {
	r.calls = append(r.calls, args)
	r.inputs = append(r.inputs, input)
	return r.output, r.err
}

// repositoryOf returns the --repo value of one gh call.
func repositoryOf(args []string) string {
	index := slices.Index(args, "--repo")
	if index < 0 || index+1 >= len(args) {
		return ""
	}
	return args[index+1]
}

// TestUpstreamIssuesAlwaysTargetsTheFactoryRepository verifies that every
// call names the Factory repository explicitly, so gh never infers the
// destination from the current directory.
func TestUpstreamIssuesAlwaysTargetsTheFactoryRepository(t *testing.T) {
	runner := &upstreamRunner{output: []byte("[]")}
	upstream := github.NewUpstreamIssues(runner)
	ctx := context.Background()
	_, _ = upstream.FindReport(ctx, "fbr-0123456789abcdef0123456789abcdef")
	_, _ = upstream.FindCandidates(ctx, "Suspected Factory defect: status exits 1")
	_, _ = upstream.LabelExists(ctx, "needs-triage")
	runner.output = []byte("https://github.com/Stevie1704/sw-factory/issues/7\n")
	_, _ = upstream.CreateIssue(ctx, "title", "body", []string{"needs-triage"})
	if len(runner.calls) != 4 {
		t.Fatalf("gh calls = %d, want 4", len(runner.calls))
	}
	for _, call := range runner.calls {
		if repositoryOf(call) != "Stevie1704/sw-factory" {
			t.Fatalf("gh %v does not target Stevie1704/sw-factory", call)
		}
	}
}

// TestUpstreamIssuesFindsOnlyTheExactReportIdentity verifies that a fuzzy
// search hit without the exact marker is not a submitted report.
func TestUpstreamIssuesFindsOnlyTheExactReportIdentity(t *testing.T) {
	runner := &upstreamRunner{output: []byte(`[
		{"number":3,"title":"other","url":"https://github.com/Stevie1704/sw-factory/issues/3","body":"mentions fbr-0123456789abcdef0123456789abcdef in text"},
		{"number":9,"title":"report","url":"https://github.com/Stevie1704/sw-factory/issues/9","body":"<!-- factory-bug-report id=fbr-0123456789abcdef0123456789abcdef schema=1 -->\nbody"}
	]`)}
	issue, err := github.NewUpstreamIssues(runner).FindReport(context.Background(), "fbr-0123456789abcdef0123456789abcdef")
	if err != nil || issue == nil || issue.Number != 9 {
		t.Fatalf("FindReport() = %#v, %v; want issue 9", issue, err)
	}
	if !slices.Contains(runner.calls[0], "all") {
		t.Fatalf("gh %v does not search closed issues", runner.calls[0])
	}
}

// TestUpstreamIssuesCreatesTheIssueFromStandardInput verifies the creation
// call: exact body through standard input, the given labels, and the issue
// number parsed from the returned URL.
func TestUpstreamIssuesCreatesTheIssueFromStandardInput(t *testing.T) {
	runner := &upstreamRunner{output: []byte("https://github.com/Stevie1704/sw-factory/issues/77\n")}
	issue, err := github.NewUpstreamIssues(runner).CreateIssue(context.Background(), "Suspected Factory defect: x", "the body", []string{"needs-triage"})
	if err != nil {
		t.Fatalf("CreateIssue() error = %v", err)
	}
	if issue.Number != 77 || issue.URL != "https://github.com/Stevie1704/sw-factory/issues/77" {
		t.Fatalf("CreateIssue() = %#v, want issue 77", issue)
	}
	call := strings.Join(runner.calls[0], " ")
	if !strings.Contains(call, "issue create") || !strings.Contains(call, "--body-file -") || !strings.Contains(call, "--label needs-triage") || string(runner.inputs[0]) != "the body" {
		t.Fatalf("gh %q with input %q, want issue create from standard input with the label", call, runner.inputs[0])
	}
}

// TestUpstreamIssuesClassifiesGhFailures verifies the failure kinds that
// Submit turns into actionable errors, and that a timeout stays unclassified,
// because the creation may have reached GitHub.
func TestUpstreamIssuesClassifiesGhFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bugreport.FailureKind
	}{
		{"authentication", errors.New("gh issue: To get started with GitHub CLI, please run:  gh auth login: exit status 4"), bugreport.FailureAuthentication},
		{"bad credentials", errors.New("gh issue: HTTP 401: Bad credentials: exit status 1"), bugreport.FailureAuthentication},
		{"rate limit", errors.New("gh issue: API rate limit exceeded for user: exit status 1"), bugreport.FailureRateLimited},
		{"missing label", errors.New("gh issue: could not add label: 'needs-triage' not found: exit status 1"), bugreport.FailureMissingLabel},
		{"permission", errors.New("gh issue: HTTP 403: Resource not accessible by integration: exit status 1"), bugreport.FailurePermission},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &upstreamRunner{err: test.err}
			_, err := github.NewUpstreamIssues(runner).CreateIssue(context.Background(), "title", "body", []string{"needs-triage"})
			var upstream *bugreport.UpstreamError
			if !errors.As(err, &upstream) || upstream.Kind != test.want {
				t.Fatalf("CreateIssue() error = %v, want %s", err, test.want)
			}
		})
	}
	runner := &upstreamRunner{err: &hostcmd.TimeoutError{Operation: "gh issue"}}
	_, err := github.NewUpstreamIssues(runner).CreateIssue(context.Background(), "title", "body", []string{"needs-triage"})
	var upstream *bugreport.UpstreamError
	if err == nil || errors.As(err, &upstream) {
		t.Fatalf("CreateIssue(timeout) error = %v, want an unclassified error", err)
	}
}
