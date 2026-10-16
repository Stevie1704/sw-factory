package factory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/plangraph"
	"github.com/Stevie1704/sw-factory/internal/publication"
	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// sessionCommandInterval is how often the coordinator reads the session
// command inbox. It is short so that a browser sees a result quickly; the
// run poll interval does not apply.
const sessionCommandInterval = time.Second

// maxSessionActivityCommands bounds the command outcomes in one activity read.
const maxSessionActivityCommands = 50

// ErrSupervisorUnavailable reports that no live coordinator can apply a
// session command. The UI appends nothing and starts no second writer.
var ErrSupervisorUnavailable = errors.New("supervisor unavailable")

// SupervisorUnavailableError is the explicit unavailable result of a session
// command. It carries the supervisor view the decision used.
type SupervisorUnavailableError struct {
	// Supervisor is the heartbeat view at submission.
	Supervisor SupervisorView
}

// Error explains the result and the corrective action.
func (e *SupervisorUnavailableError) Error() string {
	return "the coordinator is not running (no live supervisor heartbeat); run factory start and submit again"
}

// Unwrap lets callers classify the error with ErrSupervisorUnavailable.
func (*SupervisorUnavailableError) Unwrap() error { return ErrSupervisorUnavailable }

// CommandInbox is the narrow UI write seam for session commands. Its only
// write is to append a command.
type CommandInbox interface {
	AcceptIdeationCommand(context.Context, store.IdeationCommand) (store.IdeationCommand, bool, error)
	IdeationCommand(context.Context, string) (store.IdeationCommand, error)
	IdeationSession(context.Context, string) (store.IdeationSession, error)
	SupervisorHeartbeatReader
	Close() error
}

var _ CommandInbox = (*store.CommandInbox)(nil)

// CommandInboxOpener opens the command inbox of an existing, current store.
type CommandInboxOpener func(context.Context, string) (CommandInbox, error)

// SessionReadStore is the read-only store seam of session pages.
type SessionReadStore interface {
	OperationalStore
	SupervisorHeartbeatReader
	IdeationSessions(context.Context, string) ([]store.IdeationSession, error)
	IdeationHistory(context.Context, string) (store.IdeationHistory, error)
	IdeationCommand(context.Context, string) (store.IdeationCommand, error)
	IdeationCommands(context.Context, string, int) ([]store.IdeationCommand, error)
	SessionInvocations(context.Context, string) ([]store.SessionInvocation, error)
}

// SessionCommandAck is the acknowledgement of one submitted command. A new
// command is accepted, not completed: the coordinator applies it later.
type SessionCommandAck struct {
	// Command is the stored command with its current state and outcome.
	Command store.IdeationCommand
	// Replayed reports that the request ID was already in the inbox.
	Replayed bool
}

// SessionOverview is the session list model.
type SessionOverview struct {
	// Supervisor is the coordinator liveness at the read.
	Supervisor SupervisorView
	// Sessions holds every session of the repository, newest update first.
	Sessions []store.IdeationSession
}

// SessionActivity is the durable state a browser reloads after a disconnect.
// It holds application-owned structured state only; harness output never
// enters it.
type SessionActivity struct {
	// Supervisor is the coordinator liveness at the read.
	Supervisor SupervisorView
	// History is the session with its turns, revisions, approvals, receipts,
	// and publication ledger.
	History store.IdeationHistory
	// Commands holds the newest commands of the session, newest first.
	Commands []store.IdeationCommand
	// Invocations holds every session-role invocation of the session in
	// start order, with accepted payloads and typed failure causes.
	Invocations []store.SessionInvocation
}

// SessionCommandView is one command with the supervisor liveness.
type SessionCommandView struct {
	// Supervisor is the coordinator liveness at the read.
	Supervisor SupervisorView
	// Command is the stored command.
	Command store.IdeationCommand
}

// SubmitSessionCommand appends one parsed browser command to the inbox. A
// stored request ID returns its stored state and outcome, also while the
// coordinator is stopped. A new command needs a live supervisor heartbeat,
// an existing session (or none, to create one), and the session's current
// revision; otherwise it returns SupervisorUnavailableError,
// store.ErrIdeationSessionNotFound or ErrIdeationSessionExists, or a
// store.IdeationRevisionConflictError, and appends nothing.
func (s *Service) SubmitSessionCommand(ctx context.Context, command store.IdeationCommand) (SessionCommandAck, error) {
	registration, err := s.registration()
	if err != nil {
		return SessionCommandAck{}, err
	}
	if err := validateOperationalPath(registration.Path, registration.OperationalDataPath); err != nil {
		return SessionCommandAck{}, err
	}
	inbox, err := s.deps.OpenCommandInbox(ctx, registration.OperationalDataPath)
	if err != nil {
		return SessionCommandAck{}, fmt.Errorf("open session command inbox: %w", err)
	}
	defer func() { _ = inbox.Close() }()
	if stored, err := inbox.IdeationCommand(ctx, command.RequestID); err == nil {
		if !sameCommand(stored, command) {
			return SessionCommandAck{}, fmt.Errorf("%w: command %q", store.ErrIdeationRequestReused, command.RequestID)
		}
		return SessionCommandAck{Command: stored, Replayed: true}, nil
	} else if !errors.Is(err, store.ErrIdeationCommandNotFound) {
		return SessionCommandAck{}, err
	}
	supervisor, err := s.supervisorLiveness(ctx, inbox)
	if err != nil {
		return SessionCommandAck{}, err
	}
	if !supervisor.Live {
		return SessionCommandAck{}, &SupervisorUnavailableError{Supervisor: supervisor}
	}
	if err := checkCommandTarget(ctx, inbox, command); err != nil {
		return SessionCommandAck{}, err
	}
	command.AcceptedAt = supervisor.ObservedAt
	accepted, replayed, err := inbox.AcceptIdeationCommand(ctx, command)
	if err != nil {
		return SessionCommandAck{}, err
	}
	return SessionCommandAck{Command: accepted, Replayed: replayed}, nil
}

// checkCommandTarget refuses a command for a missing session, a create for an
// existing session, and a command that names a revision that is not current.
func checkCommandTarget(ctx context.Context, inbox CommandInbox, command store.IdeationCommand) error {
	session, err := inbox.IdeationSession(ctx, command.SessionID)
	if sessioncommand.Kind(command.Kind) == sessioncommand.KindCreateSession {
		if err == nil {
			return fmt.Errorf("%w: %q", store.ErrIdeationSessionExists, command.SessionID)
		}
		if errors.Is(err, store.ErrIdeationSessionNotFound) {
			return nil
		}
		return err
	}
	if err != nil {
		return err
	}
	if session.Revision != command.ExpectedRevision {
		return &store.IdeationRevisionConflictError{SessionID: session.ID, Expected: command.ExpectedRevision, Actual: session.Revision}
	}
	return nil
}

// sameCommand reports whether a retried command carries the stored content.
func sameCommand(stored, retried store.IdeationCommand) bool {
	return stored.SessionID == retried.SessionID && stored.Kind == retried.Kind &&
		stored.ExpectedRevision == retried.ExpectedRevision && bytes.Equal(stored.Payload, retried.Payload)
}

// supervisorLiveness judges the persisted heartbeat at the service clock.
func (s *Service) supervisorLiveness(ctx context.Context, reader SupervisorHeartbeatReader) (SupervisorView, error) {
	view := SupervisorView{ObservedAt: s.deps.Now().UTC()}
	heartbeat, err := reader.ReadSupervisorHeartbeat(ctx)
	if err != nil {
		return SupervisorView{}, fmt.Errorf("read supervisor heartbeat: %w", err)
	}
	view.Heartbeat = heartbeat
	view.Live = heartbeat != nil && heartbeat.Live(view.ObservedAt)
	return view, nil
}

// SessionOverview reads every session of the registered repository.
func (s *Service) SessionOverview(ctx context.Context) (SessionOverview, error) {
	return readSessionView(ctx, s, func(read sessionViewRead) (SessionOverview, error) {
		sessions, err := read.reader.IdeationSessions(ctx, read.repositoryPath)
		if err != nil {
			return SessionOverview{}, err
		}
		return SessionOverview{Supervisor: read.supervisor, Sessions: sessions}, nil
	})
}

// SessionActivity reads one session, its history, and its newest command
// outcomes. It returns store.ErrIdeationSessionNotFound for an unknown
// session.
func (s *Service) SessionActivity(ctx context.Context, sessionID string) (SessionActivity, error) {
	return readSessionView(ctx, s, func(read sessionViewRead) (SessionActivity, error) {
		history, err := read.reader.IdeationHistory(ctx, sessionID)
		if err != nil {
			return SessionActivity{}, err
		}
		commands, err := read.reader.IdeationCommands(ctx, sessionID, maxSessionActivityCommands)
		if err != nil {
			return SessionActivity{}, err
		}
		invocations, err := read.reader.SessionInvocations(ctx, sessionID)
		if err != nil {
			return SessionActivity{}, err
		}
		return SessionActivity{Supervisor: read.supervisor, History: history, Commands: commands, Invocations: invocations}, nil
	})
}

// SessionCommand reads one command. It returns store.ErrIdeationCommandNotFound
// for an unknown request ID.
func (s *Service) SessionCommand(ctx context.Context, requestID string) (SessionCommandView, error) {
	return readSessionView(ctx, s, func(read sessionViewRead) (SessionCommandView, error) {
		command, err := read.reader.IdeationCommand(ctx, requestID)
		if err != nil {
			return SessionCommandView{}, err
		}
		return SessionCommandView{Supervisor: read.supervisor, Command: command}, nil
	})
}

// sessionViewRead is what every session read gets: the registered
// repository, the open read-only store, and the supervisor view.
type sessionViewRead struct {
	repositoryPath string
	reader         SessionReadStore
	supervisor     SupervisorView
}

// readSessionView opens the store read-only, reads the supervisor view, and
// passes both to read. It closes the store when read returns.
func readSessionView[T any](ctx context.Context, s *Service, read func(sessionViewRead) (T, error)) (T, error) {
	var none T
	registration, err := s.registration()
	if err != nil {
		return none, err
	}
	if err := validateOperationalPath(registration.Path, registration.OperationalDataPath); err != nil {
		return none, err
	}
	opened, err := s.deps.OpenStoreReadOnly(ctx, registration.OperationalDataPath)
	if err != nil {
		return none, fmt.Errorf("open operational store read-only: %w", err)
	}
	defer func() { _ = opened.Close() }()
	reader, ok := opened.(SessionReadStore)
	if !ok {
		return none, errors.New("operational store does not support session views")
	}
	supervisor, err := s.supervisorView(ctx, reader)
	if err != nil {
		return none, err
	}
	return read(sessionViewRead{repositoryPath: registration.Path, reader: reader, supervisor: supervisor})
}

// sessionCommandStore is what the coordinator's command loop needs from the
// operational store.
type sessionCommandStore interface {
	sessioncommand.Store
	publication.Store
	Close() error
}

// sessionCommandLoop applies inbox commands while the coordinator runs. It is
// the only consumer of the inbox.
type sessionCommandLoop struct {
	cancel   context.CancelFunc
	done     chan struct{}
	failures chan error
}

// startSessionCommands starts the inbox loop beside the run loop. A store
// without session support, as in some test doubles, starts no loop.
func (s *Service) startSessionCommands(ctx context.Context, registration config.RepositoryRegistration) (*sessionCommandLoop, error) {
	opened, err := s.deps.OpenStore(ctx, registration.OperationalDataPath)
	if err != nil {
		return nil, fmt.Errorf("open operational store for session commands: %w", err)
	}
	commandStore, ok := opened.(sessionCommandStore)
	if !ok {
		_ = opened.Close()
		return nil, nil
	}
	dispatcher := sessioncommand.NewDispatcher(s.sessionCommandEnv(commandStore, registration))
	loopContext, cancel := context.WithCancel(ctx)
	loop := &sessionCommandLoop{cancel: cancel, done: make(chan struct{}), failures: make(chan error, 1)}
	go func() {
		defer close(loop.done)
		defer func() { _ = commandStore.Close() }()
		defer func() { _ = dispatcher.Wait() }()
		ticker := time.NewTicker(sessionCommandInterval)
		defer ticker.Stop()
		for {
			// A failed pass leaves its commands pending; the next pass
			// retries them, and the polling loop reports the failure.
			if err := dispatcher.Drain(loopContext); err != nil && loopContext.Err() == nil {
				loop.report(err)
			}
			select {
			case <-loopContext.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return loop, nil
}

// report keeps the newest pass failure for the polling loop without
// blocking the inbox loop.
func (loop *sessionCommandLoop) report(err error) {
	select {
	case loop.failures <- err:
	default:
	}
}

// failure returns a pass failure that the polling loop has not reported yet.
func (loop *sessionCommandLoop) failure() error {
	if loop == nil {
		return nil
	}
	select {
	case err := <-loop.failures:
		return err
	default:
		return nil
	}
}

// stop ends the inbox loop and waits for running commands. A command that
// the stop interrupts stays pending for the next coordinator.
func (loop *sessionCommandLoop) stop() {
	if loop == nil {
		return
	}
	loop.cancel()
	<-loop.done
}

// sessionCommandEnv builds the handler environment. Publication commands
// work only when the configured tracker adapter implements the planning
// port.
func (s *Service) sessionCommandEnv(commandStore sessionCommandStore, registration config.RepositoryRegistration) sessioncommand.Env {
	lifecycle := workflow.DefaultSessionLifecycle()
	env := sessioncommand.Env{
		Store: commandStore, Lifecycle: lifecycle, Roles: sessionRoleStarter{service: s}, ValidatePlan: plangraph.Validate,
		RepositoryPath: registration.Path, Now: s.deps.Now,
	}
	if planner, ok := s.deps.Tracker.(tracker.PlanPublisher); ok {
		env.Publisher = &publication.Publisher{
			Store: commandStore, Tracker: planner, Repository: registeredRepository(registration), Lifecycle: lifecycle, Now: s.deps.Now,
		}
	}
	return env
}

// sessionRoleStarter starts session roles for session commands through the
// coordinator's session-role service.
type sessionRoleStarter struct {
	service *Service
}

// StartIdeation starts the ideation role for the newest user turn.
func (starter sessionRoleStarter) StartIdeation(ctx context.Context, sessionID, turnID string) error {
	return starter.service.WithSessionRoles(ctx, func(roles *SessionRoles) error {
		_, err := roles.StartIdeationTurn(ctx, sessionID, turnID)
		return err
	})
}

// StartPlanning starts the planning role for a hand-off or regeneration.
func (starter sessionRoleStarter) StartPlanning(ctx context.Context, sessionID, requestID string) error {
	return starter.service.WithSessionRoles(ctx, func(roles *SessionRoles) error {
		_, err := roles.StartPlanning(ctx, sessionID, requestID)
		return err
	})
}
