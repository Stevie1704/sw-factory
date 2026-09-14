package factory_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// TestDoctorChecksBothHarnessesWithoutATerminalDependency verifies the public
// startup diagnosis follows the same terminal-free contract as dispatch.
func TestDoctorChecksBothHarnessesWithoutATerminalDependency(t *testing.T) {
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRepositoryConfig(t, repositoryPath)
	repositoryConfigPath := filepath.Join(repositoryPath, config.RepositoryConfigFileName)
	body, err := os.ReadFile(repositoryConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), "  implementation: codex", "  implementation: claude", 1))
	body = []byte(strings.Replace(string(body), "  implementation: [gpt-5]", "  implementation: [claude-opus-5]", 1))
	if err := os.WriteFile(repositoryConfigPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	operationalPath := filepath.Join(root, "state", "factory.db")
	configPath := filepath.Join(root, "host", "config.yaml")
	if err := config.SaveHost(configPath, config.HostConfig{
		SchemaVersion: config.CurrentHostSchemaVersion,
		Repositories: []config.RepositoryRegistration{{
			Path: repositoryPath, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
			AuthorizedUsers: []string{"alice"}, Polling: config.PollingConfig{Interval: "30s", Backoff: "5m"},
			OperationalDataPath: operationalPath, RepositoryConfigPath: repositoryConfigPath,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	opened, err := store.Open(t.Context(), operationalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	gitWorkspace := &doctorContractGitWorkspace{}
	workerRuntime := &doctorContractWorker{headlessAgentWorker: &headlessAgentWorker{agentWorker: &agentWorker{}}}
	service := factory.NewWithDependencies(configPath, factory.Dependencies{
		GitHub: &fakeGitHub{}, GitWorkspace: gitWorkspace, Worktree: gitWorkspace, Worker: workerRuntime,
	})
	result, err := service.Doctor(t.Context())
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	var names []string
	for _, check := range result.Report.Results {
		names = append(names, check.Name)
		if strings.Contains(check.Name, "cmux") || strings.Contains(check.Name, "tmux") {
			t.Fatalf("Doctor() retained terminal prerequisite %q", check.Name)
		}
	}
	joined := strings.Join(names, "\n")
	for _, wanted := range []string{"codex worker executable", "claude worker executable", "harness capability"} {
		if !strings.Contains(joined, wanted) {
			t.Fatalf("Doctor() checks = %q, want %q", joined, wanted)
		}
	}
	if workerRuntime.headlessChecks != 1 || workerRuntime.harnessChecks != 2 {
		t.Fatalf("Doctor() worker probes = headless:%d harness:%d, want 1 and 2", workerRuntime.headlessChecks, workerRuntime.harnessChecks)
	}
}

// doctorContractGitWorkspace supplies both the task-oriented Git seam and its
// read-only doctor checks without executing host Git commands.
type doctorContractGitWorkspace struct{ fakeWorktree }

// Inspect returns an empty diagnosis fixture projection.
func (*doctorContractGitWorkspace) Inspect(context.Context, string) (gitadapter.WorktreeState, error) {
	return gitadapter.WorktreeState{}, nil
}

// CreateCheckpoint is outside this diagnosis-only fixture.
func (*doctorContractGitWorkspace) CreateCheckpoint(context.Context, gitadapter.CheckpointRequest) (gitadapter.CheckpointResult, error) {
	return gitadapter.CheckpointResult{}, nil
}

// Push is outside this diagnosis-only fixture.
func (*doctorContractGitWorkspace) Push(context.Context, gitadapter.PushRequest) error { return nil }

// SynchronizeBase is outside this diagnosis-only fixture.
func (*doctorContractGitWorkspace) SynchronizeBase(context.Context, gitadapter.BaseSyncRequest) error {
	return nil
}

// CheckAuthentication implements the GitHub diagnosis seam.
func (*fakeGitHub) CheckAuthentication(context.Context) error { return nil }

// CheckRepositoryAccess implements the GitHub diagnosis seam.
func (*fakeGitHub) CheckRepositoryAccess(context.Context, github.Repository) error { return nil }

// CheckFactoryLabels implements the GitHub diagnosis seam.
func (*fakeGitHub) CheckFactoryLabels(context.Context, github.Repository) error { return nil }

// CheckRemote implements the Git diagnosis seam.
func (*doctorContractGitWorkspace) CheckRemote(context.Context, gitadapter.DoctorRequest) error {
	return nil
}

// CheckHooks implements the Git diagnosis seam.
func (*doctorContractGitWorkspace) CheckHooks(context.Context, gitadapter.DoctorRequest) error {
	return nil
}

// CheckWorktree implements the Git diagnosis seam.
func (*doctorContractGitWorkspace) CheckWorktree(context.Context, gitadapter.DoctorRequest) error {
	return nil
}

// CheckRoleCraft implements the optional role-craft diagnosis seam.
func (*doctorContractGitWorkspace) CheckRoleCraft(context.Context, gitadapter.DoctorRequest, string, string) error {
	return nil
}

// doctorContractWorker records public doctor probes while reusing the worker
// lifecycle fixture used by agent tests.
type doctorContractWorker struct {
	*headlessAgentWorker
	headlessChecks int
	harnessChecks  int
}

// CheckDocker reports the fixture daemon ready.
func (*doctorContractWorker) CheckDocker(context.Context) error { return nil }

// CheckImage reports the configured immutable image ready.
func (*doctorContractWorker) CheckImage(context.Context, worker.ImageReference) error { return nil }

// CheckHarness records one configured harness executable probe.
func (w *doctorContractWorker) CheckHarness(context.Context, worker.HarnessCheckRequest) error {
	w.harnessChecks++
	return nil
}

// CheckHarnessAuthentication accepts optional fixture credentials.
func (*doctorContractWorker) CheckHarnessAuthentication(context.Context, worker.HarnessAuthenticationCheckRequest) error {
	return nil
}

// CheckSkillContract reports the configured harness's skill projection.
func (*doctorContractWorker) CheckSkillContract(_ context.Context, request worker.SkillContractRequest) (worker.SkillContract, error) {
	return worker.SkillContract{Harness: request.Harness, Version: "test"}, nil
}

// CheckHeadless records the detached-process helper prerequisite.
func (w *doctorContractWorker) CheckHeadless(context.Context, worker.HeadlessCheckRequest) error {
	w.headlessChecks++
	return nil
}
