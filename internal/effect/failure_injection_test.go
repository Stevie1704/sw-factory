package effect_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/effect"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// Names of the external mutations the contract world counts and can fail.
const (
	opLabels        = "replace issue labels"
	opCommentCreate = "create comment"
	opCommentEdit   = "edit comment"
	opPullCreate    = "create pull request"
	opPullUpdate    = "update pull request"
	opCommitStatus  = "create commit status"
	opCheckpoint    = "create checkpoint commit"
	opPush          = "push branch"
	opWorkerLaunch  = "launch worker"
	opResume        = "resume native session"
	opFinish        = "finish native session"
)

// faultPhase places an injected failure relative to the external mutation.
type faultPhase string

const (
	// faultBefore fails the call before the external system changes.
	faultBefore faultPhase = "before"
	// faultAfter changes the external system and then loses the response.
	faultAfter faultPhase = "after"
)

// errInjected is the failure every armed fault returns.
var errInjected = errors.New("injected external failure")

// failForever arms a fault for more calls than any contract makes, so the
// external system keeps refusing the mutation.
const failForever = 100

// fault is one armed failure at one named external mutation.
type fault struct {
	operation string
	phase     faultPhase
	err       error
	remaining int
}

// contractRepository is the tracked repository of the contract world.
var contractRepository = tracker.Repository{Owner: "example", Name: "project"}

// effectWorld is one in-memory external world: the GitHub issue and code
// host, the Git remote and worktree, the Docker worker runtime, and the
// native harness. It implements every adapter seam the journal accepts, counts
// each mutation that reached the external system, and fails one armed
// mutation before or after it changes the world.
type effectWorld struct {
	t       *testing.T
	store   *store.Store
	run     store.Run
	applied map[string]int
	faults  []fault

	issue        tracker.Issue
	comments     []tracker.Comment
	pulls        []codehost.PullRequest
	statuses     []codehost.CommitStatus
	checkpoints  map[string]string
	commits      int
	localHead    string
	remoteHead   string
	workers      map[string]worker.StartRequest
	hideStatuses bool
}

// newEffectWorld opens a real temporary SQLite store, persists the run
// fixture, and seeds the GitHub projection of that run.
func newEffectWorld(t *testing.T) *effectWorld {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	opened, err := store.Open(context.Background(), filepath.Join(root, "state", "factory.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	run := contractRun()
	if err := opened.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("save run fixture: %v", err)
	}
	return &effectWorld{
		t: t, store: opened, run: run, applied: map[string]int{},
		issue:       tracker.Issue{Number: run.IssueNumber, State: "open", Labels: []string{"ordinary", "state:active"}},
		comments:    []tracker.Comment{{ID: run.StatusCommentID, Body: contractPresentation{}.StatusCommentBody(run)}},
		checkpoints: map[string]string{},
		localHead:   run.CheckpointSHA, remoteHead: run.CheckpointSHA,
		workers: map[string]worker.StartRequest{},
	}
}

// contractRun is a valid active run whose checkpoint already reached the
// remote branch.
func contractRun() store.Run {
	return store.Run{
		ID: "run-contract", RepositoryPath: "/repo", IssueNumber: 42, Stage: store.StageImplementation,
		Status: store.StatusActive, Branch: "factory/run-contract", Worktree: "/worktree",
		CheckpointSHA: commitSHA(1), StatusCommentID: "comment-1", ProcessedCommentID: "command-7", Revision: 3,
		CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC(),
	}
}

// commitSHA returns a deterministic full commit identity.
func commitSHA(n int) string {
	return fmt.Sprintf("%040x", n)
}

// advanced returns the run fixture at the next revision with a new status.
func advanced(run store.Run, status store.Status) store.Run {
	next := run
	next.Revision++
	next.Status = status
	return next
}

// journal builds the journal from the world's current adapters, the way the
// coordinator rebuilds it after a restart.
func (w *effectWorld) journal() *effect.Journal {
	var statuses codehost.CommitStatusPublisher = w
	if w.hideStatuses {
		statuses = statusPublisherOnly{world: w}
	}
	return effect.New(effect.Adapters{
		Now:            func() time.Time { return time.Unix(20, 0).UTC() },
		Issues:         w,
		Presentation:   contractPresentation{},
		Projector:      storeProjector{store: w.store},
		Workspace:      w,
		PullRequests:   w,
		CommitStatuses: statuses,
		Worker:         w,
		Lifecycle:      w,
	})
}

// arm injects one failure at the next matching external mutation.
func (w *effectWorld) arm(operation string, phase faultPhase) {
	w.armWith(operation, phase, errInjected, 1)
}

// armWith injects err at the next times matching external mutations.
func (w *effectWorld) armWith(operation string, phase faultPhase, err error, times int) {
	w.faults = append(w.faults, fault{operation: operation, phase: phase, err: err, remaining: times})
}

// trip consumes and returns an armed failure for one mutation phase.
func (w *effectWorld) trip(operation string, phase faultPhase) error {
	for index := range w.faults {
		armed := &w.faults[index]
		if armed.operation == operation && armed.phase == phase && armed.remaining > 0 {
			armed.remaining--
			return armed.err
		}
	}
	return nil
}

// mutate applies one external change between its before and after faults.
func (w *effectWorld) mutate(operation string, change func()) error {
	if err := w.trip(operation, faultBefore); err != nil {
		return err
	}
	change()
	w.applied[operation]++
	return w.trip(operation, faultAfter)
}

// pending returns the run's durable journal entry from the SQLite store.
func (w *effectWorld) pending() *store.PendingEffect {
	w.t.Helper()
	pending, err := w.store.PendingEffect(context.Background(), w.run.ID)
	if err != nil {
		w.t.Fatalf("read pending effect: %v", err)
	}
	return pending
}

// persisted returns the run projection stored in SQLite.
func (w *effectWorld) persisted() store.Run {
	w.t.Helper()
	run, err := w.store.LatestRun(context.Background())
	if err != nil || run == nil {
		w.t.Fatalf("read run = %v, %v", run, err)
	}
	return *run
}

// supersede persists a newer run revision that no journal entry expected.
func (w *effectWorld) supersede() {
	w.t.Helper()
	newer := w.persisted()
	newer.Revision = 9
	newer.ProcessedCommentID = "command-newer"
	if err := w.store.SaveRun(context.Background(), newer); err != nil {
		w.t.Fatalf("save newer run: %v", err)
	}
}

// Issue returns the issue projection.
func (w *effectWorld) Issue(context.Context, tracker.Repository, int) (tracker.Issue, error) {
	issue := w.issue
	issue.Labels = append([]string(nil), w.issue.Labels...)
	return issue, nil
}

// ReplaceIssueLabels replaces the complete label set.
func (w *effectWorld) ReplaceIssueLabels(_ context.Context, _ tracker.Repository, _ int, labels []string) error {
	return w.mutate(opLabels, func() { w.issue.Labels = append([]string(nil), labels...) })
}

// CreateIssueComment posts one comment.
func (w *effectWorld) CreateIssueComment(_ context.Context, _ tracker.Repository, _ int, body string) (tracker.Comment, error) {
	comment := tracker.Comment{ID: fmt.Sprintf("comment-%d", len(w.comments)+1), Body: body}
	if err := w.mutate(opCommentCreate, func() { w.comments = append(w.comments, comment) }); err != nil {
		return tracker.Comment{}, err
	}
	return comment, nil
}

// FindStatusComment returns the first comment that carries marker.
func (w *effectWorld) FindStatusComment(_ context.Context, _ tracker.Repository, _ int, marker string) (tracker.Comment, error) {
	for _, comment := range w.comments {
		if strings.Contains(comment.Body, marker) {
			return comment, nil
		}
	}
	return tracker.Comment{}, nil
}

// EditIssueComment replaces one comment body.
func (w *effectWorld) EditIssueComment(_ context.Context, _ tracker.Repository, id, body string) error {
	for index := range w.comments {
		if w.comments[index].ID == id {
			return w.mutate(opCommentEdit, func() { w.comments[index].Body = body })
		}
	}
	return fmt.Errorf("comment %q does not exist", id)
}

// FindPullRequest returns the pull request of one branch pair.
func (w *effectWorld) FindPullRequest(_ context.Context, _ tracker.Repository, head, base string) (codehost.PullRequest, error) {
	for _, pull := range w.pulls {
		if pull.HeadBranch == head && pull.BaseBranch == base {
			return pull, nil
		}
	}
	return codehost.PullRequest{}, nil
}

// CreatePullRequest opens one pull request.
func (w *effectWorld) CreatePullRequest(_ context.Context, _ tracker.Repository, request codehost.PullRequestRequest) (codehost.PullRequest, error) {
	pull := pullRequestFrom(10+len(w.pulls), request)
	if err := w.mutate(opPullCreate, func() { w.pulls = append(w.pulls, pull) }); err != nil {
		return codehost.PullRequest{}, err
	}
	return pull, nil
}

// UpdatePullRequest replaces the fields of one pull request.
func (w *effectWorld) UpdatePullRequest(_ context.Context, _ tracker.Repository, number int, request codehost.PullRequestRequest) (codehost.PullRequest, error) {
	for index := range w.pulls {
		if w.pulls[index].Number == number {
			updated := pullRequestFrom(number, request)
			if err := w.mutate(opPullUpdate, func() { w.pulls[index] = updated }); err != nil {
				return codehost.PullRequest{}, err
			}
			return updated, nil
		}
	}
	return codehost.PullRequest{}, fmt.Errorf("pull request #%d does not exist", number)
}

// pullRequestFrom projects a request onto a code-host pull request.
func pullRequestFrom(number int, request codehost.PullRequestRequest) codehost.PullRequest {
	return codehost.PullRequest{
		Number: number, URL: fmt.Sprintf("https://example.test/pull/%d", number), Title: request.Title,
		Body: request.Body, Draft: request.Draft, HeadBranch: request.HeadBranch, BaseBranch: request.BaseBranch,
	}
}

// CreateCommitStatus publishes one exact-SHA status.
func (w *effectWorld) CreateCommitStatus(_ context.Context, _ tracker.Repository, status codehost.CommitStatus) error {
	return w.mutate(opCommitStatus, func() { w.statuses = append(w.statuses, status) })
}

// ListCommitStatuses returns the statuses of one commit.
func (w *effectWorld) ListCommitStatuses(_ context.Context, _ tracker.Repository, sha string) ([]codehost.CommitStatus, error) {
	var statuses []codehost.CommitStatus
	for _, status := range w.statuses {
		if status.SHA == sha {
			statuses = append(statuses, status)
		}
	}
	return statuses, nil
}

// statusPublisherOnly is a code host that cannot list commit statuses.
type statusPublisherOnly struct{ world *effectWorld }

// CreateCommitStatus publishes one exact-SHA status.
func (p statusPublisherOnly) CreateCommitStatus(ctx context.Context, repository tracker.Repository, status codehost.CommitStatus) error {
	return p.world.CreateCommitStatus(ctx, repository, status)
}

// Create is outside the journal's authority.
func (w *effectWorld) Create(context.Context, string, string, string) (gitadapter.Workspace, error) {
	return gitadapter.Workspace{}, errors.New("unexpected worktree creation")
}

// Remove is outside the journal's authority.
func (w *effectWorld) Remove(context.Context, string, gitadapter.Workspace) error {
	return errors.New("unexpected worktree removal")
}

// SynchronizeBase is outside the journal's authority.
func (w *effectWorld) SynchronizeBase(context.Context, gitadapter.BaseSyncRequest) error {
	return errors.New("unexpected base synchronization")
}

// Inspect returns the local worktree head.
func (w *effectWorld) Inspect(context.Context, string) (gitadapter.WorktreeState, error) {
	return gitadapter.WorktreeState{HeadSHA: w.localHead}, nil
}

// CreateCheckpoint commits once per checkpoint marker and recognizes a
// commit that an earlier attempt already created, like the Git adapter.
func (w *effectWorld) CreateCheckpoint(_ context.Context, request gitadapter.CheckpointRequest) (gitadapter.CheckpointResult, error) {
	marker := request.RunID + "\x00" + request.ParentSHA + "\x00" + string(request.Kind) + "\x00" + strings.Join(request.Paths, "\x00")
	if sha, exists := w.checkpoints[marker]; exists {
		return gitadapter.CheckpointResult{SHA: sha}, nil
	}
	sha := commitSHA(100 + w.commits)
	err := w.mutate(opCheckpoint, func() {
		w.commits++
		w.checkpoints[marker] = sha
		w.localHead = sha
	})
	if err != nil {
		return gitadapter.CheckpointResult{}, err
	}
	return gitadapter.CheckpointResult{SHA: sha, Created: true}, nil
}

// RemoteBranchHead returns the remote branch head.
func (w *effectWorld) RemoteBranchHead(context.Context, gitadapter.PushRequest) (string, error) {
	return w.remoteHead, nil
}

// Push publishes the local head to the remote branch.
func (w *effectWorld) Push(context.Context, gitadapter.PushRequest) error {
	return w.mutate(opPush, func() { w.remoteHead = w.localHead })
}

// Start launches one worker, reuses a worker with the exact same identity,
// and refuses a worker whose identity conflicts, like the Docker runtime.
func (w *effectWorld) Start(_ context.Context, request worker.StartRequest) error {
	key := workerIdentity(request)
	if existing, exists := w.workers[key]; exists {
		if !reflect.DeepEqual(existing, request) {
			return fmt.Errorf("worker %q already runs another invocation", key)
		}
		return nil
	}
	return w.mutate(opWorkerLaunch, func() { w.workers[key] = request })
}

// workerIdentity is the container identity of one worker launch.
func workerIdentity(request worker.StartRequest) string {
	return request.RunID + "\x00" + request.WorkerID
}

// StopWorker stops one worker; stopping is idempotent.
func (w *effectWorld) StopWorker(context.Context, string) error { return nil }

// StopActiveWorkers stops the run's workers; stopping is idempotent.
func (w *effectWorld) StopActiveWorkers(context.Context, effect.RunStore, store.Run) error {
	return nil
}

// HarnessRuntime returns the world's native harness.
func (w *effectWorld) HarnessRuntime(string) (harness.Runtime, error) { return w, nil }

// Capabilities identifies a headless harness with native resume.
func (w *effectWorld) Capabilities() harness.Capabilities {
	return harness.Capabilities{Name: "codex", Headless: true, NativeResume: true}
}

// StartHeadless is outside the journal's authority.
func (w *effectWorld) StartHeadless(context.Context, harness.StartRequest) (harness.Session, error) {
	return harness.Session{}, errors.New("unexpected native start")
}

// ResumeHeadless continues one native session.
func (w *effectWorld) ResumeHeadless(_ context.Context, request harness.StartRequest) (harness.Session, error) {
	if err := w.mutate(opResume, func() {}); err != nil {
		return harness.Session{}, err
	}
	return harness.Session{InvocationID: request.InvocationID, RunID: request.RunID, NativeSessionID: request.ResumeSessionID}, nil
}

// FinishHeadless releases one native session.
func (w *effectWorld) FinishHeadless(context.Context, harness.Session) error {
	return w.mutate(opFinish, func() {})
}

// contractPresentation renders marker-carrying issue projections.
type contractPresentation struct{}

// StatusCommentMarker identifies the run's status comment.
func (contractPresentation) StatusCommentMarker(runID string) string {
	return "<!-- factory-status:" + runID + " -->"
}

// StatusCommentBody renders the marker, status, and revision of a run.
func (p contractPresentation) StatusCommentBody(run store.Run) string {
	return fmt.Sprintf("%s\nstatus=%s revision=%d", p.StatusCommentMarker(run.ID), run.Status, run.Revision)
}

// ClarificationCommentMarker identifies one clarification round.
func (contractPresentation) ClarificationCommentMarker(runID string, packetVersion int) string {
	return fmt.Sprintf("<!-- factory-clarification:%s:%d -->", runID, packetVersion)
}

// StateLabels keeps ordinary labels and replaces the one state label.
func (contractPresentation) StateLabels(existing []string, status store.Status) []string {
	var labels []string
	for _, label := range existing {
		if !strings.HasPrefix(label, "state:") {
			labels = append(labels, label)
		}
	}
	return append(labels, "state:"+string(status))
}

// storeProjector persists run projections in the SQLite store.
type storeProjector struct{ store *store.Store }

// Read returns the active run, or the latest terminal run.
func (p storeProjector) Read(ctx context.Context, _ effect.RunStore) (*store.Run, error) {
	run, err := p.store.CurrentRun(ctx)
	if err != nil || run != nil {
		return run, err
	}
	return p.store.LatestRun(ctx)
}

// Save persists a run projection.
func (p storeProjector) Save(ctx context.Context, _ effect.RunStore, run store.Run) error {
	return p.store.SaveRun(ctx, run)
}

// SaveAtRevision persists a run projection with compare-and-set.
func (p storeProjector) SaveAtRevision(ctx context.Context, _ effect.RunStore, expected int64, run store.Run) error {
	return p.store.SaveRunIfRevision(ctx, expected, run)
}

// SaveInvalidatingResults persists and invalidates atomically.
func (p storeProjector) SaveInvalidatingResults(ctx context.Context, _ effect.RunStore, expected int64, run store.Run) (bool, error) {
	return true, p.store.SaveRunAndInvalidateResults(ctx, expected, run)
}

// SaveInvalidatingAllResults persists and invalidates every result atomically.
func (p storeProjector) SaveInvalidatingAllResults(ctx context.Context, _ effect.RunStore, expected int64, run store.Run) (bool, error) {
	return true, p.store.SaveRunAndInvalidateAllResults(ctx, expected, run)
}

// InvalidateResults is unused because the store is atomic.
func (storeProjector) InvalidateResults(context.Context, effect.RunStore, string) error { return nil }

// InvalidateAllResults is unused because the store is atomic.
func (storeProjector) InvalidateAllResults(context.Context, effect.RunStore, string) error {
	return nil
}

// RecordTransition records no evaluation projection.
func (storeProjector) RecordTransition(context.Context, effect.RunStore, store.Run, store.Run, time.Time) error {
	return nil
}

// effectContract describes one durable effect for the failure-injection
// contract: how to apply it, which external mutation it owns, and how the
// world changes so that a replay is no longer safe.
type effectContract struct {
	name string
	kind store.PendingEffectKind
	// operation is the external mutation the effect must apply exactly once.
	operation string
	// prepare optionally adjusts the world before the effect is applied.
	prepare func(*effectWorld)
	// apply runs the effect through the journal's public apply path.
	apply func(context.Context, *effectWorld) error
	// makeUnsafe changes the world so that replaying the effect is unsafe.
	makeUnsafe func(*effectWorld)
	// workflowOwned reports that the unsafe replay is a workflow conflict.
	workflowOwned bool
	// verify asserts the durable run and the external world that the
	// completed effect leaves behind.
	verify func(*effectWorld)
}

// effectContracts lists every durable effect kind the journal applies. The
// harness resume kind has its own contract test because it deliberately
// never repeats a native resume after an ambiguous failure.
func effectContracts() []effectContract {
	return []effectContract{
		{
			name: "state transition", kind: store.PendingEffectKindStateTransition, operation: opLabels,
			verify: verifyStateTransition,
			apply: func(ctx context.Context, w *effectWorld) error {
				next := advanced(w.run, store.StatusWaitingForHuman)
				_, err := w.journal().ApplyStateTransition(ctx, w.store, effect.StateTransition{
					Repository: contractRepository, Issue: w.issue, Previous: w.run, Next: next, StopWorker: true,
				}, next)
				return err
			},
			makeUnsafe: (*effectWorld).supersede, workflowOwned: true,
		},
		{
			name: "issue projection creating the status comment", kind: store.PendingEffectKindStateTransition, operation: opCommentCreate,
			verify:  verifyCreatedStatusComment,
			prepare: func(w *effectWorld) { w.comments = nil },
			apply: func(ctx context.Context, w *effectWorld) error {
				next := advanced(w.run, store.StatusActive)
				next.StatusCommentID = ""
				_, err := w.journal().ApplyStateTransition(ctx, w.store, effect.StateTransition{
					Repository: contractRepository, Issue: w.issue, Previous: w.run, Next: next,
					CreateComment: true, InvalidateResults: true,
				}, next)
				return err
			},
			makeUnsafe: (*effectWorld).supersede, workflowOwned: true,
		},
		{
			name: "status comment", kind: store.PendingEffectKindStatusComment, operation: opCommentEdit,
			verify: verifyCommandProjection,
			apply: func(ctx context.Context, w *effectWorld) error {
				next := advanced(w.run, store.StatusActive)
				next.ProcessedCommentID = "command-8"
				_, err := w.journal().PersistCommandProjection(ctx, w.store, contractRepository, w.run, next)
				return err
			},
			makeUnsafe: (*effectWorld).supersede, workflowOwned: true,
		},
		{
			name: "clarification comment", kind: store.PendingEffectKindClarificationComment, operation: opCommentCreate,
			verify: verifyClarificationComment,
			apply: func(ctx context.Context, w *effectWorld) error {
				body := contractPresentation{}.ClarificationCommentMarker(w.run.ID, 2) + "\nWhich API version?"
				_, err := w.journal().PublishClarificationComment(ctx, w.store, contractRepository, w.run.IssueNumber, w.run, 2, body)
				return err
			},
			makeUnsafe: func(w *effectWorld) { w.armWith(opCommentCreate, faultBefore, errInjected, failForever) },
		},
		{
			name: "label transition", kind: store.PendingEffectKindLabelTransition, operation: opLabels,
			verify: verifyLabelTransition,
			apply: func(ctx context.Context, w *effectWorld) error {
				return w.journal().ApplyLabels(ctx, w.store, w.run.ID, contractRepository, w.run.IssueNumber, []string{"ordinary", "agent-failed"})
			},
			makeUnsafe: func(w *effectWorld) { w.armWith(opLabels, faultBefore, errInjected, failForever) },
		},
		{
			name: "commit status", kind: store.PendingEffectKindCommitStatus, operation: opCommitStatus,
			verify: verifyCommitStatus,
			apply: func(ctx context.Context, w *effectWorld) error {
				publisher := w.journal().CommitStatusPublisher(w.store, w.run.ID, w)
				return publisher.CreateCommitStatus(ctx, contractRepository, codehost.CommitStatus{
					SHA: w.run.CheckpointSHA, State: codehost.CommitStatusSuccess, Context: "factory/test", Description: "passed",
				})
			},
			makeUnsafe: func(w *effectWorld) { w.hideStatuses = true },
		},
		{
			name: "push", kind: store.PendingEffectKindPush, operation: opPush,
			verify:  verifyPush,
			prepare: func(w *effectWorld) { w.localHead = commitSHA(2) },
			apply: func(ctx context.Context, w *effectWorld) error {
				return w.journal().Push(ctx, w.store, w.run.ID, gitadapter.PushRequest{WorktreePath: w.run.Worktree, Branch: w.run.Branch}, commitSHA(2))
			},
			makeUnsafe: func(w *effectWorld) { w.localHead = commitSHA(3) },
		},
		{
			name: "checkpoint", kind: store.PendingEffectKindCheckpoint, operation: opCheckpoint,
			verify: verifyCheckpoint,
			apply: func(ctx context.Context, w *effectWorld) error {
				_, _, err := w.journal().Checkpoint(ctx, w.store, gitadapter.CheckpointRequest{
					RunID: w.run.ID, WorktreePath: w.run.Worktree, ParentSHA: w.run.CheckpointSHA,
					Kind: gitadapter.CheckpointKindTest, Paths: []string{"a_test.go"}, Message: "checkpoint",
				}, contractRepository, w.issue, w.run, advanced(w.run, store.StatusActive))
				return err
			},
			makeUnsafe: (*effectWorld).supersede, workflowOwned: true,
		},
		{
			name: "pull request creation", kind: store.PendingEffectKindPullRequest, operation: opPullCreate,
			verify: verifyCreatedPullRequest,
			apply: func(ctx context.Context, w *effectWorld) error {
				_, _, err := w.journal().UpsertPullRequestAndPersist(ctx, w.store, contractRepository, w.issue, w.run,
					advanced(w.run, store.StatusActive), contractPullRequest("body"), 0)
				return err
			},
			makeUnsafe: (*effectWorld).supersede, workflowOwned: true,
		},
		{
			name: "pull request update", kind: store.PendingEffectKindPullRequest, operation: opPullUpdate,
			verify: verifyUpdatedPullRequest,
			prepare: func(w *effectWorld) {
				w.pulls = []codehost.PullRequest{pullRequestFrom(11, contractPullRequest("old"))}
			},
			apply: func(ctx context.Context, w *effectWorld) error {
				return w.journal().UpdatePullRequest(ctx, w.store, w.run.ID, contractRepository, 11, contractPullRequest("new"))
			},
			makeUnsafe: func(w *effectWorld) {
				w.pulls = []codehost.PullRequest{pullRequestFrom(12, contractPullRequest("old"))}
			},
		},
		{
			name: "worker launch", kind: store.PendingEffectKindWorkerLaunch, operation: opWorkerLaunch,
			verify: verifyWorkerLaunch,
			apply: func(ctx context.Context, w *effectWorld) error {
				return w.journal().StartWorker(ctx, w.store, contractWorkerRequest(w.run.ID, "/invocation"))
			},
			makeUnsafe: func(w *effectWorld) {
				conflicting := contractWorkerRequest(w.run.ID, "/another-invocation")
				w.workers[workerIdentity(conflicting)] = conflicting
			},
		},
		{
			name: "result acceptance", kind: store.PendingEffectKindResultAcceptance, operation: opLabels,
			verify:  verifyResultAcceptance,
			prepare: func(w *effectWorld) { w.saveInvocation(contractInvocation(w.run.ID)) },
			apply: func(ctx context.Context, w *effectWorld) error {
				accepted := contractInvocation(w.run.ID)
				accepted.Status = store.InvocationStatusCompleted
				_, _, err := w.journal().AcceptResult(ctx, w.store, w.store, effect.ResultAcceptance{
					Repository: contractRepository, Harness: w, StopWorker: true,
					Session:    harness.Session{InvocationID: accepted.ID, NativeSessionID: accepted.NativeSessionID},
					Invocation: accepted, Previous: w.run, Next: advanced(w.run, store.StatusWaitingForHuman),
					Report: report.Report{SchemaVersion: report.SchemaVersion, InvocationID: accepted.ID, RunID: w.run.ID},
				})
				return err
			},
			makeUnsafe: (*effectWorld).supersede, workflowOwned: true,
		},
	}
}

// require fails the test with the observed value when ok is false.
func (w *effectWorld) require(ok bool, what string, observed any) {
	w.t.Helper()
	if !ok {
		w.t.Fatalf("%s: observed %#v", what, observed)
	}
}

// commentsWith returns every comment that carries marker.
func (w *effectWorld) commentsWith(marker string) []tracker.Comment {
	var found []tracker.Comment
	for _, comment := range w.comments {
		if strings.Contains(comment.Body, marker) {
			found = append(found, comment)
		}
	}
	return found
}

// requireAdvancedRun requires the persisted run at the next revision with
// status, and returns it.
func (w *effectWorld) requireAdvancedRun(status store.Status) store.Run {
	w.t.Helper()
	run := w.persisted()
	w.require(run.Revision == w.run.Revision+1 && run.Status == status, "persisted run revision and status", run)
	return run
}

// requireIssueProjection requires one status comment that renders run and
// the state label of its status.
func (w *effectWorld) requireIssueProjection(run store.Run) {
	w.t.Helper()
	presentation := contractPresentation{}
	comments := w.commentsWith(presentation.StatusCommentMarker(run.ID))
	w.require(len(comments) == 1 && comments[0].ID == run.StatusCommentID && comments[0].Body == presentation.StatusCommentBody(run),
		"status comment of the persisted run", comments)
	w.require(reflect.DeepEqual(w.issue.Labels, []string{"ordinary", "state:" + string(run.Status)}), "issue labels", w.issue.Labels)
}

// verifyStateTransition requires the waiting run and its issue projection.
func verifyStateTransition(w *effectWorld) {
	w.requireIssueProjection(w.requireAdvancedRun(store.StatusWaitingForHuman))
}

// verifyCreatedStatusComment requires the run to record the one status
// comment the transition created.
func verifyCreatedStatusComment(w *effectWorld) {
	run := w.requireAdvancedRun(store.StatusActive)
	w.require(run.StatusCommentID != "", "persisted status comment identity", run.StatusCommentID)
	w.requireIssueProjection(run)
}

// verifyCommandProjection requires the command watermark and its status
// comment.
func verifyCommandProjection(w *effectWorld) {
	run := w.requireAdvancedRun(store.StatusActive)
	w.require(run.ProcessedCommentID == "command-8", "processed command watermark", run.ProcessedCommentID)
	w.requireIssueProjection(run)
}

// verifyClarificationComment requires one question comment and its identity
// on the run.
func verifyClarificationComment(w *effectWorld) {
	comments := w.commentsWith(contractPresentation{}.ClarificationCommentMarker(w.run.ID, 2))
	w.require(len(comments) == 1, "clarification comments", comments)
	run := w.persisted()
	w.require(run.ClarificationCommentID == comments[0].ID, "persisted clarification comment identity", run.ClarificationCommentID)
}

// verifyLabelTransition requires the complete replacement label set.
func verifyLabelTransition(w *effectWorld) {
	w.require(reflect.DeepEqual(w.issue.Labels, []string{"ordinary", "agent-failed"}), "issue labels", w.issue.Labels)
}

// verifyCommitStatus requires exactly the one published status.
func verifyCommitStatus(w *effectWorld) {
	want := []codehost.CommitStatus{{SHA: w.run.CheckpointSHA, State: codehost.CommitStatusSuccess, Context: "factory/test", Description: "passed"}}
	w.require(reflect.DeepEqual(w.statuses, want), "commit statuses", w.statuses)
}

// verifyPush requires the remote branch at the pushed checkpoint.
func verifyPush(w *effectWorld) {
	w.require(w.remoteHead == commitSHA(2), "remote branch head", w.remoteHead)
}

// verifyCheckpoint requires the run to record the one checkpoint commit as
// its current and its test checkpoint.
func verifyCheckpoint(w *effectWorld) {
	run := w.requireAdvancedRun(store.StatusActive)
	w.require(codehost.ValidCommitSHA(w.localHead) && w.localHead != w.run.CheckpointSHA, "worktree head", w.localHead)
	w.require(run.CheckpointSHA == w.localHead && run.TestCheckpointSHA == w.localHead, "persisted checkpoint identities", run)
	w.requireIssueProjection(run)
}

// verifyCreatedPullRequest requires one pull request and its identity on the
// run.
func verifyCreatedPullRequest(w *effectWorld) {
	run := w.requireAdvancedRun(store.StatusActive)
	w.require(len(w.pulls) == 1 && w.pulls[0].Body == "body", "pull requests", w.pulls)
	w.require(run.PullRequestNumber == w.pulls[0].Number && run.PullRequestURL == w.pulls[0].URL, "persisted pull request identity", run)
	w.requireIssueProjection(run)
}

// verifyUpdatedPullRequest requires the same pull request with the new body.
func verifyUpdatedPullRequest(w *effectWorld) {
	w.require(len(w.pulls) == 1 && w.pulls[0].Number == 11 && w.pulls[0].Body == "new", "pull requests", w.pulls)
}

// verifyWorkerLaunch requires the one launched worker of the request.
func verifyWorkerLaunch(w *effectWorld) {
	request := contractWorkerRequest(w.run.ID, "/invocation")
	w.require(len(w.workers) == 1 && reflect.DeepEqual(w.workers[workerIdentity(request)], request), "launched workers", w.workers)
}

// verifyResultAcceptance requires the accepted invocation and the waiting
// run with its issue projection.
func verifyResultAcceptance(w *effectWorld) {
	invocation := w.invocation("inv-contract")
	w.require(invocation.Status == store.InvocationStatusCompleted, "accepted invocation status", invocation.Status)
	w.requireIssueProjection(w.requireAdvancedRun(store.StatusWaitingForHuman))
}

// contractPullRequest returns the draft pull-request request of the run.
func contractPullRequest(body string) codehost.PullRequestRequest {
	return codehost.PullRequestRequest{Title: "Fix #42", Body: body, HeadBranch: "factory/run-contract", BaseBranch: "main", Draft: true}
}

// contractWorkerRequest returns a run-scoped worker launch request.
func contractWorkerRequest(runID, invocationPath string) worker.StartRequest {
	return worker.StartRequest{RunID: runID, Role: "implementation", InvocationPath: invocationPath, ResultPath: "/result"}
}

// contractInvocation returns the active implementation invocation of a run.
func contractInvocation(runID string) store.Invocation {
	return store.Invocation{
		ID: "inv-contract", RunID: runID, Harness: "codex", Role: "implementation", Stage: store.StageImplementation,
		Status: store.InvocationStatusActive, NativeSessionID: "native-contract",
		CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC(),
	}
}

// saveInvocation persists one invocation in the SQLite store.
func (w *effectWorld) saveInvocation(invocation store.Invocation) {
	w.t.Helper()
	if err := w.store.SaveInvocation(context.Background(), invocation); err != nil {
		w.t.Fatalf("save invocation: %v", err)
	}
}

// invocation reads one invocation from the SQLite store.
func (w *effectWorld) invocation(id string) store.Invocation {
	w.t.Helper()
	invocation, err := w.store.Invocation(context.Background(), w.run.ID, id)
	if err != nil || invocation == nil {
		w.t.Fatalf("read invocation = %v, %v", invocation, err)
	}
	return *invocation
}

// interrupt applies the contract's effect with one armed failure and returns
// the journal entry the failure left behind.
func (c effectContract) interrupt(t *testing.T, w *effectWorld, phase faultPhase) store.PendingEffect {
	t.Helper()
	if c.prepare != nil {
		c.prepare(w)
	}
	w.arm(c.operation, phase)
	if err := c.apply(context.Background(), w); !errors.Is(err, errInjected) {
		t.Fatalf("apply error = %v, want the injected failure", err)
	}
	pending := w.pending()
	if pending == nil || pending.Kind != c.kind {
		t.Fatalf("journal after the failure = %#v, want one %s entry", pending, c.kind)
	}
	return *pending
}

// TestEveryEffectReplaysExactlyOnceAfterAnInjectedFailure injects a failure
// before the external call, after it, and during the first replay, then
// verifies that recovery leaves exactly one external mutation and an empty
// journal.
func TestEveryEffectReplaysExactlyOnceAfterAnInjectedFailure(t *testing.T) {
	ctx := context.Background()
	for _, contract := range effectContracts() {
		t.Run(contract.name, func(t *testing.T) {
			t.Run("no failure", func(t *testing.T) {
				w := newEffectWorld(t)
				if contract.prepare != nil {
					contract.prepare(w)
				}
				if err := contract.apply(ctx, w); err != nil {
					t.Fatalf("apply error = %v", err)
				}
				w.assertJournalEmpty()
				w.assertApplied(contract.operation, 1)
				contract.verify(w)
			})
			t.Run("failure before the external call", func(t *testing.T) {
				w := newEffectWorld(t)
				pending := contract.interrupt(t, w, faultBefore)
				w.assertApplied(contract.operation, 0)
				w.replay(ctx, pending)
				w.assertApplied(contract.operation, 1)
				contract.verify(w)
			})
			t.Run("failure after the external call", func(t *testing.T) {
				w := newEffectWorld(t)
				pending := contract.interrupt(t, w, faultAfter)
				w.assertApplied(contract.operation, 1)
				w.replay(ctx, pending)
				w.assertApplied(contract.operation, 1)
				contract.verify(w)
			})
			t.Run("failure during replay", func(t *testing.T) {
				w := newEffectWorld(t)
				pending := contract.interrupt(t, w, faultBefore)
				w.arm(contract.operation, faultBefore)
				if _, err := w.journal().Replay(ctx, w.store, pending); !errors.Is(err, errInjected) {
					t.Fatalf("first Replay() error = %v, want the injected failure", err)
				}
				w.assertStillPending(pending)
				w.assertApplied(contract.operation, 0)
				w.replay(ctx, pending)
				w.assertApplied(contract.operation, 1)
				contract.verify(w)
			})
		})
	}
}

// TestEveryEffectRefusesAnUnsafeReplay verifies that a replay which can no
// longer be applied safely mutates nothing, keeps its journal entry for the
// coordinator to pause on, and reports a workflow-owned conflict as a typed
// workflow projection error.
func TestEveryEffectRefusesAnUnsafeReplay(t *testing.T) {
	ctx := context.Background()
	for _, contract := range effectContracts() {
		t.Run(contract.name, func(t *testing.T) {
			w := newEffectWorld(t)
			pending := contract.interrupt(t, w, faultBefore)
			contract.makeUnsafe(w)
			_, err := w.journal().Replay(ctx, w.store, pending)
			if err == nil {
				t.Fatal("Replay() error = nil, want a refusal")
			}
			if got := effect.IsWorkflowProjectionError(err); got != contract.workflowOwned {
				t.Fatalf("IsWorkflowProjectionError(%v) = %t, want %t", err, got, contract.workflowOwned)
			}
			w.assertStillPending(pending)
			w.assertApplied(contract.operation, 0)
		})
	}
}

// replay drains one journal entry and requires an empty journal afterwards.
func (w *effectWorld) replay(ctx context.Context, pending store.PendingEffect) {
	w.t.Helper()
	returned, err := w.journal().Replay(ctx, w.store, pending)
	if err != nil {
		w.t.Fatalf("Replay() error = %v", err)
	}
	w.assertJournalEmpty()
	persisted := w.persisted()
	w.require(sameRunProjection(returned, persisted), "replayed run against the persisted run "+fmt.Sprintf("%#v", persisted), returned)
	for operation, count := range w.applied {
		if count > 1 {
			w.t.Fatalf("%s applied %d times after replay, want at most once", operation, count)
		}
	}
}

// sameRunProjection compares the run fields that durable effects write.
func sameRunProjection(left, right store.Run) bool {
	return left.ID == right.ID && left.Revision == right.Revision && left.Status == right.Status &&
		left.CheckpointSHA == right.CheckpointSHA && left.TestCheckpointSHA == right.TestCheckpointSHA &&
		left.PullRequestNumber == right.PullRequestNumber && left.PullRequestURL == right.PullRequestURL &&
		left.StatusCommentID == right.StatusCommentID && left.ClarificationCommentID == right.ClarificationCommentID &&
		left.ProcessedCommentID == right.ProcessedCommentID
}

// assertApplied requires an exact count of one external mutation.
func (w *effectWorld) assertApplied(operation string, want int) {
	w.t.Helper()
	if got := w.applied[operation]; got != want {
		w.t.Fatalf("%s applied %d times, want %d (all mutations: %v)", operation, got, want, w.applied)
	}
}

// assertJournalEmpty requires that no journal entry remains for the run.
func (w *effectWorld) assertJournalEmpty() {
	w.t.Helper()
	if left := w.pending(); left != nil {
		w.t.Fatalf("journal = %#v, want empty", left)
	}
}

// assertStillPending requires the journal to keep the same entry.
func (w *effectWorld) assertStillPending(want store.PendingEffect) {
	w.t.Helper()
	if got := w.pending(); got == nil || got.ID != want.ID {
		w.t.Fatalf("journal = %#v, want the retained entry %q", got, want.ID)
	}
}

// errRateLimited is a harness refusal that happens before the native
// continuation boundary.
var errRateLimited = errors.New("status 429: rate limit reached")

// resumeModes lists the automatic and the operator-requested native resume.
var resumeModes = []struct {
	name   string
	manual bool
}{{name: "automatic"}, {name: "manual", manual: true}}

// resume applies one native resume of the contract invocation.
func (w *effectWorld) resume(ctx context.Context, manual bool) error {
	invocation := w.invocation("inv-contract")
	operation := effect.HarnessResume{Runtime: w, Invocation: invocation, Request: harness.StartRequest{
		InvocationID: invocation.ID, RunID: w.run.ID, Role: invocation.Role, ResumeSessionID: invocation.NativeSessionID,
	}}
	var err error
	if manual {
		_, err = w.journal().ResumeHarnessManually(ctx, w.store, w.store, operation)
	} else {
		_, err = w.journal().ResumeHarness(ctx, w.store, w.store, operation)
	}
	return err
}

// interruptResume applies one native resume with an armed harness failure
// and returns the journal entry it left behind.
func (w *effectWorld) interruptResume(ctx context.Context, manual bool, phase faultPhase) store.PendingEffect {
	w.t.Helper()
	w.saveInvocation(contractInvocation(w.run.ID))
	w.arm(opResume, phase)
	if err := w.resume(ctx, manual); !errors.Is(err, errInjected) {
		w.t.Fatalf("resume error = %v, want the injected failure", err)
	}
	pending := w.pending()
	if pending == nil || pending.Kind != store.PendingEffectKindHarnessResume {
		w.t.Fatalf("journal after the failure = %#v, want one harness_resume entry", pending)
	}
	return *pending
}

// requireResumeGeneration requires the persisted invocation to record want
// consumed resumes of the mode and to keep its native session.
func (w *effectWorld) requireResumeGeneration(manual bool, want int) {
	w.t.Helper()
	invocation := w.invocation("inv-contract")
	automatic, operator := want, 0
	if manual {
		automatic, operator = 0, want
	}
	w.require(invocation.RecoveryResumeCount == automatic && invocation.ManualResumeCount == operator &&
		invocation.NativeSessionID == "native-contract", "persisted resume generation", invocation)
}

// TestHarnessResumeFailureInjection verifies the native-resume contract. The
// resume generation is reserved before the native call. A replay resumes
// exactly once when the process stopped before that reservation was stored,
// and never resumes a second time once the generation is consumed. That
// ambiguous state resumes zero times here; the coordinator pauses on it.
func TestHarnessResumeFailureInjection(t *testing.T) {
	ctx := context.Background()
	for _, mode := range resumeModes {
		t.Run(mode.name, func(t *testing.T) {
			t.Run("failure after the native call", func(t *testing.T) {
				w := newEffectWorld(t)
				pending := w.interruptResume(ctx, mode.manual, faultAfter)
				w.replay(ctx, pending)
				w.assertApplied(opResume, 1)
				w.requireResumeGeneration(mode.manual, 1)
			})
			t.Run("stop before the generation reservation", func(t *testing.T) {
				w := newEffectWorld(t)
				pending := w.interruptResume(ctx, mode.manual, faultBefore)
				// Restore the unreserved invocation: the process stopped
				// after the journal write and before the generation write.
				w.saveInvocation(contractInvocation(w.run.ID))
				w.replay(ctx, pending)
				w.assertApplied(opResume, 1)
				w.requireResumeGeneration(mode.manual, 1)
			})
			t.Run("ambiguous failure after the generation reservation", func(t *testing.T) {
				w := newEffectWorld(t)
				pending := w.interruptResume(ctx, mode.manual, faultBefore)
				w.replay(ctx, pending)
				w.assertApplied(opResume, 0)
				w.requireResumeGeneration(mode.manual, 1)
			})
			t.Run("failure during replay", func(t *testing.T) {
				w := newEffectWorld(t)
				pending := w.interruptResume(ctx, mode.manual, faultBefore)
				// Restore the unreserved invocation so the replay must
				// reserve the generation and resume itself.
				w.saveInvocation(contractInvocation(w.run.ID))
				w.arm(opResume, faultBefore)
				if _, err := w.journal().Replay(ctx, w.store, pending); !errors.Is(err, errInjected) {
					t.Fatalf("first Replay() error = %v, want the injected failure", err)
				}
				w.assertStillPending(pending)
				w.replay(ctx, pending)
				w.assertApplied(opResume, 0)
				w.requireResumeGeneration(mode.manual, 1)
			})
			t.Run("refusal before the native boundary", func(t *testing.T) {
				w := newEffectWorld(t)
				w.saveInvocation(contractInvocation(w.run.ID))
				w.armWith(opResume, faultBefore, errRateLimited, 1)
				if err := w.resume(ctx, mode.manual); !harness.IsRateLimited(err) {
					t.Fatalf("resume error = %v, want a typed rate limit", err)
				}
				w.assertJournalEmpty()
				if err := w.resume(ctx, mode.manual); err != nil {
					t.Fatalf("retried resume error = %v", err)
				}
				w.assertApplied(opResume, 1)
				w.requireResumeGeneration(mode.manual, 1)
			})
			t.Run("refusal before the native boundary during replay", func(t *testing.T) {
				w := newEffectWorld(t)
				pending := w.interruptResume(ctx, mode.manual, faultBefore)
				original := contractInvocation(w.run.ID)
				w.saveInvocation(original)
				w.armWith(opResume, faultBefore, errRateLimited, 1)
				if _, err := w.journal().Replay(ctx, w.store, pending); !harness.IsRateLimited(err) {
					t.Fatalf("Replay() error = %v, want a typed rate limit", err)
				}
				w.assertJournalEmpty()
				got := w.invocation(original.ID)
				if got.RecoveryResumeCount != 0 || got.ManualResumeCount != 0 {
					t.Fatalf("invocation after the refusal = %#v, want the generation rolled back", got)
				}
				w.assertApplied(opResume, 0)
			})
		})
	}
}
