package store_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/localissues"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// claimAuthorization records genuine source/preview/authority evidence through
// the public store seams, returning an intent observed before another writer.
func claimAuthorization(t *testing.T, db *store.Store, id, title string) store.LocalClaim {
	t.Helper()
	issue := localissues.Issue{SchemaVersion: 1, ID: "local-00112233445566778899aabbccddeeff", Title: title, Kind: localissues.KindTask, Correlation: "import/1/export", Body: "## Problem\n\nExport.\n\n## Scope and non-goals\n\nCSV.\n\n## Acceptance criteria\n\nDownload CSV.\n"}
	data, err := localissues.Render(issue)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := localissues.Validate([]localissues.File{{Path: "docs/issues/export.md", Data: data}})
	if err != nil {
		t.Fatal(err)
	}
	revision := strings.TrimPrefix(localissues.Digest(data), "sha256:")[:40]
	sources, err := db.IndexLocalIssueSnapshot(t.Context(), "/repos/service", revision, graph)
	if err != nil {
		t.Fatal(err)
	}
	source := sources[0]
	binding := store.LocalExecutionBinding{SourceKind: "target", RepositoryPath: "/repos/service", InstallationID: "install-00112233445566778899aabbccddeeff", OperatorName: "operator", CodeHostProvider: "github", CodeHostRepository: "example/service", TargetBranch: "main", PublicationPolicy: "target_visible", SourceRevision: revision, GraphDigest: graph.Digest, Payload: []byte(`{}`), RelationshipMapping: []byte(`[]`), SelectedIssueIDs: []string{issue.ID}, Issues: []store.LocalExecutionIssue{{ID: issue.ID, Number: source.Number, Path: source.Issue.Path, ContentDigest: source.Issue.ContentDigest, Bytes: data, Kind: issue.Kind}}}
	binding.PayloadSHA256, binding.RelationshipMappingSHA256 = localissues.Digest(binding.Payload), localissues.Digest(binding.RelationshipMapping)
	preview, err := db.RecordLocalExecutionPreview(t.Context(), store.LocalExecutionPreview{ID: "preview-" + id, Binding: binding})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AuthorizeLocalExecution(t.Context(), store.LocalExecutionAuthorization{RequestID: id, PreviewID: preview.ID, Binding: binding}, nil); err != nil {
		t.Fatal(err)
	}
	key := localissues.Digest([]byte(strings.Join([]string{binding.InstallationID, binding.RepositoryPath, binding.CodeHostProvider, binding.CodeHostRepository, binding.TargetBranch, issue.ID, source.Issue.ContentDigest, graph.Digest}, "\x00")))
	return store.LocalClaim{Key: key, RunID: "local-" + strings.TrimPrefix(key, "sha256:"), RepositoryPath: binding.RepositoryPath, InstallationID: binding.InstallationID, CodeHostProvider: binding.CodeHostProvider, CodeHostRepository: binding.CodeHostRepository, TargetBranch: binding.TargetBranch, IssueID: issue.ID, ContentDigest: source.Issue.ContentDigest, GraphDigest: graph.Digest, Number: source.Number, AuthorizationID: id, BaseSHA: binding.SourceRevision, SpecificationPacket: `{"version":1}`}
}

// TestNewClaimMustUseLatestSelectedAuthority models a second coordinator/inbox
// writer committing a new decision after queue observation but before reserve.
func TestNewClaimMustUseLatestSelectedAuthority(t *testing.T) {
	for _, admitted := range []bool{false, true} {
		name := "new-reservation"
		if admitted {
			name = "admitted-recovery"
		}
		t.Run(name, func(t *testing.T) {
			db, path := openIdeationStoreForTest(t)
			a := claimAuthorization(t, db, "authority-a", "Old source")
			if admitted {
				if _, err := db.ReserveLocalClaim(t.Context(), a); err != nil {
					t.Fatal(err)
				}
			}
			other, err := store.Open(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			// Force the competing writer's commit into the exact race window:
			// A was observed above, B commits here, then the first handle reserves.
			b := claimAuthorization(t, other, "authority-b", "New source")
			reserved, err := db.ReserveLocalClaim(t.Context(), a)
			if admitted {
				if err != nil || reserved.RunID != a.RunID {
					t.Fatalf("admitted intent lost recovery: %#v,%v", reserved, err)
				}
				return
			}
			if !errors.Is(err, store.ErrLocalClaimConflict) {
				t.Fatalf("superseded authority reserved: %#v,%v", reserved, err)
			}
			if reserved, err := db.ReserveLocalClaim(t.Context(), b); err != nil || reserved.RunID != b.RunID {
				t.Fatalf("new authority could not reserve: %#v,%v", reserved, err)
			}
		})
	}
}
