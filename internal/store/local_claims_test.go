package store_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

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
	key := store.LocalRevisionKey(binding.InstallationID, binding.RepositoryPath, binding.CodeHostProvider, binding.CodeHostRepository, binding.TargetBranch, issue.ID, source.Issue.ContentDigest)
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

// TestLegacyGraphKeyedClaimStillGuardsItsRevision stores a claim row in the
// earlier format, whose key also hashed the graph digest. The row stays
// readable, and a new reservation of the same revision cannot consume it twice.
func TestLegacyGraphKeyedClaimStillGuardsItsRevision(t *testing.T) {
	for _, state := range []string{"reserved", "claimed"} {
		t.Run(state, func(t *testing.T) {
			db, path := openIdeationStoreForTest(t)
			claim := claimAuthorization(t, db, "authority", "Export")
			legacy := claim
			legacy.Key = localissues.Digest([]byte(strings.Join([]string{claim.InstallationID, claim.RepositoryPath, claim.CodeHostProvider, claim.CodeHostRepository, claim.TargetBranch, claim.IssueID, claim.ContentDigest, claim.GraphDigest}, "\x00")))
			legacy.RunID = "local-" + strings.TrimPrefix(legacy.Key, "sha256:")
			data, err := json.Marshal(legacy)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.ExecContext(t.Context(), `INSERT INTO local_claims(claim_key,repository_path,run_id,state,claim) VALUES(?,?,?,?,?)`, legacy.Key, legacy.RepositoryPath, legacy.RunID, state, data); err != nil {
				t.Fatal(err)
			}
			_ = raw.Close()
			claims, err := db.LocalClaims(t.Context(), claim.RepositoryPath)
			if err != nil || len(claims) != 1 || claims[0].RevisionKey() != claim.RevisionKey() {
				t.Fatalf("legacy claim unreadable: %#v,%v", claims, err)
			}
			reserved, err := db.ReserveLocalClaim(t.Context(), claim)
			if state == "reserved" {
				if err != nil || reserved.RunID != legacy.RunID {
					t.Fatalf("legacy reservation not adopted: %#v,%v", reserved, err)
				}
				return
			}
			if !errors.Is(err, store.ErrLocalClaimConflict) {
				t.Fatalf("consumed legacy revision reserved again: %#v,%v", reserved, err)
			}
		})
	}
}

// TestReleaseLocalClaimRefusesAConsumedRevision lets a stale reservation go
// but never deletes the receipt of a revision that already became a Run.
func TestReleaseLocalClaimRefusesAConsumedRevision(t *testing.T) {
	db, _ := openIdeationStoreForTest(t)
	claim := claimAuthorization(t, db, "authority", "Export")
	reserved, err := db.ReserveLocalClaim(t.Context(), claim)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	run := store.Run{ID: reserved.RunID, RepositoryPath: reserved.RepositoryPath, IssueNumber: reserved.Number, Stage: store.StageClaim, Status: store.StatusActive, BaseCheckpointSHA: reserved.BaseSHA, CheckpointSHA: reserved.BaseSHA, SpecificationPacket: reserved.SpecificationPacket, CreatedAt: now, UpdatedAt: now}
	if _, err := db.SaveLocalClaimRun(t.Context(), reserved, run); err != nil {
		t.Fatal(err)
	}
	if err := db.ReleaseLocalClaim(t.Context(), reserved); !errors.Is(err, store.ErrLocalClaimConflict) {
		t.Fatalf("consumed claim released: %v", err)
	}
	claims, err := db.LocalClaims(t.Context(), reserved.RepositoryPath)
	if err != nil || len(claims) != 1 || claims[0].State != "claimed" {
		t.Fatalf("consumption receipt lost: %#v,%v", claims, err)
	}
}

// TestReleaseLocalClaimDeletesAnUnconsumedReservation frees the slot.
func TestReleaseLocalClaimDeletesAnUnconsumedReservation(t *testing.T) {
	db, _ := openIdeationStoreForTest(t)
	reserved, err := db.ReserveLocalClaim(t.Context(), claimAuthorization(t, db, "authority", "Export"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReleaseLocalClaim(t.Context(), reserved); err != nil {
		t.Fatal(err)
	}
	if claims, err := db.LocalClaims(t.Context(), reserved.RepositoryPath); err != nil || len(claims) != 0 {
		t.Fatalf("reservation retained: %#v,%v", claims, err)
	}
}
