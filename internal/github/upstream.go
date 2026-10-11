package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/bugreport"
)

// UpstreamIssues publishes suspected Factory defect reports through the host
// gh executable. Every call names bugreport.UpstreamRepository, so the
// destination never depends on the current directory or the registered
// repository.
type UpstreamIssues struct {
	runner CommandRunner
}

var _ bugreport.Upstream = (*UpstreamIssues)(nil)

// NewUpstreamIssues returns the adapter over runner. A nil runner uses the
// host gh executable.
func NewUpstreamIssues(runner CommandRunner) *UpstreamIssues {
	if runner == nil {
		runner = commandRunner{}
	}
	return &UpstreamIssues{runner: runner}
}

// maxCandidates bounds the likely duplicates shown to the operator.
const maxCandidates = 5

// upstreamIssue is the gh JSON projection of one issue.
type upstreamIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Body   string `json:"body"`
}

// FindReport returns the issue, in any state, whose body carries the exact
// identity marker of the report.
func (u *UpstreamIssues) FindReport(ctx context.Context, reportID string) (*bugreport.Issue, error) {
	var issues []upstreamIssue
	if err := u.list(ctx, []string{"issue", "list", "--repo", bugreport.UpstreamRepository, "--state", "all", "--search", reportID + " in:body", "--json", "number,title,url,body", "--limit", "20"}, &issues); err != nil {
		return nil, err
	}
	marker := "factory-bug-report id=" + reportID + " "
	for _, issue := range issues {
		if strings.Contains(issue.Body, marker) {
			return &bugreport.Issue{Number: issue.Number, Title: issue.Title, URL: issue.URL}, nil
		}
	}
	return nil, nil
}

// FindCandidates returns open issues whose title matches the report summary.
func (u *UpstreamIssues) FindCandidates(ctx context.Context, title string) ([]bugreport.Issue, error) {
	summary := strings.TrimSpace(strings.TrimPrefix(title, "Suspected Factory defect:"))
	var issues []upstreamIssue
	if err := u.list(ctx, []string{"issue", "list", "--repo", bugreport.UpstreamRepository, "--state", "open", "--search", summary + " in:title", "--json", "number,title,url", "--limit", strconv.Itoa(maxCandidates)}, &issues); err != nil {
		return nil, err
	}
	candidates := make([]bugreport.Issue, 0, len(issues))
	for _, issue := range issues {
		candidates = append(candidates, bugreport.Issue{Number: issue.Number, Title: issue.Title, URL: issue.URL})
	}
	return candidates, nil
}

// LabelExists reports whether the Factory repository has the exact label.
func (u *UpstreamIssues) LabelExists(ctx context.Context, label string) (bool, error) {
	var labels []struct {
		Name string `json:"name"`
	}
	if err := u.list(ctx, []string{"label", "list", "--repo", bugreport.UpstreamRepository, "--search", label, "--json", "name", "--limit", "100"}, &labels); err != nil {
		return false, err
	}
	for _, candidate := range labels {
		if candidate.Name == label {
			return true, nil
		}
	}
	return false, nil
}

// CreateIssue creates one issue with the body read from standard input. A
// failure of a known kind means GitHub created nothing; any other failure,
// such as a timeout, is returned unclassified because GitHub may have
// accepted the request.
func (u *UpstreamIssues) CreateIssue(ctx context.Context, title, body string, labels []string) (bugreport.Issue, error) {
	args := []string{"issue", "create", "--repo", bugreport.UpstreamRepository, "--title", title, "--body-file", "-"}
	for _, label := range labels {
		args = append(args, "--label", label)
	}
	output, err := u.runner.Run(ctx, args, []byte(body))
	if err != nil {
		return bugreport.Issue{}, classifyUpstreamError(err)
	}
	url := strings.TrimSpace(string(output))
	number, err := strconv.Atoi(path.Base(url))
	if err != nil || !strings.HasPrefix(url, "https://github.com/"+bugreport.UpstreamRepository+"/issues/") {
		return bugreport.Issue{}, fmt.Errorf("gh issue create returned an unexpected issue address %q", url)
	}
	return bugreport.Issue{Number: number, Title: title, URL: url}, nil
}

// list runs one gh listing and decodes its JSON array.
func (u *UpstreamIssues) list(ctx context.Context, args []string, target any) error {
	output, err := u.runner.Run(ctx, args, nil)
	if err != nil {
		return classifyUpstreamError(err)
	}
	if err := json.Unmarshal(output, target); err != nil {
		return fmt.Errorf("decode gh %s %s output: %w", args[0], args[1], err)
	}
	return nil
}

// upstreamFailureSignals maps gh error text to a failure kind. The first
// match wins.
var upstreamFailureSignals = []struct {
	kind    bugreport.FailureKind
	signals []string
}{
	{bugreport.FailureAuthentication, []string{"gh auth login", "http 401", "bad credentials", "not logged in"}},
	{bugreport.FailureRateLimited, []string{"rate limit"}},
	{bugreport.FailureMissingLabel, []string{"could not add label"}},
	{bugreport.FailurePermission, []string{"http 403", "resource not accessible", "must have"}},
}

// classifyUpstreamError wraps a gh failure of a known kind in a
// bugreport.UpstreamError, and returns any other failure unchanged.
func classifyUpstreamError(err error) error {
	message := strings.ToLower(err.Error())
	for _, entry := range upstreamFailureSignals {
		for _, signal := range entry.signals {
			if strings.Contains(message, signal) {
				return &bugreport.UpstreamError{Kind: entry.kind, Err: errors.New(signal)}
			}
		}
	}
	return err
}
