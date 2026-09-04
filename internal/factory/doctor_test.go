package factory_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestDoctorComposesEverySubsystemAndKeepsOptionalAuthNonBlocking verifies
// the high-level coordinator reports all contributors in one invocation.
func TestDoctorComposesEverySubsystemAndKeepsOptionalAuthNonBlocking(t *testing.T) {
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRepositoryConfig(t, repositoryPath)
	writeSkillSmokeEvidence(t, repositoryPath)
	configPathOnDisk := filepath.Join(repositoryPath, config.RepositoryConfigFileName)
	configData, err := os.ReadFile(configPathOnDisk)
	if err != nil {
		t.Fatal(err)
	}
	configData = append(configData, []byte("role_craft:\n  implementation: docs/factory/craft/implementation.md\n  standards_review: docs/factory/craft/standards-review.md\n")...)
	if err := os.WriteFile(configPathOnDisk, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "host", "config.yaml")
	operationalPath := filepath.Join(root, "state", "factory.db")
	if err := config.SaveHost(configPath, config.HostConfig{
		SchemaVersion: config.CurrentSchemaVersion,
		Repositories: []config.RepositoryRegistration{{
			Path:                 repositoryPath,
			GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
			AuthorizedUsers:      []string{"alice"},
			Polling:              config.PollingConfig{Interval: "30s", Backoff: "5m"},
			OperationalDataPath:  operationalPath,
			RepositoryConfigPath: filepath.Join(repositoryPath, config.RepositoryConfigFileName),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	opened, err := store.Open(context.Background(), operationalPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("Open().Close() error = %v", err)
	}

	gitWorkspace := &factoryDoctorGitWorkspace{}
	workerRuntime := &factoryDoctorWorker{}
	terminalRuntime := &factoryDoctorTerminal{}
	service := factory.NewWithDependencies(configPath, factory.Dependencies{
		GitHub:       &fakeGitHub{},
		GitWorkspace: gitWorkspace,
		Worktree:     gitWorkspace,
		Worker:       workerRuntime,
		Terminal:     terminalRuntime,
	})

	result, err := service.Doctor(context.Background())
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if !result.Ready() || len(result.Report.Failures()) != 0 || len(result.Report.Warnings()) != 2 {
		t.Fatalf("Doctor() report = %#v, want ready with two auth warnings", result.Report)
	}
	wantNames := []string{
		"configuration",
		"github authentication", "github permissions", "github labels",
		"git remote", "git hooks", "git worktree",
		"git role craft implementation", "git role craft standards_review",
		"cmux executable", "cmux socket",
		"docker daemon", "worker image",
		"harness capability",
		"claude worker executable", "claude worker skill contract",
		"codex worker executable", "codex worker skill contract",
		"codex authentication", "claude authentication", "SQLite",
	}
	if len(result.Report.Results) != len(wantNames) {
		t.Fatalf("Doctor() check count = %d, want %d (%#v)", len(result.Report.Results), len(wantNames), result.Report.Results)
	}
	for index, name := range wantNames {
		if result.Report.Results[index].Name != name {
			t.Fatalf("Doctor() result[%d].Name = %q, want %q", index, result.Report.Results[index].Name, name)
		}
	}
	if workerRuntime.harnessCalls != 2 {
		t.Fatalf("harness probes = %d, want both built-in harnesses", workerRuntime.harnessCalls)
	}
}

// CheckAuthentication implements the read-only GitHub diagnosis seam.
func (*fakeGitHub) CheckAuthentication(context.Context) error { return nil }

// CheckRepositoryAccess implements the read-only GitHub diagnosis seam.
func (*fakeGitHub) CheckRepositoryAccess(context.Context, github.Repository) error { return nil }

// CheckFactoryLabels implements the read-only GitHub diagnosis seam.
func (*fakeGitHub) CheckFactoryLabels(context.Context, github.Repository) error { return nil }

// factoryDoctorGitWorkspace implements the task-oriented Git and diagnosis
// seams without making host Git calls in the composition test.
type factoryDoctorGitWorkspace struct{}

// Create implements the worktree manager seam.
func (*factoryDoctorGitWorkspace) Create(context.Context, string, string, string) (gitadapter.Workspace, error) {
	return gitadapter.Workspace{}, nil
}

// Remove implements the worktree manager seam.
func (*factoryDoctorGitWorkspace) Remove(context.Context, string, gitadapter.Workspace) error {
	return nil
}

// Inspect implements the worktree observation seam.
func (*factoryDoctorGitWorkspace) Inspect(context.Context, string) (gitadapter.WorktreeState, error) {
	return gitadapter.WorktreeState{}, nil
}

// CreateCheckpoint implements the checkpoint seam.
func (*factoryDoctorGitWorkspace) CreateCheckpoint(context.Context, gitadapter.CheckpointRequest) (gitadapter.CheckpointResult, error) {
	return gitadapter.CheckpointResult{}, nil
}

// Push implements the host push seam.
func (*factoryDoctorGitWorkspace) Push(context.Context, gitadapter.PushRequest) error { return nil }

// SynchronizeBase implements the base synchronization seam.
func (*factoryDoctorGitWorkspace) SynchronizeBase(context.Context, gitadapter.BaseSyncRequest) error {
	return nil
}

// CheckRemote implements the read-only Git diagnosis seam.
func (*factoryDoctorGitWorkspace) CheckRemote(context.Context, gitadapter.DoctorRequest) error {
	return nil
}

// CheckHooks implements the read-only Git diagnosis seam.
func (*factoryDoctorGitWorkspace) CheckHooks(context.Context, gitadapter.DoctorRequest) error {
	return nil
}

// CheckWorktree implements the read-only Git diagnosis seam.
func (*factoryDoctorGitWorkspace) CheckWorktree(context.Context, gitadapter.DoctorRequest) error {
	return nil
}

// CheckRoleCraft implements the optional role-craft diagnosis seam.
func (*factoryDoctorGitWorkspace) CheckRoleCraft(context.Context, gitadapter.DoctorRequest, string, string) error {
	return nil
}

// factoryDoctorWorker implements worker lifecycle and diagnosis seams for the
// composition test.
type factoryDoctorWorker struct {
	harnessCalls int
}

// Start implements WorkerRuntime.
func (*factoryDoctorWorker) Start(context.Context, worker.StartRequest) error { return nil }

// Resume implements WorkerRuntime.
func (*factoryDoctorWorker) Resume(context.Context, worker.ResumeRequest) error { return nil }

// RunCommand implements WorkerRuntime.
func (*factoryDoctorWorker) RunCommand(context.Context, worker.CommandRequest) (worker.CommandResult, error) {
	return worker.CommandResult{}, nil
}

// Stop implements WorkerRuntime.
func (*factoryDoctorWorker) Stop(context.Context, string) error { return nil }

// Inspect implements WorkerRuntime.
func (*factoryDoctorWorker) Inspect(context.Context, string) (worker.Inspection, error) {
	return worker.Inspection{}, nil
}

// CheckDocker implements the read-only worker diagnosis seam.
func (*factoryDoctorWorker) CheckDocker(context.Context) error { return nil }

// CheckImage implements the read-only worker diagnosis seam.
func (*factoryDoctorWorker) CheckImage(context.Context, worker.ImageReference) error { return nil }

// CheckHarness implements the worker-owned harness executable probe seam.
func (w *factoryDoctorWorker) CheckHarness(context.Context, worker.HarnessCheckRequest) error {
	w.harnessCalls++
	return nil
}

// CheckHarnessAuthentication implements the worker-owned credential probe
// seam.
func (*factoryDoctorWorker) CheckHarnessAuthentication(context.Context, worker.HarnessAuthenticationCheckRequest) error {
	return nil
}

// CheckSkillContract implements the worker-owned skill contract probe seam.
func (*factoryDoctorWorker) CheckSkillContract(_ context.Context, request worker.SkillContractRequest) (worker.SkillContract, error) {
	return worker.SkillContract{Harness: request.Harness, Version: doctorHarnessVersion}, nil
}

// doctorHarnessVersion is the harness version the composition test's worker
// probe reports and its recorded smoke evidence is keyed by.
const doctorHarnessVersion = "1.2.3"

// writeSkillSmokeEvidence records a smoke result for both shipped harnesses at
// the repository's pinned worker digest.
func writeSkillSmokeEvidence(t *testing.T, repositoryPath string) {
	t.Helper()
	path := filepath.Join(repositoryPath, filepath.FromSlash(harness.SkillEvidenceFile))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	evidence := harness.SkillSmokeEvidence{SchemaVersion: 1}
	for _, name := range []string{harness.NameClaude, harness.NameCodex} {
		evidence.Records = append(evidence.Records, harness.SkillSmokeRecord{
			ImageDigest:    "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			Harness:        name,
			HarnessVersion: doctorHarnessVersion,
			Skills:         harness.MandatorySkills(),
			VerifiedAt:     "2026-09-04T00:00:00Z",
		})
	}
	body, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// factoryDoctorTerminal implements terminal lifecycle and diagnosis seams for
// the composition test.
type factoryDoctorTerminal struct{}

// EnsureControlWorkspace implements TerminalRuntime.
func (*factoryDoctorTerminal) EnsureControlWorkspace(context.Context, terminal.WorkspaceRequest) (terminal.Workspace, error) {
	return terminal.Workspace{}, nil
}

// EnsureRunWorkspace implements TerminalRuntime.
func (*factoryDoctorTerminal) EnsureRunWorkspace(context.Context, terminal.RunWorkspaceRequest) (terminal.RunWorkspace, error) {
	return terminal.RunWorkspace{}, nil
}

// CreateSurface implements TerminalRuntime.
func (*factoryDoctorTerminal) CreateSurface(context.Context, terminal.SurfaceRequest) (terminal.Surface, error) {
	return terminal.Surface{}, nil
}

// LaunchSurface implements TerminalRuntime.
func (*factoryDoctorTerminal) LaunchSurface(context.Context, terminal.SurfaceID, terminal.Command) error {
	return nil
}

// SendInput implements TerminalRuntime.
func (*factoryDoctorTerminal) SendInput(context.Context, terminal.SurfaceID, []byte) error {
	return nil
}

// Notify implements TerminalRuntime.
func (*factoryDoctorTerminal) Notify(context.Context, terminal.Notification) error { return nil }

// CloseSurface implements TerminalRuntime.
func (*factoryDoctorTerminal) CloseSurface(context.Context, terminal.SurfaceID) error { return nil }

// CloseWorkspace implements TerminalRuntime.
func (*factoryDoctorTerminal) CloseWorkspace(context.Context, terminal.WorkspaceID) error { return nil }

// CheckExecutable implements the read-only cmux diagnosis seam.
func (*factoryDoctorTerminal) CheckExecutable(context.Context) error { return nil }

// CheckSocket implements the read-only cmux diagnosis seam.
func (*factoryDoctorTerminal) CheckSocket(context.Context) error { return nil }

var _ github.DoctorClient = (*fakeGitHub)(nil)
var _ gitadapter.GitWorkspace = (*factoryDoctorGitWorkspace)(nil)
var _ gitadapter.DoctorChecker = (*factoryDoctorGitWorkspace)(nil)
var _ worker.WorkerRuntime = (*factoryDoctorWorker)(nil)
var _ worker.DoctorChecker = (*factoryDoctorWorker)(nil)
var _ worker.HarnessChecker = (*factoryDoctorWorker)(nil)
var _ worker.HarnessAuthenticationChecker = (*factoryDoctorWorker)(nil)
var _ terminal.TerminalRuntime = (*factoryDoctorTerminal)(nil)
var _ terminal.DoctorChecker = (*factoryDoctorTerminal)(nil)
