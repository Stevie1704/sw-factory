package factory

import (
	"context"
	"slices"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/checkpoint"
)

// TestCreateCheckpointRecordsThePublicationObjects verifies that the
// manifest records the external objects of a completed publication, which
// the publication ledger knows before any run identifies them: the tracker
// issue, the pushed branch, and the pull request.
func TestCreateCheckpointRecordsThePublicationObjects(t *testing.T) {
	t.Parallel()
	fixture := newCheckpointFixture(t)
	insert := `INSERT INTO ideation_publication_operations
		(session_id, approval_id, id, sequence, kind, state, external_id, detail, updated_at)
		VALUES ('session-1', 'approval-1', ?, ?, ?, 'succeeded', ?, ?, '2026-11-19T00:00:00Z')`
	fixture.execStore(t, insert, "op-1", 1, "git_batch", "0123456789abcdef0123456789abcdef01234567", `{"evidence":{"commit_sha":"0123456789abcdef0123456789abcdef01234567","branch":"factory/spec-1","base_sha":"","pushed":true,"pull_request_number":12,"complete":true}}`)
	fixture.execStore(t, insert, "op-2", 2, "create_item", "41", "")

	result, err := CreateCheckpoint(context.Background(), fixture.request("published"))
	if err != nil || len(result.Refusals) != 0 || result.Manifest == nil {
		t.Fatalf("CreateCheckpoint() = %+v, %v; want a checkpoint", result, err)
	}
	external := result.Manifest.ExternalState
	if !slices.ContainsFunc(external.Branches, func(branch checkpoint.BranchState) bool { return branch.Name == "factory/spec-1" }) {
		t.Fatalf("branches = %+v, want the publication branch", external.Branches)
	}
	if !slices.ContainsFunc(external.PullRequests, func(pullRequest checkpoint.PullRequestState) bool {
		return pullRequest.Number == 12 && pullRequest.HeadBranch == "factory/spec-1"
	}) {
		t.Fatalf("pull requests = %+v, want the publication pull request", external.PullRequests)
	}
	if !slices.ContainsFunc(external.Issues, func(issue checkpoint.IssueState) bool { return issue.Number == 41 }) {
		t.Fatalf("issues = %+v, want the published issue", external.Issues)
	}
}

// TestCreateCheckpointRefusesAnUnreadablePublicationObject verifies that a
// publication object that cannot be read refuses the checkpoint.
func TestCreateCheckpointRefusesAnUnreadablePublicationObject(t *testing.T) {
	t.Parallel()
	fixture := newCheckpointFixture(t)
	fixture.execStore(t, `INSERT INTO ideation_publication_operations
		(session_id, approval_id, id, sequence, kind, state, external_id, updated_at)
		VALUES ('session-1', 'approval-1', 'op-1', 1, 'create_item', 'succeeded', '41', '2026-11-19T00:00:00Z')`)
	request := fixture.request("published")
	request.External = checkpointExternal{unreachable: "issue"}

	result, err := CreateCheckpoint(context.Background(), request)
	assertRefusal(t, result, err, CheckpointExternalUnverified, CheckpointExternalUnverified)
	fixture.assertNothingWritten(t)
}
