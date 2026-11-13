package store_test

import (
	"encoding/json"
	"errors"
	"github.com/Stevie1704/sw-factory/internal/localissues"
	"github.com/Stevie1704/sw-factory/internal/store"
	"testing"
)

// TestLocalExecutionAuthorizationIsSeparateAndDurable verifies explicit authority without session mutation.
func TestLocalExecutionAuthorizationIsSeparateAndDurable(t *testing.T) {
	opened, path := openIdeationStoreForTest(t)
	issue := localissues.Issue{SchemaVersion: 1, ID: "local-00112233445566778899aabbccddeeff", Title: "Export", Kind: localissues.KindTask, Correlation: "import/1/export", Body: "## Problem\n\nExport needed.\n\n## Scope and non-goals\n\nCSV only.\n\n## Acceptance criteria\n\nCSV downloads.\n"}
	bytes, err := localissues.Render(issue)
	if err != nil {
		t.Fatal(err)
	}
	source, _ := localissues.Parse("docs/issues/export.md", bytes)
	graph, _ := localissues.Validate([]localissues.File{{Path: source.Path, Data: bytes}})
	identity, err := opened.RegisterLocalIssueIdentity(t.Context(), "/repos/service", source.ID, source.Correlation)
	if err != nil {
		t.Fatal(err)
	}
	binding := store.LocalExecutionBinding{SourceKind: "target", RepositoryPath: "/repos/service", InstallationID: "install-00112233445566778899aabbccddeeff", OperatorName: "operator", CodeHostProvider: "github", CodeHostRepository: "example/service", TargetBranch: "main", PublicationPolicy: "target_visible", SourceRevision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", GraphDigest: graph.Digest, Payload: []byte(`{"source":"target"}`), RelationshipMapping: []byte(`[]`), SelectedIssueIDs: []string{source.ID}, Issues: []store.LocalExecutionIssue{{ID: source.ID, Number: identity.Number, Path: source.Path, ContentDigest: source.ContentDigest, Bytes: bytes, Kind: source.Kind}}}
	binding.PayloadSHA256 = localissues.Digest(binding.Payload)
	binding.RelationshipMappingSHA256 = localissues.Digest(binding.RelationshipMapping)
	preview, err := opened.RecordLocalExecutionPreview(t.Context(), store.LocalExecutionPreview{ID: "preview-request", Binding: binding})
	if err != nil {
		t.Fatal(err)
	}
	auth, err := opened.AuthorizeLocalExecution(t.Context(), store.LocalExecutionAuthorization{RequestID: "approve-request", PreviewID: preview.ID, Binding: binding}, nil)
	if err != nil {
		t.Fatal(err)
	}
	opened = reopenIdeationStoreForTest(t, opened, path)
	replay, err := opened.AuthorizeLocalExecution(t.Context(), store.LocalExecutionAuthorization{RequestID: "approve-request", PreviewID: preview.ID, Binding: binding}, nil)
	if err != nil || replay.RequestID != auth.RequestID {
		t.Fatalf("replay=%#v,%v", replay, err)
	}
	binding.OperatorName = "forged"
	if _, err := opened.AuthorizeLocalExecution(t.Context(), store.LocalExecutionAuthorization{RequestID: "approve-request", PreviewID: preview.ID, Binding: binding}, nil); err == nil {
		t.Fatal("request rebound")
	}
	list, err := opened.LocalExecutionAuthorizations(t.Context(), "/repos/service")
	if err != nil || len(list) != 1 {
		t.Fatalf("authorizations=%#v,%v", list, err)
	}
}

// TestAuthorizationCannotOutliveARefusedOrMissingCommand checks the atomic authority boundary.
func TestAuthorizationCannotOutliveARefusedOrMissingCommand(t *testing.T) {
	opened, _ := openIdeationStoreForTest(t)
	issue := localissues.Issue{SchemaVersion: 1, ID: "local-00112233445566778899aabbccddeeff", Title: "Export", Kind: localissues.KindTask, Correlation: "import/1/export", Body: "## Problem\n\nExport.\n\n## Scope and non-goals\n\nCSV.\n\n## Acceptance criteria\n\nDownload CSV.\n"}
	data, _ := localissues.Render(issue)
	source, _ := localissues.Parse("docs/issues/export.md", data)
	graph, _ := localissues.Validate([]localissues.File{{Path: source.Path, Data: data}})
	identity, err := opened.RegisterLocalIssueIdentity(t.Context(), "/repos/service", source.ID, source.Correlation)
	if err != nil {
		t.Fatal(err)
	}
	binding := store.LocalExecutionBinding{SourceKind: "target", RepositoryPath: "/repos/service", InstallationID: "install-00112233445566778899aabbccddeeff", OperatorName: "operator", CodeHostProvider: "github", CodeHostRepository: "example/service", TargetBranch: "main", PublicationPolicy: "target_visible", SourceRevision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", GraphDigest: graph.Digest, Payload: []byte(`{}`), RelationshipMapping: []byte(`[]`), SelectedIssueIDs: []string{source.ID}, Issues: []store.LocalExecutionIssue{{ID: source.ID, Number: identity.Number, Path: source.Path, ContentDigest: source.ContentDigest, Bytes: data, Kind: source.Kind}}}
	binding.PayloadSHA256 = localissues.Digest(binding.Payload)
	binding.RelationshipMappingSHA256 = localissues.Digest(binding.RelationshipMapping)
	preview, err := opened.RecordLocalExecutionPreview(t.Context(), store.LocalExecutionPreview{ID: "preview", Binding: binding})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"preview_id": preview.ID, "payload_sha256": binding.PayloadSHA256, "relationship_mapping_sha256": binding.RelationshipMappingSHA256, "graph_digest": binding.GraphDigest, "selected_issue_ids": binding.SelectedIssueIDs})
	for _, state := range []string{"missing", "refused", "not-started"} {
		t.Run(state, func(t *testing.T) {
			id := "approve-" + state
			if state != "missing" {
				if _, _, err := opened.AcceptIdeationCommand(t.Context(), store.IdeationCommand{RequestID: id, Scope: "backlog", SubjectID: "repository", Kind: "authorize_execution", ExpectedRevision: preview.Revision, Payload: payload}); err != nil {
					t.Fatal(err)
				}
			}
			if state == "refused" {
				if _, err := opened.BeginIdeationCommand(t.Context(), id); err != nil {
					t.Fatal(err)
				}
				if _, err := opened.CompleteIdeationCommand(t.Context(), store.IdeationCommandResult{RequestID: id, Outcome: store.IdeationCommandRefused, Detail: "operator request refused"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := opened.AuthorizeLocalExecution(t.Context(), store.LocalExecutionAuthorization{RequestID: id, PreviewID: preview.ID, Binding: binding}, &store.IdeationCommandResult{RequestID: id, Outcome: store.IdeationCommandApplied, SessionRevision: preview.Revision}); err == nil {
				t.Fatal("authority escaped invalid command state")
			}
			if _, err := opened.LocalExecutionAuthorization(t.Context(), id); !errors.Is(err, store.ErrLocalExecutionNotFound) {
				t.Fatalf("unapplied authority retained: %v", err)
			}
		})
	}
}
