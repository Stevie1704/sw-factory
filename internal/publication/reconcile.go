package publication

import (
	"context"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// Reconciliation is the operator's explicit resolution of one item create
// whose outcome publication could not decide.
type Reconciliation struct {
	// SessionID identifies the session.
	SessionID string
	// OperationID identifies the ambiguous create_item operation of the
	// current approval.
	OperationID string
	// ExternalID adopts this tracker item. It must be one of the items the
	// correlation lookup finds now. Empty confirms that no item exists, which
	// the lookup must also show.
	ExternalID string
	// ReceiptID identifies the receipt that a reconciliation of a cancelled
	// session records. It is required for a cancelled session, because no
	// later Publish records the result there.
	ReceiptID string
}

// Reconcile resolves an ambiguous or attempted item create of a session whose
// publication is incomplete, or of a session cancelled after such a create.
// Adopting an item records its identity, so the next Publish links it.
// Confirming absence marks the create as not applied, so the next Publish
// may create it once. Both choices are checked against a fresh correlation
// lookup; Reconcile itself never writes to the tracker. A cancelled session
// stays cancelled: the result goes to the ledger and to a receipt under
// ReceiptID, and Publish stays refused.
func (p *Publisher) Reconcile(ctx context.Context, request Reconciliation) (store.PublicationOperation, error) {
	history, err := p.Store.IdeationHistory(ctx, request.SessionID)
	if err != nil {
		return store.PublicationOperation{}, err
	}
	cancelled := history.Session.State == store.IdeationStateCancelled
	if history.Session.State != store.IdeationStatePublicationIncomplete && !cancelled {
		return store.PublicationOperation{}, fmt.Errorf("%w: session %q is %s, not publication_incomplete or cancelled", ErrReconciliationRefused, request.SessionID, history.Session.State)
	}
	if cancelled && strings.TrimSpace(request.ReceiptID) == "" {
		return store.PublicationOperation{}, fmt.Errorf("%w: a cancelled session needs a receipt ID for the reconciliation", ErrReconciliationRefused)
	}
	operation, ok := findOperation(history.Operations, history.Session.ApprovalID, request.OperationID)
	if !ok {
		return store.PublicationOperation{}, fmt.Errorf("%w: the current approval has no operation %q", ErrReconciliationRefused, request.OperationID)
	}
	if operation.Kind != store.PublicationOperationCreateItem ||
		(operation.State != store.PublicationOperationAmbiguous && operation.State != store.PublicationOperationAttempted) {
		return store.PublicationOperation{}, fmt.Errorf("%w: operation %q is a %s in state %s; only an ambiguous item create needs reconciliation",
			ErrReconciliationRefused, operation.ID, operation.Kind, operation.State)
	}
	candidates, err := p.Tracker.FindPlanItems(ctx, p.Repository, operation.Correlation)
	if err != nil {
		return store.PublicationOperation{}, fmt.Errorf("look up correlation %s: %w", operation.Correlation, err)
	}
	if request.ExternalID == "" {
		if len(candidates) > 0 {
			return store.PublicationOperation{}, fmt.Errorf("%w: tracker items %s carry correlation %s; adopt one instead",
				ErrReconciliationRefused, externalIDs(candidates), operation.Correlation)
		}
		operation.State, operation.Detail = store.PublicationOperationFailed, "the operator confirmed that no tracker item exists"
	} else {
		adopted := false
		for _, candidate := range candidates {
			if candidate.ExternalID == request.ExternalID {
				operation.State, operation.ExternalID, operation.URL = store.PublicationOperationSucceeded, candidate.ExternalID, candidate.URL
				operation.Detail = "the operator adopted tracker item " + candidate.ExternalID
				adopted = true
			}
		}
		if !adopted {
			return store.PublicationOperation{}, fmt.Errorf("%w: tracker item %q does not carry correlation %s (candidates: %s)",
				ErrReconciliationRefused, request.ExternalID, operation.Correlation, externalIDs(candidates))
		}
	}
	operation.UpdatedAt = p.now()
	if cancelled {
		return operation, p.recordCancelledReconciliation(ctx, history, request.ReceiptID, operation)
	}
	return p.Store.RecordPublicationOperation(ctx, operation)
}

// recordCancelledReconciliation records the reconciled operation and an
// incomplete receipt of the cancelled session's approval in one store
// transaction. The receipt lists every item and relationship known to exist
// after the reconciliation, so a failed write leaves the operation
// unresolved and a retry repeats both. The session stays cancelled with its
// reason.
func (p *Publisher) recordCancelledReconciliation(ctx context.Context, history store.IdeationHistory, receiptID string, reconciled store.PublicationOperation) error {
	session := history.Session
	ledger := approvalOperations(history.Operations, session.ApprovalID)
	for index := range ledger {
		if ledger[index].ID == reconciled.ID {
			ledger[index] = reconciled
		}
	}
	receipt := receiptFromLedger(receiptID, session.ApprovalID, ledger)
	receipt.Outcome = store.PublicationOutcomeIncomplete
	receipt.Detail = bounded(fmt.Sprintf("the session is cancelled; the operator reconciled %s: %s", reconciled.ID, reconciled.Detail))
	write, err := p.Lifecycle.Write(session, workflow.SessionEventReconciled, session.StateReason, p.now())
	if err != nil {
		return err
	}
	_, _, err = p.Store.RecordPublicationReceipt(ctx, write, receipt, reconciled)
	return err
}

// findOperation returns the ledger row id of approvalID.
func findOperation(operations []store.PublicationOperation, approvalID, id string) (store.PublicationOperation, bool) {
	for _, operation := range operations {
		if operation.ApprovalID == approvalID && operation.ID == id {
			return operation, true
		}
	}
	return store.PublicationOperation{}, false
}
