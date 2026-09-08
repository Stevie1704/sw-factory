package effect

import (
	"context"
	"time"

	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// RunStore is the durable run projection every effect handler reads and
// writes. Richer coordinator stores satisfy it without naming this package.
type RunStore interface {
	CurrentRun(context.Context) (*store.Run, error)
	SaveRun(context.Context, store.Run) error
}

// InvocationStore adds the invocation projection required by the worker,
// harness-resume, and result-acceptance kinds.
type InvocationStore interface {
	RunStore
	SaveInvocation(context.Context, store.Invocation) error
	Invocation(context.Context, string, string) (*store.Invocation, error)
}

// IssueClient is the GitHub issue and comment seam used by the label,
// status-comment, clarification, and state-transition kinds.
type IssueClient interface {
	Issue(context.Context, github.Repository, int) (github.Issue, error)
	ReplaceIssueLabels(context.Context, github.Repository, int, []string) error
	CreateIssueComment(context.Context, github.Repository, int, string) (github.Comment, error)
	FindStatusComment(context.Context, github.Repository, int, string) (github.Comment, error)
	EditIssueComment(context.Context, github.Repository, string, string) error
}

// RunPresentation renders the coordinator-owned issue projection of a run.
// Status-comment wording and factory-label policy stay with the coordinator;
// the journal only decides when to apply them.
type RunPresentation interface {
	// StatusCommentMarker identifies the one editable status comment of a run.
	StatusCommentMarker(runID string) string
	// StatusCommentBody renders that comment for the supplied run projection.
	StatusCommentBody(run store.Run) string
	// ClarificationCommentMarker identifies one clarification round's comment.
	ClarificationCommentMarker(runID string, packetVersion int) string
	// StateLabels returns the complete label set for a run status, preserving
	// the ordinary labels already on the issue.
	StateLabels(existing []string, status store.Status) []string
}

// RunProjector persists the durable run projection an effect produces. Retry,
// compare-and-set, result invalidation, and evaluation recording are
// coordinator policy; the journal only decides which of them an effect needs.
type RunProjector interface {
	// Read returns the reconciliation run: the active run, or the latest
	// terminal run when no active run exists.
	Read(context.Context, RunStore) (*store.Run, error)
	// Save persists a run projection.
	Save(context.Context, RunStore, store.Run) error
	// SaveAtRevision persists a run projection against an expected revision.
	SaveAtRevision(context.Context, RunStore, int64, store.Run) error
	// SaveInvalidatingResults persists a packet revision boundary together
	// with the invalidation of its superseded results in one compare-and-set.
	// It reports false when the store has no atomic seam, leaving the caller
	// to persist and invalidate in two steps.
	SaveInvalidatingResults(context.Context, RunStore, int64, store.Run) (bool, error)
	// SaveInvalidatingAllResults additionally invalidates the superseded
	// packet version's baseline result, with the same false report.
	SaveInvalidatingAllResults(context.Context, RunStore, int64, store.Run) (bool, error)
	// InvalidateResults discards the superseded invocation and gate results.
	InvalidateResults(context.Context, RunStore, string) error
	// InvalidateAllResults additionally discards the superseded baseline.
	InvalidateAllResults(context.Context, RunStore, string) error
	// RecordTransition keeps the optional evaluation projection aligned with a
	// persisted run transition.
	RecordTransition(context.Context, RunStore, store.Run, store.Run, time.Time) error
}

// Lifecycle is the invocation-lifecycle behaviour the journal needs: worker
// shutdown owned by a terminal transition, and harness runtime resolution for
// a replayed native command.
type Lifecycle interface {
	// StopWorker stops one worker by its identity.
	StopWorker(context.Context, string) error
	// StopActiveWorkers stops every worker delegated to a run.
	StopActiveWorkers(context.Context, RunStore, store.Run) error
	// HarnessRuntime resolves the named harness behind the run's terminal.
	HarnessRuntime(socketPath, harnessName string) (harness.Runtime, error)
}

// WorkerLauncher is the worker seam used by the worker-launch kind. Launching
// is the only worker authority this journal holds.
type WorkerLauncher interface {
	Start(context.Context, worker.StartRequest) error
}

// Adapters are the explicit seams the journal hands to its handlers. Each
// handler receives only the ones its kind uses.
type Adapters struct {
	// Now is the coordinator clock applied to every journal record.
	Now func() time.Time
	// Issues publishes issue labels and coordinator-owned comments.
	Issues IssueClient
	// Presentation renders the coordinator-owned issue projection.
	Presentation RunPresentation
	// Projector persists the durable run projection.
	Projector RunProjector
	// Workspace owns checkpoint and push effects on the host.
	Workspace gitadapter.GitWorkspace
	// PullRequests owns idempotent draft pull-request mutation.
	PullRequests github.PullRequestClient
	// CommitStatuses publishes exact-SHA commit statuses.
	CommitStatuses github.CommitStatusPublisher
	// Worker launches the per-run isolated execution environment.
	Worker WorkerLauncher
	// Lifecycle stops workers and resolves harness runtimes during replay.
	Lifecycle Lifecycle
}

// Journal is the coordinator's durable-effect seam. It owns one handler per
// pending-effect kind and routes replay through the protocol kernel.
type Journal struct {
	dispatcher       *Dispatcher
	stateTransition  stateTransitionHandler
	statusComment    statusCommentHandler
	clarification    clarificationHandler
	labelTransition  labelTransitionHandler
	commitStatus     commitStatusHandler
	push             pushHandler
	checkpoint       checkpointHandler
	pullRequest      pullRequestHandler
	workerLaunch     workerLaunchHandler
	harnessResume    harnessResumeHandler
	resultAcceptance resultAcceptanceHandler
}

// New builds one handler per kind from the supplied adapters and registers its
// replay path. Registration failures are programmer errors: the kinds are
// constants and cannot collide.
func New(adapters Adapters) *Journal {
	clock := adapters.Now
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	labels := labelProjection{issues: adapters.Issues, presentation: adapters.Presentation}
	journal := &Journal{
		dispatcher:      NewDispatcher(),
		stateTransition: stateTransitionHandler{now: clock, labels: labels, projector: adapters.Projector, lifecycle: adapters.Lifecycle},
		statusComment:   statusCommentHandler{now: clock, labels: labels, projector: adapters.Projector},
		clarification:   clarificationHandler{now: clock, issues: adapters.Issues, presentation: adapters.Presentation, projector: adapters.Projector},
		labelTransition: labelTransitionHandler{issues: adapters.Issues, projector: adapters.Projector},
		commitStatus:    commitStatusHandler{now: clock, statuses: adapters.CommitStatuses, workspace: adapters.Workspace, projector: adapters.Projector},
		push:            pushHandler{now: clock, workspace: adapters.Workspace, projector: adapters.Projector},
		checkpoint:      checkpointHandler{now: clock, workspace: adapters.Workspace, labels: labels, projector: adapters.Projector},
		pullRequest:     pullRequestHandler{now: clock, clients: adapters.PullRequests, labels: labels, projector: adapters.Projector},
		workerLaunch:    workerLaunchHandler{now: clock, worker: adapters.Worker, projector: adapters.Projector},
		harnessResume:   harnessResumeHandler{now: clock, lifecycle: adapters.Lifecycle, projector: adapters.Projector},
		resultAcceptance: resultAcceptanceHandler{
			now: clock, issues: adapters.Issues, labels: labels,
			projector: adapters.Projector, lifecycle: adapters.Lifecycle,
		},
	}
	register(journal.dispatcher, store.PendingEffectKindStateTransition, journal.stateTransition)
	register(journal.dispatcher, store.PendingEffectKindStatusComment, journal.statusComment)
	register(journal.dispatcher, store.PendingEffectKindClarificationComment, journal.clarification)
	register(journal.dispatcher, store.PendingEffectKindLabelTransition, journal.labelTransition)
	register(journal.dispatcher, store.PendingEffectKindCommitStatus, journal.commitStatus)
	register(journal.dispatcher, store.PendingEffectKindPush, journal.push)
	register(journal.dispatcher, store.PendingEffectKindCheckpoint, journal.checkpoint)
	register(journal.dispatcher, store.PendingEffectKindPullRequest, journal.pullRequest)
	register(journal.dispatcher, store.PendingEffectKindWorkerLaunch, journal.workerLaunch)
	register(journal.dispatcher, store.PendingEffectKindHarnessResume, journal.harnessResume)
	register(journal.dispatcher, store.PendingEffectKindResultAcceptance, journal.resultAcceptance)
	return journal
}

// register makes a registration failure a programmer error, because every
// kind is a package constant that can neither collide nor be empty.
func register(dispatcher *Dispatcher, kind store.PendingEffectKind, handler ReplayHandler) {
	if err := dispatcher.Register(kind, handler); err != nil {
		panic(err)
	}
}

// Replay completes a journaled effect whose process boundary was crossed
// before the journal could be cleared. An unregistered kind is rejected with
// the kernel's typed error.
func (j *Journal) Replay(ctx context.Context, runStore RunStore, pending store.PendingEffect) (store.Run, error) {
	return j.dispatcher.Replay(ctx, runStore, pending)
}
