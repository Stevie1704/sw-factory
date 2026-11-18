package checkpoint

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// OwnedObjectLister lists the factory-owned objects on the work tracker,
// the code host, and the Git remote, so a restore can find an object that
// a checkpoint does not know. It only reads. A listing that it cannot read
// is an error, never an empty success.
type OwnedObjectLister interface {
	// FactoryBranches lists the remote branches below factory/.
	FactoryBranches(ctx context.Context) ([]string, error)
	// FactoryPullRequests lists the pull requests, in every state, whose
	// head branch is below factory/.
	FactoryPullRequests(ctx context.Context) ([]PullRequestState, error)
	// FactoryLabeledIssues lists the issues, in every state, that carry a
	// factory run-state label.
	FactoryLabeledIssues(ctx context.Context) ([]IssueState, error)
}

// Divergence is the result of a comparison of the recorded external state
// with the external state now.
type Divergence struct {
	// Diverged names each object whose state differs or that the
	// checkpoint does not know.
	Diverged []string
	// Unverified names each object or source that could not be read.
	Unverified []string
}

// CompareExternalState reads every known object of recorded again and lists
// every factory-owned object (ADR 0032, restore preconditions 7 and 8). The
// target branch head is not compared: it is not a factory-owned object, and
// a human merge moves it.
func CompareExternalState(ctx context.Context, reader ExternalReader, owned OwnedObjectLister, recorded ExternalState) Divergence {
	comparison := &comparison{}
	comparison.compareKnown(ctx, reader, recorded)
	if owned == nil {
		comparison.unverified("factory-owned objects", "this tracker or code host cannot list factory-owned objects")
		return comparison.Divergence
	}
	comparison.findUnknown(ctx, owned, recorded)
	return comparison.Divergence
}

// comparison collects the divergence of one comparison.
type comparison struct {
	Divergence
}

// diverged records one object whose state differs.
func (c *comparison) diverged(object, detail string) {
	c.Diverged = append(c.Diverged, object+": "+detail)
}

// unverified records one object or source that could not be read.
func (c *comparison) unverified(object, detail string) {
	c.Unverified = append(c.Unverified, object+": "+detail)
}

// compareKnown compares each known object with its recorded state.
func (c *comparison) compareKnown(ctx context.Context, reader ExternalReader, recorded ExternalState) {
	for _, branch := range recorded.Branches {
		object := "branch " + branch.Name
		if head, err := reader.BranchHead(ctx, branch.Name); err != nil {
			c.unverified(object, err.Error())
		} else if head != branch.Head {
			c.diverged(object, fmt.Sprintf("the remote head is %q; the checkpoint records %q", head, branch.Head))
		}
	}
	for _, pullRequest := range recorded.PullRequests {
		object := fmt.Sprintf("pull request #%d", pullRequest.Number)
		if now, err := reader.PullRequest(ctx, pullRequest.Number, pullRequest.HeadBranch); err != nil {
			c.unverified(object, err.Error())
		} else if now != pullRequest {
			c.diverged(object, fmt.Sprintf("it is %+v; the checkpoint records %+v", now, pullRequest))
		}
	}
	for _, issue := range recorded.Issues {
		object := fmt.Sprintf("issue #%d", issue.Number)
		if now, err := reader.Issue(ctx, issue.Number); err != nil {
			c.unverified(object, err.Error())
		} else if now.Open != issue.Open || !slices.Equal(now.FactoryLabels, issue.FactoryLabels) {
			c.diverged(object, fmt.Sprintf("open=%t labels=%s; the checkpoint records open=%t labels=%s", now.Open, strings.Join(now.FactoryLabels, ","), issue.Open, strings.Join(issue.FactoryLabels, ",")))
		}
	}
	for _, comment := range recorded.StatusComments {
		object := "status comment " + comment.ID
		if now, err := reader.StatusComment(ctx, comment.IssueNumber, comment.RunID); err != nil {
			c.unverified(object, err.Error())
		} else if now.ID != comment.ID || !now.UpdatedAt.Equal(comment.UpdatedAt) {
			c.diverged(object, fmt.Sprintf("updated at %s; the checkpoint records %s", now.UpdatedAt, comment.UpdatedAt))
		}
	}
}

// findUnknown lists every factory-owned object and records each one that
// the checkpoint does not know.
func (c *comparison) findUnknown(ctx context.Context, owned OwnedObjectLister, recorded ExternalState) {
	if branches, err := owned.FactoryBranches(ctx); err != nil {
		c.unverified("factory branches", err.Error())
	} else {
		for _, branch := range branches {
			if !slices.ContainsFunc(recorded.Branches, func(known BranchState) bool { return known.Name == branch }) {
				c.diverged("branch "+branch, "the checkpoint does not know this factory branch")
			}
		}
	}
	if pullRequests, err := owned.FactoryPullRequests(ctx); err != nil {
		c.unverified("factory pull requests", err.Error())
	} else {
		for _, pullRequest := range pullRequests {
			if !slices.ContainsFunc(recorded.PullRequests, func(known PullRequestState) bool { return known.Number == pullRequest.Number }) {
				c.diverged(fmt.Sprintf("pull request #%d", pullRequest.Number), "the checkpoint does not know this pull request from "+pullRequest.HeadBranch)
			}
		}
	}
	if issues, err := owned.FactoryLabeledIssues(ctx); err != nil {
		c.unverified("factory-labeled issues", err.Error())
	} else {
		for _, issue := range issues {
			if !slices.ContainsFunc(recorded.Issues, func(known IssueState) bool { return known.Number == issue.Number }) {
				c.diverged(fmt.Sprintf("issue #%d", issue.Number), "the checkpoint knows no run of this issue with labels "+strings.Join(issue.FactoryLabels, ","))
			}
		}
	}
}
