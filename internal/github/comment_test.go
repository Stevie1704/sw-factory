package github_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/github"
)

// TestIssueCommentsReturnsAuthorAndRevision verifies that command polling gets
// the immutable comment identity, author, and edit revision it needs.
func TestIssueCommentsReturnsAuthorAndRevision(t *testing.T) {
	t.Parallel()

	updatedAt := "2026-08-23T10:11:12Z"
	runner := &fakeCommandRunner{outputs: [][]byte{[]byte(`[[{"id":42,"body":"/factory status","user":{"login":"alice"},"updated_at":"` + updatedAt + `"}]]`)}}
	client := &github.GhClient{Runner: runner}

	comments, err := client.IssueComments(context.Background(), github.Repository{Owner: "example", Name: "project"}, 8)
	if err != nil {
		t.Fatalf("IssueComments() error = %v", err)
	}
	if len(comments) != 1 || comments[0].ID != "42" || comments[0].Author != "alice" || comments[0].Body != "/factory status" {
		t.Fatalf("comments = %#v, want one authored command comment", comments)
	}
	parsed, err := time.Parse(time.RFC3339, updatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !comments[0].UpdatedAt.Equal(parsed) {
		t.Fatalf("comment revision = %s, want %s", comments[0].UpdatedAt, parsed)
	}
	if len(runner.calls) != 1 || !containsArgs(runner.calls[0].args, "--paginate") || !containsArgs(runner.calls[0].args, "--slurp") {
		t.Fatalf("gh calls = %#v, want paginated issue comments", runner.calls)
	}
}
