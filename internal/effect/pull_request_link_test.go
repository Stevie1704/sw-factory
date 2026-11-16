package effect_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/effect"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// linkingPullRequestsForTest is a code host with native work-item links. It
// reports existing as the branch pull request and records every link check.
type linkingPullRequestsForTest struct {
	existing codehost.PullRequest
	created  int
	links    []codehost.WorkItemLink
	numbers  []int
}

// FindPullRequest reports the scripted branch pull request.
func (c *linkingPullRequestsForTest) FindPullRequest(context.Context, tracker.Repository, string, string) (codehost.PullRequest, error) {
	return c.existing, nil
}

// CreatePullRequest creates pull request 5.
func (c *linkingPullRequestsForTest) CreatePullRequest(_ context.Context, _ tracker.Repository, request codehost.PullRequestRequest) (codehost.PullRequest, error) {
	c.created++
	return codehost.PullRequest{Number: 5, Title: request.Title, Body: request.Body, HeadBranch: request.HeadBranch, BaseBranch: request.BaseBranch, Draft: request.Draft}, nil
}

// UpdatePullRequest returns the updated pull request.
func (c *linkingPullRequestsForTest) UpdatePullRequest(_ context.Context, _ tracker.Repository, number int, request codehost.PullRequestRequest) (codehost.PullRequest, error) {
	return codehost.PullRequest{Number: number, Title: request.Title, Body: request.Body}, nil
}

// EnsurePullRequestWorkItem records the link check.
func (c *linkingPullRequestsForTest) EnsurePullRequestWorkItem(_ context.Context, _ tracker.Repository, number int, link codehost.WorkItemLink) error {
	c.numbers = append(c.numbers, number)
	c.links = append(c.links, link)
	return nil
}

// linkedRequest is a draft request of a Run with a frozen Azure Boards
// repository.
func linkedRequest() codehost.PullRequestRequest {
	return codehost.PullRequestRequest{Title: "Add export", Body: "body", HeadBranch: "factory/run-1", BaseBranch: "main", Draft: true,
		WorkItem: &codehost.WorkItemLink{ID: 42, RepositoryID: "3b1f6a2e-8c4d-4e5f-9a6b-7c8d9e0f1a2b"}}
}

// TestUpsertPullRequestEnsuresTheSourceWorkItemOnEveryPath verifies that a
// created pull request and an unchanged adopted one both have their native
// work-item association verified, so a lost create response or an
// interrupted association recovers without a second pull request.
func TestUpsertPullRequestEnsuresTheSourceWorkItemOnEveryPath(t *testing.T) {
	t.Parallel()

	request := linkedRequest()
	created := &linkingPullRequestsForTest{}
	if _, err := effect.UpsertPullRequest(context.Background(), created, tracker.Repository{}, 0, request); err != nil {
		t.Fatalf("UpsertPullRequest(create) error = %v", err)
	}
	if created.created != 1 || len(created.links) != 1 || created.numbers[0] != 5 || created.links[0] != *request.WorkItem {
		t.Fatalf("create = %d created, links %v on %v; want one create and one link check on #5", created.created, created.links, created.numbers)
	}

	adopted := &linkingPullRequestsForTest{existing: codehost.PullRequest{Number: 9, Title: request.Title, Body: request.Body, HeadBranch: request.HeadBranch, BaseBranch: request.BaseBranch, Draft: true}}
	if _, err := effect.UpsertPullRequest(context.Background(), adopted, tracker.Repository{}, 9, request); err != nil {
		t.Fatalf("UpsertPullRequest(adopt) error = %v", err)
	}
	if adopted.created != 0 || len(adopted.links) != 1 || adopted.numbers[0] != 9 {
		t.Fatalf("adopt = %d created, links %v on %v; want no create and a link check on #9", adopted.created, adopted.links, adopted.numbers)
	}
}

// TestUpsertPullRequestLinksNothingWithoutASourceWorkItem verifies that a
// Run without a frozen Azure Boards repository, such as a local Markdown
// Run on Azure Repos, never sends its issue number as a work-item link.
func TestUpsertPullRequestLinksNothingWithoutASourceWorkItem(t *testing.T) {
	t.Parallel()

	request := linkedRequest()
	request.WorkItem = nil
	client := &linkingPullRequestsForTest{}
	if _, err := effect.UpsertPullRequest(context.Background(), client, tracker.Repository{}, 0, request); err != nil {
		t.Fatalf("UpsertPullRequest() error = %v", err)
	}
	if len(client.links) != 0 {
		t.Fatalf("links = %v, want none", client.links)
	}
}

// TestUpsertPullRequestRefusesALinkTheCodeHostCannotMake verifies that a
// source work item on a code host without native links fails instead of
// silently dropping the traceability.
func TestUpsertPullRequestRefusesALinkTheCodeHostCannotMake(t *testing.T) {
	t.Parallel()

	client := journalPullRequestsForTest{}
	if _, err := effect.UpsertPullRequest(context.Background(), client, tracker.Repository{}, 0, linkedRequest()); err == nil || errors.Is(err, errExternal) {
		t.Fatalf("UpsertPullRequest() error = %v, want a work-item link refused before any host call", err)
	}
}
