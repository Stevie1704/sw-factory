package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	effectkernel "github.com/Stevie1704/sw-factory/internal/effect"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// PendingEffectStore is retained as a factory-local alias while journal users
// migrate to the effect package's store-facing protocol contract.
type PendingEffectStore = effectkernel.PendingEffectStore

// stateTransitionEffectPayload is the serialized intent for one paired issue
// label and status-comment projection.
type stateTransitionEffectPayload struct {
	Repository           github.Repository
	Issue                github.Issue
	Previous             store.Run
	Next                 store.Run
	CreateComment        bool
	StopWorker           bool
	InvalidateResults    bool
	InvalidateAllResults bool
}

// statusCommentEffectPayload is the serialized intent for a command
// watermark and its corresponding status-comment edit.
type statusCommentEffectPayload struct {
	Repository github.Repository
	Previous   store.Run
	Next       store.Run
}

// clarificationCommentEffectPayload is the serialized intent for one
// coordinator-authored clarification comment and its run identity projection.
type clarificationCommentEffectPayload struct {
	Repository github.Repository
	Target     int
	Body       string
	// PacketVersion scopes the comment marker to one clarification round so a
	// replay repairs that round's comment instead of overwriting an earlier one.
	PacketVersion int
}

// labelTransitionEffectPayload is the serialized intent for a standalone
// complete issue-label replacement.
type labelTransitionEffectPayload struct {
	Repository  github.Repository
	IssueNumber int
	Labels      []string
}

// commitStatusEffectPayload is the serialized intent for one exact-SHA status.
type commitStatusEffectPayload struct {
	Repository github.Repository
	Status     github.CommitStatus
}

// checkpointRequestJSON keeps the git adapter request independent from the
// unexported effect implementation while retaining every replay input.
type checkpointRequestJSON struct {
	RunID        string
	WorktreePath string
	ParentSHA    string
	Kind         string
	Paths        []string
	Message      string
}

// pushEffectPayload is the serialized intent for one branch push.
type pushEffectPayload struct {
	Request     pushRequestJSON
	ExpectedSHA string
}

// pushRequestJSON keeps the git adapter request independent from the effect
// journal package boundary.
type pushRequestJSON struct {
	WorktreePath string
	Branch       string
}

// pullRequestEffectPayload is the serialized intent for one draft PR mutation.
type pullRequestEffectPayload struct {
	Repository github.Repository
	Number     int
	Request    github.PullRequestRequest
	PersistRun bool
	Issue      github.Issue
	Previous   store.Run
	Next       store.Run
}

// workerLaunchEffectPayload is the serialized intent for one worker launch or
// reuse. The request type itself is already a portable worker seam.
type workerLaunchEffectPayload struct {
	Request worker.StartRequest
}

// harnessResumeEffectPayload is the durable intent for one native-session
// continuation. The target resume count lets recovery recognize a reservation
// that crossed the harness boundary before the invocation row was finalized.
type harnessResumeEffectPayload struct {
	SocketPath        string
	Request           harness.StartRequest
	Invocation        store.Invocation
	TargetResumeCount int
	// Manual marks an operator-requested resume. It does not consume the
	// automatic recovery ceiling and requires a later explicit attach.
	Manual bool
}

// resultAcceptanceEffectPayload is the complete durable intent for accepting
// a validated visible report.
type resultAcceptanceEffectPayload struct {
	Repository github.Repository
	SocketPath string
	Issue      github.Issue
	Session    harness.Session
	Invocation store.Invocation
	// WorkerID identifies the worker that owns this invocation. Empty legacy
	// payloads intentionally fall back to the run-scoped worker identity.
	WorkerID       string
	Previous       store.Run
	Next           store.Run
	StopWorker     bool
	AcceptedReport string // JSON-encoded report.Report snapshot for deterministic replay
}

// workflowProjectionError marks a deterministic conflict between a replay
// payload and the newer persisted workflow projection. It lets reconciliation
// distinguish workflow ownership from external infrastructure uncertainty.
type workflowProjectionError struct {
	cause error
}

// Error returns the deterministic workflow conflict.
func (e *workflowProjectionError) Error() string {
	if e == nil || e.cause == nil {
		return "deterministic workflow projection conflict"
	}
	return e.cause.Error()
}

// Unwrap exposes the underlying bounded workflow conflict.
func (e *workflowProjectionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// workflowProjectionFailuref creates a typed deterministic replay conflict.
func workflowProjectionFailuref(format string, arguments ...any) error {
	return &workflowProjectionError{cause: fmt.Errorf(format, arguments...)}
}

// withPendingEffect journals one external mutation before running it and
// acknowledges the journal only after the mutation succeeds. When an older
// operational-store adapter has no journal seam, it retains the historical
// direct execution behavior.
func (s *Service) withPendingEffect(ctx context.Context, runStore RunStore, effect store.PendingEffect, action func() error) error {
	return effectkernel.WithPendingEffect(ctx, runStore, effect, pendingEffectApplier{action: action})
}

// pendingEffectApplier adapts a factory-owned effect action to the kernel's
// interface-valued application seam.
type pendingEffectApplier struct {
	action func() error
}

// Apply runs the factory-owned effect action.
func (a pendingEffectApplier) Apply() error {
	return a.action()
}

// newPendingEffect encodes a replay intent and applies the coordinator clock
// so all journal records share the run's operational time source.
func (s *Service) newPendingEffect(runID string, kind store.PendingEffectKind, identity string, payload any) (store.PendingEffect, error) {
	return effectkernel.NewPendingEffect(s.deps.Now().UTC(), runID, kind, identity, payload)
}

// decodePendingEffect decodes one bounded replay payload and reports malformed
// journal data as an infrastructure discrepancy rather than a workflow result.
func decodePendingEffect(effect store.PendingEffect, destination any) error {
	return effectkernel.DecodePendingEffect(effect, destination)
}

// commitStatusPublisher wraps the gate status seam with durable idempotency.
type commitStatusPublisher struct {
	service  *Service
	runStore RunStore
	runID    string
	delegate github.CommitStatusPublisher
}

// CreateCommitStatus publishes or recognizes one exact-SHA status without
// creating a second semantic status after a process interruption.
func (p commitStatusPublisher) CreateCommitStatus(ctx context.Context, repository github.Repository, status github.CommitStatus) error {
	payload := commitStatusEffectPayload{Repository: repository, Status: status}
	effect, err := p.service.newPendingEffect(p.runID, store.PendingEffectKindCommitStatus, status.SHA+"\x00"+status.Context+"\x00"+string(status.State)+"\x00"+status.Description, payload)
	if err != nil {
		return err
	}
	return p.service.withPendingEffect(ctx, p.runStore, effect, func() error {
		if reader, ok := p.delegate.(github.CommitStatusReader); ok {
			statuses, err := reader.ListCommitStatuses(ctx, repository, status.SHA)
			if err != nil {
				return fmt.Errorf("inspect existing commit status: %w", err)
			}
			for _, existing := range statuses {
				if sameCommitStatus(existing, status) {
					return nil
				}
			}
		}
		return p.delegate.CreateCommitStatus(ctx, repository, status)
	})
}

// sameCommitStatus compares every semantic field that makes a status replay
// safe; GitHub may assign its own numeric status identity.
func sameCommitStatus(left, right github.CommitStatus) bool {
	return left.SHA == right.SHA && left.State == right.State && left.Context == right.Context && left.Description == right.Description && left.TargetURL == right.TargetURL
}

// applyStateTransitionEffects makes the two GitHub projections converge on a
// persisted run revision. Reads before each mutation recognize an effect that
// completed just before a process interruption.
func (s *Service) applyStateTransitionEffects(ctx context.Context, next *store.Run, transition stateTransition) error {
	if next == nil {
		return errors.New("state transition run is required")
	}
	issue := transition.Issue
	if s.deps.GitHub == nil {
		return errors.New("GitHub client is required for state transition")
	}
	observed, err := s.deps.GitHub.Issue(ctx, transition.Repository, next.IssueNumber)
	if err != nil {
		return fmt.Errorf("read issue before state transition: %w", err)
	}
	if observed.Number != 0 {
		issue = observed
	}
	desiredLabels := replaceFactoryState(issue.Labels, factoryLabelForStatus(next.Status))
	if !sameStringSlice(issue.Labels, desiredLabels) {
		if err := s.deps.GitHub.ReplaceIssueLabels(ctx, transition.Repository, next.IssueNumber, desiredLabels); err != nil {
			return fmt.Errorf("set issue #%d state: %w", next.IssueNumber, err)
		}
	}

	marker := statusCommentMarker(next.ID)
	comment, err := s.deps.GitHub.FindStatusComment(ctx, transition.Repository, next.IssueNumber, marker)
	if err != nil {
		return fmt.Errorf("find status comment during state transition: %w", err)
	}
	if transition.CreateComment {
		if strings.TrimSpace(comment.ID) != "" {
			next.StatusCommentID = comment.ID
			if comment.Body != statusCommentBody(*next) {
				if err := s.deps.GitHub.EditIssueComment(ctx, transition.Repository, comment.ID, statusCommentBody(*next)); err != nil {
					return fmt.Errorf("repair existing status comment: %w", err)
				}
			}
			return nil
		}
		created, err := s.deps.GitHub.CreateIssueComment(ctx, transition.Repository, next.IssueNumber, statusCommentBody(*next))
		if err != nil {
			return fmt.Errorf("create status comment: %w", err)
		}
		if strings.TrimSpace(created.ID) == "" {
			return errors.New("create status comment returned an empty comment id")
		}
		next.StatusCommentID = created.ID
		return nil
	}
	if strings.TrimSpace(next.StatusCommentID) == "" {
		if strings.TrimSpace(comment.ID) == "" {
			return errors.New("active run has no recoverable status comment")
		}
		next.StatusCommentID = comment.ID
	}
	if comment.ID == next.StatusCommentID && comment.Body == statusCommentBody(*next) {
		return nil
	}
	if err := s.deps.GitHub.EditIssueComment(ctx, transition.Repository, next.StatusCommentID, statusCommentBody(*next)); err != nil {
		return fmt.Errorf("edit status comment: %w", err)
	}
	return nil
}

// applyStatusCommentEffect observes the marker-owned status comment before
// editing it. A matching body is already complete, so a replay after an
// ambiguous GitHub response performs no second mutation.
func (s *Service) applyStatusCommentEffect(ctx context.Context, payload statusCommentEffectPayload) error {
	if s.deps.GitHub == nil {
		return errors.New("GitHub client is required for status comment effect")
	}
	if strings.TrimSpace(payload.Next.StatusCommentID) == "" {
		return errors.New("status comment effect has no comment identity")
	}
	comment, err := s.deps.GitHub.FindStatusComment(ctx, payload.Repository, payload.Next.IssueNumber, statusCommentMarker(payload.Next.ID))
	if err != nil {
		return fmt.Errorf("find status comment for replay: %w", err)
	}
	body := statusCommentBody(payload.Next)
	if comment.ID == payload.Next.StatusCommentID && comment.Body == body {
		return nil
	}
	if comment.ID != "" && comment.ID != payload.Next.StatusCommentID {
		if comment.Body == body {
			return nil
		}
		return fmt.Errorf("status comment identity changed from %q to %q", payload.Next.StatusCommentID, comment.ID)
	}
	if err := s.deps.GitHub.EditIssueComment(ctx, payload.Repository, payload.Next.StatusCommentID, body); err != nil {
		return fmt.Errorf("edit status comment for replay: %w", err)
	}
	return nil
}

// sameStringSlice compares complete ordered projections for idempotent PUT
// suppression while preserving ordinary labels exactly as returned by GitHub.
func sameStringSlice(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// replayPendingEffect completes a journaled effect whose process boundary was
// crossed before the journal could be cleared. The protocol kernel routes the
// durable effect to the factory-owned handler for its kind.
func (s *Service) replayPendingEffect(ctx context.Context, runStore RunStore, effect store.PendingEffect) (store.Run, error) {
	return newPendingEffectDispatcher(s).Replay(ctx, runStore, effect)
}

// replayPendingClarificationComment completes a question publication after a
// response-loss boundary by finding the marker before creating anything.
func (s *Service) replayPendingClarificationComment(ctx context.Context, runStore RunStore, effect store.PendingEffect) (store.Run, error) {
	var payload clarificationCommentEffectPayload
	if err := decodePendingEffect(effect, &payload); err != nil {
		return store.Run{}, err
	}
	current, err := readReconciliationRun(ctx, runStore)
	if err != nil {
		return store.Run{}, fmt.Errorf("read run during clarification replay: %w", err)
	}
	if current == nil {
		return store.Run{}, fmt.Errorf("run %q disappeared during clarification replay", effect.RunID)
	}
	comment, err := s.findOrCreateClarificationComment(ctx, payload.Repository, payload.Target, effect.RunID, payload.PacketVersion, payload.Body)
	if err != nil {
		return store.Run{}, fmt.Errorf("replay clarification comment: %w", err)
	}
	current.ClarificationCommentID = comment.ID
	current.UpdatedAt = s.deps.Now().UTC()
	if err := saveRunWithRetry(ctx, runStore, *current); err != nil {
		return store.Run{}, fmt.Errorf("persist replayed clarification comment identity: %w", err)
	}
	journal, ok := runStore.(PendingEffectStore)
	if !ok {
		return *current, nil
	}
	if err := journal.ClearPendingEffect(ctx, effect.RunID, effect.ID); err != nil {
		return store.Run{}, fmt.Errorf("clear replayed clarification comment: %w", err)
	}
	return *current, nil
}

// persistCommandProjectionWithEffect journals a command's local watermark
// together with its status-comment mutation. The local projection is part of
// the action so a process stop between the two writes leaves a replayable
// intent instead of an apparently processed but stale command.
func (s *Service) persistCommandProjectionWithEffect(ctx context.Context, runStore RunStore, repository github.Repository, previous, next store.Run) (store.Run, error) {
	payload := statusCommentEffectPayload{Repository: repository, Previous: previous, Next: next}
	effect, err := s.newPendingEffect(next.ID, store.PendingEffectKindStatusComment, fmt.Sprintf("revision=%d\x00comment=%s", next.Revision, next.ProcessedCommentID), payload)
	if err != nil {
		return next, err
	}
	action := func() error {
		current, err := readReconciliationRun(ctx, runStore)
		if err != nil {
			return fmt.Errorf("read run before command projection: %w", err)
		}
		if current == nil {
			return fmt.Errorf("run %q disappeared before command projection", next.ID)
		}
		switch {
		case current.Revision < next.Revision:
			if current.Revision != previous.Revision {
				return fmt.Errorf("command projection expected revision %d, found %d", previous.Revision, current.Revision)
			}
			if err := saveCommandRun(ctx, runStore, previous.Revision, next); err != nil {
				return fmt.Errorf("persist command watermark: %w", err)
			}
		case current.Revision == next.Revision:
			if current.ProcessedCommentID != next.ProcessedCommentID || current.LastCommandName != next.LastCommandName {
				return fmt.Errorf("command projection revision %d belongs to another command", current.Revision)
			}
		default:
			if current.ProcessedCommentID != next.ProcessedCommentID {
				return fmt.Errorf("command projection was superseded at revision %d", current.Revision)
			}
		}
		return s.applyStatusCommentEffect(ctx, payload)
	}
	if err := s.withPendingEffect(ctx, runStore, effect, action); err != nil {
		return next, err
	}
	return next, nil
}

// replayPendingStatusComment finishes a command projection after a process
// boundary. It persists the watermark only when it is still missing, then
// recognizes or applies the exact status-comment body before clearing intent.
func (s *Service) replayPendingStatusComment(ctx context.Context, runStore RunStore, effect store.PendingEffect) (store.Run, error) {
	var payload statusCommentEffectPayload
	if err := decodePendingEffect(effect, &payload); err != nil {
		return store.Run{}, err
	}
	current, err := readReconciliationRun(ctx, runStore)
	if err != nil {
		return store.Run{}, fmt.Errorf("read run during status-comment replay: %w", err)
	}
	if current == nil {
		return store.Run{}, fmt.Errorf("run %q disappeared during status-comment replay", effect.RunID)
	}
	next := payload.Next
	if current.Revision < next.Revision {
		if current.Revision != payload.Previous.Revision {
			return store.Run{}, workflowProjectionFailuref("status-comment replay expected revision %d, found %d", payload.Previous.Revision, current.Revision)
		}
		if err := saveCommandRun(ctx, runStore, payload.Previous.Revision, next); err != nil {
			return store.Run{}, fmt.Errorf("persist replayed command watermark: %w", err)
		}
	} else if current.Revision == next.Revision {
		if current.ProcessedCommentID != next.ProcessedCommentID || current.LastCommandName != next.LastCommandName {
			return store.Run{}, workflowProjectionFailuref("status-comment replay revision %d belongs to another command", current.Revision)
		}
	} else if current.ProcessedCommentID != next.ProcessedCommentID {
		return store.Run{}, workflowProjectionFailuref("status-comment replay was superseded at revision %d", current.Revision)
	}
	if err := s.applyStatusCommentEffect(ctx, payload); err != nil {
		return store.Run{}, fmt.Errorf("replay status comment: %w", err)
	}
	if journal, ok := runStore.(PendingEffectStore); ok {
		if err := journal.ClearPendingEffect(ctx, effect.RunID, effect.ID); err != nil {
			return store.Run{}, fmt.Errorf("clear replayed status comment: %w", err)
		}
	}
	if current.Revision < next.Revision {
		return next, nil
	}
	return *current, nil
}

// applyLabelTransitionEffect observes the complete factory-owned label set
// before replacing it, making a successful mutation reported as an error safe
// to recognize on replay.
func (s *Service) applyLabelTransitionEffect(ctx context.Context, payload labelTransitionEffectPayload) error {
	if s.deps.GitHub == nil {
		return errors.New("GitHub client is required for label effect")
	}
	issue, err := s.deps.GitHub.Issue(ctx, payload.Repository, payload.IssueNumber)
	if err != nil {
		return fmt.Errorf("read issue for label replay: %w", err)
	}
	if sameStringSlice(issue.Labels, payload.Labels) {
		return nil
	}
	if err := s.deps.GitHub.ReplaceIssueLabels(ctx, payload.Repository, payload.IssueNumber, append([]string(nil), payload.Labels...)); err != nil {
		return fmt.Errorf("replace labels for replay: %w", err)
	}
	return nil
}

// replayPendingLabelTransition completes a standalone label reservation and
// leaves workflow state untouched.
func (s *Service) replayPendingLabelTransition(ctx context.Context, runStore RunStore, effect store.PendingEffect) (store.Run, error) {
	var payload labelTransitionEffectPayload
	if err := decodePendingEffect(effect, &payload); err != nil {
		return store.Run{}, err
	}
	if err := s.applyLabelTransitionEffect(ctx, payload); err != nil {
		return store.Run{}, err
	}
	journal, ok := runStore.(PendingEffectStore)
	if !ok {
		return store.Run{}, errors.New("operational store does not support label replay")
	}
	if err := journal.ClearPendingEffect(ctx, effect.RunID, effect.ID); err != nil {
		return store.Run{}, fmt.Errorf("clear replayed labels: %w", err)
	}
	run, err := readReconciliationRun(ctx, runStore)
	if err != nil {
		return store.Run{}, fmt.Errorf("read run after label replay: %w", err)
	}
	if run == nil {
		return store.Run{}, fmt.Errorf("run %q disappeared during label replay", effect.RunID)
	}
	return *run, nil
}

// startWorkerWithEffect reserves a worker launch before crossing into Docker.
// DockerRuntime makes Start itself idempotent for an exact run/image/mount
// identity, so replaying a completed launch is safe.
func (s *Service) startWorkerWithEffect(ctx context.Context, runStore RunStore, request worker.StartRequest) error {
	payload := workerLaunchEffectPayload{Request: request}
	effect, err := s.newPendingEffect(request.RunID, store.PendingEffectKindWorkerLaunch, request.Role+"\x00"+request.InvocationPath+"\x00"+request.ResultPath, payload)
	if err != nil {
		return err
	}
	return s.withPendingEffect(ctx, runStore, effect, func() error {
		if s.deps.Worker == nil {
			return errors.New("worker runtime is required")
		}
		return s.deps.Worker.Start(ctx, request)
	})
}

// replayPendingWorkerLaunch completes a worker reservation from its portable
// start request and leaves the durable run projection unchanged.
func (s *Service) replayPendingWorkerLaunch(ctx context.Context, runStore RunStore, effect store.PendingEffect) (store.Run, error) {
	var payload workerLaunchEffectPayload
	if err := decodePendingEffect(effect, &payload); err != nil {
		return store.Run{}, err
	}
	if s.deps.Worker == nil {
		return store.Run{}, errors.New("worker runtime is required to replay worker launch")
	}
	if err := s.deps.Worker.Start(ctx, payload.Request); err != nil {
		return store.Run{}, fmt.Errorf("replay worker launch: %w", err)
	}
	if journal, ok := runStore.(PendingEffectStore); ok {
		if err := journal.ClearPendingEffect(ctx, effect.RunID, effect.ID); err != nil {
			return store.Run{}, fmt.Errorf("clear replayed worker launch: %w", err)
		}
	}
	run, err := readReconciliationRun(ctx, runStore)
	if err != nil {
		return store.Run{}, fmt.Errorf("read run after worker launch replay: %w", err)
	}
	if run == nil {
		return store.Run{}, fmt.Errorf("run %q disappeared during worker launch replay", effect.RunID)
	}
	return *run, nil
}

// resumeHarnessWithEffect journals a native-session continuation before the
// harness command crosses into a visible terminal. The invocation counter is
// reserved before the native command, so an ambiguous post-launch failure is
// never replayed as a second visible session.
func (s *Service) resumeHarnessWithEffect(ctx context.Context, runStore RunStore, invocationStore InvocationStore, socketPath string, runtime harness.Runtime, invocation store.Invocation, request harness.StartRequest) (store.Invocation, error) {
	if runtime == nil {
		return invocation, errors.New("harness runtime is required for native resume")
	}
	targetCount := invocation.RecoveryResumeCount + 1
	reserved := invocation
	reserved.RecoveryResumeCount = targetCount
	reserved.UpdatedAt = s.deps.Now().UTC()
	if _, journaled := runStore.(PendingEffectStore); !journaled {
		// Keep the compatibility behavior of older embedding stores: they do
		// not expose a restart journal, so their historical resume boundary is
		// the native command followed by invocation persistence.
		session, err := runtime.Resume(ctx, request)
		if err != nil {
			return invocation, fmt.Errorf("resume native harness session: %w", classifyHarnessRuntimeError(runtime, err))
		}
		updated := reserved
		if session.NativeSessionID != "" {
			updated.NativeSessionID = session.NativeSessionID
		}
		updated.UpdatedAt = s.deps.Now().UTC()
		if err := invocationStore.SaveInvocation(ctx, updated); err != nil {
			return updated, fmt.Errorf("persist resumed native session: %w", err)
		}
		return updated, nil
	}
	payload := harnessResumeEffectPayload{
		SocketPath: socketPath, Request: request, Invocation: invocation,
		TargetResumeCount: targetCount,
	}
	effect, err := s.newPendingEffect(invocation.RunID, store.PendingEffectKindHarnessResume, invocation.ID+"\x00"+fmt.Sprint(targetCount), payload)
	if err != nil {
		return invocation, err
	}
	updated := invocation
	var waitingFailure error
	apply := func() error {
		if err := invocationStore.SaveInvocation(ctx, reserved); err != nil {
			return fmt.Errorf("reserve native session resume: %w", err)
		}
		updated = reserved
		session, err := runtime.Resume(ctx, request)
		if err != nil {
			classified := classifyHarnessRuntimeError(runtime, err)
			if harness.IsRateLimited(classified) || harness.IsAuthenticationExpired(classified) {
				// Capacity and expired-auth failures happen before a native
				// continuation boundary. Roll the reservation back and clear the
				// effect so the same invocation remains retryable.
				if saveErr := invocationStore.SaveInvocation(ctx, invocation); saveErr != nil {
					return fmt.Errorf("rollback native session resume after %s: %w", classified, saveErr)
				}
				updated = invocation
				waitingFailure = classified
				return nil
			}
			return fmt.Errorf("resume native harness session: %w", classified)
		}
		if session.NativeSessionID != "" {
			updated.NativeSessionID = session.NativeSessionID
		}
		updated.UpdatedAt = s.deps.Now().UTC()
		if err := invocationStore.SaveInvocation(ctx, updated); err != nil {
			return fmt.Errorf("persist resumed native session: %w", err)
		}
		return nil
	}
	if err := s.withPendingEffect(ctx, runStore, effect, apply); err != nil {
		// A journaled resume is never rolled back after reservation is
		// attempted. Returning the reserved projection makes callers retain
		// the one-resume ceiling even when the journal or native adapter fails.
		return reserved, err
	}
	if waitingFailure != nil {
		return invocation, waitingFailure
	}
	return updated, nil
}

// resumeHarnessManuallyWithEffect performs an explicit operator resume. The
// native command is journaled, but the automatic recovery counter is left
// unchanged because manual intervention is outside that bounded policy.
func (s *Service) resumeHarnessManuallyWithEffect(ctx context.Context, runStore RunStore, invocationStore InvocationStore, socketPath string, runtime harness.Runtime, invocation store.Invocation, request harness.StartRequest) (store.Invocation, error) {
	if runtime == nil {
		return invocation, errors.New("harness runtime is required for manual native resume")
	}
	if _, journaled := runStore.(PendingEffectStore); !journaled {
		return s.resumeHarnessManually(ctx, invocationStore, runtime, invocation, request)
	}
	payload := harnessResumeEffectPayload{
		SocketPath: socketPath, Request: request, Invocation: invocation,
		TargetResumeCount: invocation.RecoveryResumeCount, Manual: true,
	}
	effect, err := s.newPendingEffect(invocation.RunID, store.PendingEffectKindHarnessResume, invocation.ID+"\x00manual", payload)
	if err != nil {
		return invocation, err
	}
	updated := invocation
	reserved := invocation
	reserved.AttachRequired = true
	reserved.UpdatedAt = s.deps.Now().UTC()
	var waitingFailure error
	apply := func() error {
		if err := invocationStore.SaveInvocation(ctx, reserved); err != nil {
			return fmt.Errorf("reserve manual native session resume: %w", err)
		}
		updated = reserved
		session, resumeErr := runtime.Resume(ctx, request)
		if resumeErr != nil {
			classified := classifyHarnessRuntimeError(runtime, resumeErr)
			if harness.IsRateLimited(classified) || harness.IsAuthenticationExpired(classified) {
				// A capacity or credential rejection happens before the
				// operator can attach a resumed session. Restore the original
				// invocation so the explicit command remains retryable.
				if saveErr := invocationStore.SaveInvocation(ctx, invocation); saveErr != nil {
					return fmt.Errorf("rollback manual native session resume after %s: %w", classified, saveErr)
				}
				updated = invocation
				waitingFailure = classified
				return nil
			}
			return fmt.Errorf("resume native harness session manually: %w", classified)
		}
		if session.NativeSessionID != "" {
			updated.NativeSessionID = session.NativeSessionID
		}
		updated.UpdatedAt = s.deps.Now().UTC()
		if err := invocationStore.SaveInvocation(ctx, updated); err != nil {
			return fmt.Errorf("persist manually resumed native session: %w", err)
		}
		return nil
	}
	if err := s.withPendingEffect(ctx, runStore, effect, apply); err != nil {
		return updated, err
	}
	if waitingFailure != nil {
		return invocation, waitingFailure
	}
	return updated, nil
}

// resumeHarnessManually executes the compatibility path for stores without a
// pending-effect journal.
func (s *Service) resumeHarnessManually(ctx context.Context, invocationStore InvocationStore, runtime harness.Runtime, invocation store.Invocation, request harness.StartRequest) (store.Invocation, error) {
	session, err := runtime.Resume(ctx, request)
	if err != nil {
		return invocation, fmt.Errorf("resume native harness session manually: %w", classifyHarnessRuntimeError(runtime, err))
	}
	updated := invocation
	updated.AttachRequired = true
	if session.NativeSessionID != "" {
		updated.NativeSessionID = session.NativeSessionID
	}
	updated.UpdatedAt = s.deps.Now().UTC()
	if err := invocationStore.SaveInvocation(ctx, updated); err != nil {
		return updated, fmt.Errorf("persist manually resumed native session: %w", err)
	}
	return updated, nil
}

// classifyHarnessRuntimeError turns adapter status text into a redacted typed
// failure while preserving unrelated infrastructure errors.
func classifyHarnessRuntimeError(runtime harness.Runtime, err error) error {
	name := ""
	if runtime != nil {
		name = runtime.Capabilities().Name
	}
	return harness.ClassifyError(err, name)
}

// replayPendingHarnessResume completes a native-resume reservation. A durable
// reservation is treated as an already-crossed boundary; a missing
// reservation is recorded before replay so a second restart cannot launch a
// duplicate session after an ambiguous native command.
func (s *Service) replayPendingHarnessResume(ctx context.Context, runStore RunStore, effect store.PendingEffect) (store.Run, error) {
	var payload harnessResumeEffectPayload
	if err := decodePendingEffect(effect, &payload); err != nil {
		return store.Run{}, err
	}
	invocationStore, ok := runStore.(InvocationStore)
	if !ok {
		return store.Run{}, errors.New("operational store does not support harness resume replay")
	}
	invocation, err := invocationStore.Invocation(ctx, effect.RunID, payload.Invocation.ID)
	if err != nil {
		return store.Run{}, fmt.Errorf("read invocation during harness resume replay: %w", err)
	}
	if invocation == nil {
		return store.Run{}, fmt.Errorf("invocation %q disappeared during harness resume replay", payload.Invocation.ID)
	}
	if payload.Manual {
		if !invocation.AttachRequired {
			reserved := *invocation
			reserved.AttachRequired = true
			reserved.UpdatedAt = s.deps.Now().UTC()
			if err := invocationStore.SaveInvocation(ctx, reserved); err != nil {
				return store.Run{}, fmt.Errorf("reserve replayed manual native session resume: %w", err)
			}
			_, harnessRuntime, runtimeErr := s.ensureAgentRuntime(payload.SocketPath, config.Harness(payload.Invocation.Harness))
			if runtimeErr != nil {
				return store.Run{}, fmt.Errorf("ensure harness for manual native resume replay: %w", runtimeErr)
			}
			session, resumeErr := harnessRuntime.Resume(ctx, payload.Request)
			if resumeErr != nil {
				classified := classifyHarnessRuntimeError(harnessRuntime, resumeErr)
				if harness.IsRateLimited(classified) || harness.IsAuthenticationExpired(classified) {
					return store.Run{}, rollbackPendingHarnessResume(ctx, runStore, effect, *invocation, classified, "manual")
				}
				return store.Run{}, fmt.Errorf("resume native harness session during manual replay: %w", classified)
			}
			if session.NativeSessionID != "" {
				reserved.NativeSessionID = session.NativeSessionID
			}
			reserved.UpdatedAt = s.deps.Now().UTC()
			if err := invocationStore.SaveInvocation(ctx, reserved); err != nil {
				return store.Run{}, fmt.Errorf("persist replayed manual native session: %w", err)
			}
		}
	} else if invocation.RecoveryResumeCount < payload.TargetResumeCount {
		reserved := *invocation
		reserved.RecoveryResumeCount = payload.TargetResumeCount
		reserved.UpdatedAt = s.deps.Now().UTC()
		if err := invocationStore.SaveInvocation(ctx, reserved); err != nil {
			return store.Run{}, fmt.Errorf("reserve replayed native session resume: %w", err)
		}
		_, harnessRuntime, runtimeErr := s.ensureAgentRuntime(payload.SocketPath, config.Harness(payload.Invocation.Harness))
		if runtimeErr != nil {
			return store.Run{}, fmt.Errorf("ensure harness for native resume replay: %w", runtimeErr)
		}
		session, resumeErr := harnessRuntime.Resume(ctx, payload.Request)
		if resumeErr != nil {
			classified := classifyHarnessRuntimeError(harnessRuntime, resumeErr)
			if harness.IsRateLimited(classified) || harness.IsAuthenticationExpired(classified) {
				return store.Run{}, rollbackPendingHarnessResume(ctx, runStore, effect, *invocation, classified, "automatic")
			}
			return store.Run{}, fmt.Errorf("resume native harness session during replay: %w", classified)
		}
		if session.NativeSessionID != "" {
			reserved.NativeSessionID = session.NativeSessionID
		}
		reserved.UpdatedAt = s.deps.Now().UTC()
		if err := invocationStore.SaveInvocation(ctx, reserved); err != nil {
			return store.Run{}, fmt.Errorf("persist replayed native session: %w", err)
		}
	}
	if journal, ok := runStore.(PendingEffectStore); ok {
		if err := journal.ClearPendingEffect(ctx, effect.RunID, effect.ID); err != nil {
			return store.Run{}, fmt.Errorf("clear replayed harness resume: %w", err)
		}
	}
	run, err := readReconciliationRun(ctx, runStore)
	if err != nil {
		return store.Run{}, fmt.Errorf("read run after harness resume replay: %w", err)
	}
	if run == nil {
		return store.Run{}, fmt.Errorf("run %q disappeared during harness resume replay", effect.RunID)
	}
	return *run, nil
}

// rollbackPendingHarnessResume restores the invocation and clears a replay
// reservation when the adapter rejects the native resume before its boundary.
// Rate and authentication failures are retryable classifications, so replay
// must leave the automatic ceiling untouched and let the coordinator publish
// the appropriate waiting state.
func rollbackPendingHarnessResume(ctx context.Context, runStore RunStore, effect store.PendingEffect, original store.Invocation, classified error, mode string) error {
	invocationStore, ok := runStore.(InvocationStore)
	if !ok {
		return errors.Join(classified, fmt.Errorf("rollback %s harness resume: invocation store unavailable", mode))
	}
	if err := invocationStore.SaveInvocation(ctx, original); err != nil {
		return errors.Join(classified, fmt.Errorf("rollback %s harness resume: %w", mode, err))
	}
	journal, ok := runStore.(PendingEffectStore)
	if !ok {
		return classified
	}
	if err := journal.ClearPendingEffect(ctx, effect.RunID, effect.ID); err != nil {
		return errors.Join(classified, fmt.Errorf("clear %s harness resume after rollback: %w", mode, err))
	}
	return classified
}

// pushWithEffect publishes one run branch while recognizing an already
// matching remote head. A repeated Git push is transport-safe, but the remote
// head read makes the semantic effect exactly-once at the coordinator seam.
func (s *Service) pushWithEffect(ctx context.Context, runStore RunStore, runID string, workspace gitadapter.GitWorkspace, request gitadapter.PushRequest, expectedSHA string) error {
	payload := pushEffectPayload{
		Request:     pushRequestJSON{WorktreePath: request.WorktreePath, Branch: request.Branch},
		ExpectedSHA: expectedSHA,
	}
	effect, err := s.newPendingEffect(runID, store.PendingEffectKindPush, request.WorktreePath+"\x00"+request.Branch+"\x00"+expectedSHA, payload)
	if err != nil {
		return err
	}
	return s.withPendingEffect(ctx, runStore, effect, func() error {
		return s.pushOnce(ctx, workspace, request, expectedSHA)
	})
}

// pushOnce observes the remote branch when the adapter supports it and only
// invokes the mutating push when the expected checkpoint is not already there.
func (s *Service) pushOnce(ctx context.Context, workspace gitadapter.GitWorkspace, request gitadapter.PushRequest, expectedSHA string) error {
	if workspace == nil {
		return errors.New("GitWorkspace is required for push")
	}
	if inspector, ok := workspace.(gitadapter.RemoteBranchInspector); ok {
		head, err := inspector.RemoteBranchHead(ctx, request)
		if err != nil {
			return fmt.Errorf("inspect remote branch before push: %w", err)
		}
		if strings.TrimSpace(expectedSHA) != "" && head == expectedSHA {
			return nil
		}
	}
	state, err := workspace.Inspect(ctx, request.WorktreePath)
	if err != nil {
		return fmt.Errorf("inspect local worktree before push: %w", err)
	}
	if strings.TrimSpace(expectedSHA) != "" && state.HeadSHA != expectedSHA {
		return fmt.Errorf("local worktree HEAD %q does not match pending push checkpoint %q", state.HeadSHA, expectedSHA)
	}
	if err := workspace.Push(ctx, request); err != nil {
		return err
	}
	return nil
}

// replayPendingPush completes or recognizes a branch push recorded before a
// process interruption.
func (s *Service) replayPendingPush(ctx context.Context, runStore RunStore, effect store.PendingEffect) (store.Run, error) {
	var payload pushEffectPayload
	if err := decodePendingEffect(effect, &payload); err != nil {
		return store.Run{}, err
	}
	workspace := s.gitWorkspace()
	if workspace == nil {
		return store.Run{}, errors.New("GitWorkspace is required to replay push")
	}
	request := gitadapter.PushRequest{WorktreePath: payload.Request.WorktreePath, Branch: payload.Request.Branch}
	if err := s.pushOnce(ctx, workspace, request, payload.ExpectedSHA); err != nil {
		return store.Run{}, fmt.Errorf("replay push: %w", err)
	}
	if journal, ok := runStore.(PendingEffectStore); ok {
		if err := journal.ClearPendingEffect(ctx, effect.RunID, effect.ID); err != nil {
			return store.Run{}, fmt.Errorf("clear replayed push: %w", err)
		}
	}
	run, err := readReconciliationRun(ctx, runStore)
	if err != nil {
		return store.Run{}, fmt.Errorf("read run after push replay: %w", err)
	}
	if run == nil {
		return store.Run{}, fmt.Errorf("run %q disappeared during push replay", effect.RunID)
	}
	return *run, nil
}

// replayPendingCommitStatus recognizes or republishes one exact-SHA status and
// then clears its durable reservation. A reader is required during recovery so
// an adapter cannot blindly create a duplicate after an ambiguous response.
func (s *Service) replayPendingCommitStatus(ctx context.Context, runStore RunStore, effect store.PendingEffect) (store.Run, error) {
	var payload commitStatusEffectPayload
	if err := decodePendingEffect(effect, &payload); err != nil {
		return store.Run{}, err
	}
	if s.deps.CommitStatuses == nil {
		return store.Run{}, errors.New("commit-status publisher is required to replay status")
	}
	reader, ok := s.deps.CommitStatuses.(github.CommitStatusReader)
	if !ok {
		return store.Run{}, errors.New("commit-status reader is required to replay status safely")
	}
	run, err := readReconciliationRun(ctx, runStore)
	if err != nil {
		return store.Run{}, fmt.Errorf("read run during commit status replay: %w", err)
	}
	if run == nil {
		return store.Run{}, fmt.Errorf("run %q disappeared during commit status replay", effect.RunID)
	}
	if err := s.publishStatusCheckpoint(ctx, *run, payload.Status.SHA); err != nil {
		return store.Run{}, err
	}
	statuses, err := reader.ListCommitStatuses(ctx, payload.Repository, payload.Status.SHA)
	if err != nil {
		return store.Run{}, fmt.Errorf("inspect commit status during replay: %w", err)
	}
	found := false
	for _, status := range statuses {
		if sameCommitStatus(status, payload.Status) {
			found = true
			break
		}
	}
	if !found {
		if err := s.deps.CommitStatuses.CreateCommitStatus(ctx, payload.Repository, payload.Status); err != nil {
			return store.Run{}, fmt.Errorf("replay commit status: %w", err)
		}
	}
	if journal, ok := runStore.(PendingEffectStore); ok {
		if err := journal.ClearPendingEffect(ctx, effect.RunID, effect.ID); err != nil {
			return store.Run{}, fmt.Errorf("clear replayed commit status: %w", err)
		}
	}
	current, err := readReconciliationRun(ctx, runStore)
	if err != nil {
		return store.Run{}, fmt.Errorf("read run after commit status replay: %w", err)
	}
	if current == nil {
		return store.Run{}, fmt.Errorf("run %q disappeared during commit status replay", effect.RunID)
	}
	return *current, nil
}

// publishStatusCheckpoint makes the remote resolve the commit a journaled
// status names before the status is inspected or republished. An interrupted
// attempt can journal a status for a checkpoint that never reached GitHub, and
// GitHub answers every later attempt with an unknown-SHA rejection, so the
// reservation could otherwise never be completed. Only the run's own
// checkpoint is published this way, and the push observes the remote head
// first, so it adds no unobserved second mutation.
func (s *Service) publishStatusCheckpoint(ctx context.Context, run store.Run, sha string) error {
	workspace := s.gitWorkspace()
	if workspace == nil || sha == "" || sha != run.CheckpointSHA || strings.TrimSpace(run.Branch) == "" {
		return nil
	}
	request := gitadapter.PushRequest{WorktreePath: run.Worktree, Branch: run.Branch}
	if err := s.pushOnce(ctx, workspace, request, sha); err != nil {
		return fmt.Errorf("publish checkpoint %s before commit status replay: %w", sha, err)
	}
	return nil
}

// acceptResultWithEffect journals harness finalization, invocation state, and
// the resulting workflow projection as one replayable acceptance operation.
func (s *Service) acceptResultWithEffect(ctx context.Context, runStore RunStore, invocationStore InvocationStore, registration config.RepositoryRegistration, harnessRuntime harness.Runtime, session harness.Session, invocation store.Invocation, previous, next store.Run, stopWorker bool, acceptedReport report.Report) (store.Invocation, store.Run, error) {
	// Encode the accepted report once for durable replay
	acceptedReportJSON, err := json.Marshal(acceptedReport)
	if err != nil {
		return invocation, next, fmt.Errorf("encode accepted report for effect: %w", err)
	}
	payload := resultAcceptanceEffectPayload{
		Repository:     github.Repository{Owner: registration.GitHub.Owner, Name: registration.GitHub.Repository},
		SocketPath:     registration.Cmux.SocketPath,
		Issue:          github.Issue{Number: next.IssueNumber},
		Session:        session,
		Invocation:     invocation,
		WorkerID:       workerIDForInvocation(invocation),
		Previous:       previous,
		Next:           next,
		StopWorker:     stopWorker,
		AcceptedReport: string(acceptedReportJSON),
	}
	// Keep the issue snapshot in the payload so replay does not have to infer
	// it from the mutable repository configuration.
	if s.deps.GitHub != nil {
		if issue, err := s.deps.GitHub.Issue(ctx, payload.Repository, next.IssueNumber); err == nil {
			payload.Issue = issue
		}
	}
	effect, err := s.newPendingEffect(next.ID, store.PendingEffectKindResultAcceptance, invocation.ID+"\x00"+string(invocation.Status)+"\x00"+fmt.Sprint(next.Revision), payload)
	if err != nil {
		return invocation, next, err
	}
	apply := func() error {
		currentInvocation, err := invocationStore.Invocation(ctx, invocation.RunID, invocation.ID)
		if err != nil {
			return fmt.Errorf("read invocation before accepting result: %w", err)
		}
		if currentInvocation == nil {
			return fmt.Errorf("invocation %q disappeared before accepting result", invocation.ID)
		}
		if currentInvocation.Status == store.InvocationStatusActive {
			// Mark the invocation terminal before Finish crosses the external
			// boundary. A restart after this reservation safely abandons Finish
			// because the native exit command has no observable idempotency key.
			if err := invocationStore.SaveInvocation(ctx, invocation); err != nil {
				return fmt.Errorf("reserve accepted invocation: %w", err)
			}
		} else if currentInvocation.Status != invocation.Status || currentInvocation.NativeSessionID != invocation.NativeSessionID {
			return fmt.Errorf("invocation %q changed while accepting result", invocation.ID)
		}
		if harnessRuntime == nil {
			return errors.New("harness runtime is required to accept result")
		}
		if err := harnessRuntime.Finish(ctx, session); err != nil {
			// Fail-closed: return immediately without clearing the pending effect.
			// Worker shutdown and workflow projection will not advance while native
			// exit delivery remains unconfirmed. The pending effect remains until
			// completion is observable or explicit abandonment is requested.
			return fmt.Errorf("finish accepted harness session: %w", err)
		}
		if stopWorker {
			workerID := payload.WorkerID
			if workerID == "" {
				workerID = previous.ID
			}
			if err := s.stopRunWorker(ctx, workerID); err != nil {
				return err
			}
		}
		if err := s.applyStateTransitionEffects(ctx, &next, stateTransition{
			Repository: payload.Repository,
			Issue:      payload.Issue,
			Previous:   previous,
			Next:       next,
		}); err != nil {
			return err
		}
		next.UpdatedAt = s.deps.Now().UTC()
		if err := saveRunWithRetry(ctx, runStore, next); err != nil {
			return fmt.Errorf("persist accepted result state: %w", err)
		}
		if recorder, ok := runStore.(evaluationRecorder); ok {
			if err := recordEvaluationTransition(ctx, recorder, previous, next, next.UpdatedAt); err != nil {
				return fmt.Errorf("record accepted result transition: %w", err)
			}
		}
		return nil
	}
	if err := s.withPendingEffect(ctx, runStore, effect, apply); err != nil {
		return invocation, next, err
	}
	return invocation, next, nil
}

// replayPendingResultAcceptance completes a report acceptance without
// repeating harness finalization after its terminal invocation reservation.
// The external exit command has no observable idempotency key, so an ambiguous
// prior attempt is safely abandoned while the durable projections converge.
func (s *Service) replayPendingResultAcceptance(ctx context.Context, runStore RunStore, effect store.PendingEffect) (store.Run, error) {
	var payload resultAcceptanceEffectPayload
	if err := decodePendingEffect(effect, &payload); err != nil {
		return store.Run{}, err
	}
	currentRun, err := readReconciliationRun(ctx, runStore)
	if err != nil {
		return store.Run{}, fmt.Errorf("read run during result acceptance replay: %w", err)
	}
	next := payload.Next
	if currentRun != nil && currentRun.ID != effect.RunID {
		return store.Run{}, workflowProjectionFailuref("result acceptance replay belongs to run %q, current run is %q", effect.RunID, currentRun.ID)
	}
	if currentRun != nil && currentRun.Revision > next.Revision {
		return store.Run{}, workflowProjectionFailuref("result acceptance revision %d is older than current revision %d", next.Revision, currentRun.Revision)
	}
	if currentRun != nil && currentRun.Revision < next.Revision && currentRun.Revision != payload.Previous.Revision {
		return store.Run{}, workflowProjectionFailuref("result acceptance replay expected revision %d, found %d", payload.Previous.Revision, currentRun.Revision)
	}
	invocationStore, ok := runStore.(InvocationStore)
	if !ok {
		return store.Run{}, errors.New("operational store does not support result acceptance replay")
	}
	currentInvocation, err := invocationStore.Invocation(ctx, effect.RunID, payload.Invocation.ID)
	if err != nil {
		return store.Run{}, fmt.Errorf("read invocation during result acceptance replay: %w", err)
	}
	if currentInvocation == nil {
		return store.Run{}, fmt.Errorf("invocation %q disappeared during result acceptance replay", payload.Invocation.ID)
	}
	finishNeeded := currentInvocation.Status == store.InvocationStatusActive
	if finishNeeded {
		// Reserve the terminal invocation projection before replaying Finish.
		if err := invocationStore.SaveInvocation(ctx, payload.Invocation); err != nil {
			return store.Run{}, fmt.Errorf("reserve replayed accepted invocation: %w", err)
		}
	} else if currentInvocation.Status != payload.Invocation.Status || currentInvocation.NativeSessionID != payload.Invocation.NativeSessionID {
		return store.Run{}, workflowProjectionFailuref("invocation %q changed during result acceptance replay", payload.Invocation.ID)
	}
	if finishNeeded {
		_, harnessRuntime, runtimeErr := s.ensureAgentRuntime(payload.SocketPath, config.Harness(payload.Invocation.Harness))
		if runtimeErr != nil {
			return store.Run{}, fmt.Errorf("ensure harness for result acceptance replay: %w", runtimeErr)
		}
		if err := harnessRuntime.Finish(ctx, payload.Session); err != nil {
			// Fail-closed: do not advance worker shutdown or workflow projection
			// while native exit delivery remains unconfirmed. Retain the pending
			// effect so a subsequent reconciliation can retry or require explicit
			// abandonment before allowing projection advancement.
			return store.Run{}, fmt.Errorf("finish replayed harness session: %w", err)
		}
	}
	if payload.StopWorker {
		// Stopping is idempotent for the run-scoped worker. Repeat it even when
		// the invocation was already marked terminal before an earlier process
		// crossed this boundary; that state is the durable evidence that the
		// acceptance reservation was already in flight.
		workerID := payload.WorkerID
		if workerID == "" {
			workerID = effect.RunID
		}
		if err := s.stopRunWorker(ctx, workerID); err != nil {
			return store.Run{}, err
		}
	}
	if currentRun != nil && currentRun.Revision == next.Revision && currentRun.StatusCommentID != "" {
		next.StatusCommentID = currentRun.StatusCommentID
	}
	if err := s.applyStateTransitionEffects(ctx, &next, stateTransition{
		Repository: payload.Repository,
		Issue:      payload.Issue,
		Previous:   payload.Previous,
		Next:       next,
	}); err != nil {
		return store.Run{}, fmt.Errorf("replay accepted result projection: %w", err)
	}
	next.UpdatedAt = s.deps.Now().UTC()
	if currentRun == nil || currentRun.Revision < next.Revision {
		if err := saveRunWithRetry(ctx, runStore, next); err != nil {
			return store.Run{}, fmt.Errorf("persist replayed accepted result: %w", err)
		}
	}
	if journal, ok := runStore.(PendingEffectStore); ok {
		if err := journal.ClearPendingEffect(ctx, effect.RunID, effect.ID); err != nil {
			return store.Run{}, fmt.Errorf("clear replayed result acceptance: %w", err)
		}
	}
	return next, nil
}

// upsertPullRequestRequest performs a branch-scoped, idempotent PR mutation.
// The expected number prevents a replay from silently attaching to another PR.
func (s *Service) upsertPullRequestRequest(ctx context.Context, client github.PullRequestClient, repository github.Repository, expectedNumber int, request github.PullRequestRequest) (github.PullRequest, error) {
	if client == nil {
		return github.PullRequest{}, errors.New("pull-request client is required")
	}
	existing, err := client.FindPullRequest(ctx, repository, request.HeadBranch, request.BaseBranch)
	if err != nil {
		return github.PullRequest{}, err
	}
	if existing.Number > 0 {
		if expectedNumber > 0 && existing.Number != expectedNumber {
			return github.PullRequest{}, fmt.Errorf("pull request changed from #%d to #%d during replay", expectedNumber, existing.Number)
		}
		if samePullRequestRequest(existing, request) {
			return existing, nil
		}
		updated, err := client.UpdatePullRequest(ctx, repository, existing.Number, request)
		if err != nil {
			return github.PullRequest{}, err
		}
		if updated.Number <= 0 {
			return github.PullRequest{}, errors.New("pull-request update returned no pull-request identity")
		}
		return updated, nil
	}
	if expectedNumber > 0 {
		return github.PullRequest{}, fmt.Errorf("pull request #%d disappeared before replay", expectedNumber)
	}
	created, err := client.CreatePullRequest(ctx, repository, request)
	if err != nil {
		return github.PullRequest{}, err
	}
	if created.Number <= 0 {
		return github.PullRequest{}, errors.New("pull-request creation returned no pull-request identity")
	}
	return created, nil
}

// samePullRequestRequest suppresses a redundant update after an interrupted
// create or update has already reached GitHub.
func samePullRequestRequest(existing github.PullRequest, request github.PullRequestRequest) bool {
	return existing.Title == request.Title && existing.Body == request.Body && existing.Draft == request.Draft &&
		existing.HeadBranch == request.HeadBranch && existing.BaseBranch == request.BaseBranch
}

// upsertPullRequestAndPersistWithEffect journals a draft PR mutation together
// with the run projection that records its identity. Recovery can therefore
// discover a created PR and finish the missing durable state without creating a
// second PR.
func (s *Service) upsertPullRequestAndPersistWithEffect(ctx context.Context, runStore RunStore, client github.PullRequestClient, repository github.Repository, issue github.Issue, previous, next store.Run, request github.PullRequestRequest, expectedNumber int) (github.PullRequest, store.Run, error) {
	payload := pullRequestEffectPayload{Repository: repository, Number: expectedNumber, Request: request, PersistRun: true, Issue: issue, Previous: previous, Next: next}
	effect, err := s.newPendingEffect(next.ID, store.PendingEffectKindPullRequest, request.HeadBranch+"\x00"+request.BaseBranch+"\x00"+request.Body, payload)
	if err != nil {
		return github.PullRequest{}, next, err
	}
	var pullRequest github.PullRequest
	apply := func() error {
		pullRequest, err = s.upsertPullRequestRequest(ctx, client, repository, expectedNumber, request)
		if err != nil {
			return fmt.Errorf("upsert draft pull request: %w", err)
		}
		next.PullRequestNumber = pullRequest.Number
		next.PullRequestURL = pullRequest.URL
		if err := s.applyStateTransitionEffects(ctx, &next, stateTransition{
			Repository: repository,
			Issue:      issue,
			Previous:   previous,
			Next:       next,
		}); err != nil {
			return err
		}
		next.UpdatedAt = s.deps.Now().UTC()
		if err := saveRunWithRetry(ctx, runStore, next); err != nil {
			return fmt.Errorf("persist draft pull-request projection: %w", err)
		}
		if recorder, ok := runStore.(evaluationRecorder); ok {
			if err := recordEvaluationTransition(ctx, recorder, previous, next, next.UpdatedAt); err != nil {
				return fmt.Errorf("record draft pull-request transition: %w", err)
			}
		}
		return nil
	}
	if err := s.withPendingEffect(ctx, runStore, effect, apply); err != nil {
		return pullRequest, next, err
	}
	return pullRequest, next, nil
}

// updatePullRequestWithEffect journals a standalone generated-body update,
// such as review regeneration, when no run-state change accompanies it.
func (s *Service) updatePullRequestWithEffect(ctx context.Context, runStore RunStore, runID string, client github.PullRequestClient, repository github.Repository, number int, request github.PullRequestRequest) error {
	payload := pullRequestEffectPayload{Repository: repository, Number: number, Request: request}
	effect, err := s.newPendingEffect(runID, store.PendingEffectKindPullRequest, fmt.Sprintf("update=%d\x00%s", number, request.Body), payload)
	if err != nil {
		return err
	}
	return s.withPendingEffect(ctx, runStore, effect, func() error {
		_, err := s.upsertPullRequestRequest(ctx, client, repository, number, request)
		return err
	})
}

// replayPendingPullRequest finishes a PR mutation and its run projection from
// the persisted request, recognizing an already-created PR by branch identity.
func (s *Service) replayPendingPullRequest(ctx context.Context, runStore RunStore, effect store.PendingEffect) (store.Run, error) {
	var payload pullRequestEffectPayload
	if err := decodePendingEffect(effect, &payload); err != nil {
		return store.Run{}, err
	}
	client := s.pullRequestClient()
	if client == nil {
		return store.Run{}, errors.New("pull-request client is required to replay draft pull request")
	}
	var current *store.Run
	if payload.PersistRun {
		var currentErr error
		current, currentErr = readReconciliationRun(ctx, runStore)
		if currentErr != nil {
			return store.Run{}, fmt.Errorf("read run during pull-request replay: %w", currentErr)
		}
		if current != nil {
			if current.ID != effect.RunID {
				return store.Run{}, workflowProjectionFailuref("pull-request replay belongs to run %q, current run is %q", effect.RunID, current.ID)
			}
			if current.Revision > payload.Next.Revision {
				return store.Run{}, workflowProjectionFailuref("pull-request replay revision %d is older than current revision %d", payload.Next.Revision, current.Revision)
			}
			if current.Revision < payload.Next.Revision && current.Revision != payload.Previous.Revision {
				return store.Run{}, workflowProjectionFailuref("pull-request replay expected revision %d, found %d", payload.Previous.Revision, current.Revision)
			}
		}
	}
	pullRequest, err := s.upsertPullRequestRequest(ctx, client, payload.Repository, payload.Number, payload.Request)
	if err != nil {
		return store.Run{}, fmt.Errorf("replay draft pull request: %w", err)
	}
	if !payload.PersistRun {
		if journal, ok := runStore.(PendingEffectStore); ok {
			if err := journal.ClearPendingEffect(ctx, effect.RunID, effect.ID); err != nil {
				return store.Run{}, fmt.Errorf("clear replayed draft pull-request update: %w", err)
			}
		}
		run, err := readReconciliationRun(ctx, runStore)
		if err != nil {
			return store.Run{}, fmt.Errorf("read run after draft pull-request update replay: %w", err)
		}
		if run == nil {
			return store.Run{}, fmt.Errorf("run %q disappeared during draft pull-request update replay", effect.RunID)
		}
		return *run, nil
	}
	next := payload.Next
	next.PullRequestNumber = pullRequest.Number
	next.PullRequestURL = pullRequest.URL
	if err := s.applyStateTransitionEffects(ctx, &next, stateTransition{
		Repository: payload.Repository,
		Issue:      payload.Issue,
		Previous:   payload.Previous,
		Next:       next,
	}); err != nil {
		return store.Run{}, fmt.Errorf("replay draft pull-request state projection: %w", err)
	}
	next.UpdatedAt = s.deps.Now().UTC()
	if current == nil || current.Revision < next.Revision {
		if err := saveRunWithRetry(ctx, runStore, next); err != nil {
			return store.Run{}, fmt.Errorf("persist replayed draft pull request: %w", err)
		}
	}
	if journal, ok := runStore.(PendingEffectStore); ok {
		if err := journal.ClearPendingEffect(ctx, effect.RunID, effect.ID); err != nil {
			return store.Run{}, fmt.Errorf("clear replayed draft pull request: %w", err)
		}
	}
	return next, nil
}

// checkpointEffectPayload is the complete restart intent for a checkpoint and
// its immediately following run projection.
type checkpointEffectPayload struct {
	Request    checkpointRequestJSON
	Repository github.Repository
	Issue      github.Issue
	Previous   store.Run
	Next       store.Run
}

// checkpointRequest converts a serialized checkpoint request back to the Git
// adapter's portable input type.
func checkpointRequest(value checkpointRequestJSON) gitadapter.CheckpointRequest {
	return gitadapter.CheckpointRequest{
		RunID:        value.RunID,
		WorktreePath: value.WorktreePath,
		ParentSHA:    value.ParentSHA,
		Kind:         gitadapter.CheckpointKind(value.Kind),
		Paths:        append([]string(nil), value.Paths...),
		Message:      value.Message,
	}
}

// checkpointAndPersistWithEffect makes a checkpoint commit and the immediate
// run projection one restart-safe operation. The marker in the Git adapter
// handles a commit that was created just before the process stopped.
func (s *Service) checkpointAndPersistWithEffect(ctx context.Context, runStore RunStore, workspace gitadapter.GitWorkspace, request gitadapter.CheckpointRequest, repository github.Repository, issue github.Issue, previous, nextTemplate store.Run) (gitadapter.CheckpointResult, store.Run, error) {
	payload := checkpointEffectPayload{
		Request: checkpointRequestJSON{
			RunID:        request.RunID,
			WorktreePath: request.WorktreePath,
			ParentSHA:    request.ParentSHA,
			Kind:         string(request.Kind),
			Paths:        append([]string(nil), request.Paths...),
			Message:      request.Message,
		},
		Repository: repository,
		Issue:      issue,
		Previous:   previous,
		Next:       nextTemplate,
	}
	effect, err := s.newPendingEffect(request.RunID, store.PendingEffectKindCheckpoint, request.ParentSHA+"\x00"+string(request.Kind)+"\x00"+strings.Join(request.Paths, "\x00"), payload)
	if err != nil {
		return gitadapter.CheckpointResult{}, nextTemplate, err
	}
	var checkpoint gitadapter.CheckpointResult
	next := nextTemplate
	action := func() error {
		checkpoint, err = workspace.CreateCheckpoint(ctx, request)
		if err != nil {
			return fmt.Errorf("create checkpoint: %w", err)
		}
		if !github.ValidCommitSHA(checkpoint.SHA) {
			return errors.New("GitWorkspace returned an invalid checkpoint SHA")
		}
		next.AcceptedImplementationCheckpointSHA = ""
		next.CheckpointSHA = checkpoint.SHA
		if request.Kind == gitadapter.CheckpointKindTest {
			next.TestCheckpointSHA = checkpoint.SHA
		}
		if err := s.applyStateTransitionEffects(ctx, &next, stateTransition{
			Repository: repository,
			Issue:      issue,
			Previous:   previous,
			Next:       next,
		}); err != nil {
			return err
		}
		next.UpdatedAt = s.deps.Now().UTC()
		if err := saveRunWithRetry(ctx, runStore, next); err != nil {
			return fmt.Errorf("persist checkpoint run projection: %w", err)
		}
		if recorder, ok := runStore.(evaluationRecorder); ok {
			if err := recordEvaluationTransition(ctx, recorder, previous, next, next.UpdatedAt); err != nil {
				return fmt.Errorf("record checkpoint state transition: %w", err)
			}
		}
		return nil
	}
	if err := s.withPendingEffect(ctx, runStore, effect, action); err != nil {
		return checkpoint, next, err
	}
	return checkpoint, next, nil
}

// replayPendingCheckpoint completes a checkpoint reservation by replaying the
// idempotent Git marker and its persisted run transition.
func (s *Service) replayPendingCheckpoint(ctx context.Context, runStore RunStore, effect store.PendingEffect) (store.Run, error) {
	var payload checkpointEffectPayload
	if err := decodePendingEffect(effect, &payload); err != nil {
		return store.Run{}, err
	}
	workspace := s.gitWorkspace()
	if workspace == nil {
		return store.Run{}, errors.New("GitWorkspace is required to replay checkpoint")
	}
	current, err := readReconciliationRun(ctx, runStore)
	if err != nil {
		return store.Run{}, fmt.Errorf("read run during checkpoint replay: %w", err)
	}
	if current != nil {
		if current.ID != effect.RunID {
			return store.Run{}, workflowProjectionFailuref("checkpoint replay belongs to run %q, current run is %q", effect.RunID, current.ID)
		}
		if current.Revision > payload.Next.Revision {
			return store.Run{}, workflowProjectionFailuref("checkpoint replay revision %d is older than current revision %d", payload.Next.Revision, current.Revision)
		}
		if current.Revision < payload.Next.Revision && current.Revision != payload.Previous.Revision {
			return store.Run{}, workflowProjectionFailuref("checkpoint replay expected revision %d, found %d", payload.Previous.Revision, current.Revision)
		}
	}
	request := checkpointRequest(payload.Request)
	checkpoint, err := workspace.CreateCheckpoint(ctx, request)
	if err != nil {
		return store.Run{}, fmt.Errorf("replay checkpoint: %w", err)
	}
	if !github.ValidCommitSHA(checkpoint.SHA) {
		return store.Run{}, errors.New("replayed checkpoint returned an invalid SHA")
	}
	next := payload.Next
	next.AcceptedImplementationCheckpointSHA = ""
	next.CheckpointSHA = checkpoint.SHA
	if request.Kind == gitadapter.CheckpointKindTest {
		next.TestCheckpointSHA = checkpoint.SHA
	}
	if err := s.applyStateTransitionEffects(ctx, &next, stateTransition{
		Repository: payload.Repository,
		Issue:      payload.Issue,
		Previous:   payload.Previous,
		Next:       next,
	}); err != nil {
		return store.Run{}, fmt.Errorf("replay checkpoint state projection: %w", err)
	}
	next.UpdatedAt = s.deps.Now().UTC()
	if current == nil || current.Revision < next.Revision {
		if err := saveRunWithRetry(ctx, runStore, next); err != nil {
			return store.Run{}, fmt.Errorf("persist replayed checkpoint: %w", err)
		}
	}
	if journal, ok := runStore.(PendingEffectStore); ok {
		if err := journal.ClearPendingEffect(ctx, effect.RunID, effect.ID); err != nil {
			return store.Run{}, fmt.Errorf("clear replayed checkpoint: %w", err)
		}
	}
	return next, nil
}

// replayPendingStateTransition restores the complete persisted run revision
// associated with a label/comment effect and then acknowledges its journal.
func (s *Service) replayPendingStateTransition(ctx context.Context, runStore RunStore, effect store.PendingEffect) (store.Run, error) {
	var payload stateTransitionEffectPayload
	if err := decodePendingEffect(effect, &payload); err != nil {
		return store.Run{}, err
	}
	if payload.Next.ID == "" || payload.Next.ID != effect.RunID {
		return store.Run{}, errors.New("pending state transition run identity does not match its journal")
	}
	current, err := readReconciliationRun(ctx, runStore)
	if err != nil {
		return store.Run{}, fmt.Errorf("read run while replaying state transition: %w", err)
	}
	next := payload.Next
	if current != nil {
		if current.ID != next.ID {
			return store.Run{}, workflowProjectionFailuref("pending state transition belongs to run %q, current run is %q", next.ID, current.ID)
		}
		if current.Revision > next.Revision {
			return store.Run{}, workflowProjectionFailuref("pending state transition revision %d is older than current revision %d", next.Revision, current.Revision)
		}
		if current.Revision == next.Revision {
			// The run may already be persisted while the final comment identity was
			// still in memory. Preserve any newer durable fields, then fill the
			// effect-owned identity from the replay payload.
			if current.StatusCommentID != "" {
				next.StatusCommentID = current.StatusCommentID
			}
		}
		if current.Revision < next.Revision && current.Revision != payload.Previous.Revision {
			return store.Run{}, workflowProjectionFailuref("pending state transition expected revision %d, found %d", payload.Previous.Revision, current.Revision)
		}
	}
	if payload.StopWorker {
		if err := s.stopActiveRunWorkers(ctx, runStore, payload.Previous); err != nil {
			return store.Run{}, fmt.Errorf("stop worker during state-transition replay: %w", err)
		}
	}
	transition := stateTransition{
		Repository:           payload.Repository,
		Issue:                payload.Issue,
		Previous:             payload.Previous,
		Next:                 next,
		CreateComment:        payload.CreateComment,
		StopWorker:           payload.StopWorker,
		InvalidateResults:    payload.InvalidateResults,
		InvalidateAllResults: payload.InvalidateAllResults,
	}
	if err := s.applyStateTransitionEffects(ctx, &next, transition); err != nil {
		return store.Run{}, fmt.Errorf("replay state transition effects: %w", err)
	}
	next.UpdatedAt = s.deps.Now().UTC()
	if current == nil || current.Revision < next.Revision {
		if payload.InvalidateAllResults {
			if atomicStore, ok := runStore.(atomicAllPacketTransitionStore); ok {
				if err := atomicStore.SaveRunAndInvalidateAllResults(ctx, payload.Previous.Revision, next); err != nil {
					return store.Run{}, fmt.Errorf("persist replayed state transition and invalidate all results: %w", err)
				}
			} else {
				if err := saveCommandRun(ctx, runStore, payload.Previous.Revision, next); err != nil {
					return store.Run{}, fmt.Errorf("persist replayed state transition: %w", err)
				}
				if err := invalidateAllRunResults(ctx, runStore, next.ID); err != nil {
					return store.Run{}, err
				}
			}
		} else if payload.InvalidateResults {
			if atomicStore, ok := runStore.(atomicPacketTransitionStore); ok {
				if err := atomicStore.SaveRunAndInvalidateResults(ctx, payload.Previous.Revision, next); err != nil {
					return store.Run{}, fmt.Errorf("persist replayed state transition and invalidate results: %w", err)
				}
			} else {
				if err := saveCommandRun(ctx, runStore, payload.Previous.Revision, next); err != nil {
					return store.Run{}, fmt.Errorf("persist replayed state transition: %w", err)
				}
				if err := invalidateRunResults(ctx, runStore, next.ID); err != nil {
					return store.Run{}, err
				}
			}
		} else if err := saveRunWithRetry(ctx, runStore, next); err != nil {
			return store.Run{}, fmt.Errorf("persist replayed state transition: %w", err)
		}
	}
	journal, ok := runStore.(PendingEffectStore)
	if !ok {
		return next, nil
	}
	if err := journal.ClearPendingEffect(ctx, effect.RunID, effect.ID); err != nil {
		return store.Run{}, fmt.Errorf("clear replayed state transition: %w", err)
	}
	return next, nil
}
