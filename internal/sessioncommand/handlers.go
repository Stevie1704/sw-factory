package sessioncommand

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/publication"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// DefaultHandlers returns the coordinator behavior of every command kind.
// They change sessions only through the session lifecycle and the store, and
// start session roles through Env.Roles. Without Env.Roles, hand-off and
// regeneration check the request and report that no role can start.
func DefaultHandlers() map[Kind]Handler {
	return map[Kind]Handler{
		KindCreateSession:       createSession,
		KindSubmitTurn:          submitTurn,
		KindEditBrief:           editBrief,
		KindHandOff:             handOff,
		KindEditDraft:           editDraft,
		KindRegenerateDraft:     regenerateDraft,
		KindPreviewPlan:         previewPlan,
		KindApprove:             approve,
		KindPublish:             publish,
		KindReconcile:           reconcile,
		KindDefer:               Transition(workflow.SessionEventDeferred),
		KindResume:              Transition(workflow.SessionEventResumed),
		KindInvestigationNeeded: investigationNeeded,
		KindNoBuild:             Transition(workflow.SessionEventNoBuild),
		KindCancel:              Transition(workflow.SessionEventCancelled),
	}
}

// Applied returns the applied outcome for session.
func Applied(session store.IdeationSession, detail string) Outcome {
	return Outcome{Code: store.IdeationCommandApplied, SessionRevision: session.Revision, Detail: detail}
}

// CurrentSession reads the command's session and returns a revision conflict
// when the browser showed another revision.
func CurrentSession(ctx context.Context, env Env, command store.IdeationCommand) (store.IdeationSession, error) {
	session, err := env.Store.IdeationSession(ctx, command.SessionID)
	if err != nil {
		return store.IdeationSession{}, err
	}
	if session.Revision != command.ExpectedRevision {
		return session, &store.IdeationRevisionConflictError{SessionID: session.ID, Expected: command.ExpectedRevision, Actual: session.Revision}
	}
	return session, nil
}

// CommandWrite resolves event for session through the lifecycle and names
// command, so the store completes the command in the same transaction.
func CommandWrite(env Env, session store.IdeationSession, command store.IdeationCommand, event workflow.SessionEvent, reason string) (store.IdeationWrite, error) {
	write, err := env.Lifecycle.Write(session, event, reason, env.now())
	if err != nil {
		return store.IdeationWrite{}, err
	}
	write.CommandID = command.RequestID
	return write, nil
}

// payloadOf decodes the stored payload of command into T.
func payloadOf[T any](command store.IdeationCommand) (*T, error) {
	decoded, err := DecodePayload(Kind(command.Kind), command.Payload)
	if err != nil {
		return nil, err
	}
	payload, ok := decoded.(*T)
	if !ok {
		return nil, fmt.Errorf("session command %q has an unexpected payload type %T", command.RequestID, decoded)
	}
	return payload, nil
}

// createSession creates the session the command names. A resumed command
// whose session exists was applied before the coordinator stopped.
func createSession(ctx context.Context, env Env, command store.IdeationCommand) (Outcome, error) {
	payload, err := payloadOf[CreateSessionPayload](command)
	if err != nil {
		return Outcome{}, err
	}
	session, err := env.Store.CreateIdeationSession(ctx, store.IdeationSession{
		ID: command.SessionID, RepositoryPath: env.RepositoryPath, Title: payload.Title,
		State: env.Lifecycle.InitialState(), CreatedAt: env.now(),
	})
	if errors.Is(err, store.ErrIdeationSessionExists) && command.Attempts > 1 {
		if session, err = env.Store.IdeationSession(ctx, command.SessionID); err == nil {
			return Applied(session, "session created"), nil
		}
	}
	if err != nil {
		return Outcome{}, err
	}
	return Applied(session, "session created"), nil
}

// submitTurn appends the user turn with the request ID as turn ID and, when
// session roles are available, starts the ideation role for it. With a role
// to start, the turn does not complete the command, so a coordinator stop
// before the role start resumes with the stored turn and starts the role.
func submitTurn(ctx context.Context, env Env, command store.IdeationCommand) (Outcome, error) {
	payload, err := payloadOf[TurnPayload](command)
	if err != nil {
		return Outcome{}, err
	}
	history, err := env.Store.IdeationHistory(ctx, command.SessionID)
	if err != nil {
		return Outcome{}, err
	}
	session := history.Session
	if !hasTurn(history, command.RequestID) {
		if session.Revision != command.ExpectedRevision {
			return Outcome{}, &store.IdeationRevisionConflictError{SessionID: session.ID, Expected: command.ExpectedRevision, Actual: session.Revision}
		}
		write, err := CommandWrite(env, session, command, workflow.SessionEventTurnAccepted, "")
		if err != nil {
			return Outcome{}, err
		}
		if env.Roles != nil {
			write.CommandID = ""
		}
		session, _, err = env.Store.AppendIdeationTurn(ctx, write, store.IdeationTurn{
			ID: command.RequestID, Author: store.IdeationTurnAuthorUser, Body: payload.Body, Corrects: payload.Corrects,
		})
		if err != nil {
			return Outcome{}, err
		}
	}
	if env.Roles == nil {
		return Applied(session, "turn accepted"), nil
	}
	detail := "turn accepted; the ideation role started"
	if err := env.Roles.StartIdeation(ctx, session.ID, command.RequestID); err != nil {
		if ctx.Err() != nil {
			return Outcome{}, err
		}
		detail = "turn accepted; the ideation role did not start: " + err.Error()
	}
	if current, err := env.Store.IdeationSession(ctx, session.ID); err == nil {
		session = current
	}
	return Applied(session, detail), nil
}

// hasTurn reports whether history holds the turn id.
func hasTurn(history store.IdeationHistory, id string) bool {
	for _, turn := range history.Turns {
		if turn.ID == id {
			return true
		}
	}
	return false
}

// editBrief saves the edited brief as the next brief revision.
func editBrief(ctx context.Context, env Env, command store.IdeationCommand) (Outcome, error) {
	payload, err := payloadOf[BriefPayload](command)
	if err != nil {
		return Outcome{}, err
	}
	session, err := CurrentSession(ctx, env, command)
	if err != nil {
		return Outcome{}, err
	}
	write, err := CommandWrite(env, session, command, workflow.SessionEventEdited, "")
	if err != nil {
		return Outcome{}, err
	}
	session, revision, err := env.Store.SaveBriefRevision(ctx, write, payload.Brief.Brief())
	if err != nil {
		return Outcome{}, err
	}
	return Applied(session, fmt.Sprintf("brief revision %d saved", revision.Revision)), nil
}

// handOff checks that the displayed brief revision is current, that the user
// deferred every unresolved question of it explicitly, and that the
// lifecycle allows a role start. It then starts planning.
func handOff(ctx context.Context, env Env, command store.IdeationCommand) (Outcome, error) {
	payload, err := payloadOf[HandOffPayload](command)
	if err != nil {
		return Outcome{}, err
	}
	brief, session, err := currentBrief(ctx, env, command, payload.BriefRevision)
	if err != nil {
		return Outcome{}, err
	}
	if err := checkDeferredQuestions(brief, payload.DeferredQuestions); err != nil {
		return Outcome{}, err
	}
	return startPlanning(ctx, env, session, command)
}

// currentBrief reads the session at the command's revision and returns its
// current brief, which must be the brief revision the user saw.
func currentBrief(ctx context.Context, env Env, command store.IdeationCommand, briefRevision int64) (store.Brief, store.IdeationSession, error) {
	history, err := env.Store.IdeationHistory(ctx, command.SessionID)
	if err != nil {
		return store.Brief{}, store.IdeationSession{}, err
	}
	session := history.Session
	if session.Revision != command.ExpectedRevision {
		return store.Brief{}, session, &store.IdeationRevisionConflictError{SessionID: session.ID, Expected: command.ExpectedRevision, Actual: session.Revision}
	}
	if briefRevision != session.BriefRevision || len(history.Briefs) == 0 {
		return store.Brief{}, session, fmt.Errorf("brief revision %d is not the current brief revision %d", briefRevision, session.BriefRevision)
	}
	return history.Briefs[len(history.Briefs)-1].Brief, session, nil
}

// checkDeferredQuestions refuses a handoff whose deferred questions are not
// exactly the unresolved questions of brief, so that no unknown goes to
// planning without the user's explicit decision.
func checkDeferredQuestions(brief store.Brief, deferred []string) error {
	named := map[string]bool{}
	for _, id := range deferred {
		named[id] = true
	}
	var missing []string
	for _, question := range brief.UnresolvedQuestions {
		if !named[question.ID] {
			missing = append(missing, question.ID)
		}
		delete(named, question.ID)
	}
	if len(missing) > 0 {
		return fmt.Errorf("unresolved questions %s are not deferred; defer each one explicitly, answer it, or record that investigation is needed", strings.Join(missing, ", "))
	}
	for id := range named {
		return fmt.Errorf("deferred question %q is not an unresolved question of the brief", id)
	}
	return nil
}

// investigationNeeded records that material unknowns need investigation
// before planning. The unresolved questions of the current brief are the
// investigation items, so the brief must hold at least one.
func investigationNeeded(ctx context.Context, env Env, command store.IdeationCommand) (Outcome, error) {
	history, err := env.Store.IdeationHistory(ctx, command.SessionID)
	if err != nil {
		return Outcome{}, err
	}
	if count := len(history.Briefs); count == 0 || len(history.Briefs[count-1].Brief.UnresolvedQuestions) == 0 {
		return Outcome{}, errors.New("the brief has no unresolved question to investigate; record the unknown as an unresolved question of the brief first")
	}
	return Transition(workflow.SessionEventInvestigationNeeded)(ctx, env, command)
}

// regenerateDraft checks that the displayed plan revision is current and that
// the lifecycle allows a role start. This build has no planning role.
func regenerateDraft(ctx context.Context, env Env, command store.IdeationCommand) (Outcome, error) {
	payload, err := payloadOf[PlanRevisionPayload](command)
	if err != nil {
		return Outcome{}, err
	}
	session, err := CurrentSession(ctx, env, command)
	if err != nil {
		return Outcome{}, err
	}
	if payload.PlanRevision != session.PlanRevision {
		return Outcome{}, fmt.Errorf("plan revision %d is not the current plan revision %d", payload.PlanRevision, session.PlanRevision)
	}
	return startPlanning(ctx, env, session, command)
}

// startPlanning starts the planning role for command, or reports that this
// build cannot start one.
func startPlanning(ctx context.Context, env Env, session store.IdeationSession, command store.IdeationCommand) (Outcome, error) {
	if env.Roles == nil {
		return roleUnavailable(env, session)
	}
	if err := env.Roles.StartPlanning(ctx, session.ID, command.RequestID); err != nil {
		return Outcome{}, err
	}
	if current, err := env.Store.IdeationSession(ctx, session.ID); err == nil {
		session = current
	}
	return Applied(session, "the planning role started"), nil
}

// roleUnavailable reports that no session role can start, after it checked
// that the lifecycle would allow one.
func roleUnavailable(env Env, session store.IdeationSession) (Outcome, error) {
	if _, err := env.Lifecycle.Next(session.State, workflow.SessionEventRoleStarted); err != nil {
		return Outcome{}, err
	}
	return Outcome{Code: store.IdeationCommandUnsupported, SessionRevision: session.Revision,
		Detail: "this build cannot start the planning role; the session is unchanged"}, nil
}

// editDraft saves the edited plan as the next plan revision. It keeps the
// repository context of the current draft and derives from the current
// brief.
func editDraft(ctx context.Context, env Env, command store.IdeationCommand) (Outcome, error) {
	payload, err := payloadOf[DraftPayload](command)
	if err != nil {
		return Outcome{}, err
	}
	history, err := env.Store.IdeationHistory(ctx, command.SessionID)
	if err != nil {
		return Outcome{}, err
	}
	session := history.Session
	if session.Revision != command.ExpectedRevision {
		return Outcome{}, &store.IdeationRevisionConflictError{SessionID: session.ID, Expected: command.ExpectedRevision, Actual: session.Revision}
	}
	if len(history.Plans) == 0 {
		return Outcome{}, errors.New("the session has no plan draft to edit")
	}
	current := history.Plans[len(history.Plans)-1]
	nodes := PlanNodes(payload.Nodes)
	if env.ValidatePlan != nil {
		if err := env.ValidatePlan(nodes); err != nil {
			return Outcome{}, err
		}
	}
	write, err := CommandWrite(env, session, command, workflow.SessionEventEdited, "")
	if err != nil {
		return Outcome{}, err
	}
	session, revision, err := env.Store.SavePlanRevision(ctx, write, store.PlanRevision{
		SourceBriefRevision: session.BriefRevision, ContextRevision: current.ContextRevision, Nodes: nodes,
	})
	if err != nil {
		return Outcome{}, err
	}
	return Applied(session, fmt.Sprintf("plan revision %d saved", revision.Revision)), nil
}

// Transition returns a handler that records event with the payload reason.
func Transition(event workflow.SessionEvent) Handler {
	return func(ctx context.Context, env Env, command store.IdeationCommand) (Outcome, error) {
		payload, err := payloadOf[ReasonPayload](command)
		if err != nil {
			return Outcome{}, err
		}
		session, err := CurrentSession(ctx, env, command)
		if err != nil {
			return Outcome{}, err
		}
		write, err := CommandWrite(env, session, command, event, payload.Reason)
		if err != nil {
			return Outcome{}, err
		}
		if session, err = env.Store.TransitionIdeationSession(ctx, write); err != nil {
			return Outcome{}, err
		}
		return Applied(session, "session is "+string(session.State)), nil
	}
}

// PreviewResult is the JSON result of a preview command: the exact rendered
// publication and the digests an approval must name.
type PreviewResult struct {
	// PlanRevision is the rendered plan revision.
	PlanRevision int64 `json:"plan_revision"`
	// Target is the provider target.
	Target string `json:"target"`
	// Payload is the canonical rendering of the items.
	Payload json.RawMessage `json:"payload"`
	// RelationshipMapping is the canonical rendering of the relationships.
	RelationshipMapping json.RawMessage `json:"relationship_mapping"`
	// PayloadSHA256 is the digest of Payload.
	PayloadSHA256 string `json:"payload_sha256"`
	// RelationshipMappingSHA256 is the digest of RelationshipMapping.
	RelationshipMappingSHA256 string `json:"relationship_mapping_sha256"`
}

// errNoPlanningTracker reports a tracker adapter without planning capability.
var errNoPlanningTracker = errors.New("the configured tracker adapter cannot preview or publish plans")

// unsupportedPlanning is the outcome when env has no publisher.
func unsupportedPlanning(ctx context.Context, env Env, command store.IdeationCommand) Outcome {
	outcome := Outcome{Code: store.IdeationCommandUnsupported, Detail: errNoPlanningTracker.Error()}
	if session, err := env.Store.IdeationSession(ctx, command.SessionID); err == nil {
		outcome.SessionRevision = session.Revision
	}
	return outcome
}

// previewPlan renders the publication of the displayed plan revision. It
// makes no tracker write and changes no session state.
func previewPlan(ctx context.Context, env Env, command store.IdeationCommand) (Outcome, error) {
	if env.Publisher == nil {
		return unsupportedPlanning(ctx, env, command), nil
	}
	payload, err := payloadOf[PlanRevisionPayload](command)
	if err != nil {
		return Outcome{}, err
	}
	session, err := CurrentSession(ctx, env, command)
	if err != nil {
		return Outcome{}, err
	}
	binding, err := env.Publisher.Preview(ctx, session.ID)
	if err != nil {
		return Outcome{}, err
	}
	if binding.PlanRevision != payload.PlanRevision {
		return Outcome{}, fmt.Errorf("plan revision %d is not the current plan revision %d", payload.PlanRevision, binding.PlanRevision)
	}
	result, err := json.Marshal(PreviewResult{
		PlanRevision: binding.PlanRevision, Target: binding.Target, Payload: binding.Payload, RelationshipMapping: binding.RelationshipMapping,
		PayloadSHA256: digest(binding.Payload), RelationshipMappingSHA256: digest(binding.RelationshipMapping),
	})
	if err != nil {
		return Outcome{}, fmt.Errorf("encode preview: %w", err)
	}
	outcome := Applied(session, "preview rendered for "+binding.Target)
	outcome.Result = result
	return outcome, nil
}

// approve records an approval that binds the previewed publication, with the
// request ID as approval ID, and publishes when the payload asks for it. A
// changed rendering is refused before any tracker write. A resumed command
// whose approval exists continues with publication.
func approve(ctx context.Context, env Env, command store.IdeationCommand) (Outcome, error) {
	if env.Publisher == nil {
		return unsupportedPlanning(ctx, env, command), nil
	}
	payload, err := payloadOf[ApprovePayload](command)
	if err != nil {
		return Outcome{}, err
	}
	history, err := env.Store.IdeationHistory(ctx, command.SessionID)
	if err != nil {
		return Outcome{}, err
	}
	session := history.Session
	if !hasApproval(history, command.RequestID) {
		if session.Revision != command.ExpectedRevision {
			return Outcome{}, &store.IdeationRevisionConflictError{SessionID: session.ID, Expected: command.ExpectedRevision, Actual: session.Revision}
		}
		if session, err = recordApproval(ctx, env, session, command, payload); err != nil {
			return Outcome{}, err
		}
	}
	if !payload.Publish {
		return Applied(session, "plan approved; publication was not requested"), nil
	}
	return publishAttempt(ctx, env, command)
}

// recordApproval checks the preview digests against a fresh rendering and
// records the approval.
func recordApproval(ctx context.Context, env Env, session store.IdeationSession, command store.IdeationCommand, payload *ApprovePayload) (store.IdeationSession, error) {
	if payload.PlanRevision != session.PlanRevision {
		return store.IdeationSession{}, fmt.Errorf("plan revision %d is not the current plan revision %d", payload.PlanRevision, session.PlanRevision)
	}
	binding, err := env.Publisher.Preview(ctx, session.ID)
	if err != nil {
		return store.IdeationSession{}, err
	}
	if digest(binding.Payload) != payload.PayloadSHA256 || digest(binding.RelationshipMapping) != payload.RelationshipMappingSHA256 {
		return store.IdeationSession{}, errors.New("the rendered publication changed since the preview; preview the plan again and review the new preview")
	}
	write, err := env.Lifecycle.Write(session, workflow.SessionEventApproved, "", env.now())
	if err != nil {
		return store.IdeationSession{}, err
	}
	session, _, err = env.Store.ApprovePlan(ctx, write, binding.Approval(command.RequestID))
	return session, err
}

// hasApproval reports whether history holds the approval id.
func hasApproval(history store.IdeationHistory, id string) bool {
	for _, approval := range history.Approvals {
		if approval.ID == id {
			return true
		}
	}
	return false
}

// publish starts or retries publication of the current approval. A resumed
// command continues the same attempt without the revision check, because
// its first attempt already moved the session.
func publish(ctx context.Context, env Env, command store.IdeationCommand) (Outcome, error) {
	if env.Publisher == nil {
		return unsupportedPlanning(ctx, env, command), nil
	}
	if command.Attempts <= 1 {
		if _, err := CurrentSession(ctx, env, command); err != nil {
			return Outcome{}, err
		}
	}
	return publishAttempt(ctx, env, command)
}

// publishAttempt publishes with the request ID as attempt and receipt ID.
func publishAttempt(ctx context.Context, env Env, command store.IdeationCommand) (Outcome, error) {
	result, err := env.Publisher.Publish(ctx, publication.Request{SessionID: command.SessionID, AttemptID: command.RequestID})
	if err != nil {
		return Outcome{}, err
	}
	detail := "published"
	if result.Receipt.Outcome == store.PublicationOutcomeIncomplete {
		detail = "publication incomplete: " + result.Receipt.Detail
	}
	return Applied(result.Session, detail), nil
}

// reconcile resolves one ambiguous item create. It writes only the ledger, so
// the session revision does not change.
func reconcile(ctx context.Context, env Env, command store.IdeationCommand) (Outcome, error) {
	if env.Publisher == nil {
		return unsupportedPlanning(ctx, env, command), nil
	}
	payload, err := payloadOf[ReconcilePayload](command)
	if err != nil {
		return Outcome{}, err
	}
	session, err := CurrentSession(ctx, env, command)
	if err != nil {
		return Outcome{}, err
	}
	operation, err := env.Publisher.Reconcile(ctx, publication.Reconciliation{
		SessionID: session.ID, OperationID: payload.OperationID, ExternalID: payload.ExternalID,
	})
	if err != nil {
		return Outcome{}, err
	}
	return Applied(session, fmt.Sprintf("operation %s is %s; publish again to continue", operation.ID, operation.State)), nil
}

// digest returns the lowercase SHA-256 of value.
func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
