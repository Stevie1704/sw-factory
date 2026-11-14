package factory

import (
	"context"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// separateStreamComments is a local-style tracker: its issue numbers are a
// namespace of their own, and it records every issue-comment read.
type separateStreamComments struct {
	tracker.Client
	read []int
}

// SeparateIssueCommentStream declares the independent issue namespace.
func (*separateStreamComments) SeparateIssueCommentStream() bool { return true }

// IssueComments records the number it was asked for.
func (c *separateStreamComments) IssueComments(_ context.Context, _ tracker.Repository, number int) ([]tracker.Comment, error) {
	c.read = append(c.read, number)
	return nil, nil
}

// TestSeparateStreamWithoutPullRequestReaderFailsClosed never reads pull
// request comments from the local issue-number space.
func TestSeparateStreamWithoutPullRequestReaderFailsClosed(t *testing.T) {
	comments := &separateStreamComments{}
	s := &Service{deps: Dependencies{Tracker: comments, Comments: comments}}
	targets := s.commentTargets(store.Run{IssueNumber: 3, PullRequestNumber: 7})
	failed := false
	for _, target := range targets {
		if _, err := target.comments(t.Context(), tracker.Repository{}); err != nil && target.stream == "pull_request" {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("pull request stream did not fail closed: %d targets", len(targets))
	}
	for _, number := range comments.read {
		if number == 7 {
			t.Fatal("pull request comments were read from the local issue-number space")
		}
	}
}
