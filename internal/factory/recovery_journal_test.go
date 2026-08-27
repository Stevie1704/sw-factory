package factory

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

// TestJournaledStartupResumesOneAgreeingInvocationAcrossRestart verifies the
// real startup reconciliation path accepts matching projections, resumes the
// native session once, persists the resume ceiling, and performs no second
// resume when a fresh coordinator starts again.
func TestJournaledStartupResumesOneAgreeingInvocationAcrossRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	worktreePath := filepath.Join(root, "worktree")
	if err := os.MkdirAll(worktreePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
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

	registration := config.RepositoryRegistration{
		Path:   root,
		GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
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
	workerRequest := worker.StartRequest{
		RunID:           run.ID,
		WorktreePath:    run.Worktree,
		GitMetadataPath: gitMetadataProjectionPath(run.ID, run.Worktree),
		Image:           packet.RepositoryConfig.WorkerBuild.Image,
		ImageDigest:     run.ImageDigest,
		InvocationPath:  invocation.InvocationDirectory,
		ResultPath:      invocation.ResultDirectory,
		Role:            invocation.Role,
	}
	workerRuntime := &journalRecoveryWorker{inspection: worker.Inspection{
		Exists:           true,
		Running:          true,
		Image:            workerRequest.Image + "@" + workerRequest.ImageDigest,
		MountFingerprint: worker.MountContractFingerprint(workerRequest),
	}}
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
		Now:          func() time.Time { return now },
	}}

	updatedRun := run
	if err := service.ensureAgentStartup(ctx, registration, opened, &updatedRun, AgentRequest{}); err != nil {
		t.Fatalf("first journaled startup = %v", err)
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
	if err := fresh.ensureAgentStartup(ctx, registration, opened, freshRun, AgentRequest{}); err != nil {
		t.Fatalf("second journaled startup = %v", err)
	}
	if harnessRuntime.resumeCalls != 1 {
		t.Fatalf("native resumes after second startup = %d, want no duplicate", harnessRuntime.resumeCalls)
	}
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
	inspection worker.Inspection
}

// Start satisfies WorkerRuntime for the no-launch recovery fixture.
func (*journalRecoveryWorker) Start(context.Context, worker.StartRequest) error { return nil }

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
	return w.inspection, nil
}

var _ worker.WorkerRuntime = (*journalRecoveryWorker)(nil)

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
	nativeSessionID string
	resumeCalls     int
	failResumeOnce  bool
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

var _ harness.Runtime = (*journalRecoveryHarness)(nil)
var _ harness.NativeSessionInspector = (*journalRecoveryHarness)(nil)

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
