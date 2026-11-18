package factory_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// TestFailedLaunchKeepsTheInvocationActiveWhenTheWorkerStopFails verifies
// that a failed launch whose worker stop fails does not mark the invocation
// terminal, and that the returned error names the stop failure.
func TestFailedLaunchKeepsTheInvocationActiveWhenTheWorkerStopFails(t *testing.T) {
	fixture := newReviewFixture(t)
	run := *fixture.runStore.current
	fixture.harness.startErr = errors.New("native session identity unavailable")
	fixture.worker.stopErr = errors.New("docker daemon unavailable")

	_, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{
		RunID: run.ID, Role: workflow.RoleSpecificationReview, Stage: store.StageReview,
	})
	if err == nil || !strings.Contains(err.Error(), "native session identity unavailable") || !strings.Contains(err.Error(), "docker daemon unavailable") {
		t.Fatalf("StartAgent() error = %v, want the launch failure and the named worker stop failure", err)
	}
	failed, err := fixture.runStore.LatestInvocation(context.Background(), run.ID)
	if err != nil || failed == nil {
		t.Fatalf("read failed invocation = %#v, %v", failed, err)
	}
	if failed.Status != store.InvocationStatusActive {
		t.Fatalf("failed invocation status = %q, want active until the worker stop is proven", failed.Status)
	}
}

// TestFailedLaunchClosesTheInvocationAfterTheWorkerStops verifies that a
// failed launch whose worker stop succeeds keeps the existing result: the
// worker is stopped and the invocation is a voided superseded launch.
func TestFailedLaunchClosesTheInvocationAfterTheWorkerStops(t *testing.T) {
	fixture := newReviewFixture(t)
	run := *fixture.runStore.current
	fixture.harness.startErr = errors.New("native session identity unavailable")

	if _, err := fixture.service.StartAgent(context.Background(), factory.AgentRequest{
		RunID: run.ID, Role: workflow.RoleSpecificationReview, Stage: store.StageReview,
	}); err == nil {
		t.Fatal("StartAgent() error = nil, want the launch failure")
	}
	failed, err := fixture.runStore.LatestInvocation(context.Background(), run.ID)
	if err != nil || failed == nil {
		t.Fatalf("read failed invocation = %#v, %v", failed, err)
	}
	if failed.Status != store.InvocationStatusSuperseded || !failed.LaunchVoided || fixture.worker.stops == 0 {
		t.Fatalf("failed invocation = %s voided=%t, worker stops = %d; want a voided superseded launch with its worker stopped", failed.Status, failed.LaunchVoided, fixture.worker.stops)
	}
}

// TestReconcileRetriesTheWorkerStopOfARolledBackLaunch verifies that the
// migration gate refuses a store whose rolled-back launch still has a
// running worker, and that a later reconciliation that stops the worker
// releases the invocation and the journal entry.
func TestReconcileRetriesTheWorkerStopOfARolledBackLaunch(t *testing.T) {
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
	seedRolledBackLaunch(ctx, t, databasePath, run)
	runtime := &stopRefusingWorker{err: errors.New("docker daemon unavailable")}
	service := launchRollbackService(root, databasePath, run, runtime)

	if _, err := service.Reconcile(ctx); err == nil || !strings.Contains(err.Error(), "docker daemon unavailable") {
		t.Fatalf("first Reconcile() error = %v, want the named worker stop failure", err)
	}
	if status := persistedInvocationStatus(ctx, t, databasePath, run.ID); status != store.InvocationStatusActive {
		t.Fatalf("invocation after a failed stop = %q, want active", status)
	}
	findings := migrationGateFindings(ctx, t, databasePath)
	if !findings["invocation.active"] || !findings["effect.pending"] {
		t.Fatalf("migration gate findings = %v, want invocation.active and effect.pending", findings)
	}

	runtime.err = nil
	_, _ = service.Reconcile(ctx)
	if len(runtime.stopped) == 0 || runtime.stopped[0] != run.ID {
		t.Fatalf("stopped workers = %v, want the rolled-back worker %q", runtime.stopped, run.ID)
	}
	if status := persistedInvocationStatus(ctx, t, databasePath, run.ID); status != store.InvocationStatusCannotProceed {
		t.Fatalf("invocation after the stop = %q, want cannot_proceed", status)
	}
	findings = migrationGateFindings(ctx, t, databasePath)
	if findings["invocation.active"] || findings["effect.pending"] {
		t.Fatalf("migration gate findings = %v, want the rolled-back launch released", findings)
	}
}

// launchRollbackInvocation returns the active invocation of a launch that
// failed before it got a native session.
func launchRollbackInvocation(runID string) store.Invocation {
	return store.Invocation{
		ID: "inv-launch", RunID: runID, Harness: "codex", Role: "implementation", Stage: store.StageImplementation,
		Status:    store.InvocationStatusActive,
		CreatedAt: time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC),
	}
}

// seedRolledBackLaunch persists the run, its active launch invocation, and
// the journaled rollback whose worker stop failed.
func seedRolledBackLaunch(ctx context.Context, t *testing.T, databasePath string, run store.Run) {
	t.Helper()
	opened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	if err := opened.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	invocation := launchRollbackInvocation(run.ID)
	if err := opened.SaveInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	terminal := invocation
	terminal.Status = store.InvocationStatusCannotProceed
	payload, err := json.Marshal(map[string]any{"Invocation": terminal, "WorkerID": run.ID})
	if err != nil {
		t.Fatal(err)
	}
	effect := store.PendingEffect{RunID: run.ID, ID: "launch_rollback:inv-launch", Kind: store.PendingEffectKindLaunchRollback, Payload: string(payload)}
	if err := opened.SavePendingEffect(ctx, effect); err != nil {
		t.Fatal(err)
	}
}

// launchRollbackService builds a coordinator over the seeded store and the
// supplied worker runtime.
func launchRollbackService(root, databasePath string, run store.Run, runtime *stopRefusingWorker) *factory.Service {
	githubAdapter := &fakeGitHub{
		issueValue:    tracker.Issue{Number: run.IssueNumber, State: "open", Labels: []string{tracker.LabelAgentRunning}},
		statusComment: tracker.Comment{ID: run.StatusCommentID, Body: factory.StatusCommentBody(run)},
	}
	workspace := &draftGitWorkspace{}
	return factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config: &fakeConfig{value: config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{{
			Path: root, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
			OperationalDataPath: databasePath, RepositoryConfigPath: filepath.Join(root, "factory.yaml"),
		}}}},
		OpenStore:    func(ctx context.Context, path string) (factory.OperationalStore, error) { return store.Open(ctx, path) },
		Tracker:      githubAdapter,
		GitWorkspace: workspace, Worktree: workspace, PullRequests: &fakePullRequests{},
		Worker: runtime,
		Now:    func() time.Time { return time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC) },
	})
}

// persistedInvocationStatus reads the launch invocation status from SQLite.
func persistedInvocationStatus(ctx context.Context, t *testing.T, databasePath, runID string) store.InvocationStatus {
	t.Helper()
	opened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	invocation, err := opened.Invocation(ctx, runID, "inv-launch")
	if err != nil || invocation == nil {
		t.Fatalf("read invocation = %#v, %v", invocation, err)
	}
	return invocation.Status
}

// migrationGateFindings returns the activity finding codes of the migration
// gate for the store.
func migrationGateFindings(ctx context.Context, t *testing.T, databasePath string) map[string]bool {
	t.Helper()
	inspection, err := store.InspectForUpgrade(ctx, databasePath)
	if err != nil {
		t.Fatalf("InspectForUpgrade() error = %v", err)
	}
	codes := map[string]bool{}
	for _, finding := range inspection.ActivityFindings() {
		codes[finding.Code] = true
	}
	return codes
}

// stopRefusingWorker is a worker runtime whose stop fails while err is set
// and records each successful stop.
type stopRefusingWorker struct {
	recoveryWorker
	err     error
	stopped []string
}

// Stop fails with err, or records the stopped worker.
func (w *stopRefusingWorker) Stop(_ context.Context, workerID string) error {
	if w.err != nil {
		return w.err
	}
	w.stopped = append(w.stopped, workerID)
	return nil
}
