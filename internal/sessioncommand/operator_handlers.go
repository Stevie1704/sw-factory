package sessioncommand

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// operatorCommand routes scoped input to the one coordinator implementation.
func operatorCommand(ctx context.Context, env Env, command store.IdeationCommand) (Outcome, error) {
	if env.Operator == nil {
		return operatorUnavailable(Kind(command.Kind)), nil
	}
	return env.Operator.ApplyOperatorCommand(ctx, command)
}

// approveStart preserves ordinary approval/publication logic and records a
// separate exact local decision after approval but before content publication.
func approveStart(ctx context.Context, env Env, command store.IdeationCommand) (Outcome, error) {
	if env.AuthorizePlan == nil {
		return operatorUnavailable(KindApproveStart), nil
	}
	payload, err := payloadOf[ApproveStartPayload](command)
	if err != nil {
		return Outcome{}, err
	}
	if env.Publisher == nil {
		return unsupportedPlanning(ctx, env, command), nil
	}
	history, err := env.Store.IdeationHistory(ctx, command.SessionID)
	if err != nil {
		return Outcome{}, err
	}
	if !hasApproval(history, command.RequestID) {
		if history.Session.Revision != command.ExpectedRevision {
			return Outcome{}, &store.IdeationRevisionConflictError{SessionID: command.SessionID, Expected: command.ExpectedRevision, Actual: history.Session.Revision}
		}
		if _, err = recordApproval(ctx, env, history.Session, command, &payload.ApprovePayload); err != nil {
			return Outcome{}, err
		}
	}
	if err := env.AuthorizePlan(ctx, command, sortedSelection(payload.SelectedIssueIDs)); err != nil {
		return Outcome{}, err
	}
	// Publication remains the same content-only path; it does not create authority.
	plain := command
	plain.Kind = string(KindApprove)
	plain.Payload, _ = json.Marshal(payload.ApprovePayload)
	outcome, err := approve(ctx, env, plain)
	if err != nil {
		return outcome, err
	}
	if outcome.Code != store.IdeationCommandApplied {
		return outcome, errors.New("publication did not accept the approved local plan")
	}
	outcome.Detail += "; execution authorized and queued subject to complete publication and specification merge"
	return outcome, nil
}
