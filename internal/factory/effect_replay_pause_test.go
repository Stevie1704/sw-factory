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

// undecodablePayload returns a payload that no effect kind can decode.
func undecodablePayload(store.Run) any { return []string{} }

// unsafeReplays lists one unsafe journal entry for every durable effect kind.
func unsafeReplays() []unsafeReplay {
	workflow := func(name string, kind store.PendingEffectKind, extra map[string]any) unsafeReplay {
		return unsafeReplay{
			name: name, kind: kind, payload: stalePayload(extra), source: "pending effect", field: string(kind),
			discrepancyKind: factory.RecoveryDiscrepancyWorkflow, retained: true,
		}
	}
	infrastructure := func(name string, kind store.PendingEffectKind) unsafeReplay {
		return unsafeReplay{
			name: name, kind: kind, payload: undecodablePayload, source: "pending effect", field: string(kind),
			discrepancyKind: factory.RecoveryDiscrepancyInfrastructure, retained: true,
		}
	}
	return []unsafeReplay{
		workflow("stale state transition", store.PendingEffectKindStateTransition, nil),
		workflow("superseded status comment", store.PendingEffectKindStatusComment, nil),
		workflow("stale checkpoint", store.PendingEffectKindCheckpoint, map[string]any{"Request": map[string]any{}}),
		workflow("stale pull request", store.PendingEffectKindPullRequest, map[string]any{"PersistRun": true}),
		workflow("stale result acceptance", store.PendingEffectKindResultAcceptance, nil),
		infrastructure("undecodable clarification comment", store.PendingEffectKindClarificationComment),
		infrastructure("undecodable label transition", store.PendingEffectKindLabelTransition),
		infrastructure("undecodable commit status", store.PendingEffectKindCommitStatus),
		infrastructure("undecodable push", store.PendingEffectKindPush),
		infrastructure("undecodable worker launch", store.PendingEffectKindWorkerLaunch),
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
			effect := seedUnsafeReplay(ctx, t, databasePath, run, test)

			githubAdapter := &fakeGitHub{
				issueValue:    tracker.Issue{Number: run.IssueNumber, State: "open", Labels: []string{tracker.LabelAgentRunning}},
				statusComment: tracker.Comment{ID: run.StatusCommentID, Body: factory.StatusCommentBody(run)},
			}
			workspace := &draftGitWorkspace{}
			pullRequests := &fakePullRequests{}
			service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
				Config: &fakeConfig{value: config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{{
					Path: root, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
					OperationalDataPath: databasePath, RepositoryConfigPath: filepath.Join(root, "factory.yaml"),
				}}}},
				OpenStore:    func(ctx context.Context, path string) (factory.OperationalStore, error) { return store.Open(ctx, path) },
				Tracker:      githubAdapter,
				GitWorkspace: workspace, Worktree: workspace, PullRequests: pullRequests,
				Worker: &recoveryWorker{},
				Now:    func() time.Time { return time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC) },
			})

			result, err := service.Reconcile(ctx)
			assertTypedReconcileError(t, err, test.discrepancyKind)
			if result.Outcome != factory.RecoveryOutcomeWaitingForHuman {
				t.Fatalf("Reconcile() outcome = %q, want %q", result.Outcome, factory.RecoveryOutcomeWaitingForHuman)
			}
			assertDiscrepancy(t, result.Diagnosis, test)
			if len(workspace.checkpoints) != 0 || len(workspace.pushes) != 0 || len(pullRequests.createdRequests) != 0 || len(pullRequests.updatedRequests) != 0 {
				t.Fatalf("refused replay crossed Git or code-host effects: checkpoints=%d pushes=%d created=%d updated=%d",
					len(workspace.checkpoints), len(workspace.pushes), len(pullRequests.createdRequests), len(pullRequests.updatedRequests))
			}
			assertPausedJournal(ctx, t, databasePath, effect, test.retained)
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
