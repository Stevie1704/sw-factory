package factory

import (
	"context"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	effectkernel "github.com/Stevie1704/sw-factory/internal/effect"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// PendingEffectStore is retained as a factory-local alias while journal users
// migrate to the effect package's store-facing protocol contract.
type PendingEffectStore = effectkernel.PendingEffectStore

// stateTransition is the shared state-machine input used by initial claims and
// later transitions. It is the effect package's transition contract.
type stateTransition = effectkernel.StateTransition

// journal builds the durable-effect seam from the coordinator's current
// adapters. It is rebuilt per call so a late adapter substitution reaches the
// handlers exactly as a direct `s.deps` read did before the extraction.
func (s *Service) journal() *effectkernel.Journal {
	return effectkernel.New(effectkernel.Adapters{
		Now:            s.deps.Now,
		Issues:         s.deps.GitHub,
		Presentation:   runPresentation{},
		Projector:      runProjector{},
		Workspace:      s.gitWorkspace(),
		PullRequests:   s.pullRequestClient(),
		CommitStatuses: s.deps.CommitStatuses,
		Worker:         s.deps.Worker,
		Lifecycle:      journalLifecycle{service: s},
	})
}

// runPresentation renders the coordinator-owned issue projection of a run for
// the journal. Wording and factory-label policy stay with the coordinator.
type runPresentation struct{}

// StatusCommentMarker identifies the one editable status comment for a run.
func (runPresentation) StatusCommentMarker(runID string) string {
	return statusCommentMarker(runID)
}

// StatusCommentBody renders the single editable supervision comment.
func (runPresentation) StatusCommentBody(run store.Run) string {
	return statusCommentBody(run)
}

// ClarificationCommentMarker identifies one clarification round's comment.
func (runPresentation) ClarificationCommentMarker(runID string, packetVersion int) string {
	return clarificationCommentMarker(runID, packetVersion)
}

// StateLabels returns the complete label set for a run status while preserving
// the ordinary labels already on the issue.
func (runPresentation) StateLabels(existing []string, status store.Status) []string {
	return replaceFactoryState(existing, factoryLabelForStatus(status))
}

// runProjector persists the durable run projection an effect produces, using
// the coordinator's retry, compare-and-set, invalidation, and evaluation
// policies.
type runProjector struct{}

// Read returns the reconciliation run for a journal handler.
func (runProjector) Read(ctx context.Context, runStore effectkernel.RunStore) (*store.Run, error) {
	return readReconciliationRun(ctx, runStore)
}

// Save persists a run projection, retrying one transient store failure.
func (runProjector) Save(ctx context.Context, runStore effectkernel.RunStore, run store.Run) error {
	return saveRunWithRetry(ctx, runStore, run)
}

// SaveAtRevision persists a run projection against an expected revision.
func (runProjector) SaveAtRevision(ctx context.Context, runStore effectkernel.RunStore, expectedRevision int64, next store.Run) error {
	return saveCommandRun(ctx, runStore, expectedRevision, next)
}

// SaveInvalidatingResults uses the atomic packet-transition seam when the
// store offers one, and reports its absence rather than inventing one.
func (runProjector) SaveInvalidatingResults(ctx context.Context, runStore effectkernel.RunStore, expectedRevision int64, next store.Run) (bool, error) {
	atomicStore, ok := runStore.(atomicPacketTransitionStore)
	if !ok {
		return false, nil
	}
	return true, atomicStore.SaveRunAndInvalidateResults(ctx, expectedRevision, next)
}

// SaveInvalidatingAllResults uses the stronger atomic seam of an authorized
// specification amendment when the store offers one.
func (runProjector) SaveInvalidatingAllResults(ctx context.Context, runStore effectkernel.RunStore, expectedRevision int64, next store.Run) (bool, error) {
	atomicStore, ok := runStore.(atomicAllPacketTransitionStore)
	if !ok {
		return false, nil
	}
	return true, atomicStore.SaveRunAndInvalidateAllResults(ctx, expectedRevision, next)
}

// InvalidateResults discards the superseded invocation and gate projections.
func (runProjector) InvalidateResults(ctx context.Context, runStore effectkernel.RunStore, runID string) error {
	return invalidateRunResults(ctx, runStore, runID)
}

// InvalidateAllResults additionally discards the superseded baseline result.
func (runProjector) InvalidateAllResults(ctx context.Context, runStore effectkernel.RunStore, runID string) error {
	return invalidateAllRunResults(ctx, runStore, runID)
}

// RecordTransition keeps the optional evaluation projection aligned with a
// persisted run transition. A store without the projection records nothing.
func (runProjector) RecordTransition(ctx context.Context, runStore effectkernel.RunStore, previous, next store.Run, at time.Time) error {
	recorder, ok := runStore.(evaluationRecorder)
	if !ok {
		return nil
	}
	return recordEvaluationTransition(ctx, recorder, previous, next, at)
}

// journalLifecycle exposes the invocation-lifecycle behaviour the journal
// needs. It resolves the module on each call rather than closing over it, so
// the journal never holds a reference into the coordinator's method graph.
type journalLifecycle struct {
	service *Service
}

// StopWorker stops one worker by its identity.
func (l journalLifecycle) StopWorker(ctx context.Context, workerID string) error {
	return l.service.lifecycleModule().stopRunWorker(ctx, workerID)
}

// StopActiveWorkers stops every worker currently delegated to a run.
func (l journalLifecycle) StopActiveWorkers(ctx context.Context, runStore effectkernel.RunStore, run store.Run) error {
	return l.service.lifecycleModule().stopActiveRunWorkers(ctx, runStore, run)
}

// HarnessRuntime resolves the named harness behind the run's terminal.
func (l journalLifecycle) HarnessRuntime(socketPath, harnessName string) (harness.Runtime, error) {
	_, harnessRuntime, err := l.service.lifecycleModule().ensureAgentRuntime(socketPath, config.Harness(harnessName))
	return harnessRuntime, err
}

// invocationJournal is the durable-effect seam the invocation-lifecycle module
// uses for the two external mutations it owns.
type invocationJournal interface {
	StartWorker(context.Context, effectkernel.RunStore, worker.StartRequest) error
	ResumeHarness(context.Context, effectkernel.RunStore, effectkernel.InvocationStore, string, harness.Runtime, store.Invocation, harness.StartRequest) (store.Invocation, error)
	ResumeHarnessManually(context.Context, effectkernel.RunStore, effectkernel.InvocationStore, string, harness.Runtime, store.Invocation, harness.StartRequest) (store.Invocation, error)
}

// serviceJournal resolves the coordinator's journal on each call, so the
// long-lived lifecycle module never captures a stale adapter set.
type serviceJournal struct {
	service *Service
}

// StartWorker reserves and performs one worker launch.
func (j serviceJournal) StartWorker(ctx context.Context, runStore effectkernel.RunStore, request worker.StartRequest) error {
	return j.service.journal().StartWorker(ctx, runStore, request)
}

// ResumeHarness reserves and performs one automatic native-session resume.
func (j serviceJournal) ResumeHarness(ctx context.Context, runStore effectkernel.RunStore, invocationStore effectkernel.InvocationStore, socketPath string, runtime harness.Runtime, invocation store.Invocation, request harness.StartRequest) (store.Invocation, error) {
	return j.service.journal().ResumeHarness(ctx, runStore, invocationStore, socketPath, runtime, invocation, request)
}

// ResumeHarnessManually reserves and performs one operator-requested resume.
func (j serviceJournal) ResumeHarnessManually(ctx context.Context, runStore effectkernel.RunStore, invocationStore effectkernel.InvocationStore, socketPath string, runtime harness.Runtime, invocation store.Invocation, request harness.StartRequest) (store.Invocation, error) {
	return j.service.journal().ResumeHarnessManually(ctx, runStore, invocationStore, socketPath, runtime, invocation, request)
}
