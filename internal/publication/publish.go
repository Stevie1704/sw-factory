package publication

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// Request asks for one publication attempt of the session's current approval.
type Request struct {
	// SessionID identifies the session.
	SessionID string
	// AttemptID identifies the attempt. It becomes the receipt ID, so a
	// repeated request with the same ID returns the stored receipt and makes
	// no tracker call.
	AttemptID string
}

// Result is the outcome of one publication attempt.
type Result struct {
	// Session is the session after the attempt.
	Session store.IdeationSession
	// Receipt is the recorded evidence of the attempt. It is empty when the
	// session was cancelled during the attempt.
	Receipt store.PublicationReceipt
	// Operations is the ledger after the attempt.
	Operations []store.PublicationOperation
}

// Publish publishes the session's current approval. It refuses a missing or
// stale approval before any tracker write. It then creates every approved item
// and relationship in dependency order through the ledger and records a
// published or an incomplete receipt. An incomplete attempt is not an error:
// the receipt names the operation that stopped and why, and a later Publish
// resumes the same ledger. Publication never deletes tracker items, never adds
// agent-ready, and never starts a run.
func (p *Publisher) Publish(ctx context.Context, request Request) (Result, error) {
	history, err := p.Store.IdeationHistory(ctx, request.SessionID)
	if err != nil {
		return Result{}, err
	}
	if receipt, ok := findReceipt(history.Receipts, request.AttemptID); ok {
		return Result{Session: history.Session, Receipt: receipt, Operations: approvalOperations(history.Operations, receipt.ApprovalID)}, nil
	}
	session := history.Session
	approval, ok := currentApproval(history)
	if !ok {
		return Result{}, fmt.Errorf("%w: session %q", ErrNotApproved, session.ID)
	}
	if session.State != store.IdeationStateApproved && session.State != store.IdeationStatePublishing &&
		session.State != store.IdeationStatePublicationIncomplete {
		return Result{}, fmt.Errorf("%w: session %q is %s", ErrNotPublishable, session.ID, session.State)
	}
	fresh, err := p.render(ctx, history, approval.PlanRevision)
	if err != nil {
		return Result{}, err
	}
	if !approval.Binds(session.PlanRevision, fresh.Target, fresh.Payload, fresh.RelationshipMapping) {
		return Result{}, fmt.Errorf("%w: approval %q of session %q; review and approve the current preview", ErrStaleApproval, approval.ID, session.ID)
	}
	approved, err := decodeApproval(approval)
	if err != nil {
		return Result{}, err
	}
	planned, err := orderOperations(approved)
	if err != nil {
		return Result{}, err
	}
	if session.State != store.IdeationStatePublishing {
		if session, err = p.transition(ctx, session, workflow.SessionEventPublicationStarted, ""); err != nil {
			return Result{}, err
		}
	}
	ledger, err := p.Store.StartPublicationLedger(ctx, session.ID, approval.ID, planned, p.now())
	if err != nil {
		return Result{}, err
	}
	run := newExecution(p, session.ID, approved, ledger)
	halt, runErr := run.execute(ctx)
	if runErr != nil {
		halt = "publication stopped on a local store failure: " + runErr.Error()
	}
	result, err := p.finish(ctx, session.ID, request.AttemptID, approval.ID, run.ledger, halt)
	return result, errors.Join(runErr, err)
}

// finish records the receipt and lifecycle result of one attempt. It runs
// without the request's cancellation, so an interrupted attempt still leaves
// precise evidence.
func (p *Publisher) finish(ctx context.Context, sessionID, attemptID, approvalID string, ledger []store.PublicationOperation, halt string) (Result, error) {
	ctx = context.WithoutCancel(ctx)
	session, err := p.Store.IdeationSession(ctx, sessionID)
	if err != nil {
		return Result{Operations: ledger}, err
	}
	if session.State == store.IdeationStateCancelled {
		return Result{Session: session, Operations: ledger}, fmt.Errorf("%w: session %q; created items stay in the tracker", ErrPublicationCancelled, session.ID)
	}
	receipt := receiptFromLedger(attemptID, approvalID, ledger)
	if halt != "" {
		receipt.Outcome = store.PublicationOutcomeIncomplete
	}
	event := workflow.SessionEventPublicationCompleted
	if receipt.Outcome == store.PublicationOutcomeIncomplete {
		event = workflow.SessionEventPublicationIncomplete
		if halt == "" {
			halt = "publication stopped before every operation succeeded"
		}
		receipt.Detail = bounded(halt + "; retry resumes the same approved plan")
	}
	write, err := p.Lifecycle.Write(session, event, receipt.Detail, p.now())
	if err != nil {
		return Result{Session: session, Operations: ledger}, err
	}
	session, receipt, err = p.Store.RecordPublicationReceipt(ctx, write, receipt)
	if err != nil {
		return Result{Operations: ledger}, err
	}
	return Result{Session: session, Receipt: receipt, Operations: ledger}, nil
}

// receiptFromLedger returns the receipt id of approvalID that lists every
// item and relationship that ledger shows as succeeded. Its outcome is
// published only when every operation succeeded.
func receiptFromLedger(id, approvalID string, ledger []store.PublicationOperation) store.PublicationReceipt {
	receipt := store.PublicationReceipt{ID: id, ApprovalID: approvalID, Outcome: store.PublicationOutcomePublished}
	for _, operation := range ledger {
		if operation.State != store.PublicationOperationSucceeded {
			receipt.Outcome = store.PublicationOutcomeIncomplete
			continue
		}
		if operation.Kind == store.PublicationOperationCreateItem {
			receipt.Items = append(receipt.Items, store.PublishedItem{NodeID: operation.NodeID, ExternalID: operation.ExternalID, URL: operation.URL})
		} else {
			receipt.Relationships = append(receipt.Relationships, store.PublishedRelationship{
				FromNodeID: operation.FromNodeID, ToNodeID: operation.ToNodeID, Kind: operation.RelationshipKind,
			})
		}
	}
	return receipt
}

// Interrupt settles an attempt that ended without a receipt while the
// session is publishing: a resumed attempt whose preflight failed, or one
// that the coordinator started too often. It records an incomplete receipt
// under the attempt ID with reason and moves the session to
// publication_incomplete, where retry, reconciliation, and cancellation are
// offered. A session in another state is returned unchanged.
func (p *Publisher) Interrupt(ctx context.Context, request Request, reason string) (store.IdeationSession, error) {
	history, err := p.Store.IdeationHistory(ctx, request.SessionID)
	if err != nil {
		return store.IdeationSession{}, err
	}
	session := history.Session
	if session.State != store.IdeationStatePublishing {
		return session, nil
	}
	if strings.TrimSpace(reason) == "" {
		reason = "publication stopped without a result"
	}
	result, err := p.finish(ctx, session.ID, request.AttemptID, session.ApprovalID, approvalOperations(history.Operations, session.ApprovalID), reason)
	return result.Session, err
}

// transition records one lifecycle event of session.
func (p *Publisher) transition(ctx context.Context, session store.IdeationSession, event workflow.SessionEvent, reason string) (store.IdeationSession, error) {
	write, err := p.Lifecycle.Write(session, event, reason, p.now())
	if err != nil {
		return store.IdeationSession{}, err
	}
	return p.Store.TransitionIdeationSession(ctx, write)
}

// execution is one pass over the ledger of one approval.
type execution struct {
	publisher     *Publisher
	sessionID     string
	items         map[string]tracker.RenderedPlanItem
	relationships map[string]tracker.PlanRelationshipMapping
	refs          map[string]tracker.PlanItemRef
	ledger        []store.PublicationOperation
}

// newExecution indexes the approved preview for one pass over ledger.
func newExecution(publisher *Publisher, sessionID string, approved tracker.PlanPreview, ledger []store.PublicationOperation) *execution {
	run := &execution{
		publisher: publisher, sessionID: sessionID, ledger: ledger,
		items:         make(map[string]tracker.RenderedPlanItem, len(approved.Items)),
		relationships: make(map[string]tracker.PlanRelationshipMapping, len(approved.Relationships)),
		refs:          make(map[string]tracker.PlanItemRef, len(approved.Items)),
	}
	for _, item := range approved.Items {
		run.items[item.NodeID] = item
	}
	for _, relationship := range approved.Relationships {
		run.relationships[relationshipOperationID(relationship)] = relationship
	}
	return run
}

// execute runs every unfinished operation in ledger order. It returns a
// non-empty halt reason when an operation stopped the pass, and an error only
// for a local store failure.
func (e *execution) execute(ctx context.Context) (string, error) {
	for index, operation := range e.ledger {
		if operation.State == store.PublicationOperationSucceeded {
			if operation.Kind == store.PublicationOperationCreateItem {
				e.refs[operation.NodeID] = tracker.PlanItemRef{ExternalID: operation.ExternalID, URL: operation.URL}
			}
			continue
		}
		if err := ctx.Err(); err != nil {
			return fmt.Sprintf("publication was interrupted before %s: %v", operation.ID, err), nil
		}
		session, err := e.publisher.Store.IdeationSession(ctx, e.sessionID)
		if err != nil {
			return "", err
		}
		if session.State == store.IdeationStateCancelled {
			return "the session was cancelled before " + operation.ID, nil
		}
		var halt string
		if operation.Kind == store.PublicationOperationCreateItem {
			halt, err = e.createItem(ctx, index)
		} else {
			halt, err = e.createRelationship(ctx, index)
		}
		if err != nil || halt != "" {
			return halt, err
		}
	}
	return "", nil
}

// createItem reconciles and, when it is safe, creates the item of operation
// index. It always looks the item up first, so a retry after a lost response
// adopts the existing item instead of creating a second one. An unknown
// earlier outcome with no matching item, or more than one match, stops for
// explicit reconciliation.
func (e *execution) createItem(ctx context.Context, index int) (string, error) {
	operation := e.ledger[index]
	tracking := e.publisher.Tracker
	candidates, err := tracking.FindPlanItems(ctx, e.publisher.Repository, operation.Correlation)
	if err != nil {
		return fmt.Sprintf("%s: correlation lookup failed: %v", operation.ID, err), nil
	}
	switch {
	case len(candidates) == 1:
		return "", e.succeed(ctx, index, candidates[0], "found by correlation lookup")
	case len(candidates) > 1:
		detail := fmt.Sprintf("%d tracker items carry correlation %s (%s); adopt one through reconciliation",
			len(candidates), operation.Correlation, externalIDs(candidates))
		return e.halt(ctx, index, store.PublicationOperationAmbiguous, detail)
	case operation.State == store.PublicationOperationAttempted || operation.State == store.PublicationOperationAmbiguous:
		detail := fmt.Sprintf("an earlier create has an unknown outcome and no tracker item carries correlation %s; "+
			"confirm that none exists or adopt the item through reconciliation", operation.Correlation)
		return e.halt(ctx, index, store.PublicationOperationAmbiguous, detail)
	}
	if err := e.reserve(ctx, index); err != nil {
		return "", err
	}
	ref, err := tracking.CreatePlanItem(ctx, e.publisher.Repository, e.items[operation.NodeID])
	switch {
	case err == nil:
		return "", e.succeed(ctx, index, ref, "")
	case tracker.WriteNotApplied(err):
		return e.halt(ctx, index, store.PublicationOperationFailed, fmt.Sprintf("create made no change: %v", err))
	default:
		return e.halt(ctx, index, store.PublicationOperationAmbiguous, fmt.Sprintf("create outcome is unknown: %v; retry reconciles before it creates again", err))
	}
}

// createRelationship verifies and, when it is absent, creates the
// relationship of operation index between two created items.
func (e *execution) createRelationship(ctx context.Context, index int) (string, error) {
	operation := e.ledger[index]
	mapping := e.relationships[operation.ID]
	from, fromOK := e.refs[operation.FromNodeID]
	to, toOK := e.refs[operation.ToNodeID]
	if !fromOK || !toOK {
		return fmt.Sprintf("%s: an item of the relationship does not exist yet", operation.ID), nil
	}
	relationship := tracker.PlanRelationship{Kind: mapping.Kind, Mode: mapping.Mode, From: from, To: to}
	tracking := e.publisher.Tracker
	exists, err := tracking.FindPlanRelationship(ctx, e.publisher.Repository, relationship)
	if err != nil {
		return fmt.Sprintf("%s: relationship lookup failed: %v", operation.ID, err), nil
	}
	if exists {
		return "", e.record(ctx, index, func(operation *store.PublicationOperation) {
			operation.State, operation.Detail = store.PublicationOperationSucceeded, "verified an existing relationship"
		})
	}
	if err := e.reserve(ctx, index); err != nil {
		return "", err
	}
	err = tracking.CreatePlanRelationship(ctx, e.publisher.Repository, relationship)
	switch {
	case err == nil:
		return "", e.record(ctx, index, func(operation *store.PublicationOperation) {
			operation.State, operation.Detail = store.PublicationOperationSucceeded, ""
		})
	case tracker.WriteNotApplied(err):
		return e.halt(ctx, index, store.PublicationOperationFailed, fmt.Sprintf("relationship create made no change: %v", err))
	default:
		return e.halt(ctx, index, store.PublicationOperationAmbiguous, fmt.Sprintf("relationship create outcome is unknown: %v; retry verifies it before it creates it again", err))
	}
}

// reserve records the attempt of operation index before its write is sent.
func (e *execution) reserve(ctx context.Context, index int) error {
	return e.record(ctx, index, func(operation *store.PublicationOperation) {
		operation.State, operation.Detail = store.PublicationOperationAttempted, ""
		operation.Attempts++
	})
}

// succeed records the tracker identity of a created item before any
// dependent operation starts.
func (e *execution) succeed(ctx context.Context, index int, ref tracker.PlanItemRef, detail string) error {
	err := e.record(ctx, index, func(operation *store.PublicationOperation) {
		operation.State, operation.ExternalID, operation.URL, operation.Detail = store.PublicationOperationSucceeded, ref.ExternalID, ref.URL, detail
	})
	if err == nil {
		e.refs[e.ledger[index].NodeID] = ref
	}
	return err
}

// halt records a stopping state of operation index and returns the halt
// reason.
func (e *execution) halt(ctx context.Context, index int, state store.PublicationOperationState, detail string) (string, error) {
	detail = bounded(detail)
	err := e.record(ctx, index, func(operation *store.PublicationOperation) {
		operation.State, operation.Detail = state, detail
	})
	return e.ledger[index].ID + " is " + string(state) + ": " + detail, err
}

// record applies change to operation index and persists it. The stored row
// replaces the in-memory row only after the store accepted it.
func (e *execution) record(ctx context.Context, index int, change func(*store.PublicationOperation)) error {
	operation := e.ledger[index]
	change(&operation)
	operation.UpdatedAt = e.publisher.now()
	stored, err := e.publisher.Store.RecordPublicationOperation(context.WithoutCancel(ctx), operation)
	if err != nil {
		return fmt.Errorf("record publication operation %s: %w", operation.ID, err)
	}
	e.ledger[index] = stored
	return nil
}

// currentApproval returns the approval named by the session.
func currentApproval(history store.IdeationHistory) (store.PlanApproval, bool) {
	if history.Session.ApprovalID == "" {
		return store.PlanApproval{}, false
	}
	for _, approval := range history.Approvals {
		if approval.ID == history.Session.ApprovalID {
			return approval, true
		}
	}
	return store.PlanApproval{}, false
}

// findReceipt returns the receipt with id.
func findReceipt(receipts []store.PublicationReceipt, id string) (store.PublicationReceipt, bool) {
	for _, receipt := range receipts {
		if receipt.ID == id {
			return receipt, true
		}
	}
	return store.PublicationReceipt{}, false
}

// approvalOperations returns the ledger rows of one approval.
func approvalOperations(operations []store.PublicationOperation, approvalID string) []store.PublicationOperation {
	selected := []store.PublicationOperation{}
	for _, operation := range operations {
		if operation.ApprovalID == approvalID {
			selected = append(selected, operation)
		}
	}
	return selected
}

// externalIDs lists the identities of candidates for an operator message.
func externalIDs(candidates []tracker.PlanItemRef) string {
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.ExternalID)
	}
	return strings.Join(ids, ", ")
}

// bounded shortens a ledger detail, receipt detail, or state reason to the
// store limit at a rune boundary.
func bounded(detail string) string {
	return store.TruncateUTF8(detail, store.MaxIdeationReasonBytes)
}
