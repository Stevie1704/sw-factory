package factory_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// unsafeReplay is one journal entry that reconciliation cannot replay
// safely, with the typed discrepancy the pause must report.
type unsafeReplay struct {
	name string
	kind store.PendingEffectKind
	// payload builds the journal payload against the persisted run.
	payload func(run store.Run) any
	// seed optionally persists more state before reconciliation.
	seed func(context.Context, *testing.T, *store.Store, store.Run)
	// source and field identify the expected discrepancy.
	source, field string
	// discrepancyKind is the expected discrepancy type.
	discrepancyKind factory.RecoveryDiscrepancyKind
	// retained reports that the journal entry stays for the operator.
	retained bool
	// adapt optionally makes an adapter refuse the replayed mutation.
	adapt func(*pauseAdapters)
}

// pauseAdapters are the external adapters reconciliation replays through.
type pauseAdapters struct {
	github  *fakeGitHub
	tracker tracker.Client
	worker  worker.WorkerRuntime
	// refusals counts the replayed mutations an adapter refused.
	refusals int
}

// stalePayload returns a transition payload whose revisions are older than
// the persisted run, which a replay must refuse as a workflow conflict.
func stalePayload(extra map[string]any) func(store.Run) any {
	return func(run store.Run) any {
		previous, next := run, run
		previous.Revision, next.Revision = 1, 2
		next.ProcessedCommentID = "command-stale"
		payload := map[string]any{"Previous": previous, "Next": next}
		for key, value := range extra {
			payload[key] = value
		}
		return payload
	}
}

// refusedBy makes adapt refuse the replayed mutation of one unsafe replay.
func refusedBy(replay unsafeReplay, adapt func(*pauseAdapters)) unsafeReplay {
	replay.adapt = adapt
	return replay
}

// unsafeReplays lists one unsafe journal entry for every durable effect kind.
func unsafeReplays() []unsafeReplay {
	workflow := func(name string, kind store.PendingEffectKind, extra map[string]any) unsafeReplay {
		return unsafeReplay{
			name: name, kind: kind, payload: stalePayload(extra), source: "pending effect", field: string(kind),
			discrepancyKind: factory.RecoveryDiscrepancyWorkflow, retained: true,
		}
	}
	infrastructure := func(name string, kind store.PendingEffectKind, payload func(store.Run) any) unsafeReplay {
		return unsafeReplay{
			name: name, kind: kind, payload: payload, source: "pending effect", field: string(kind),
			discrepancyKind: factory.RecoveryDiscrepancyInfrastructure, retained: true,
		}
	}
	repository := map[string]any{"Owner": "example", "Name": "project"}
	return []unsafeReplay{
		workflow("stale state transition", store.PendingEffectKindStateTransition, nil),
		workflow("superseded status comment", store.PendingEffectKindStatusComment, nil),
		workflow("stale checkpoint", store.PendingEffectKindCheckpoint, map[string]any{"Request": map[string]any{}}),
		workflow("stale pull request", store.PendingEffectKindPullRequest, map[string]any{"PersistRun": true}),
		workflow("stale result acceptance", store.PendingEffectKindResultAcceptance, nil),
		refusedBy(infrastructure("clarification comment that GitHub refuses", store.PendingEffectKindClarificationComment, func(run store.Run) any {
			return map[string]any{"Repository": repository, "Target": run.IssueNumber, "Body": "Which API version?", "PacketVersion": 1}
		}), func(adapters *pauseAdapters) {
			adapters.github.statusComment = tracker.Comment{}
			adapters.tracker = refusingTracker{fakeGitHub: adapters.github, refused: opRefuseComment, refusals: &adapters.refusals}
		}),
		refusedBy(infrastructure("label transition that GitHub refuses", store.PendingEffectKindLabelTransition, func(run store.Run) any {
			return map[string]any{"Repository": repository, "IssueNumber": run.IssueNumber, "Labels": []string{"ordinary", "agent-failed"}}
		}), func(adapters *pauseAdapters) {
			adapters.tracker = refusingTracker{fakeGitHub: adapters.github, refused: opRefuseLabels, refusals: &adapters.refusals}
		}),
		infrastructure("commit status without a status publisher", store.PendingEffectKindCommitStatus, func(run store.Run) any {
			return map[string]any{"Status": map[string]any{"SHA": run.CheckpointSHA, "Context": "factory/test", "State": "success"}}
		}),
		infrastructure("push of a checkpoint the worktree no longer holds", store.PendingEffectKindPush, func(run store.Run) any {
			return map[string]any{
				"Request":     map[string]any{"WorktreePath": run.Worktree, "Branch": run.Branch},
				"ExpectedSHA": "0000000000000000000000000000000000000abc",
			}
		}),
		refusedBy(infrastructure("worker launch with a conflicting worker identity", store.PendingEffectKindWorkerLaunch, func(run store.Run) any {
			return map[string]any{"Request": worker.StartRequest{RunID: run.ID, Role: "implementation", InvocationPath: "/invocation", ResultPath: "/result"}}
		}), func(adapters *pauseAdapters) {
			adapters.worker = &conflictingWorker{refusals: &adapters.refusals}
		}),
		{
			name: "consumed harness resume generation", kind: store.PendingEffectKindHarnessResume,
			payload: func(run store.Run) any {
				return map[string]any{
					"Invocation":        pausedReplayInvocation(run.ID, 0),
					"TargetResumeCount": 1,
					"Request":           harness.StartRequest{InvocationID: "inv-paused", RunID: run.ID, Role: "implementation"},
				}
			},
			seed: func(ctx context.Context, t *testing.T, opened *store.Store, run store.Run) {
				if err := opened.SaveInvocation(ctx, pausedReplayInvocation(run.ID, 1)); err != nil {
					t.Fatal(err)
				}
			},
			source: "harness", field: "native session resume",
			discrepancyKind: factory.RecoveryDiscrepancyInfrastructure,
		},
	}
}

// pausedReplayInvocation returns the active invocation of the paused run with
// the given consumed automatic resume generation.
func pausedReplayInvocation(runID string, resumes int) store.Invocation {
	return store.Invocation{
		ID: "inv-paused", RunID: runID, Harness: "codex", Role: "implementation", Stage: store.StageImplementation,
		Status: store.InvocationStatusActive, NativeSessionID: "native-paused", RecoveryResumeCount: resumes,
		CreatedAt: time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC),
	}
}

// TestReconcilePausesWithATypedDiscrepancyWhenAnEffectCannotBeReplayed
// verifies, for every durable effect kind, that reconciliation pauses the run
// for a human with a typed discrepancy instead of guessing, crosses no
// external mutation for the refused effect, and keeps a refused journal entry
// for the operator.
func TestReconcilePausesWithATypedDiscrepancyWhenAnEffectCannotBeReplayed(t *testing.T) {
	for _, test := range unsafeReplays() {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			if err := os.Chmod(root, 0o700); err != nil {
				t.Fatal(err)
			}
			worktreePath := filepath.Join(root, "worktree")
			if err := mkdir(worktreePath); err != nil {
				t.Fatal(err)
			}
			databasePath := filepath.Join(root, "state", "factory.db")
			run := recoveryRun(worktreePath)
			run.RepositoryPath = root
			run.Revision = 5
			seeded := seedUnsafeReplay(ctx, t, databasePath, run, test)

			githubAdapter := &fakeGitHub{
				issueValue:    tracker.Issue{Number: run.IssueNumber, State: "open", Labels: []string{tracker.LabelAgentRunning}},
				statusComment: tracker.Comment{ID: run.StatusCommentID, Body: factory.StatusCommentBody(run)},
			}
			adapters := pauseAdapters{github: githubAdapter, tracker: githubAdapter, worker: &recoveryWorker{}}
			if test.adapt != nil {
				test.adapt(&adapters)
			}
			workspace := &draftGitWorkspace{}
			pullRequests := &fakePullRequests{}
			service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
				Config: &fakeConfig{value: config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{{
					Path: root, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
					OperationalDataPath: databasePath, RepositoryConfigPath: filepath.Join(root, "factory.yaml"),
				}}}},
				OpenStore:    func(ctx context.Context, path string) (factory.OperationalStore, error) { return store.Open(ctx, path) },
				Tracker:      adapters.tracker,
				GitWorkspace: workspace, Worktree: workspace, PullRequests: pullRequests,
				Worker: adapters.worker,
				Now:    func() time.Time { return time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC) },
			})

			result, err := service.Reconcile(ctx)
			assertTypedReconcileError(t, err, test.discrepancyKind)
			if result.Outcome != factory.RecoveryOutcomeWaitingForHuman {
				t.Fatalf("Reconcile() outcome = %q, want %q", result.Outcome, factory.RecoveryOutcomeWaitingForHuman)
			}
			assertDiscrepancy(t, result.Diagnosis, test)
			if test.adapt != nil && adapters.refusals == 0 {
				t.Fatal("replay paused before it reached the adapter refusal")
			}
			if len(workspace.checkpoints) != 0 || len(workspace.pushes) != 0 || len(pullRequests.createdRequests) != 0 ||
				len(pullRequests.updatedRequests) != 0 || len(githubAdapter.createdComments) != 0 {
				t.Fatalf("refused replay crossed external effects: checkpoints=%d pushes=%d pulls created=%d updated=%d comments=%d",
					len(workspace.checkpoints), len(workspace.pushes), len(pullRequests.createdRequests),
					len(pullRequests.updatedRequests), len(githubAdapter.createdComments))
			}
			assertPausedJournal(ctx, t, databasePath, seeded, test.retained)
		})
	}
}

// seedUnsafeReplay persists the run and its unsafe journal entry.
func seedUnsafeReplay(ctx context.Context, t *testing.T, databasePath string, run store.Run, test unsafeReplay) store.PendingEffect {
	t.Helper()
	opened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	if err := opened.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if test.seed != nil {
		test.seed(ctx, t, opened, run)
	}
	payload, err := json.Marshal(test.payload(run))
	if err != nil {
		t.Fatal(err)
	}
	effect := store.PendingEffect{RunID: run.ID, ID: string(test.kind) + ":unsafe", Kind: test.kind, Payload: string(payload)}
	if err := opened.SavePendingEffect(ctx, effect); err != nil {
		t.Fatal(err)
	}
	return effect
}

// assertTypedReconcileError requires the reconciliation error type that
// matches the discrepancy type.
func assertTypedReconcileError(t *testing.T, err error, kind factory.RecoveryDiscrepancyKind) {
	t.Helper()
	var workflowErr *factory.WorkflowFailureError
	var infrastructureErr *factory.InfrastructureDiscrepancyError
	switch {
	case kind == factory.RecoveryDiscrepancyWorkflow && errors.As(err, &workflowErr):
	case kind == factory.RecoveryDiscrepancyInfrastructure && errors.As(err, &infrastructureErr):
	default:
		t.Fatalf("Reconcile() error = %T %v, want the typed %s refusal", err, err, kind)
	}
}

// assertDiscrepancy requires the expected typed discrepancy in a diagnosis.
func assertDiscrepancy(t *testing.T, diagnosis factory.RecoveryDiagnosis, test unsafeReplay) {
	t.Helper()
	for _, discrepancy := range diagnosis.Discrepancies {
		if discrepancy.Source == test.source && discrepancy.Field == test.field && discrepancy.Kind == test.discrepancyKind {
			return
		}
	}
	t.Fatalf("discrepancies = %#v, want %s %s.%s", diagnosis.Discrepancies, test.discrepancyKind, test.source, test.field)
}

// assertPausedJournal requires a persisted waiting-for-human run and checks
// whether the journal kept the refused entry.
func assertPausedJournal(ctx context.Context, t *testing.T, databasePath string, effect store.PendingEffect, retained bool) {
	t.Helper()
	reopened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	latest, err := reopened.LatestRun(ctx)
	if err != nil || latest == nil || latest.Status != store.StatusWaitingForHuman {
		t.Fatalf("persisted run = %#v, %v, want waiting for human", latest, err)
	}
	pending, err := reopened.PendingEffect(ctx, effect.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if kept := pending != nil && pending.ID == effect.ID; kept != retained {
		t.Fatalf("journal after the pause = %#v, want retained=%t", pending, retained)
	}
}

// trackerMutation names one GitHub mutation a refusingTracker refuses.
type trackerMutation string

const (
	// opRefuseLabels refuses complete label replacements.
	opRefuseLabels trackerMutation = "labels"
	// opRefuseComment refuses new issue comments.
	opRefuseComment trackerMutation = "comment"
)

// refusingTracker is a GitHub tracker that refuses one kind of mutation, as
// GitHub does for a locked issue, and counts each refusal.
type refusingTracker struct {
	*fakeGitHub
	refused  trackerMutation
	refusals *int
}

// ReplaceIssueLabels refuses the mutation when labels are refused.
func (r refusingTracker) ReplaceIssueLabels(ctx context.Context, repository tracker.Repository, number int, labels []string) error {
	if r.refused == opRefuseLabels {
		*r.refusals++
		return errors.New("label replacement refused: the issue is locked")
	}
	return r.fakeGitHub.ReplaceIssueLabels(ctx, repository, number, labels)
}

// CreateIssueComment refuses the mutation when comments are refused.
func (r refusingTracker) CreateIssueComment(ctx context.Context, repository tracker.Repository, number int, body string) (tracker.Comment, error) {
	if r.refused == opRefuseComment {
		*r.refusals++
		return tracker.Comment{}, errors.New("comment refused: the issue is locked")
	}
	return r.fakeGitHub.CreateIssueComment(ctx, repository, number, body)
}

// conflictingWorker is a worker runtime that refuses a launch because another
// invocation already owns the worker identity, and counts each refusal.
type conflictingWorker struct {
	recoveryWorker
	refusals *int
}

// Start refuses the launch.
func (w *conflictingWorker) Start(context.Context, worker.StartRequest) error {
	*w.refusals++
	return errors.New("worker identity already runs another invocation")
}
