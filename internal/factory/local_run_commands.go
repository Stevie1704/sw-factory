package factory

import (
	"context"
	"errors"
	"strconv"

	commandlanguage "github.com/Stevie1704/sw-factory/internal/command"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// runSubjectReader selects an explicitly displayed Run, including terminal Runs.
type runSubjectReader interface {
	Run(context.Context, string) (*store.Run, error)
}

// applyLocalRunCommand binds protected operator input to ordinary Run handlers.
// Scoped numeric watermarks keep inbox replay separate from issue/PR comments.
func (s *Service) applyLocalRunCommand(ctx context.Context, registration config.RepositoryRegistration, command store.IdeationCommand) (sessioncommand.Outcome, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	_, runStore, err := s.openRunStore(ctx)
	if err != nil {
		return sessioncommand.Outcome{}, err
	}
	defer runStore.Close()
	reader, ok := runStore.(runSubjectReader)
	if !ok {
		return sessioncommand.Outcome{}, errors.New("store cannot select a displayed Run")
	}
	run, err := reader.Run(ctx, command.SubjectID)
	if err != nil {
		return sessioncommand.Outcome{}, err
	}
	if run == nil || run.RepositoryPath != registration.Path {
		return sessioncommand.Outcome{}, errors.New("displayed Run no longer exists in this repository")
	}
	if command.Sequence < 1 {
		return sessioncommand.Outcome{}, errors.New("local Run command requires durable inbox sequence")
	}
	if err := validateRunAdapter(registration, *run); err != nil {
		return sessioncommand.Outcome{}, err
	}
	comment := tracker.Comment{ID: scopedCommentID("local_operator", strconv.FormatInt(command.Sequence, 10)), Author: registration.LocalOperator.Name, UpdatedAt: command.AcceptedAt}
	// Ordinary effect recovery wins over revision validation: a packet transition
	// may have crossed its durable boundary before the previous process stopped.
	if journaled, ok := runStore.(PendingEffectStore); ok {
		pending, err := journaled.PendingEffect(ctx, run.ID)
		if err != nil {
			return sessioncommand.Outcome{}, err
		}
		if pending != nil {
			updated, err := s.journal().Replay(ctx, runStore, *pending)
			if err != nil {
				return sessioncommand.Outcome{}, err
			}
			run = &updated
		}
	}
	if eventIDAlreadyProcessed(run.ProcessedCommentID, comment.ID) {
		code, detail := replayLocalOperatorReceipt(*run, comment.ID)
		return sessioncommand.Outcome{Code: code, SessionRevision: run.Revision, Detail: detail}, nil
	}
	if run.Revision != command.ExpectedRevision {
		return sessioncommand.Outcome{}, &store.CommandRevisionConflictError{Scope: "run", SubjectID: run.ID, Expected: command.ExpectedRevision, Actual: run.Revision}
	}
	if err := validateRunOperatorBinding(*run, command); err != nil {
		return sessioncommand.Outcome{}, err
	}
	decoded, err := sessioncommand.DecodePayload(sessioncommand.Kind(command.Kind), command.Payload)
	if err != nil {
		return sessioncommand.Outcome{}, err
	}
	payload := decoded.(*sessioncommand.RunOperatorPayload)
	parsed := commandlanguage.Request{}
	switch sessioncommand.Kind(command.Kind) {
	case sessioncommand.KindAnswerRun:
		parsed.Kind = commandlanguage.Answer
		parsed.QuestionID = payload.QuestionID
		parsed.Answer = payload.Answer
	case sessioncommand.KindCancelRun:
		parsed.Kind = commandlanguage.Cancel
	case sessioncommand.KindRetryRun:
		parsed.Kind = commandlanguage.Retry
	case sessioncommand.KindResumeRun:
		parsed.Kind = commandlanguage.Resume
	default:
		return sessioncommand.Outcome{Code: store.IdeationCommandUnsupported, Detail: "unsupported local Run recovery operation"}, nil
	}
	if active, err := runStore.CurrentRun(ctx); err != nil {
		return sessioncommand.Outcome{}, err
	} else if active != nil && active.ID != run.ID {
		return sessioncommand.Outcome{}, errors.New("another non-terminal Run owns the repository; the displayed Run cannot be resumed or retried")
	}
	if published, err := s.ensureClarificationPublication(ctx, registration, runStore, *run); err != nil {
		return sessioncommand.Outcome{}, err
	} else {
		run = &published
	}
	var result CommandResult
	switch parsed.Kind {
	case commandlanguage.Answer:
		result, err = s.handleAnswerCommand(ctx, registration, runStore, *run, comment, parsed)
	case commandlanguage.Cancel:
		result, err = s.handleCancelCommand(ctx, registration, runStore, *run, comment, parsed)
	case commandlanguage.Retry:
		result, err = s.handleRetryCommand(ctx, registration, runStore, *run, comment, parsed)
	case commandlanguage.Resume:
		result, err = s.handleResumeCommand(ctx, registration, runStore, *run, comment, parsed)
	}
	if err != nil {
		return sessioncommand.Outcome{}, err
	}
	detail := "local Run command applied through the coordinator"
	if payload.Reason != "" {
		detail += "; " + payload.Reason
	}
	return sessioncommand.Outcome{Code: store.IdeationCommandApplied, SessionRevision: result.Run.Revision, Detail: detail}, nil
}

// LocalRunQuestion is a displayed pending question with exact command binding.
type LocalRunQuestion struct{ ID, Prompt, Digest string }

// LocalRunOperatorView exposes supported actions and bound structured input.
type LocalRunOperatorView struct {
	Supervisor    SupervisorView
	RunID         string
	Revision      int64
	PacketVersion int
	Questions     []LocalRunQuestion
	Actions       []string
	Commands      []store.IdeationCommand
}

// LocalRunOperator reads a Run without requiring a live supervisor or session.
func (s *Service) LocalRunOperator(ctx context.Context, runID string) (LocalRunOperatorView, error) {
	registration, err := s.registration()
	if err != nil {
		return LocalRunOperatorView{}, err
	}
	if registration.EffectiveTracker() != config.ProviderLocal {
		return LocalRunOperatorView{}, errors.New("local Run operator controls require the local tracker")
	}
	opened, err := s.deps.OpenStoreReadOnly(ctx, registration.OperationalDataPath)
	if err != nil {
		return LocalRunOperatorView{}, err
	}
	defer opened.Close()
	reader, ok := opened.(interface {
		runSubjectReader
		SupervisorHeartbeatReader
		ScopedIdeationCommands(context.Context, string, string, int) ([]store.IdeationCommand, error)
	})
	if !ok {
		return LocalRunOperatorView{}, errors.New("store lacks scoped Run inspection")
	}
	run, err := reader.Run(ctx, runID)
	if err != nil {
		return LocalRunOperatorView{}, err
	}
	if run == nil || run.RepositoryPath != registration.Path {
		return LocalRunOperatorView{}, errors.New("Run not found")
	}
	if err := validateRunAdapter(registration, *run); err != nil {
		return LocalRunOperatorView{}, err
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return LocalRunOperatorView{}, err
	}
	supervisor, err := s.supervisorLiveness(ctx, reader)
	if err != nil {
		return LocalRunOperatorView{}, err
	}
	view := LocalRunOperatorView{Supervisor: supervisor, RunID: run.ID, Revision: run.Revision, PacketVersion: packet.Version, Actions: []string{}, Questions: []LocalRunQuestion{}}
	for _, question := range run.PendingQuestions {
		view.Questions = append(view.Questions, LocalRunQuestion{ID: question.ID, Prompt: question.Prompt, Digest: LocalQuestionDigest(run.ID, packet.Version, question)})
	}
	if !store.IsTerminalStatus(run.Status) {
		view.Actions = append(view.Actions, string(sessioncommand.KindCancelRun))
	}
	if (run.Status == store.StatusWaitingForHuman || run.Status == store.StatusActive) && len(run.PendingQuestions) > 0 {
		view.Actions = append(view.Actions, string(sessioncommand.KindAnswerRun))
	}
	active, activeErr := opened.CurrentRun(ctx)
	if activeErr != nil {
		return LocalRunOperatorView{}, activeErr
	}
	otherActive := active != nil && active.ID != run.ID
	if !otherActive && (run.Status == store.StatusFailed || run.Status == store.StatusCancelled) {
		view.Actions = append(view.Actions, string(sessioncommand.KindRetryRun))
	}
	if !otherActive && resumeAdmissionReason(*run) == "" {
		view.Actions = append(view.Actions, string(sessioncommand.KindResumeRun))
	}
	view.Commands, err = reader.ScopedIdeationCommands(ctx, "run", run.ID, 50)
	return view, err
}

// LocalExecutionOverview is read-only durable backlog approval evidence.
type LocalExecutionOverview struct {
	Supervisor     SupervisorView
	Preview        *store.LocalExecutionPreview
	Authorizations []store.LocalExecutionAuthorization
	Commands       []store.IdeationCommand
}

// LocalExecution reads exact preview, approvals and outcomes while stopped too.
func (s *Service) LocalExecution(ctx context.Context) (LocalExecutionOverview, error) {
	registration, err := s.registration()
	if err != nil {
		return LocalExecutionOverview{}, err
	}
	if registration.EffectiveTracker() != config.ProviderLocal {
		return LocalExecutionOverview{}, errors.New("local backlog requires the local tracker")
	}
	opened, err := s.deps.OpenStoreReadOnly(ctx, registration.OperationalDataPath)
	if err != nil {
		return LocalExecutionOverview{}, err
	}
	defer opened.Close()
	reader, ok := opened.(interface {
		localExecutionStore
		SupervisorHeartbeatReader
		ScopedIdeationCommands(context.Context, string, string, int) ([]store.IdeationCommand, error)
	})
	if !ok {
		return LocalExecutionOverview{}, errors.New("store lacks local execution read surface")
	}
	supervisor, err := s.supervisorLiveness(ctx, reader)
	if err != nil {
		return LocalExecutionOverview{}, err
	}
	result := LocalExecutionOverview{Supervisor: supervisor}
	preview, err := reader.LatestLocalExecutionPreview(ctx, registration.Path)
	if err == nil {
		result.Preview = &preview
	} else if !errors.Is(err, store.ErrLocalExecutionNotFound) {
		return result, err
	}
	result.Authorizations, err = reader.LocalExecutionAuthorizations(ctx, registration.Path)
	if err != nil {
		return result, err
	}
	result.Commands, err = reader.ScopedIdeationCommands(ctx, "backlog", "repository", 50)
	return result, err
}
