package factory

import (
	"context"
	"errors"
	"fmt"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/localissues"
	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// operatorCommandInbox exposes only the extra reads needed by scoped admission.
type operatorCommandInbox interface {
	Run(context.Context, string) (*store.Run, error)
	LatestLocalExecutionPreview(context.Context, string) (store.LocalExecutionPreview, error)
	LocalExecutionPreview(context.Context, string, string) (store.LocalExecutionPreview, error)
}

// checkOperatorCommandTarget protects current revisions before the inbox append.
// It never fetches Git, writes authority, invokes a worker, or changes Run state.
func (s *Service) checkOperatorCommandTarget(ctx context.Context, inbox CommandInbox, registration config.RepositoryRegistration, command store.IdeationCommand) error {
	if registration.EffectiveTracker() != config.ProviderLocal {
		return errors.New("scoped operator commands require the local tracker")
	}
	if err := sessioncommand.ValidateSubject(command); err != nil {
		return err
	}
	reader, ok := inbox.(operatorCommandInbox)
	if !ok {
		return errors.New("command inbox lacks scoped subject reads")
	}
	if sessioncommand.EffectiveScope(command) == "backlog" {
		preview, err := reader.LatestLocalExecutionPreview(ctx, registration.Path)
		if errors.Is(err, store.ErrLocalExecutionNotFound) && sessioncommand.Kind(command.Kind) == sessioncommand.KindPreviewExecution && command.ExpectedRevision == 0 {
			return nil
		}
		if err != nil {
			return err
		}
		if sessioncommand.Kind(command.Kind) == sessioncommand.KindPreviewExecution && command.ExpectedRevision == 0 {
			return nil
		}
		if preview.Revision != command.ExpectedRevision {
			return &store.CommandRevisionConflictError{Scope: "backlog", SubjectID: command.SubjectID, Expected: command.ExpectedRevision, Actual: preview.Revision}
		}
		if sessioncommand.Kind(command.Kind) == sessioncommand.KindAuthorizeExecution {
			decoded, err := sessioncommand.DecodePayload(sessioncommand.KindAuthorizeExecution, command.Payload)
			if err != nil {
				return err
			}
			payload := decoded.(*sessioncommand.ExecutionAuthorizationPayload)
			if payload.PreviewID != preview.ID {
				return store.ErrLocalExecutionConflict
			}
		}
		return nil
	}
	run, err := reader.Run(ctx, command.SubjectID)
	if err != nil {
		return err
	}
	if run == nil || run.RepositoryPath != registration.Path {
		return errors.New("displayed Run does not exist in this registered repository")
	}
	if run.Revision != command.ExpectedRevision {
		return &store.CommandRevisionConflictError{Scope: "run", SubjectID: run.ID, Expected: command.ExpectedRevision, Actual: run.Revision}
	}
	return validateRunOperatorBinding(*run, command)
}

// validateRunOperatorBinding binds question text as well as packet and Run identity.
func validateRunOperatorBinding(run store.Run, command store.IdeationCommand) error {
	decoded, err := sessioncommand.DecodePayload(sessioncommand.Kind(command.Kind), command.Payload)
	if err != nil {
		return err
	}
	payload := decoded.(*sessioncommand.RunOperatorPayload)
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return err
	}
	if packet.Version != payload.PacketVersion {
		return fmt.Errorf("%w: displayed Specification packet changed", store.ErrLocalExecutionConflict)
	}
	if sessioncommand.Kind(command.Kind) == sessioncommand.KindAnswerRun {
		question, found := pendingQuestion(run.PendingQuestions, payload.QuestionID)
		if !found || payload.Answer == "" || payload.QuestionDigest != LocalQuestionDigest(run.ID, packet.Version, question) {
			return fmt.Errorf("%w: displayed clarification question changed or is no longer pending", store.ErrLocalExecutionConflict)
		}
	} else if payload.Answer != "" || payload.QuestionID != "" || payload.QuestionDigest != "" {
		return errors.New("non-answer Run commands cannot carry clarification content")
	}
	return nil
}

// LocalQuestionDigest binds the displayed Run, Specification and exact question.
func LocalQuestionDigest(runID string, packetVersion int, question store.PendingQuestion) string {
	return localissues.Digest([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s", runID, packetVersion, question.ID, question.Prompt)))
}
