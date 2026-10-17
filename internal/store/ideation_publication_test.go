package store_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// publicationOperationsForTest returns the ledger of the approved test plan:
// three item creates followed by their relationships.
func publicationOperationsForTest() []store.PublicationOperation {
	return []store.PublicationOperation{
		{ID: "item.n-1", Kind: store.PublicationOperationCreateItem, NodeID: "n-1", Correlation: "session-1/1/n-1"},
		{ID: "item.n-2", Kind: store.PublicationOperationCreateItem, NodeID: "n-2", Correlation: "session-1/1/n-2"},
		{ID: "item.n-3", Kind: store.PublicationOperationCreateItem, NodeID: "n-3", Correlation: "session-1/1/n-3"},
		{ID: "link.parent.n-2.n-1", Kind: store.PublicationOperationCreateRelationship, RelationshipKind: "parent", FromNodeID: "n-2", ToNodeID: "n-1"},
	}
}

// TestPublicationLedgerStartsOnceForTheCurrentApprovalAndSurvivesRestart
// verifies that the ledger is planned once per approval, that a repeated start
// returns the stored operations, that a different plan for the same approval
// is refused, and that recorded identities survive a restart.
func TestPublicationLedgerStartsOnceForTheCurrentApprovalAndSurvivesRestart(t *testing.T) {
	t.Parallel()

	opened, path := openIdeationStoreForTest(t)
	approvedSessionForTest(t, opened, "session-1")
	if _, err := opened.StartPublicationLedger(t.Context(), "session-1", "approval-9", publicationOperationsForTest(), ideationNow); !errors.Is(err, store.ErrApprovalNotCurrent) {
		t.Fatalf("StartPublicationLedger(stale approval) error = %v, want ErrApprovalNotCurrent", err)
	}
	planned, err := opened.StartPublicationLedger(t.Context(), "session-1", "approval-1", publicationOperationsForTest(), ideationNow)
	if err != nil {
		t.Fatalf("StartPublicationLedger() error = %v", err)
	}
	if len(planned) != 4 || planned[0].State != store.PublicationOperationPending || planned[3].Sequence != 4 ||
		planned[0].SessionID != "session-1" || planned[0].ApprovalID != "approval-1" {
		t.Fatalf("planned ledger = %#v", planned)
	}
	attempted := planned[0]
	attempted.State = store.PublicationOperationAttempted
	attempted.Attempts = 1
	if _, err := opened.RecordPublicationOperation(t.Context(), attempted); err != nil {
		t.Fatalf("RecordPublicationOperation(attempted) error = %v", err)
	}
	again, err := opened.StartPublicationLedger(t.Context(), "session-1", "approval-1", publicationOperationsForTest(), ideationNow)
	if err != nil || len(again) != 4 || again[0].State != store.PublicationOperationAttempted {
		t.Fatalf("repeated StartPublicationLedger() = %#v, %v; want the stored ledger", again, err)
	}
	changed := publicationOperationsForTest()[:3]
	if _, err := opened.StartPublicationLedger(t.Context(), "session-1", "approval-1", changed, ideationNow); !errors.Is(err, store.ErrPublicationLedgerMismatch) {
		t.Fatalf("StartPublicationLedger(changed plan) error = %v, want ErrPublicationLedgerMismatch", err)
	}
	succeeded := attempted
	succeeded.State = store.PublicationOperationSucceeded
	succeeded.ExternalID = "301"
	succeeded.URL = "https://github.com/owner/repo/issues/301"
	if _, err := opened.RecordPublicationOperation(t.Context(), succeeded); err != nil {
		t.Fatalf("RecordPublicationOperation(succeeded) error = %v", err)
	}

	reopened := reopenIdeationStoreForTest(t, opened, path)
	ledger, err := reopened.PublicationLedger(t.Context(), "session-1", "approval-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger) != 4 || ledger[0].State != store.PublicationOperationSucceeded || ledger[0].ExternalID != "301" ||
		ledger[0].Attempts != 1 || ledger[1].State != store.PublicationOperationPending || ledger[3].RelationshipKind != "parent" {
		t.Fatalf("ledger after restart = %#v", ledger)
	}
	history, err := reopened.IdeationHistory(t.Context(), "session-1")
	if err != nil || len(history.Operations) != 4 || history.Operations[0].ExternalID != "301" {
		t.Fatalf("history operations = %#v, %v", history.Operations, err)
	}
}

// TestPublicationLedgerKeepsEachExternalIdentityImmutable verifies that a
// recorded external identity never changes, that a succeeded operation stays
// settled, and that a created item needs an external identity.
func TestPublicationLedgerKeepsEachExternalIdentityImmutable(t *testing.T) {
	t.Parallel()

	opened, _ := openIdeationStoreForTest(t)
	approvedSessionForTest(t, opened, "session-1")
	planned, err := opened.StartPublicationLedger(t.Context(), "session-1", "approval-1", publicationOperationsForTest(), ideationNow)
	if err != nil {
		t.Fatal(err)
	}
	missing := planned[0]
	missing.State = store.PublicationOperationSucceeded
	if _, err := opened.RecordPublicationOperation(t.Context(), missing); err == nil {
		t.Fatal("RecordPublicationOperation() accepted a created item without an external identity")
	}
	succeeded := missing
	succeeded.ExternalID = "301"
	if _, err := opened.RecordPublicationOperation(t.Context(), succeeded); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.RecordPublicationOperation(t.Context(), succeeded); err != nil {
		t.Fatalf("replayed RecordPublicationOperation() error = %v", err)
	}
	other := succeeded
	other.ExternalID = "999"
	if _, err := opened.RecordPublicationOperation(t.Context(), other); !errors.Is(err, store.ErrPublicationOperationSettled) {
		t.Fatalf("changed identity error = %v, want ErrPublicationOperationSettled", err)
	}
	reverted := succeeded
	reverted.State = store.PublicationOperationPending
	if _, err := opened.RecordPublicationOperation(t.Context(), reverted); !errors.Is(err, store.ErrPublicationOperationSettled) {
		t.Fatalf("reverted state error = %v, want ErrPublicationOperationSettled", err)
	}
	unknown := planned[1]
	unknown.ID = "item.absent"
	if _, err := opened.RecordPublicationOperation(t.Context(), unknown); !errors.Is(err, store.ErrPublicationOperationNotFound) {
		t.Fatalf("unknown operation error = %v, want ErrPublicationOperationNotFound", err)
	}
	undeclared := planned[1]
	undeclared.State = "done"
	if _, err := opened.RecordPublicationOperation(t.Context(), undeclared); err == nil {
		t.Fatal("RecordPublicationOperation() accepted an undeclared state")
	}
}

// TestSchema43MigrationAddsThePublicationLedger verifies that a schema 42
// store gains the ledger table and keeps its sessions.
func TestSchema43MigrationAddsThePublicationLedger(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	approvedSessionForTest(t, opened, "session-1")
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(t.Context(), `
		DROP TABLE ideation_publication_operations;
		UPDATE schema_metadata SET version = 42 WHERE singleton = 1`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open() migration error = %v", err)
	}
	defer func() { _ = migrated.Close() }()
	if got := migrated.SchemaVersion(); got != store.CurrentSchemaVersion {
		t.Fatalf("SchemaVersion() = %d, want %d", got, store.CurrentSchemaVersion)
	}
	if session, err := migrated.IdeationSession(t.Context(), "session-1"); err != nil || session.ApprovalID != "approval-1" {
		t.Fatalf("IdeationSession() after migration = %#v, %v", session, err)
	}
	if _, err := migrated.StartPublicationLedger(t.Context(), "session-1", "approval-1", publicationOperationsForTest(), ideationNow); err != nil {
		t.Fatalf("StartPublicationLedger() after migration error = %v", err)
	}
}

// TestPublicationReceiptSettlesLedgerOperationsInTheSameTransaction verifies
// that a receipt write that names ledger operations records them only with
// the receipt: a refused receipt leaves the ledger unchanged, so a retry can
// repeat the whole write.
func TestPublicationReceiptSettlesLedgerOperationsInTheSameTransaction(t *testing.T) {
	t.Parallel()

	opened, _ := openIdeationStoreForTest(t)
	session := approvedSessionForTest(t, opened, "session-1")
	planned, err := opened.StartPublicationLedger(t.Context(), "session-1", "approval-1", publicationOperationsForTest(), ideationNow)
	if err != nil {
		t.Fatal(err)
	}
	adopted := planned[0]
	adopted.State, adopted.ExternalID, adopted.Detail = store.PublicationOperationSucceeded, "301", "the operator adopted tracker item 301"
	receipt := store.PublicationReceipt{
		ID: "receipt-1", ApprovalID: "approval-1", Outcome: store.PublicationOutcomeIncomplete,
		Items: []store.PublishedItem{{NodeID: "n-1", ExternalID: "301"}},
	}
	stale := ideationWrite(session, store.IdeationStateApproved)
	stale.ExpectedRevision--
	if _, _, err := opened.RecordPublicationReceipt(t.Context(), stale, receipt, adopted); !errors.Is(err, store.ErrIdeationRevisionConflict) {
		t.Fatalf("RecordPublicationReceipt(stale) error = %v, want a revision conflict", err)
	}
	if ledger, err := opened.PublicationLedger(t.Context(), "session-1", "approval-1"); err != nil || ledger[0].State != store.PublicationOperationPending {
		t.Fatalf("ledger after refused receipt = %#v, %v; want item.n-1 pending", ledger, err)
	}
	if _, _, err := opened.RecordPublicationReceipt(t.Context(), ideationWrite(session, store.IdeationStateApproved), receipt, adopted); err != nil {
		t.Fatalf("RecordPublicationReceipt() error = %v", err)
	}
	if ledger, err := opened.PublicationLedger(t.Context(), "session-1", "approval-1"); err != nil || ledger[0].ExternalID != "301" || ledger[0].State != store.PublicationOperationSucceeded {
		t.Fatalf("ledger after receipt = %#v, %v; want item.n-1 adopted", ledger, err)
	}
}
