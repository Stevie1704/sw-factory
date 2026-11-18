package github

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// ListPullRequestsByHeadPrefix lists the pull requests, in every state,
// whose head branch starts with prefix. A restore uses it to find a
// factory-owned pull request that a checkpoint does not know.
func (c *GhClient) ListPullRequestsByHeadPrefix(ctx context.Context, repository tracker.Repository, prefix string) ([]codehost.PullRequest, error) {
	args := []string{"api", fmt.Sprintf("repos/%s/pulls", repository.String()), "--paginate", "--slurp", "--method", "GET", "-f", "state=all"}
	output, err := c.callBytes(ctx, args, nil)
	if err != nil {
		return nil, fmt.Errorf("list pull requests from %q: %w", prefix, err)
	}
	responses, err := decodeJSONPages[pullRequestResponse](output)
	if err != nil {
		return nil, fmt.Errorf("decode pull requests from %q: %w", prefix, err)
	}
	var found []codehost.PullRequest
	for _, response := range responses {
		if strings.HasPrefix(response.Head.Ref, prefix) {
			found = append(found, response.pullRequest())
		}
	}
	return found, nil
}

// ListIssuesWithLabels lists the issues, in every state, that carry any of
// labels, each once and in number order. Pull requests are not issues.
func (c *GhClient) ListIssuesWithLabels(ctx context.Context, repository tracker.Repository, labels []string) ([]tracker.Issue, error) {
	seen := map[int]bool{}
	var issues []tracker.Issue
	for _, label := range labels {
		args := []string{"api", fmt.Sprintf("repos/%s/issues", repository.String()), "--paginate", "--slurp", "--method", "GET", "-f", "state=all", "-f", "labels=" + label}
		output, err := c.callBytes(ctx, args, nil)
		if err != nil {
			return nil, fmt.Errorf("list issues with label %q: %w", label, err)
		}
		responses, err := decodeIssueList(output)
		if err != nil {
			return nil, fmt.Errorf("decode issues with label %q: %w", label, err)
		}
		for _, response := range responses {
			issue := response.issue()
			if issue.IsPullRequest || seen[issue.Number] {
				continue
			}
			seen[issue.Number] = true
			issues = append(issues, issue)
		}
	}
	sort.Slice(issues, func(left, right int) bool { return issues[left].Number < issues[right].Number })
	return issues, nil
}
