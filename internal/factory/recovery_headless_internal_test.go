package factory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

const recoveryTestImageDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestHeadlessStartupRecreatesWorkerAndResumesOnce verifies every supported
// harness recovers exclusively through the frozen worker and native-session
// seams, and that a second coordinator cannot repeat the automatic resume.
func TestHeadlessStartupRecreatesWorkerAndResumesOnce(t *testing.T) {
	for _, harnessName := range []config.Harness{config.HarnessCodex, config.HarnessClaude, config.HarnessCopilot} {
		t.Run(string(harnessName), func(t *testing.T) {
			ctx := t.Context()
			fixture := newHeadlessRecoveryFixture(t, harnessName, "session-headless-recovery-"+string(harnessName))
			run, invocation, workerRuntime := fixture.run, fixture.invocation, fixture.worker
			harnessRuntime := &headlessRecoveryHarness{name: string(harnessName), nativeSessionID: invocation.NativeSessionID}
			dependencies := fixture.dependencies(map[config.Harness]harness.HeadlessRuntime{harnessName: harnessRuntime})

			first := NewWithDependencies(filepath.Join(fixture.root, "host.yaml"), dependencies)
			if err := first.reconcileRegisteredRun(ctx, fixture.registration); err != nil {
				t.Fatalf("first startup reconciliation: %v", err)
			}
			if len(workerRuntime.starts) != 1 {
				t.Fatalf("worker starts = %#v, want one recreation", workerRuntime.starts)
			}
			start := workerRuntime.starts[0]
			if start.ImageDigest != run.ImageDigest || start.InvocationPath != invocation.InvocationDirectory || start.ResultPath != invocation.ResultDirectory || start.CredentialStoreID != fixture.root {
				t.Fatalf("recreated worker = %#v, want frozen invocation contract", start)
			}
			if len(workerRuntime.seeds) != 1 || workerRuntime.seeds[0].AuthPath != fixture.authPath || workerRuntime.seededHarnesses[0] != harnessName {
				t.Fatalf("credential seeds = %#v (%v), want registered %s source", workerRuntime.seeds, workerRuntime.seededHarnesses, harnessName)
			}
			if len(harnessRuntime.resumes) != 1 || harnessRuntime.resumes[0].ResumeSessionID != invocation.NativeSessionID {
				t.Fatalf("native resumes = %#v, want one exact-session continuation", harnessRuntime.resumes)
			}

			if persisted := fixture.persistedInvocation(t); persisted.RecoveryResumeCount != 1 {
				t.Fatalf("persisted invocation = %#v; want recovery generation one", persisted)
			}
			if pending := fixture.pendingEffect(t); pending != nil {
				t.Fatalf("pending effect = %#v; want cleared", pending)
			}

			second := NewWithDependencies(filepath.Join(fixture.root, "host.yaml"), dependencies)
			if err := second.reconcileRegisteredRun(ctx, fixture.registration); err != nil {
				t.Fatalf("second startup reconciliation: %v", err)
			}
			if len(workerRuntime.starts) != 1 || len(harnessRuntime.resumes) != 1 {
				t.Fatalf("second startup duplicated recovery: starts %d resumes %d", len(workerRuntime.starts), len(harnessRuntime.resumes))
			}
			if len(workerRuntime.seeds) != 2 {
				t.Fatalf("credential seeds after restart = %d, want projection restored once per startup", len(workerRuntime.seeds))
			}
		})
	}
}

// headlessRecoveryFixture is one persisted active implementation invocation
// whose worker is gone, registered with a credential source for its harness.
type headlessRecoveryFixture struct {
	root, databasePath, authPath string
	run                          store.Run
	invocation                   store.Invocation
	registration                 config.RepositoryRegistration
	github                       *headlessRecoveryGitHub
	worker                       *headlessRecoveryWorker
	now                          time.Time
}

// newHeadlessRecoveryFixture persists the run and invocation in a real
// operational store, as a coordinator that stopped mid-invocation leaves them.
func newHeadlessRecoveryFixture(t *testing.T, harnessName config.Harness, nativeSessionID string) headlessRecoveryFixture {
	t.Helper()
	ctx := t.Context()
	root := t.TempDir()
	runID := "run-headless-recovery-" + string(harnessName)
	branch := "factory/" + runID
	worktreePath := filepath.Join(root, "worktree")
	invocationDirectory := filepath.Join(root, "invocation")
	resultDirectory := filepath.Join(root, "results")
	for _, path := range []string{worktreePath, invocationDirectory, resultDirectory, filepath.Join(root, ".git")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/"+branch+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(root, string(harnessName)+"-auth.json")
	if err := os.WriteFile(authPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	packet := SpecificationPacket{
		Version: specificationPacketVersion,
		Issue:   tracker.Issue{Number: 165, Title: "Recover headless work", Body: "resume the persisted invocation"},
		RepositoryConfig: config.RepositoryConfig{
			TargetBranch: "main", WorkerBuild: config.WorkerBuildConfig{Image: "ghcr.io/example/factory-worker"},
		},
	}
	packetData, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	run := store.Run{
		ID: runID, RepositoryPath: root, IssueNumber: packet.Issue.Number,
		Stage: store.StageImplementation, Status: store.StatusActive, Branch: branch,
		Worktree: worktreePath, CheckpointSHA: "checkpoint-recovery", ImageDigest: recoveryTestImageDigest,
		Coordinator: "coordinator-test", StatusCommentID: "status-recovery", SpecificationPacket: string(packetData),
		CreatedAt: now, UpdatedAt: now,
	}
	invocation := store.Invocation{
		ID: "inv-headless-recovery-" + string(harnessName), RunID: run.ID, Harness: string(harnessName),
		Role: "implementation", Stage: store.StageImplementation, Model: "model-test", CredentialStoreID: root,
		NativeSessionID: nativeSessionID, InvocationDirectory: invocationDirectory,
		ResultDirectory: resultDirectory, PromptVersion: prompt.VersionFor("implementation", string(store.StageImplementation)),
		Status: store.InvocationStatusActive, CreatedAt: now, UpdatedAt: now,
	}
	if err := writeInvocationPacket(invocationDirectory, InvocationPacket{
		SchemaVersion: invocationPacketVersion, InvocationID: invocation.ID, RunID: run.ID,
		Role: invocation.Role, Stage: invocation.Stage, SpecificationPacket: run.SpecificationPacket,
		PromptVersion: invocation.PromptVersion,
	}); err != nil {
		t.Fatal(err)
	}

	databasePath := filepath.Join(root, "state", "factory.db")
	opened, err := store.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := opened.SaveInvocation(ctx, invocation); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	registration := config.RepositoryRegistration{
		Path: root, OperationalDataPath: databasePath,
		GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
	}
	switch harnessName {
	case config.HarnessCodex:
		registration.Authentication.CodexAuthPath = authPath
	case config.HarnessClaude:
		registration.Authentication.ClaudeAuthPath = authPath
	case config.HarnessCopilot:
		registration.Authentication.CopilotAuthPath = authPath
	}
	return headlessRecoveryFixture{
		root: root, databasePath: databasePath, authPath: authPath, run: run, invocation: invocation,
		registration: registration, now: now, worker: &headlessRecoveryWorker{},
		github: &headlessRecoveryGitHub{
			issue:   tracker.Issue{Number: run.IssueNumber, State: "open", Labels: []string{factoryLabelForStatus(run.Status)}},
			comment: tracker.Comment{ID: run.StatusCommentID, Body: statusCommentBody(run)},
		},
	}
}

// dependencies wires the fixture into a coordinator with the given adapters.
// Each service built from them is a new coordinator over the same durable
// state, which is how a test restarts the coordinator.
func (f headlessRecoveryFixture) dependencies(adapters map[config.Harness]harness.HeadlessRuntime) Dependencies {
	return Dependencies{
		Config:    &headlessRecoveryConfig{host: config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{f.registration}}},
		OpenStore: func(ctx context.Context, path string) (OperationalStore, error) { return store.Open(ctx, path) },
		Tracker:   f.github, Worktree: &headlessRecoveryWorktree{state: gitadapter.WorktreeState{
			RepositoryPath: f.root, Branch: f.run.Branch, HeadSHA: f.run.CheckpointSHA,
		}}, Worker: f.worker,
		HeadlessHarnesses: adapters,
		Now:               func() time.Time { return f.now },
	}
}

// persistedRun reads the durable run projection.
func (f headlessRecoveryFixture) persistedRun(t *testing.T) store.Run {
	t.Helper()
	opened, err := store.Open(t.Context(), f.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	run, err := opened.Run(t.Context(), f.run.ID)
	if err != nil || run == nil {
		t.Fatalf("persisted run = %#v, %v", run, err)
	}
	return *run
}

// persistedInvocation reads the durable invocation projection.
func (f headlessRecoveryFixture) persistedInvocation(t *testing.T) store.Invocation {
	t.Helper()
	opened, err := store.Open(t.Context(), f.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	invocation, err := opened.Invocation(t.Context(), f.run.ID, f.invocation.ID)
	if err != nil || invocation == nil {
		t.Fatalf("persisted invocation = %#v, %v", invocation, err)
	}
	return *invocation
}

// pendingEffect reads the run's durable pending-effect reservation.
func (f headlessRecoveryFixture) pendingEffect(t *testing.T) *store.PendingEffect {
	t.Helper()
	opened, err := store.Open(t.Context(), f.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	pending, err := opened.PendingEffect(t.Context(), f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return pending
}

// headlessRecoveryConfig returns one fixed host registration.
type headlessRecoveryConfig struct{ host config.HostConfig }

// Load returns the fixture host configuration.
func (c *headlessRecoveryConfig) Load(string) (config.HostConfig, error) { return c.host, nil }

// Save is unused by startup recovery.
func (*headlessRecoveryConfig) Save(string, config.HostConfig) error {
	return errors.New("unexpected save")
}

// Create is unused by startup recovery.
func (*headlessRecoveryConfig) Create(string) (config.HostConfig, error) {
	return config.HostConfig{}, errors.New("unexpected create")
}

// headlessRecoveryGitHub exposes a converged issue and status comment.
type headlessRecoveryGitHub struct {
	issue   tracker.Issue
	comment tracker.Comment
}

// Issue returns the current issue projection.
func (g *headlessRecoveryGitHub) Issue(context.Context, tracker.Repository, int) (tracker.Issue, error) {
	return g.issue, nil
}

// CreateLabel is unused by recovery.
func (*headlessRecoveryGitHub) CreateLabel(context.Context, tracker.Repository, tracker.Label) error {
	return errors.New("unexpected label creation")
}

// ReplaceIssueLabels updates the fixture issue projection.
func (g *headlessRecoveryGitHub) ReplaceIssueLabels(_ context.Context, _ tracker.Repository, _ int, labels []string) error {
	g.issue.Labels = append([]string(nil), labels...)
	return nil
}

// CreateIssueComment is unused because the run has a status comment.
func (*headlessRecoveryGitHub) CreateIssueComment(context.Context, tracker.Repository, int, string) (tracker.Comment, error) {
	return tracker.Comment{}, errors.New("unexpected comment creation")
}

// FindStatusComment returns the current coordinator-owned status projection.
func (g *headlessRecoveryGitHub) FindStatusComment(context.Context, tracker.Repository, int, string) (tracker.Comment, error) {
	return g.comment, nil
}

// EditIssueComment updates the fixture status projection.
func (g *headlessRecoveryGitHub) EditIssueComment(_ context.Context, _ tracker.Repository, _ string, body string) error {
	g.comment.Body = body
	return nil
}

// headlessRecoveryWorktree exposes one matching Git projection.
type headlessRecoveryWorktree struct{ state gitadapter.WorktreeState }

// Create is unused by recovery.
func (*headlessRecoveryWorktree) Create(context.Context, string, string, string) (gitadapter.Workspace, error) {
	return gitadapter.Workspace{}, errors.New("unexpected create")
}

// Remove is unused by recovery.
func (*headlessRecoveryWorktree) Remove(context.Context, string, gitadapter.Workspace) error {
	return errors.New("unexpected remove")
}

// Inspect returns the matching worktree projection.
func (w *headlessRecoveryWorktree) Inspect(context.Context, string) (gitadapter.WorktreeState, error) {
	return w.state, nil
}

// headlessRecoveryWorker records frozen recreation and credential projections.
type headlessRecoveryWorker struct {
	inspection      worker.Inspection
	starts          []worker.StartRequest
	seeds           []worker.CredentialSeedRequest
	seededHarnesses []config.Harness
}

// Start records a recreated worker and exposes its matching live projection.
func (w *headlessRecoveryWorker) Start(_ context.Context, request worker.StartRequest) error {
	w.starts = append(w.starts, request)
	w.inspection = worker.Inspection{
		Exists: true, Running: true, Image: request.Image + "@" + request.ImageDigest,
		MountFingerprint: worker.MountContractFingerprint(request),
	}
	return nil
}

// Resume is unused because recovery recreates the worker from its frozen request.
func (*headlessRecoveryWorker) Resume(context.Context, worker.ResumeRequest) error { return nil }

// RunCommand is unused by recovery.
func (*headlessRecoveryWorker) RunCommand(context.Context, worker.CommandRequest) (worker.CommandResult, error) {
	return worker.CommandResult{}, errors.New("unexpected command")
}

// Stop accepts idempotent worker shutdown before recreation.
func (*headlessRecoveryWorker) Stop(context.Context, string) error { return nil }

// Inspect returns the current worker projection.
func (w *headlessRecoveryWorker) Inspect(context.Context, string) (worker.Inspection, error) {
	return w.inspection, nil
}

// SeedCodexCredentials records a Codex credential projection.
func (w *headlessRecoveryWorker) SeedCodexCredentials(_ context.Context, request worker.CredentialSeedRequest) error {
	w.seeds = append(w.seeds, request)
	w.seededHarnesses = append(w.seededHarnesses, config.HarnessCodex)
	return nil
}

// SeedClaudeCredentials records a Claude credential projection.
func (w *headlessRecoveryWorker) SeedClaudeCredentials(_ context.Context, request worker.CredentialSeedRequest) error {
	w.seeds = append(w.seeds, request)
	w.seededHarnesses = append(w.seededHarnesses, config.HarnessClaude)
	return nil
}

// SeedCopilotCredentials records a Copilot token projection.
func (w *headlessRecoveryWorker) SeedCopilotCredentials(_ context.Context, request worker.CredentialSeedRequest) error {
	w.seeds = append(w.seeds, request)
	w.seededHarnesses = append(w.seededHarnesses, config.HarnessCopilot)
	return nil
}

// headlessRecoveryHarness records exact native-session continuations.
type headlessRecoveryHarness struct {
	name            string
	nativeSessionID string
	resumes         []harness.HeadlessStartRequest
}

// Capabilities identifies one supported headless harness.
func (h *headlessRecoveryHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{Name: h.name, NativeResume: true, Headless: true}
}

// StartHeadless is unused by persisted recovery.
func (*headlessRecoveryHarness) StartHeadless(context.Context, harness.HeadlessStartRequest) (harness.HeadlessSession, error) {
	return harness.HeadlessSession{}, errors.New("unexpected fresh launch")
}

// ResumeHeadless records one exact native-session continuation.
func (h *headlessRecoveryHarness) ResumeHeadless(_ context.Context, request harness.HeadlessStartRequest) (harness.HeadlessSession, error) {
	h.resumes = append(h.resumes, request)
	return harness.HeadlessSession{
		InvocationID: request.InvocationID, RunID: request.RunID, WorkerID: request.WorkerID,
		Role: request.Role, NativeSessionID: request.ResumeSessionID,
	}, nil
}

// InspectHeadless reports the persisted process running.
func (*headlessRecoveryHarness) InspectHeadless(context.Context, harness.HeadlessInspectionRequest) (harness.HeadlessInspection, error) {
	return harness.HeadlessInspection{Status: worker.HeadlessStatusRunning}, nil
}

// CancelHeadless is unused by successful recovery.
func (*headlessRecoveryHarness) CancelHeadless(context.Context, harness.HeadlessSession) error {
	return nil
}

// FinishHeadless is unused by active recovery.
func (*headlessRecoveryHarness) FinishHeadless(context.Context, harness.HeadlessSession) error {
	return nil
}

// NativeSessionID returns the exact persisted native identity.
func (h *headlessRecoveryHarness) NativeSessionID(context.Context, harness.NativeSessionRequest) (string, error) {
	return h.nativeSessionID, nil
}

// NativeSessionRunning reports the persisted native process active.
func (*headlessRecoveryHarness) NativeSessionRunning(context.Context, harness.NativeSessionRequest) (bool, error) {
	return true, nil
}
