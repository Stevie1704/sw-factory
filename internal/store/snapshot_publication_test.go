package store_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestInspectSnapshotReadsTheObjectsOfThePublicationLedger verifies that a
// snapshot names the external objects that the publication ledger records
// before any run exists: each created tracker item, and the branch and the
// pull request of an atomic Git batch. An operation without an external
// identity names nothing.
func TestInspectSnapshotReadsTheObjectsOfThePublicationLedger(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := quiescentStore(t, completedRun("run-1", 7))
	ledger := openRaw(t, path)
	for _, row := range []struct{ id, kind, state, externalID, detail string }{
		{"op-1", "git_batch", "succeeded", "0123456789abcdef0123456789abcdef01234567", `{"evidence":{"commit_sha":"0123456789abcdef0123456789abcdef01234567","branch":"factory/spec-1","base_sha":"","pushed":true,"pull_request_number":12,"complete":true}}`},
		{"op-2", "create_item", "succeeded", "41", ""},
		{"op-3", "create_item", "failed", "", "rate limited"},
		{"op-4", "create_relationship", "succeeded", "", "linked"},
	} {
		if _, err := ledger.ExecContext(ctx, `INSERT INTO ideation_publication_operations
			(session_id, approval_id, id, sequence, kind, state, external_id, detail, updated_at)
			VALUES ('session-1', 'approval-1', ?, ?, ?, ?, ?, ?, '2026-11-19T00:00:00Z')`,
			row.id, row.id[3:], row.kind, row.state, row.externalID, row.detail); err != nil {
			t.Fatal(err)
		}
	}
	destination := filepath.Join(privateDirectory(t), "store.db")

	snapshot, err := store.Snapshot(ctx, path, destination, nil)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	want := []store.KnownPublication{
		{SessionID: "session-1", ApprovalID: "approval-1", OperationID: "op-1", Branch: "factory/spec-1", PullRequestNumber: 12},
		{SessionID: "session-1", ApprovalID: "approval-1", OperationID: "op-2", IssueNumber: 41},
	}
	if !slices.Equal(snapshot.KnownPublications, want) {
		t.Fatalf("known publications = %+v, want %+v", snapshot.KnownPublications, want)
	}
}
