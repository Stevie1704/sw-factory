package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// journalRecoveryImageDigest is the frozen worker image identity used by the
// journaled restart fixture.
const journalRecoveryImageDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestRecoveryTargetsOnlyTheReviewerWithARecoverableProjection verifies that
// one failed reviewer cannot make the coordinator resume a healthy concurrent
// reviewer after restart.
func TestRecoveryTargetsOnlyTheReviewerWithARecoverableProjection(t *testing.T) {
	active := []store.Invocation{
		{ID: "inv-spec", RunID: "run-recovery-review", Status: store.InvocationStatusActive},
		{ID: "inv-standards", RunID: "run-recovery-review", Status: store.InvocationStatusActive},
	}
	diagnosis := RecoveryDiagnosis{Discrepancies: []RecoveryDiscrepancy{{InvocationID: "inv-standards", Recoverable: true}}}
	target := recoveryTargetActiveInvocation(diagnosis, active)
	if target == nil || target.ID != "inv-standards" {
		t.Fatalf("recovery target = %#v, want inv-standards", target)
	}
}

// TestJournaledStartupRecreatesALostWorkerAndResumesOnce verifies the real
// startup reconciliation path recreates a missing worker from the persisted
// start request, resumes the native session once, persists the resume ceiling,
// and performs no second resume when a fresh coordinator starts again.
func TestJournaledStartupRecreatesALostWorkerAndResumesOnce(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	worktreePath := filepath.Join(root, "worktree")
	if err := os.MkdirAll(worktreePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/factory/run-journaled-recovery\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(root, "codex-auth.json")
	if err := os.WriteFile(authPath, []byte(`{"access_token":"journaled-recovery-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	packet := SpecificationPacket{
		Version: specificationPacketVersion,
		Issue:   github.Issue{Number: 42, Title: "Journaled recovery", Body: "resume the active invocation"},
		RepositoryConfig: config.RepositoryConfig{
			TargetBranch: "main",
			WorkerBuild:  config.WorkerBuildConfig{Image: "ghcr.io/example/factory-worker"},
		},
	}
	packetData, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	run := store.Run{
		ID:                  "run-journaled-recovery",
		RepositoryPath:      root,
		IssueNumber:         packet.Issue.Number,
		Stage:               store.StageImplementation,
		Status:              store.StatusActive,
		Branch:              "factory/run-journaled-recovery",
		Worktree:            worktreePath,
		CheckpointSHA:       "checkpoint-journaled-recovery",
		ImageDigest:         journalRecoveryImageDigest,
		Coordinator:         "coordinator-test",
		StatusCommentID:     "status-journaled-recovery",
		SpecificationPacket: string(packetData),
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	invocationDirectory := filepath.Join(root, "invocation")
	resultDirectory := filepath.Join(root, "results")
	if err := os.MkdirAll(invocationDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(resultDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	invocation := store.Invocation{
		ID:                      "inv-journaled-recovery",
		RunID:                   run.ID,
		Harness:                 harness.NameCodex,
		Role:                    "implementation",
		Stage:                   store.StageImplementation,
		Model:                   "gpt-5",
		CredentialStoreID:       root,
		NativeSessionID:         "session-journaled-recovery",
		WorkspaceID:             "workspace-journaled-recovery",
		StatusSurfaceID:         "surface-status-journaled-recovery",
		ImplementationSurfaceID: "surface-implementation-journaled-recovery",
		ChecksSurfaceID:         "surface-checks-journaled-recovery",
		InvocationDirectory:     invocationDirectory,
		ResultDirectory:         resultDirectory,
		PromptVersion:           prompt.VersionFor("implementation", string(store.StageImplementation)),
		Status:                  store.InvocationStatusActive,
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	opened, err := store.Open(ctx, filepath.Join(root, "state", "factory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	if err := opened.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := opened.SaveInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	if err := writeInvocationPacket(invocationDirectory, InvocationPacket{
		SchemaVersion:       invocationPacketVersion,
		InvocationID:        invocation.ID,
		RunID:               run.ID,
		Role:                invocation.Role,
		Stage:               invocation.Stage,
		SpecificationPacket: run.SpecificationPacket,
		PromptVersion:       invocation.PromptVersion,
	}); err != nil {
		t.Fatal(err)
	}

	databasePath := filepath.Join(root, "state", "factory.db")
	registration := config.RepositoryRegistration{
		Path:                root,
		OperationalDataPath: databasePath,
		GitHub:              config.GitHubConfig{Owner: "example", Repository: "project"},
		Authentication:      config.AuthenticationConfig{CodexAuthPath: authPath},
	}
	githubRuntime := &effectMatrixGitHub{
		issue:         github.Issue{Number: run.IssueNumber, Labels: []string{factoryLabelForStatus(run.Status)}},
		statusComment: github.Comment{ID: run.StatusCommentID, Body: statusCommentBody(run)},
	}
	workspace := &journalRecoveryWorkspace{state: gitadapter.WorktreeState{
		RepositoryPath: root,
		Branch:         run.Branch,
		HeadSHA:        run.CheckpointSHA,
	}}
	workerRuntime := &journalRecoveryWorker{inspection: worker.Inspection{}}
	terminalRuntime := &journalRecoveryTerminal{inspection: terminal.WorkspaceInspection{
		Exists:      true,
		WorkspaceID: terminal.WorkspaceID(invocation.WorkspaceID),
		Surfaces: []terminal.Surface{
			{ID: terminal.SurfaceID(invocation.StatusSurfaceID), WorkspaceID: terminal.WorkspaceID(invocation.WorkspaceID)},
			{ID: terminal.SurfaceID(invocation.ImplementationSurfaceID), WorkspaceID: terminal.WorkspaceID(invocation.WorkspaceID)},
			{ID: terminal.SurfaceID(invocation.ChecksSurfaceID), WorkspaceID: terminal.WorkspaceID(invocation.WorkspaceID)},
		},
	}}
	harnessRuntime := &journalRecoveryHarness{nativeSessionID: invocation.NativeSessionID}
	service := &Service{deps: Dependencies{
		GitHub:       githubRuntime,
		GitWorkspace: workspace,
		Worker:       workerRuntime,
		Terminal:     terminalRuntime,
		Harness:      harnessRuntime,
		OpenStore: func(ctx context.Context, path string) (OperationalStore, error) {
			return store.Open(ctx, path)
		},
		Now: func() time.Time { return now },
	}}

	if err := service.reconcileRegisteredRun(ctx, registration); err != nil {
		t.Fatalf("first journaled startup = %v", err)
	}
	if workerRuntime.startCalls != 1 || len(workerRuntime.startRequests) != 1 {
		t.Fatalf("worker recreation = calls %d requests %d, want one pinned recreation", workerRuntime.startCalls, len(workerRuntime.startRequests))
	}
	if workerRuntime.startRequests[0].ImageDigest != run.ImageDigest || workerRuntime.startRequests[0].InvocationPath != invocation.InvocationDirectory || workerRuntime.startRequests[0].ResultPath != invocation.ResultDirectory {
		t.Fatalf("recreated worker request = %#v, want frozen image and invocation mounts", workerRuntime.startRequests[0])
	}
	if len(workerRuntime.codexSeeds) != 1 || workerRuntime.codexSeeds[0].AuthPath != authPath {
		t.Fatalf("recovery credential seeds = %#v, want one seed from the registered source", workerRuntime.codexSeeds)
	}
	if harnessRuntime.resumeCalls != 1 {
		t.Fatalf("native resumes after first startup = %d, want one", harnessRuntime.resumeCalls)
	}
	persisted, err := opened.Invocation(ctx, run.ID, invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted == nil || persisted.RecoveryResumeCount != 1 {
		t.Fatalf("persisted invocation after first startup = %#v, want resume count one", persisted)
	}
	pending, err := opened.PendingEffect(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending != nil {
		t.Fatalf("pending effect after first startup = %#v, want cleared", pending)
	}

	fresh := &Service{deps: service.deps}
	freshRun, err := opened.CurrentRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if freshRun == nil {
		t.Fatal("fresh coordinator fixture lost its run")
	}
	// Model a restart after the managed volume contents were lost while its
	// mount identity still matched the persisted worker contract.
	workerRuntime.codexSeeds = nil
	if err := fresh.reconcileRegisteredRun(ctx, registration); err != nil {
		t.Fatalf("second journaled startup = %v", err)
	}
	if len(workerRuntime.codexSeeds) != 1 || workerRuntime.codexSeeds[0].AuthPath != authPath {
		t.Fatalf("credential restoration after matching-worker restart = %#v, want one registered-source seed", workerRuntime.codexSeeds)
	}
	if harnessRuntime.resumeCalls != 1 {
		t.Fatalf("native resumes after second startup = %d, want no duplicate", harnessRuntime.resumeCalls)
	}

	// Reuse the persisted identity to model a later unexpected exit. The
	// first automatic failure must escalate, and the next coordinator must
	// consume the ambiguous reservation without issuing a second resume.
	persisted.RecoveryResumeCount = 0
	persisted.UpdatedAt = now.Add(time.Minute)
	if err := opened.SaveInvocation(ctx, *persisted); err != nil {
		t.Fatal(err)
	}
	activeRun := *freshRun
	activeRun.Status = store.StatusActive
	activeRun.UpdatedAt = now.Add(time.Minute)
	if err := opened.SaveRun(ctx, activeRun); err != nil {
		t.Fatal(err)
	}
	harnessRuntime.resumeErr = harness.NewUnexpectedExitError(harness.NameCodex)
	failureService := &Service{deps: service.deps}
	failureErr := failureService.reconcileRegisteredRun(ctx, registration)
	if !harness.IsUnexpectedExit(failureErr) {
		t.Fatalf("unexpected exit recovery error = %v, want typed unexpected exit", failureErr)
	}
	if harnessRuntime.resumeCalls != 2 {
		t.Fatalf("automatic resume calls after unexpected exit = %d, want one additional attempt", harnessRuntime.resumeCalls)
	}
	persisted, err = opened.Invocation(ctx, run.ID, invocation.ID)
	if err != nil || persisted == nil || persisted.RecoveryResumeCount != 1 {
		t.Fatalf("invocation after unexpected exit = %#v, error = %v; want consumed automatic attempt", persisted, err)
	}
	escalated, err := opened.CurrentRun(ctx)
	if err != nil || escalated == nil || escalated.Status != store.StatusWaitingForHuman || !strings.HasPrefix(escalated.LifecycleReason, "automatic harness recovery exhausted") {
		t.Fatalf("run after unexpected exit = %#v, error = %v; want manual recovery escalation", escalated, err)
	}
	lastService := &Service{deps: service.deps}
	_ = lastService.reconcileRegisteredRun(ctx, registration)
	if harnessRuntime.resumeCalls != 2 {
		t.Fatalf("automatic resume calls after escalation restart = %d, want no duplicate", harnessRuntime.resumeCalls)
	}
	if pending, err := opened.PendingEffect(ctx, run.ID); err != nil || pending != nil {
		t.Fatalf("pending effect after escalation restart = %#v, error = %v; want cleared", pending, err)
	}

	// A later worker loss must be repaired, but the already-consumed automatic
	// slot must still force a human before another native resume.
	persisted.RecoveryResumeCount = 1
	persisted.UpdatedAt = now.Add(2 * time.Minute)
	if err := opened.SaveInvocation(ctx, *persisted); err != nil {
		t.Fatal(err)
	}
	activeRun = *escalated
	activeRun.Status = store.StatusActive
	activeRun.UpdatedAt = now.Add(2 * time.Minute)
	if err := opened.SaveRun(ctx, activeRun); err != nil {
		t.Fatal(err)
	}
	githubRuntime.issue.Labels = []string{factoryLabelForStatus(activeRun.Status)}
	githubRuntime.statusComment.Body = statusCommentBody(activeRun)
	workerRuntime.inspection = worker.Inspection{}
	harnessRuntime.resumeErr = nil
	lossService := &Service{deps: service.deps}
	lossErr := lossService.reconcileRegisteredRun(ctx, registration)
	if !harness.IsUnexpectedExit(lossErr) {
		t.Fatalf("post-resume worker loss error = %v, want typed manual-recovery error", lossErr)
	}
	if harnessRuntime.resumeCalls != 2 || workerRuntime.startCalls != 2 {
		t.Fatalf("post-resume worker loss effects = resumes %d, worker recreations %d; want no resume and one recreation", harnessRuntime.resumeCalls, workerRuntime.startCalls)
	}
	repairedRun, err := opened.CurrentRun(ctx)
	if err != nil || repairedRun == nil || repairedRun.Status != store.StatusWaitingForHuman {
		t.Fatalf("run after post-resume worker loss = %#v, error = %v; want human recovery", repairedRun, err)
	}

	// A live polling pass must detect a harness that exits after launch and
	// route it through the same one-shot automatic native resume policy.
	persisted.RecoveryResumeCount = 0
	persisted.UpdatedAt = now.Add(3 * time.Minute)
	if err := opened.SaveInvocation(ctx, *persisted); err != nil {
		t.Fatal(err)
	}
	activeRun = *repairedRun
	activeRun.Status = store.StatusActive
	activeRun.LifecycleReason = "harness resumed after live interruption"
	activeRun.UpdatedAt = now.Add(3 * time.Minute)
	if err := opened.SaveRun(ctx, activeRun); err != nil {
		t.Fatal(err)
	}
	githubRuntime.issue.Labels = []string{factoryLabelForStatus(activeRun.Status)}
	githubRuntime.statusComment.Body = statusCommentBody(activeRun)
	harnessRuntime.nativeSessionExited = true
	if err := lossService.retryWaitingForHarness(ctx, registration); err != nil {
		t.Fatalf("live harness interruption recovery = %v", err)
	}
	if harnessRuntime.resumeCalls != 3 {
		t.Fatalf("native resumes after live harness exit = %d, want one recovery resume", harnessRuntime.resumeCalls)
	}
	liveRecovered, err := opened.CurrentRun(ctx)
	if err != nil || liveRecovered == nil || liveRecovered.Status != store.StatusActive {
		t.Fatalf("run after live harness recovery = %#v, error = %v; want active run", liveRecovered, err)
	}

	// A configured source that disappears before the next restart must pause
	// before native resume and keep its path and contents out of the error.
	if err := os.Remove(authPath); err != nil {
		t.Fatal(err)
	}
	persisted, err = opened.Invocation(ctx, run.ID, invocation.ID)
	if err != nil || persisted == nil {
		t.Fatalf("read invocation before missing-source recovery = %#v, error = %v", persisted, err)
	}
	persisted.RecoveryResumeCount = 0
	persisted.UpdatedAt = now.Add(4 * time.Minute)
	if err := opened.SaveInvocation(ctx, *persisted); err != nil {
		t.Fatal(err)
	}
	activeRun = *liveRecovered
	activeRun.Status = store.StatusActive
	activeRun.LifecycleReason = "worker recovered with a missing credential source"
	activeRun.UpdatedAt = now.Add(4 * time.Minute)
	if err := opened.SaveRun(ctx, activeRun); err != nil {
		t.Fatal(err)
	}
	githubRuntime.issue.Labels = []string{factoryLabelForStatus(activeRun.Status)}
	githubRuntime.statusComment.Body = statusCommentBody(activeRun)
	workerRuntime.inspection = worker.Inspection{}
	workerRuntime.seedErr = errors.New(authPath + ": journaled-recovery-secret")
	harnessRuntime.nativeSessionExited = false
	missingSourceService := &Service{deps: service.deps}
	missingSourceErr := missingSourceService.reconcileRegisteredRun(ctx, registration)
	if missingSourceErr == nil || !strings.Contains(missingSourceErr.Error(), "factory auth refresh") {
		t.Fatalf("missing-source recovery error = %v, want actionable auth refresh guidance", missingSourceErr)
	}
	if strings.Contains(missingSourceErr.Error(), authPath) || strings.Contains(missingSourceErr.Error(), "journaled-recovery-secret") {
		t.Fatalf("missing-source recovery leaked credential details: %v", missingSourceErr)
	}
	paused, err := opened.CurrentRun(ctx)
	if err != nil || paused == nil || paused.Status != store.StatusWaitingForHuman || !strings.HasPrefix(paused.LifecycleReason, "harness authentication expired") {
		t.Fatalf("run after missing-source recovery = %#v, error = %v; want authentication pause", paused, err)
	}
}

// TestRecoveryInspectsTheLatestTerminalInvocation verifies a stage boundary
// cannot bypass Docker, cmux, and native-session reconciliation merely because
// its newest persisted invocation is already terminal.
func TestRecoveryInspectsTheLatestTerminalInvocation(t *testing.T) {
	now := time.Unix(1, 0).UTC()
	run := store.Run{ID: "run-terminal-projection", Stage: store.StageReview, Status: store.StatusActive}
	invocation := store.Invocation{
		ID: "inv-terminal-projection", RunID: run.ID, Harness: harness.NameCodex,
		Role: "spec_review", Stage: store.StageReview,
		NativeSessionID: "session-terminal-projection", WorkspaceID: "workspace-terminal-projection",
		ImplementationSurfaceID: "surface-terminal-projection",
		Status:                  store.InvocationStatusCompleted, CreatedAt: now, UpdatedAt: now,
	}
	workerRuntime := &journalRecoveryWorker{inspectErr: errors.New("worker projection unavailable")}
	terminalRuntime := &journalRecoveryTerminal{inspection: terminal.WorkspaceInspection{
		Exists: true, WorkspaceID: terminal.WorkspaceID(invocation.WorkspaceID),
		Surfaces: []terminal.Surface{{ID: terminal.SurfaceID(invocation.ImplementationSurfaceID), WorkspaceID: terminal.WorkspaceID(invocation.WorkspaceID)}},
	}}
	service := &Service{deps: Dependencies{
		Worker: workerRuntime, Terminal: terminalRuntime,
		Harness: &journalRecoveryHarness{nativeSessionID: invocation.NativeSessionID},
	}}
	baseStore := &repairLaunchStore{run: run, invocations: map[string]store.Invocation{invocation.ID: invocation}}
	runStore := &terminalProjectionStore{repairLaunchStore: baseStore}
	diagnosis := newRecoveryDiagnosis(run.ID)

	service.inspectInvocationProjection(context.Background(), &diagnosis, config.RepositoryRegistration{}, runStore, run)
	if workerRuntime.inspectCalls != 1 {
		t.Fatalf("worker inspections = %d, want one for latest terminal invocation", workerRuntime.inspectCalls)
	}
	found := false
	for _, discrepancy := range diagnosis.Discrepancies {
		if discrepancy.Source == "worker" && discrepancy.Field == "inspection" {
			found = true
		}
	}
	if !found {
		t.Fatalf("recovery discrepancies = %#v, want terminal worker inspection failure", diagnosis.Discrepancies)
	}
}

// TestRecoveryAcceptsAStoppedWorkerAfterReadiness verifies that a worker
// stopped by the readiness transition is not mistaken for an interrupted
// active invocation.
func TestRecoveryAcceptsAStoppedWorkerAfterReadiness(t *testing.T) {
	now := time.Unix(2, 0).UTC()
	run := store.Run{ID: "run-ready-stopped-worker", Stage: store.StageReady, Status: store.StatusActive}
	invocation := store.Invocation{
		ID: "inv-ready-stopped-worker", RunID: run.ID, Harness: harness.NameCodex,
		Role: "spec_review", Stage: store.StageReview,
		NativeSessionID: "session-ready-stopped-worker", WorkspaceID: "workspace-ready-stopped-worker",
		StatusSurfaceID: "surface-status-ready-stopped-worker", ImplementationSurfaceID: "surface-ready-stopped-worker",
		ChecksSurfaceID: "surface-checks-ready-stopped-worker", Status: store.InvocationStatusCompleted,
		CreatedAt: now, UpdatedAt: now,
	}
	workerRuntime := &journalRecoveryWorker{inspection: worker.Inspection{}}
	terminalRuntime := &journalRecoveryTerminal{inspection: terminal.WorkspaceInspection{
		Exists: true, WorkspaceID: terminal.WorkspaceID(invocation.WorkspaceID),
		Surfaces: []terminal.Surface{
			{ID: terminal.SurfaceID(invocation.StatusSurfaceID), WorkspaceID: terminal.WorkspaceID(invocation.WorkspaceID)},
			{ID: terminal.SurfaceID(invocation.ImplementationSurfaceID), WorkspaceID: terminal.WorkspaceID(invocation.WorkspaceID)},
			{ID: terminal.SurfaceID(invocation.ChecksSurfaceID), WorkspaceID: terminal.WorkspaceID(invocation.WorkspaceID)},
		},
	}}
	service := &Service{deps: Dependencies{
		Worker: workerRuntime, Terminal: terminalRuntime,
		Harness: &journalRecoveryHarness{nativeSessionID: invocation.NativeSessionID},
	}}
	baseStore := &repairLaunchStore{run: run, invocations: map[string]store.Invocation{invocation.ID: invocation}}
	runStore := &terminalProjectionStore{repairLaunchStore: baseStore}
	diagnosis := newRecoveryDiagnosis(run.ID)

	service.inspectInvocationProjection(context.Background(), &diagnosis, config.RepositoryRegistration{}, runStore, run)
	if len(diagnosis.Discrepancies) != 0 {
		t.Fatalf("recovery discrepancies = %#v, want stopped worker accepted after readiness", diagnosis.Discrepancies)
	}
}

// TestPollLifecycleCompletesAMergedRunBeforeRecovery verifies that a deleted
// remote branch and historical worker projection cannot block the public
// lifecycle seam from recording an already-merged pull request.
func TestPollLifecycleCompletesAMergedRunBeforeRecovery(t *testing.T) {
	fixture := newDraftPRRecoveryFixture(t, "run-poll-merged-before-recovery", 17, true, "")

	result, err := fixture.lifecycleService().PollLifecycle(context.Background(), LifecycleRequest{RunID: fixture.run.ID})
	if err != nil {
		t.Fatalf("PollLifecycle() error = %v, want merged completion before recovery", err)
	}
	if result.Outcome != LifecycleCompleted || result.Run.Status != store.StatusComplete || result.Run.MergeCommitSHA != fixture.mergedSHA {
		t.Fatalf("PollLifecycle() result = %#v, want completed run at merge %s", result, fixture.mergedSHA)
	}
	if fixture.worker.inspectCalls != 0 {
		t.Fatalf("worker recovery inspections = %d, want lifecycle observation first", fixture.worker.inspectCalls)
	}
}

// TestStartCompletesAMergedRunBeforeRecovery verifies the persistent
// supervisor records an already-merged pull request before startup
// reconciliation can reject its deleted remote branch.
func TestStartCompletesAMergedRunBeforeRecovery(t *testing.T) {
	fixture := newDraftPRRecoveryFixture(t, "run-start-merged-before-recovery", 18, true, "")
	pollContext, cancel := context.WithCancel(context.Background())

	if err := fixture.supervisorService(cancel).Start(pollContext); err != nil {
		t.Fatalf("Start() error = %v, want merged completion before recovery", err)
	}
	latest := fixture.latestRun(t)
	if latest.Status != store.StatusComplete || latest.MergeCommitSHA != fixture.mergedSHA {
		t.Fatalf("latest run = %#v, want completed run at merge %s", latest, fixture.mergedSHA)
	}
	if fixture.workspace.remoteInspections != 0 {
		t.Fatalf("remote recovery inspections = %d, want lifecycle observation first", fixture.workspace.remoteInspections)
	}
}

// TestStartTreatsACompletedImplementationAsHistoricalAtDraftPR verifies an
// open pull request can survive coordinator restart after its gate worker has
// replaced the completed implementation worker projection.
func TestStartTreatsACompletedImplementationAsHistoricalAtDraftPR(t *testing.T) {
	fixture := newDraftPRRecoveryFixture(
		t,
		"run-start-open-draft-pr",
		19,
		false,
		strings.Repeat("c", 40),
	)
	pollContext, cancel := context.WithCancel(context.Background())

	if err := fixture.supervisorService(cancel).Start(pollContext); err != nil {
		t.Fatalf("Start() error = %v, want open draft PR supervision", err)
	}
	if fixture.worker.inspectCalls != 0 {
		t.Fatalf("historical implementation inspections = %d, want none at draft PR", fixture.worker.inspectCalls)
	}
}

// draftPRRecoveryFixture contains one journaled draft-PR run and its external
// projections for lifecycle-before-recovery tests.
type draftPRRecoveryFixture struct {
	databasePath string
	run          store.Run
	mergedSHA    string
	github       *mergedRecoveryGitHub
	workspace    *mergedRecoveryWorkspace
	worker       *journalRecoveryWorker
	host         config.HostConfig
	now          time.Time
}

// newDraftPRRecoveryFixture persists one draft-PR run whose current worker has
// gate mounts while its latest invocation is the completed implementation.
func newDraftPRRecoveryFixture(t *testing.T, runID string, pullRequestNumber int, merged bool, remoteHead string) *draftPRRecoveryFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	worktreePath := filepath.Join(root, "worktree")
	if err := os.MkdirAll(worktreePath, 0o700); err != nil {
		t.Fatal(err)
	}
	packetData, err := json.Marshal(SpecificationPacket{
		Version: specificationPacketVersion,
		Issue:   github.Issue{Number: 42, Title: "Draft PR recovery", Body: "observe lifecycle before recovery"},
		RepositoryConfig: config.RepositoryConfig{
			TargetBranch: "main",
			WorkerBuild:  config.WorkerBuildConfig{Image: "ghcr.io/example/factory-worker"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 28, 13, pullRequestNumber, 0, 0, time.UTC)
	checkpointSHA := strings.Repeat("c", 40)
	run := store.Run{
		ID: runID, RepositoryPath: root, IssueNumber: 42,
		Stage: store.StageDraftPR, Status: store.StatusActive,
		Branch: "factory/" + runID, Worktree: worktreePath,
		CheckpointSHA: checkpointSHA, ImageDigest: journalRecoveryImageDigest,
		StatusCommentID: "status-" + runID, PullRequestNumber: pullRequestNumber,
		PullRequestURL:      fmt.Sprintf("https://github.com/example/project/pull/%d", pullRequestNumber),
		SpecificationPacket: string(packetData), CreatedAt: now, UpdatedAt: now,
	}
	invocation := store.Invocation{
		ID: "inv-" + runID, RunID: run.ID, Harness: harness.NameCodex,
		Role: "implementation", Stage: store.StageImplementation,
		NativeSessionID: "session-" + runID, WorkspaceID: "workspace-" + runID,
		StatusSurfaceID: "surface-status-" + runID, ImplementationSurfaceID: "surface-implementation-" + runID,
		ChecksSurfaceID: "surface-checks-" + runID, Status: store.InvocationStatusCompleted,
		CreatedAt: now, UpdatedAt: now,
	}
	databasePath := filepath.Join(root, "state", "factory.db")
	opened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.SaveRun(ctx, run); err != nil {
		_ = opened.Close()
		t.Fatal(err)
	}
	if err := opened.SaveInvocation(ctx, invocation); err != nil {
		_ = opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	mergedSHA := ""
	pullRequestState := "open"
	if merged {
		mergedSHA = strings.Repeat("d", 40)
		pullRequestState = "closed"
	}
	githubRuntime := &mergedRecoveryGitHub{
		effectMatrixGitHub: &effectMatrixGitHub{
			issue:         github.Issue{Number: run.IssueNumber, State: "open", Labels: []string{github.LabelAgentRunning}},
			statusComment: github.Comment{ID: run.StatusCommentID, Body: statusCommentBody(run)},
		},
		pullRequest: github.PullRequest{
			Number: run.PullRequestNumber, URL: run.PullRequestURL, State: pullRequestState, Merged: merged,
			MergeCommitSHA: mergedSHA, HeadBranch: run.Branch, BaseBranch: "main",
		},
	}
	workspace := &mergedRecoveryWorkspace{
		state:      gitadapter.WorktreeState{RepositoryPath: root, Branch: run.Branch, HeadSHA: checkpointSHA},
		remoteHead: remoteHead,
	}
	workerRuntime := &journalRecoveryWorker{inspection: worker.Inspection{
		Exists: true, Running: true, MountFingerprint: "gate-worker-mounts",
	}}
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
		Path: root, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
		Polling:             config.PollingConfig{Interval: "1ms", Backoff: "1ms"},
		OperationalDataPath: databasePath, RepositoryConfigPath: filepath.Join(root, "factory.yaml"),
	}}}
	return &draftPRRecoveryFixture{
		databasePath: databasePath, run: run, mergedSHA: mergedSHA,
		github: githubRuntime, workspace: workspace, worker: workerRuntime,
		host: host, now: now,
	}
}

// lifecycleService constructs the public lifecycle seam for the fixture.
func (f *draftPRRecoveryFixture) lifecycleService() *Service {
	return NewWithDependencies("/host/config.yaml", f.dependencies())
}

// supervisorService constructs the persistent supervisor seam and cancels it
// after the first lease renewal.
func (f *draftPRRecoveryFixture) supervisorService(cancel context.CancelFunc) *Service {
	dependencies := f.dependencies()
	dependencies.LoadRepository = func(string) (config.RepositoryConfig, error) {
		return config.RepositoryConfig{TargetBranch: "main"}, nil
	}
	dependencies.IssuePoller = &mergedRecoveryIssuePoller{}
	dependencies.Lease = &mergedRecoveryLease{onRenew: cancel}
	dependencies.StartupDiagnosis = func(context.Context) (DoctorResult, error) { return DoctorResult{}, nil }
	return NewWithDependencies("/host/config.yaml", dependencies)
}

// dependencies returns the common real-store and external-boundary adapters.
func (f *draftPRRecoveryFixture) dependencies() Dependencies {
	return Dependencies{
		Config: effectMatrixConfig{host: f.host},
		OpenStore: func(ctx context.Context, path string) (OperationalStore, error) {
			return store.Open(ctx, path)
		},
		GitHub: f.github, PullRequests: f.github,
		GitWorkspace: f.workspace, Worktree: f.workspace,
		Worker: f.worker, Terminal: &journalRecoveryTerminal{},
		Harness: &journalRecoveryHarness{}, Now: func() time.Time { return f.now.Add(time.Minute) },
	}
}

// latestRun reads the public persisted run projection after supervisor exit.
func (f *draftPRRecoveryFixture) latestRun(t *testing.T) store.Run {
	t.Helper()
	opened, err := store.Open(context.Background(), f.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	latest, err := opened.LatestRun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil {
		t.Fatal("LatestRun() = nil, want persisted run")
	}
	return *latest
}

// mergedRecoveryGitHub combines exact persisted issue state with a merged pull
// request so public lifecycle tests exercise only external system boundaries.
type mergedRecoveryGitHub struct {
	*effectMatrixGitHub
	pullRequest github.PullRequest
}

// FindPullRequest returns the tracked merged pull request.
func (g *mergedRecoveryGitHub) FindPullRequest(context.Context, github.Repository, string, string) (github.PullRequest, error) {
	return g.pullRequest, nil
}

// CreatePullRequest satisfies the lifecycle pull-request boundary.
func (g *mergedRecoveryGitHub) CreatePullRequest(context.Context, github.Repository, github.PullRequestRequest) (github.PullRequest, error) {
	return g.pullRequest, nil
}

// UpdatePullRequest satisfies the lifecycle pull-request boundary.
func (g *mergedRecoveryGitHub) UpdatePullRequest(context.Context, github.Repository, int, github.PullRequestRequest) (github.PullRequest, error) {
	return g.pullRequest, nil
}

// mergedRecoveryWorkspace exposes a valid local worktree and a deleted remote
// run branch, matching GitHub's post-merge branch cleanup.
type mergedRecoveryWorkspace struct {
	state             gitadapter.WorktreeState
	remoteHead        string
	remoteInspections int
}

// Create satisfies the Git workspace seam.
func (*mergedRecoveryWorkspace) Create(context.Context, string, string, string) (gitadapter.Workspace, error) {
	return gitadapter.Workspace{}, nil
}

// Remove satisfies the Git workspace seam.
func (*mergedRecoveryWorkspace) Remove(context.Context, string, gitadapter.Workspace) error {
	return nil
}

// Inspect returns the persisted local worktree projection.
func (w *mergedRecoveryWorkspace) Inspect(context.Context, string) (gitadapter.WorktreeState, error) {
	return w.state, nil
}

// CreateCheckpoint satisfies the Git workspace seam.
func (*mergedRecoveryWorkspace) CreateCheckpoint(context.Context, gitadapter.CheckpointRequest) (gitadapter.CheckpointResult, error) {
	return gitadapter.CheckpointResult{}, nil
}

// Push satisfies the Git workspace seam.
func (*mergedRecoveryWorkspace) Push(context.Context, gitadapter.PushRequest) error { return nil }

// SynchronizeBase satisfies the Git workspace seam.
func (*mergedRecoveryWorkspace) SynchronizeBase(context.Context, gitadapter.BaseSyncRequest) error {
	return nil
}

// RemoteBranchHead reports a deleted remote run branch.
func (w *mergedRecoveryWorkspace) RemoteBranchHead(context.Context, gitadapter.PushRequest) (string, error) {
	w.remoteInspections++
	return w.remoteHead, nil
}

var _ gitadapter.GitWorkspace = (*mergedRecoveryWorkspace)(nil)
var _ gitadapter.RemoteBranchInspector = (*mergedRecoveryWorkspace)(nil)

// mergedRecoveryIssuePoller returns an empty eligible queue after terminal
// lifecycle observation releases the completed run's active slot.
type mergedRecoveryIssuePoller struct{}

// ListEligibleIssues reports no queued work.
func (*mergedRecoveryIssuePoller) ListEligibleIssues(context.Context, github.Repository) ([]github.Issue, error) {
	return nil, nil
}

// mergedRecoveryLease cancels the supervisor after its first visible renewal.
type mergedRecoveryLease struct {
	onRenew context.CancelFunc
}

// RenewLease records no external state and stops the focused supervisor test.
func (l *mergedRecoveryLease) RenewLease(context.Context, github.Repository, github.Lease) error {
	if l.onRenew != nil {
		l.onRenew()
	}
	return nil
}

// terminalProjectionStore exposes a latest terminal invocation while reporting
// that no active invocation exists at the stage boundary.
type terminalProjectionStore struct {
	*repairLaunchStore
}

// ActiveInvocation reports the expected absence of an active invocation.
func (*terminalProjectionStore) ActiveInvocation(context.Context, string) (*store.Invocation, error) {
	return nil, nil
}

// HasInvocation reports the terminal invocation history.
func (*terminalProjectionStore) HasInvocation(context.Context, string) (bool, error) {
	return true, nil
}

// journalRecoveryWorkspace supplies the read-only Git projection and no-op
// task methods needed by the journaled startup fixture.
type journalRecoveryWorkspace struct {
	state gitadapter.WorktreeState
}

// Create satisfies the Git workspace seam for a recovery-only fixture.
func (*journalRecoveryWorkspace) Create(context.Context, string, string, string) (gitadapter.Workspace, error) {
	return gitadapter.Workspace{}, nil
}

// Remove satisfies the Git workspace seam for a recovery-only fixture.
func (*journalRecoveryWorkspace) Remove(context.Context, string, gitadapter.Workspace) error {
	return nil
}

// Inspect returns the persisted worktree projection.
func (w *journalRecoveryWorkspace) Inspect(context.Context, string) (gitadapter.WorktreeState, error) {
	return w.state, nil
}

// CreateCheckpoint satisfies the Git workspace seam for a recovery-only fixture.
func (*journalRecoveryWorkspace) CreateCheckpoint(context.Context, gitadapter.CheckpointRequest) (gitadapter.CheckpointResult, error) {
	return gitadapter.CheckpointResult{}, nil
}

// Push satisfies the Git workspace seam for a recovery-only fixture.
func (*journalRecoveryWorkspace) Push(context.Context, gitadapter.PushRequest) error { return nil }

// SynchronizeBase satisfies the Git workspace seam for a recovery-only fixture.
func (*journalRecoveryWorkspace) SynchronizeBase(context.Context, gitadapter.BaseSyncRequest) error {
	return nil
}

var _ gitadapter.GitWorkspace = (*journalRecoveryWorkspace)(nil)

// journalRecoveryWorker supplies a matching active worker projection without
// exposing the worker's private runtime identity to the factory test.
type journalRecoveryWorker struct {
	inspection    worker.Inspection
	inspectErr    error
	inspectCalls  int
	startCalls    int
	startRequests []worker.StartRequest
	codexSeeds    []worker.CredentialSeedRequest
	seedErr       error
}

// Start recreates the worker from the coordinator's frozen request and updates
// the fixture projection so the next restart observes the repaired worker.
func (w *journalRecoveryWorker) Start(_ context.Context, request worker.StartRequest) error {
	w.startCalls++
	w.startRequests = append(w.startRequests, request)
	w.inspection = worker.Inspection{
		Exists:           true,
		Running:          true,
		Image:            request.Image + "@" + request.ImageDigest,
		MountFingerprint: worker.MountContractFingerprint(request),
	}
	return nil
}

// Resume satisfies WorkerRuntime for the no-launch recovery fixture.
func (*journalRecoveryWorker) Resume(context.Context, worker.ResumeRequest) error { return nil }

// RunCommand satisfies WorkerRuntime for the no-launch recovery fixture.
func (*journalRecoveryWorker) RunCommand(context.Context, worker.CommandRequest) (worker.CommandResult, error) {
	return worker.CommandResult{}, nil
}

// Stop satisfies WorkerRuntime for the no-launch recovery fixture.
func (*journalRecoveryWorker) Stop(context.Context, string) error { return nil }

// Inspect returns the matching active worker projection.
func (w *journalRecoveryWorker) Inspect(context.Context, string) (worker.Inspection, error) {
	w.inspectCalls++
	return w.inspection, w.inspectErr
}

// SeedCodexCredentials records restoration of the configured factory source.
func (w *journalRecoveryWorker) SeedCodexCredentials(_ context.Context, request worker.CredentialSeedRequest) error {
	w.codexSeeds = append(w.codexSeeds, request)
	return w.seedErr
}

var _ worker.WorkerRuntime = (*journalRecoveryWorker)(nil)
var _ worker.CredentialSeeder = (*journalRecoveryWorker)(nil)

// journalRecoveryTerminal supplies the persisted cmux workspace projection.
type journalRecoveryTerminal struct {
	inspection terminal.WorkspaceInspection
}

// EnsureControlWorkspace satisfies TerminalRuntime for the recovery fixture.
func (*journalRecoveryTerminal) EnsureControlWorkspace(context.Context, terminal.WorkspaceRequest) (terminal.Workspace, error) {
	return terminal.Workspace{ID: "workspace-control"}, nil
}

// EnsureRunWorkspace satisfies TerminalRuntime for the recovery fixture.
func (*journalRecoveryTerminal) EnsureRunWorkspace(context.Context, terminal.RunWorkspaceRequest) (terminal.RunWorkspace, error) {
	return terminal.RunWorkspace{}, nil
}

// CreateSurface satisfies TerminalRuntime for the recovery fixture.
func (*journalRecoveryTerminal) CreateSurface(context.Context, terminal.SurfaceRequest) (terminal.Surface, error) {
	return terminal.Surface{}, nil
}

// LaunchSurface satisfies TerminalRuntime for the recovery fixture.
func (*journalRecoveryTerminal) LaunchSurface(context.Context, terminal.SurfaceID, terminal.Command) error {
	return nil
}

// SendInput satisfies TerminalRuntime for the recovery fixture.
func (*journalRecoveryTerminal) SendInput(context.Context, terminal.SurfaceID, []byte) error {
	return nil
}

// Notify satisfies TerminalRuntime for the recovery fixture.
func (*journalRecoveryTerminal) Notify(context.Context, terminal.Notification) error { return nil }

// CloseSurface satisfies TerminalRuntime for the recovery fixture.
func (*journalRecoveryTerminal) CloseSurface(context.Context, terminal.SurfaceID) error { return nil }

// CloseWorkspace satisfies TerminalRuntime for the recovery fixture.
func (*journalRecoveryTerminal) CloseWorkspace(context.Context, terminal.WorkspaceID) error {
	return nil
}

// InspectWorkspace returns the persisted terminal topology.
func (t *journalRecoveryTerminal) InspectWorkspace(context.Context, terminal.WorkspaceID) (terminal.WorkspaceInspection, error) {
	return t.inspection, nil
}

var _ terminal.TerminalRuntime = (*journalRecoveryTerminal)(nil)
var _ terminal.WorkspaceInspector = (*journalRecoveryTerminal)(nil)

// journalRecoveryHarness supplies native-session identity and counts resume
// calls so the startup test can detect duplicate continuation.
type journalRecoveryHarness struct {
	nativeSessionID     string
	resumeCalls         int
	failResumeOnce      bool
	resumeErr           error
	nativeSessionExited bool
}

// Capabilities reports Codex's native continuation support.
func (*journalRecoveryHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{Name: harness.NameCodex, InteractiveResume: true}
}

// Start satisfies Runtime for the restart-only fixture.
func (*journalRecoveryHarness) Start(context.Context, harness.StartRequest) (harness.Session, error) {
	return harness.Session{}, nil
}

// Resume counts one native continuation and returns its stable identity.
func (h *journalRecoveryHarness) Resume(_ context.Context, request harness.StartRequest) (harness.Session, error) {
	h.resumeCalls++
	session := harness.Session{InvocationID: request.InvocationID, NativeSessionID: h.nativeSessionID, Surface: request.Surface}
	if h.resumeErr != nil {
		return session, h.resumeErr
	}
	if h.failResumeOnce {
		h.failResumeOnce = false
		return session, errors.New("native resume response lost")
	}
	return session, nil
}

// Finish satisfies Runtime for the restart-only fixture.
func (*journalRecoveryHarness) Finish(context.Context, harness.Session) error { return nil }

// NativeSessionID returns the stable native session projection.
func (h *journalRecoveryHarness) NativeSessionID(context.Context, harness.NativeSessionRequest) (string, error) {
	return h.nativeSessionID, nil
}

// NativeSessionRunning returns the controlled native process projection used
// to exercise live interruption detection.
func (h *journalRecoveryHarness) NativeSessionRunning(context.Context, harness.NativeSessionRequest) (bool, error) {
	return !h.nativeSessionExited, nil
}

var _ harness.Runtime = (*journalRecoveryHarness)(nil)
var _ harness.NativeSessionInspector = (*journalRecoveryHarness)(nil)
var _ harness.NativeSessionLivenessInspector = (*journalRecoveryHarness)(nil)

// ancestryGitWorkspace reports a remote head plus a fixed ancestry answer so a
// reconciliation test can distinguish an unpushed checkpoint from a diverged
// branch without running Git.
type ancestryGitWorkspace struct {
	remoteHead string
	ancestor   bool
	calls      int
}

// Create satisfies the Git workspace seam.
func (*ancestryGitWorkspace) Create(context.Context, string, string, string) (gitadapter.Workspace, error) {
	return gitadapter.Workspace{}, nil
}

// Remove satisfies the Git workspace seam.
func (*ancestryGitWorkspace) Remove(context.Context, string, gitadapter.Workspace) error { return nil }

// Inspect satisfies the read-only worktree seam.
func (*ancestryGitWorkspace) Inspect(context.Context, string) (gitadapter.WorktreeState, error) {
	return gitadapter.WorktreeState{}, nil
}

// CreateCheckpoint satisfies the Git workspace seam.
func (*ancestryGitWorkspace) CreateCheckpoint(context.Context, gitadapter.CheckpointRequest) (gitadapter.CheckpointResult, error) {
	return gitadapter.CheckpointResult{}, nil
}

// Push satisfies the Git workspace seam.
func (*ancestryGitWorkspace) Push(context.Context, gitadapter.PushRequest) error { return nil }

// SynchronizeBase satisfies the Git workspace seam.
func (*ancestryGitWorkspace) SynchronizeBase(context.Context, gitadapter.BaseSyncRequest) error {
	return nil
}

// RemoteBranchHead returns the configured remote projection.
func (w *ancestryGitWorkspace) RemoteBranchHead(context.Context, gitadapter.PushRequest) (string, error) {
	return w.remoteHead, nil
}

// IsAncestor reports the configured reachability answer.
func (w *ancestryGitWorkspace) IsAncestor(_ context.Context, _, _, _ string) (bool, error) {
	w.calls++
	return w.ancestor, nil
}

var _ gitadapter.GitWorkspace = (*ancestryGitWorkspace)(nil)
var _ gitadapter.RemoteBranchInspector = (*ancestryGitWorkspace)(nil)
var _ gitadapter.AncestryInspector = (*ancestryGitWorkspace)(nil)

// TestRemoteBranchBehindCheckpointIsNotADiscrepancy verifies that an unpushed
// checkpoint does not pause the run. A checkpoint is committed and persisted
// before the gate suite runs and pushed only afterwards, so a remote head that
// is an ancestor of the local checkpoint is an ordinary in-flight state.
func TestRemoteBranchBehindCheckpointIsNotADiscrepancy(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	run := store.Run{
		ID: "run-remote", Branch: "factory/run-remote", Worktree: "/worktree",
		CheckpointSHA: strings.Repeat("b", 40), PullRequestNumber: 7,
	}
	behind := &ancestryGitWorkspace{remoteHead: strings.Repeat("a", 40), ancestor: true}
	diagnosis := RecoveryDiagnosis{}
	inspectRemoteBranchProjection(ctx, &diagnosis, behind, run)
	if len(diagnosis.Discrepancies) != 0 {
		t.Fatalf("discrepancies for an unpushed checkpoint = %#v, want none", diagnosis.Discrepancies)
	}
	if behind.calls != 1 {
		t.Fatalf("ancestry checks = %d, want one", behind.calls)
	}

	// A remote head that is not reachable from the checkpoint has diverged and
	// must still pause the run.
	diverged := &ancestryGitWorkspace{remoteHead: strings.Repeat("c", 40), ancestor: false}
	divergedDiagnosis := RecoveryDiagnosis{}
	inspectRemoteBranchProjection(ctx, &divergedDiagnosis, diverged, run)
	if len(divergedDiagnosis.Discrepancies) != 1 {
		t.Fatalf("discrepancies for a diverged branch = %#v, want one", divergedDiagnosis.Discrepancies)
	}
	if divergedDiagnosis.Discrepancies[0].Field != "head" {
		t.Fatalf("diverged discrepancy field = %q, want %q", divergedDiagnosis.Discrepancies[0].Field, "head")
	}
}

// TestReconcileRepairsStaleProjectionAfterInterruptedCheck verifies that a
// status-effect recovery can repair the GitHub projection after an earlier
// local pause, while treating the completed implementation runtime as stopped
// during the coordinator-owned check stage.
func TestReconcileRepairsStaleProjectionAfterInterruptedCheck(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	worktreePath := filepath.Join(root, "worktree")
	if err := os.MkdirAll(worktreePath, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "state", "factory.db")
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	packetData, err := json.Marshal(SpecificationPacket{
		Version: specificationPacketVersion,
		Issue:   github.Issue{Number: 42, Title: "Recover check", Body: "recover the check stage"},
		RepositoryConfig: config.RepositoryConfig{
			TargetBranch: "main",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	run := store.Run{
		ID: "run-stale-check-projection", RepositoryPath: root, IssueNumber: 42,
		Stage: store.StageCheck, Status: store.StatusWaitingForHuman,
		Branch: "factory/run-stale-check-projection", Worktree: worktreePath,
		CheckpointSHA: strings.Repeat("a", 40), StatusCommentID: "status-stale-check-projection",
		LifecycleReason:     "restart reconciliation paused: pending commit status could not be published",
		SpecificationPacket: string(packetData), CreatedAt: now, UpdatedAt: now,
	}
	activeProjection := run
	activeProjection.Status = store.StatusActive
	activeProjection.LifecycleReason = "evaluating implementation checkpoint"
	invocation := store.Invocation{
		ID: "inv-stale-check-projection", RunID: run.ID, Harness: harness.NameCodex,
		Role: "implementation", Stage: store.StageImplementation,
		NativeSessionID: "session-stale-check-projection", WorkspaceID: "workspace-stale-check-projection",
		StatusSurfaceID:         "surface-status-stale-check-projection",
		ImplementationSurfaceID: "surface-implementation-stale-check-projection",
		ChecksSurfaceID:         "surface-checks-stale-check-projection",
		Status:                  store.InvocationStatusCompleted, CreatedAt: now, UpdatedAt: now,
	}
	opened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.SaveRun(ctx, run); err != nil {
		_ = opened.Close()
		t.Fatal(err)
	}
	if err := opened.SaveInvocation(ctx, invocation); err != nil {
		_ = opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	githubRuntime := &effectMatrixGitHub{
		issue:         github.Issue{Number: run.IssueNumber, State: "open", Labels: []string{github.LabelAgentRunning}},
		statusComment: github.Comment{ID: run.StatusCommentID, Body: statusCommentBody(activeProjection)},
	}
	workspace := &journalRecoveryWorkspace{state: gitadapter.WorktreeState{
		RepositoryPath: root, Branch: run.Branch, HeadSHA: run.CheckpointSHA,
	}}
	workerRuntime := &journalRecoveryWorker{inspectErr: errors.New("stopped worker cannot be inspected")}
	terminalRuntime := &journalRecoveryTerminal{inspection: terminal.WorkspaceInspection{
		Exists: true, WorkspaceID: terminal.WorkspaceID(invocation.WorkspaceID),
		Surfaces: []terminal.Surface{
			{ID: terminal.SurfaceID(invocation.StatusSurfaceID), WorkspaceID: terminal.WorkspaceID(invocation.WorkspaceID)},
			{ID: terminal.SurfaceID(invocation.ImplementationSurfaceID), WorkspaceID: terminal.WorkspaceID(invocation.WorkspaceID)},
			{ID: terminal.SurfaceID(invocation.ChecksSurfaceID), WorkspaceID: terminal.WorkspaceID(invocation.WorkspaceID)},
		},
	}}
	// An implementation session cannot be inspected through Docker after its
	// worker has been stopped. Check-stage recovery must not turn that expected
	// condition into a second discrepancy.
	harnessRuntime := &journalRecoveryHarness{}
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
		Path: root, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
		OperationalDataPath: databasePath, RepositoryConfigPath: filepath.Join(root, "factory.yaml"),
	}}}
	service := &Service{configPath: "/host/config.yaml", deps: Dependencies{
		Config:    effectMatrixConfig{host: host},
		OpenStore: func(ctx context.Context, path string) (OperationalStore, error) { return store.Open(ctx, path) },
		GitHub:    githubRuntime, GitWorkspace: workspace, Worktree: workspace,
		Worker: workerRuntime, Terminal: terminalRuntime, Harness: harnessRuntime,
		Now: func() time.Time { return now },
	}}

	result, err := service.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile() error = %v, want stale projection repair", err)
	}
	if result.Outcome != RecoveryOutcomeReconciled || !result.Diagnosis.SourcesAgree || len(result.Diagnosis.Discrepancies) != 0 {
		t.Fatalf("Reconcile() result = %#v, want reconciled projections", result)
	}
	if workerRuntime.inspectCalls != 0 {
		t.Fatalf("worker inspections = %d, want none for completed implementation at check boundary", workerRuntime.inspectCalls)
	}
	if got := githubRuntime.issue.Labels; len(got) != 1 || got[0] != github.LabelAgentNeedsInput {
		t.Fatalf("repaired issue labels = %#v, want agent-needs-input", got)
	}
	reopened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	persisted, err := reopened.CurrentRun(ctx)
	if err != nil || persisted == nil {
		t.Fatalf("persisted recovered run = %#v, error = %v", persisted, err)
	}
	if persisted.Status != store.StatusWaitingForHuman || persisted.Revision <= run.Revision {
		t.Fatalf("persisted recovered run = %#v, want waiting state with repaired revision", persisted)
	}
	if githubRuntime.statusComment.Body != statusCommentBody(*persisted) {
		t.Fatalf("repaired status comment = %q, want %q", githubRuntime.statusComment.Body, statusCommentBody(*persisted))
	}
}

// TestResumeReentersARecoveryPausedCheck verifies that explicit resume clears
// only the restart-reconciliation pause and does not launch a new agent for a
// coordinator-owned check stage.
func TestResumeReentersARecoveryPausedCheck(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 28, 12, 30, 0, 0, time.UTC)
	run := store.Run{
		ID: "run-resume-check", RepositoryPath: "/repo", IssueNumber: 42,
		Stage: store.StageCheck, Status: store.StatusWaitingForHuman,
		Branch: "factory/run-resume-check", Worktree: "/worktree/run-resume-check",
		CheckpointSHA: strings.Repeat("b", 40), StatusCommentID: "status-resume-check",
		LifecycleReason: "restart reconciliation paused: stale GitHub projection",
		CreatedAt:       now, UpdatedAt: now,
	}
	runStore := &recoveryResumeStore{run: run}
	githubRuntime := &effectMatrixGitHub{
		issue:         github.Issue{Number: run.IssueNumber, State: "open", Labels: []string{github.LabelAgentNeedsInput}},
		statusComment: github.Comment{ID: run.StatusCommentID, Body: statusCommentBody(run)},
	}
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
		Path: run.RepositoryPath, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
		OperationalDataPath: "/state/factory.db", RepositoryConfigPath: "/repo/factory.yaml",
	}}}
	service := &Service{configPath: "/host/config.yaml", deps: Dependencies{
		Config:    effectMatrixConfig{host: host},
		OpenStore: func(context.Context, string) (OperationalStore, error) { return runStore, nil },
		GitHub:    githubRuntime, Now: func() time.Time { return now },
	}}

	result, err := service.Resume(ctx, ResumeRequest{RunID: run.ID})
	if err != nil {
		t.Fatalf("Resume() error = %v, want check re-entry", err)
	}
	if result.Run.Status != store.StatusActive || runStore.run.Status != store.StatusActive {
		t.Fatalf("resumed run = %#v, want active check", result.Run)
	}
	if len(runStore.saved) != 1 {
		t.Fatalf("resume persistence count = %d, want one state transition", len(runStore.saved))
	}
	if got := githubRuntime.issue.Labels; len(got) != 1 || got[0] != github.LabelAgentRunning {
		t.Fatalf("resumed issue labels = %#v, want agent-running", got)
	}
	if githubRuntime.statusComment.Body != statusCommentBody(runStore.run) {
		t.Fatalf("resumed status comment = %q, want %q", githubRuntime.statusComment.Body, statusCommentBody(runStore.run))
	}
}

// recoveryResumeStore supplies the small public resume seam without exposing
// a visible invocation, allowing the test to detect an accidental fresh launch.
type recoveryResumeStore struct {
	run   store.Run
	saved []store.Run
}

// CurrentRun returns the one recovery-paused run.
func (s *recoveryResumeStore) CurrentRun(context.Context) (*store.Run, error) {
	run := s.run
	return &run, nil
}

// SaveRun records and persists the resumed run projection.
func (s *recoveryResumeStore) SaveRun(_ context.Context, run store.Run) error {
	s.run = run
	s.saved = append(s.saved, run)
	return nil
}

// ClaimLifecycleNotification satisfies the run-store notification seam.
func (*recoveryResumeStore) ClaimLifecycleNotification(context.Context, string, store.Status) (bool, error) {
	return true, nil
}

// ReleaseLifecycleNotification satisfies the run-store notification seam.
func (*recoveryResumeStore) ReleaseLifecycleNotification(context.Context, string, store.Status) error {
	return nil
}

// ActiveInvocation reports that check re-entry has no visible agent to launch.
func (*recoveryResumeStore) ActiveInvocation(context.Context, string) (*store.Invocation, error) {
	return nil, nil
}

// Close satisfies the operational-store seam.
func (*recoveryResumeStore) Close() error { return nil }

var _ RunStore = (*recoveryResumeStore)(nil)
var _ ActiveInvocationStore = (*recoveryResumeStore)(nil)
