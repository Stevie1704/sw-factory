package sessioncommand

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Stevie1704/sw-factory/internal/publication"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// maxAttempts bounds how often the coordinator starts one command. A command
// that a coordinator started this often without an outcome completes as
// failed, so one bad command cannot stop every later command.
const maxAttempts = 3

// Store is the operational-store seam of the coordinator-side dispatcher.
type Store interface {
	PendingIdeationCommands(context.Context) ([]store.IdeationCommand, error)
	BeginIdeationCommand(context.Context, string) (store.IdeationCommand, error)
	CompleteIdeationCommand(context.Context, store.IdeationCommandResult) (store.IdeationCommand, error)
	IdeationSession(context.Context, string) (store.IdeationSession, error)
	IdeationHistory(context.Context, string) (store.IdeationHistory, error)
	CreateIdeationSession(context.Context, store.IdeationSession) (store.IdeationSession, error)
	AppendIdeationTurn(context.Context, store.IdeationWrite, store.IdeationTurn) (store.IdeationSession, store.IdeationTurn, error)
	SaveBriefRevision(context.Context, store.IdeationWrite, store.Brief) (store.IdeationSession, store.BriefRevision, error)
	SavePlanRevision(context.Context, store.IdeationWrite, store.PlanRevision) (store.IdeationSession, store.PlanRevision, error)
	ApprovePlan(context.Context, store.IdeationWrite, store.PlanApproval) (store.IdeationSession, store.PlanApproval, error)
	TransitionIdeationSession(context.Context, store.IdeationWrite) (store.IdeationSession, error)
}

var _ Store = (*store.Store)(nil)

// Publisher is the plan publication seam. *publication.Publisher implements
// it.
type Publisher interface {
	Preview(context.Context, string) (publication.Binding, error)
	Publish(context.Context, publication.Request) (publication.Result, error)
	Reconcile(context.Context, publication.Reconciliation) (store.PublicationOperation, error)
}

var _ Publisher = (*publication.Publisher)(nil)

// Roles is the session role seam. Session roles (#268) implement it in the
// coordinator.
type Roles interface {
	// StartIdeation starts the ideation role for the newest user turn.
	StartIdeation(ctx context.Context, sessionID, turnID string) error
	// StartPlanning starts the planning role for the current brief; requestID
	// is the hand-off or regeneration request.
	StartPlanning(ctx context.Context, sessionID, requestID string) error
}

// Env is what a handler uses to apply a command. Only the coordinator builds
// it.
type Env struct {
	// Store is the operational store.
	Store Store
	// Lifecycle resolves every session state change.
	Lifecycle workflow.SessionLifecycle
	// Publisher previews and publishes plans. It is nil when the configured
	// tracker adapter has no planning capability.
	Publisher Publisher
	// Roles starts session roles. It is nil when this build cannot start
	// one; hand-off and regeneration then complete as unsupported.
	Roles Roles
	// ValidatePlan checks a user-edited plan graph before it is saved. Nil
	// leaves only the store's integrity check.
	ValidatePlan func([]store.PlanNode) error
	// RepositoryPath is the registered repository that owns new sessions.
	RepositoryPath string
	// Now returns the coordinator time.
	Now func() time.Time
}

// now returns the coordinator time in UTC.
func (env Env) now() time.Time {
	if env.Now == nil {
		return time.Now().UTC()
	}
	return env.Now().UTC()
}

// Outcome is what a handler reports for one command.
type Outcome struct {
	// Code is the final result.
	Code store.IdeationCommandOutcome
	// Detail explains the result.
	Detail string
	// SessionRevision is the session revision after the command.
	SessionRevision int64
	// Result is optional JSON output.
	Result []byte
}

// Handler applies one command kind. It returns an error for a refused or
// conflicting command; the dispatcher turns it into an outcome. A handler
// must be safe to run again for a command whose Attempts is above one,
// because a coordinator can stop after a handler changed the session and
// before the outcome was recorded.
type Handler func(context.Context, Env, store.IdeationCommand) (Outcome, error)

// Dispatcher applies inbox commands in the coordinator. It is the only
// consumer of the inbox.
type Dispatcher struct {
	// Env is passed to every handler.
	Env Env
	// Handlers holds one handler per command kind. A test replaces one to
	// observe dispatch.
	Handlers map[Kind]Handler

	mu      sync.Mutex
	running map[string]bool
	busy    map[string]bool
	work    sync.WaitGroup
}

// NewDispatcher returns a dispatcher with the default handlers.
func NewDispatcher(env Env) *Dispatcher {
	return &Dispatcher{Env: env, Handlers: DefaultHandlers(), running: map[string]bool{}, busy: map[string]bool{}}
}

// callsTracker reports whether kind calls the tracker. Such commands run
// beside the inbox pass, at most one per session, so a slow tracker never
// delays a cancel or another session's command.
func callsTracker(kind Kind) bool {
	spec, ok := lookupKind(kind)
	return ok && spec.tracker
}

// Drain applies every pending command once, in inbox order. Tracker commands
// start in the background; Wait waits for them. A command whose session
// already has tracker work waits for a later pass. Drain returns a store
// error that stopped the pass; the commands stay pending.
func (d *Dispatcher) Drain(ctx context.Context) error {
	pending, err := d.Env.Store.PendingIdeationCommands(ctx)
	if err != nil {
		return fmt.Errorf("read pending session commands: %w", err)
	}
	for _, command := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		background := callsTracker(Kind(command.Kind))
		if !d.claim(command, background) {
			continue
		}
		if !background {
			err := d.apply(ctx, command)
			d.release(command, background)
			if err != nil {
				return err
			}
			continue
		}
		d.work.Add(1)
		go func() {
			defer d.work.Done()
			defer d.release(command, background)
			_ = d.apply(ctx, command)
		}()
	}
	return nil
}

// Wait waits for every background command that Drain started.
func (d *Dispatcher) Wait() {
	d.work.Wait()
}

// claim reserves command for this dispatcher. A running command, or tracker
// work for a session that already has tracker work, is not claimed.
func (d *Dispatcher) claim(command store.IdeationCommand, background bool) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.running[command.RequestID] || (background && d.busy[command.SessionID]) {
		return false
	}
	d.running[command.RequestID] = true
	if background {
		d.busy[command.SessionID] = true
	}
	return true
}

// release ends the reservation of claim.
func (d *Dispatcher) release(command store.IdeationCommand, background bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.running, command.RequestID)
	if background {
		delete(d.busy, command.SessionID)
	}
}

// apply starts command, runs its handler, and records the outcome. When the
// coordinator stops during the handler, or the handler meets a transient
// store failure, the command stays applying and a later pass resumes it.
func (d *Dispatcher) apply(ctx context.Context, command store.IdeationCommand) error {
	started, err := d.Env.Store.BeginIdeationCommand(ctx, command.RequestID)
	if errors.Is(err, store.ErrIdeationCommandSettled) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("start session command %q: %w", command.RequestID, err)
	}
	var outcome Outcome
	if started.Attempts > maxAttempts {
		outcome = Outcome{Code: store.IdeationCommandFailed, Detail: fmt.Sprintf("the coordinator started this command %d times without an outcome; submit it again", maxAttempts)}
	} else {
		outcome, err = d.run(ctx, started)
		if err != nil && ctx.Err() != nil {
			return nil
		}
		if store.IsTransient(err) {
			return fmt.Errorf("apply session command %q: %w", started.RequestID, err)
		}
		if err != nil {
			outcome = d.outcomeOf(context.WithoutCancel(ctx), started, err)
		}
	}
	_, err = d.Env.Store.CompleteIdeationCommand(context.WithoutCancel(ctx), store.IdeationCommandResult{
		RequestID: started.RequestID, Outcome: outcome.Code, Detail: store.TruncateUTF8(outcome.Detail, store.MaxIdeationReasonBytes),
		SessionRevision: outcome.SessionRevision, Result: outcome.Result, At: d.Env.now(),
	})
	if err != nil {
		return fmt.Errorf("record session command %q: %w", started.RequestID, err)
	}
	return nil
}

// run calls the handler of command, or reports a kind without one.
func (d *Dispatcher) run(ctx context.Context, command store.IdeationCommand) (Outcome, error) {
	handler := d.Handlers[Kind(command.Kind)]
	if handler == nil {
		return Outcome{Code: store.IdeationCommandUnsupported, Detail: fmt.Sprintf("this build cannot apply %s commands", command.Kind)}, nil
	}
	return handler(ctx, d.Env, command)
}

// outcomeOf turns a handler error into the final outcome. A revision
// conflict names the current revision; every other error that is not
// transient is a refusal that explains itself.
func (d *Dispatcher) outcomeOf(ctx context.Context, command store.IdeationCommand, err error) Outcome {
	var conflict *store.IdeationRevisionConflictError
	if errors.As(err, &conflict) {
		current := conflict.Actual
		if current < 0 {
			if session, readErr := d.Env.Store.IdeationSession(ctx, command.SessionID); readErr == nil {
				current = session.Revision
			}
		}
		return Outcome{
			Code: store.IdeationCommandConflict, SessionRevision: current,
			Detail: fmt.Sprintf("the session is at revision %d, but the page showed revision %d; reload the session and decide again", current, command.ExpectedRevision),
		}
	}
	outcome := Outcome{Code: store.IdeationCommandRefused, Detail: err.Error()}
	if session, readErr := d.Env.Store.IdeationSession(ctx, command.SessionID); readErr == nil {
		outcome.SessionRevision = session.Revision
	}
	return outcome
}
