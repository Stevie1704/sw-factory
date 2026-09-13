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
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

const recoveryTestImageDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestHeadlessStartupRecreatesWorkerAndResumesOnce verifies both supported
// harnesses recover exclusively through the frozen worker and native-session
// seams, and that a second coordinator cannot repeat the automatic resume.
func TestHeadlessStartupRecreatesWorkerAndResumesOnce(t *testing.T) {
	for _, harnessName := range []config.Harness{config.HarnessCodex, config.HarnessClaude} {
		t.Run(string(harnessName), func(t *testing.T) {
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
				Issue:   github.Issue{Number: 165, Title: "Recover headless work", Body: "resume the persisted invocation"},
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
				NativeSessionID: "session-headless-recovery-" + string(harnessName), InvocationDirectory: invocationDirectory,
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
			if harnessName == config.HarnessCodex {
				registration.Authentication.CodexAuthPath = authPath
			} else {
				registration.Authentication.ClaudeAuthPath = authPath
			}
			githubRuntime := &headlessRecoveryGitHub{
				issue:   github.Issue{Number: run.IssueNumber, State: "open", Labels: []string{factoryLabelForStatus(run.Status)}},
				comment: github.Comment{ID: run.StatusCommentID, Body: statusCommentBody(run)},
			}
			workerRuntime := &headlessRecoveryWorker{}
			harnessRuntime := &headlessRecoveryHarness{name: string(harnessName), nativeSessionID: invocation.NativeSessionID}
			dependencies := Dependencies{
				Config:    &headlessRecoveryConfig{host: config.HostConfig{SchemaVersion: config.CurrentHostSchemaVersion, Repositories: []config.RepositoryRegistration{registration}}},
				OpenStore: func(ctx context.Context, path string) (OperationalStore, error) { return store.Open(ctx, path) },
				GitHub:    githubRuntime, Worktree: &headlessRecoveryWorktree{state: gitadapter.WorktreeState{
					RepositoryPath: root, Branch: run.Branch, HeadSHA: run.CheckpointSHA,
				}}, Worker: workerRuntime,
				HeadlessHarnesses: map[config.Harness]harness.HeadlessRuntime{harnessName: harnessRuntime},
				Now:               func() time.Time { return now },
			}

			first := NewWithDependencies(filepath.Join(root, "host.yaml"), dependencies)
			if err := first.reconcileRegisteredRun(ctx, registration); err != nil {
				t.Fatalf("first startup reconciliation: %v", err)
			}
			if len(workerRuntime.starts) != 1 {
				t.Fatalf("worker starts = %#v, want one recreation", workerRuntime.starts)
			}
			start := workerRuntime.starts[0]
			if start.ImageDigest != run.ImageDigest || start.InvocationPath != invocation.InvocationDirectory || start.ResultPath != invocation.ResultDirectory || start.CredentialStoreID != root {
				t.Fatalf("recreated worker = %#v, want frozen invocation contract", start)
			}
			if len(workerRuntime.seeds) != 1 || workerRuntime.seeds[0].AuthPath != authPath {
				t.Fatalf("credential seeds = %#v, want registered %s source", workerRuntime.seeds, harnessName)
			}
			if len(harnessRuntime.resumes) != 1 || harnessRuntime.resumes[0].ResumeSessionID != invocation.NativeSessionID {
				t.Fatalf("native resumes = %#v, want one exact-session continuation", harnessRuntime.resumes)
			}

			persistedStore, err := store.Open(ctx, databasePath)
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := persistedStore.Invocation(ctx, run.ID, invocation.ID)
			if err != nil || persisted == nil || persisted.RecoveryResumeCount != 1 {
				t.Fatalf("persisted invocation = %#v, %v; want recovery generation one", persisted, err)
			}
			if pending, err := persistedStore.PendingEffect(ctx, run.ID); err != nil || pending != nil {
				t.Fatalf("pending effect = %#v, %v; want cleared", pending, err)
			}
			if err := persistedStore.Close(); err != nil {
				t.Fatal(err)
			}

			second := NewWithDependencies(filepath.Join(root, "host.yaml"), dependencies)
			if err := second.reconcileRegisteredRun(ctx, registration); err != nil {
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
	issue   github.Issue
	comment github.Comment
}

// Issue returns the current issue projection.
func (g *headlessRecoveryGitHub) Issue(context.Context, github.Repository, int) (github.Issue, error) {
	return g.issue, nil
}

// CreateLabel is unused by recovery.
func (*headlessRecoveryGitHub) CreateLabel(context.Context, github.Repository, github.Label) error {
	return errors.New("unexpected label creation")
}

// ReplaceIssueLabels updates the fixture issue projection.
func (g *headlessRecoveryGitHub) ReplaceIssueLabels(_ context.Context, _ github.Repository, _ int, labels []string) error {
	g.issue.Labels = append([]string(nil), labels...)
	return nil
}

// CreateIssueComment is unused because the run has a status comment.
func (*headlessRecoveryGitHub) CreateIssueComment(context.Context, github.Repository, int, string) (github.Comment, error) {
	return github.Comment{}, errors.New("unexpected comment creation")
}

// FindStatusComment returns the current coordinator-owned status projection.
func (g *headlessRecoveryGitHub) FindStatusComment(context.Context, github.Repository, int, string) (github.Comment, error) {
	return g.comment, nil
}

// EditIssueComment updates the fixture status projection.
func (g *headlessRecoveryGitHub) EditIssueComment(_ context.Context, _ github.Repository, _ string, body string) error {
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
	inspection worker.Inspection
	starts     []worker.StartRequest
	seeds      []worker.CredentialSeedRequest
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
	return nil
}

// SeedClaudeCredentials records a Claude credential projection.
func (w *headlessRecoveryWorker) SeedClaudeCredentials(_ context.Context, request worker.CredentialSeedRequest) error {
	w.seeds = append(w.seeds, request)
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
