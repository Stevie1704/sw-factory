package publication

import (
	"context"
	"fmt"

	"github.com/Stevie1704/sw-factory/internal/store"
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
}

// Reconcile resolves an ambiguous or attempted item create of a session whose
// publication is incomplete. Adopting an item records its identity, so the
// next Publish links it. Confirming absence marks the create as not applied,
// so the next Publish may create it once. Both choices are checked against a
// fresh correlation lookup; Reconcile itself never writes to the tracker.
func (p *Publisher) Reconcile(ctx context.Context, request Reconciliation) (store.PublicationOperation, error) {
	history, err := p.Store.IdeationHistory(ctx, request.SessionID)
	if err != nil {
		return store.PublicationOperation{}, err
	}
	if history.Session.State != store.IdeationStatePublicationIncomplete {
		return store.PublicationOperation{}, fmt.Errorf("%w: session %q is %s, not publication_incomplete", ErrReconciliationRefused, request.SessionID, history.Session.State)
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
	return p.Store.RecordPublicationOperation(ctx, operation)
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
