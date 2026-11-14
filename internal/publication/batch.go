package publication

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// BatchOperationDetail is the bounded durable effect evidence of git/batch.
// Item references live in their own ledger rows, never in this bounded detail.
type BatchOperationDetail struct {
	Evidence tracker.PlanBatchResult `json:"evidence"`
	Problem  string                  `json:"problem,omitempty"`
}

// batchDetail serializes evidence without duplicating an unbounded item graph.
func batchDetail(result tracker.PlanBatchResult, problem string) string {
	result.Items = nil
	detail := BatchOperationDetail{Evidence: result, Problem: store.TruncateUTF8(problem, 600)}
	data, _ := json.Marshal(detail)
	if len(data) > store.MaxIdeationReasonBytes {
		detail.Problem = "publication evidence exceeds the bounded ledger detail; reconcile the provider result"
		detail.Evidence.PullRequestURL = ""
		data, _ = json.Marshal(detail)
	}
	return string(data)
}

// executeBatch reserves one atomic effect before sending it. Item/link rows
// become reference projections only after every approved batch effect succeeds.
func (e *execution) executeBatch(ctx context.Context, batch tracker.PlanBatchPublisher) (string, error) {
	if len(e.ledger) == 0 || e.ledger[0].Kind != store.PublicationOperationGitBatch {
		return "local publication has no atomic batch reservation", nil
	}
	if halt, err := e.batchAvailable(ctx); halt != "" || err != nil {
		return halt, err
	}
	operation := e.ledger[0]
	observed, found, err := batch.FindPlanBatch(ctx, e.publisher.Repository, e.approved)
	if err != nil {
		if operation.State == store.PublicationOperationSucceeded {
			return "recorded specification ref/push/PR is missing or conflicting: " + err.Error(), nil
		}
		var previous BatchOperationDetail
		if json.Unmarshal([]byte(operation.Detail), &previous) == nil {
			observed = retainBatchEvidence(observed, previous.Evidence)
		}
		return e.halt(ctx, 0, store.PublicationOperationAmbiguous, batchDetail(observed, "batch lookup failed: "+err.Error()))
	}
	if operation.State == store.PublicationOperationSucceeded {
		if !found || !observed.Complete || observed.CommitSHA != operation.ExternalID {
			return "recorded specification ref/push/PR moved or disappeared; reconcile before retry", nil
		}
	} else {
		if operation.State == store.PublicationOperationAttempted || operation.State == store.PublicationOperationAmbiguous {
			var previous BatchOperationDetail
			if json.Unmarshal([]byte(operation.Detail), &previous) == nil {
				evidence := previous.Evidence
				if evidence.CommitSHA != "" && evidence.CommitSHA != observed.CommitSHA {
					return e.halt(ctx, 0, store.PublicationOperationAmbiguous, batchDetail(evidence, "the recorded specification commit/ref moved or disappeared"))
				}
				if evidence.Pushed && !observed.Pushed || evidence.PullRequestNumber != 0 && evidence.PullRequestNumber != observed.PullRequestNumber {
					return e.halt(ctx, 0, store.PublicationOperationAmbiguous, batchDetail(evidence, "a recorded push/PR effect disappeared or changed; inspect and reconcile before retry"))
				}
				if evidence.CommitAttempted && evidence.CommitSHA == "" && observed.CommitSHA == "" || evidence.PushAttempted && !evidence.Pushed && !observed.Pushed || evidence.PullRequestAttempted && evidence.PullRequestNumber == 0 && observed.PullRequestNumber == 0 {
					return e.halt(ctx, 0, store.PublicationOperationAmbiguous, batchDetail(evidence, "an attempted commit/push/PR effect is not observable; inspect Git/code-host history and confirm the pending effect is absent before retry"))
				}
			}
		}
		if !found && (operation.State == store.PublicationOperationAttempted || operation.State == store.PublicationOperationAmbiguous) {
			return e.halt(ctx, 0, store.PublicationOperationAmbiguous, batchDetail(observed, "an earlier Git batch has an unknown outcome and its ref is absent; inspect Git history and confirm absence through reconciliation"))
		}
		if !observed.Complete {
			if err := e.reserve(ctx, 0); err != nil {
				return "", err
			}
			observed, err = batch.PublishPlanBatch(ctx, e.publisher.Repository, e.approved, func(progress tracker.PlanBatchResult) error {
				return e.recordBatchProgress(ctx, progress)
			})
			if err != nil {
				state := store.PublicationOperationAmbiguous
				if tracker.WriteNotApplied(err) && observed.CommitSHA == "" {
					state = store.PublicationOperationFailed
				}
				return e.halt(ctx, 0, state, batchDetail(observed, err.Error()))
			}
		}
		if !observed.Complete || observed.CommitSHA == "" {
			return e.halt(ctx, 0, store.PublicationOperationAmbiguous, batchDetail(observed, "batch did not establish all approved effects"))
		}
		if err := e.record(ctx, 0, func(row *store.PublicationOperation) {
			row.State = store.PublicationOperationSucceeded
			row.ExternalID = observed.CommitSHA
			row.URL = observed.PullRequestURL
			row.Detail = batchDetail(observed, "")
		}); err != nil {
			return "", err
		}
	}
	for index := 1; index < len(e.ledger); index++ {
		if halt, err := e.batchAvailable(ctx); halt != "" || err != nil {
			return halt, err
		}
		row := e.ledger[index]
		if row.State == store.PublicationOperationSucceeded {
			continue
		}
		if row.Kind == store.PublicationOperationCreateItem {
			ref, ok := observed.Items[row.NodeID]
			if !ok || ref.ExternalID == "" {
				return fmt.Sprintf("batch has no reference for %s", row.NodeID), nil
			}
			if err := e.succeed(ctx, index, ref, "published in atomic specification commit "+observed.CommitSHA); err != nil {
				return "", err
			}
		} else if err := e.record(ctx, index, func(row *store.PublicationOperation) {
			row.State = store.PublicationOperationSucceeded
			row.Detail = "relationship is committed in atomic specification batch"
		}); err != nil {
			return "", err
		}
	}
	return "", nil
}

// recordBatchProgress keeps evidence even when cancellation arrived during an
// effect, then stops the adapter before it crosses the next external boundary.
func (e *execution) recordBatchProgress(ctx context.Context, progress tracker.PlanBatchResult) error {
	if err := e.record(ctx, 0, func(row *store.PublicationOperation) {
		row.Detail = batchDetail(progress, "")
		if progress.CommitSHA != "" {
			row.ExternalID = progress.CommitSHA
		}
	}); err != nil {
		return err
	}
	if halt, err := e.batchAvailable(ctx); err != nil {
		return err
	} else if halt != "" {
		return fmt.Errorf("%w: %s", ErrPublicationCancelled, halt)
	}
	return nil
}

// retainBatchEvidence keeps durable intent/identity through an unavailable or
// ambiguous lookup, so later empty observations cannot justify a blind create.
func retainBatchEvidence(observed, previous tracker.PlanBatchResult) tracker.PlanBatchResult {
	observed.CommitAttempted = observed.CommitAttempted || previous.CommitAttempted
	observed.PushAttempted = observed.PushAttempted || previous.PushAttempted
	observed.PullRequestAttempted = observed.PullRequestAttempted || previous.PullRequestAttempted
	observed.Pushed = observed.Pushed || previous.Pushed
	if observed.CommitSHA == "" {
		observed.CommitSHA = previous.CommitSHA
	}
	if observed.BaseSHA == "" {
		observed.BaseSHA = previous.BaseSHA
	}
	if observed.Branch == "" {
		observed.Branch = previous.Branch
	}
	if observed.PullRequestNumber == 0 {
		observed.PullRequestNumber = previous.PullRequestNumber
		observed.PullRequestURL = previous.PullRequestURL
	}
	return observed
}

// batchAvailable checks cancellation before each publication/projection step.
func (e *execution) batchAvailable(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "publication was interrupted: " + err.Error(), nil
	}
	session, err := e.publisher.Store.IdeationSession(ctx, e.sessionID)
	if err != nil {
		return "", err
	}
	if session.State == store.IdeationStateCancelled {
		return "the session was cancelled during batch publication", nil
	}
	return "", nil
}

// reconcileBatch checks an explicit adoption or absence decision against Git.
func (p *Publisher) reconcileBatch(ctx context.Context, history store.IdeationHistory, request Reconciliation, operation store.PublicationOperation) (store.PublicationOperation, error) {
	adapter, ok := p.Tracker.(tracker.PlanBatchPublisher)
	if !ok {
		return operation, fmt.Errorf("%w: no batch publication capability", ErrReconciliationRefused)
	}
	approval, ok := currentApproval(history)
	if !ok {
		return operation, ErrNotApproved
	}
	preview, err := decodeApproval(approval)
	if err != nil {
		return operation, err
	}
	observed, found, err := adapter.FindPlanBatch(ctx, p.Repository, preview)
	if err != nil {
		return operation, err
	}
	if request.ExternalID == "" {
		if operation.ExternalID != "" && (!found || observed.CommitSHA != operation.ExternalID) {
			return operation, fmt.Errorf("%w: the recorded commit/ref is missing; restore it before reconciling later effects", ErrReconciliationRefused)
		}
		if found && observed.Complete {
			return operation, fmt.Errorf("%w: the complete specification publication exists; adopt its commit instead", ErrReconciliationRefused)
		}
		operation.State = store.PublicationOperationFailed
		operation.Detail = batchDetail(observed, "the operator inspected Git/code-host history and confirmed that the pending effect is absent")
	} else {
		if !found || !observed.Complete || observed.CommitSHA != request.ExternalID {
			return operation, fmt.Errorf("%w: no complete matching specification commit %q", ErrReconciliationRefused, request.ExternalID)
		}
		operation.State = store.PublicationOperationSucceeded
		operation.ExternalID = observed.CommitSHA
		operation.URL = observed.PullRequestURL
		operation.Detail = batchDetail(observed, "")
	}
	operation.UpdatedAt = p.now()
	if history.Session.State == store.IdeationStateCancelled {
		return operation, p.recordCancelledReconciliation(ctx, history, request.ReceiptID, operation)
	}
	return p.Store.RecordPublicationOperation(ctx, operation)
}
