package factory_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/terminal"
	"gopkg.in/yaml.v3"
)

// TestResetReturnsARealInstallationToItsFreshHostState is the end-to-end proof
// of the reset contract over real SQLite and real Git worktrees. Only the
// external GitHub, Docker, and terminal adapters remain controlled fakes.
func TestResetReturnsARealInstallationToItsFreshHostState(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "project")
	configPath := filepath.Join(root, "config", "config.yaml")
	operationalPath := filepath.Join(root, "state", "factory.db")
	worktreeRoot := filepath.Join(root, "worktrees")
	initializeIntegrationRepository(t, root, repositoryPath)

	// A real fresh-host journey creates the configuration and registration.
	if _, err := factory.New(configPath).Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	registerIntegrationInstallation(t, configPath, repositoryPath, operationalPath)

	// A real Git worktree, local run branch, and private Git projection.
	gitWorkspace := &gitadapter.LocalWorktreeManager{WorktreeDir: worktreeRoot}
	workspace, err := gitWorkspace.Create(ctx, repositoryPath, "main", "run-integration")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	projection := filepath.Join(worktreeRoot, ".factory-git", "run-integration")
	invocationRoot := filepath.Join(worktreeRoot, ".factory-agents", "run-integration", "invocation-1")
	packetPath := filepath.Join(invocationRoot, "packet")
	resultPath := filepath.Join(invocationRoot, "results")
	for _, path := range []string{projection, packetPath, resultPath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	saveIntegrationRun(t, operationalPath, repositoryPath, workspace, packetPath, resultPath)

	terminalRuntime := &resetTerminal{control: terminal.Workspace{ID: "workspace-control", Name: "factory-control"}}
	workerRuntime := &resetWorker{}
	service := factory.NewWithDependencies(configPath, factory.Dependencies{
		OpenStore:    func(ctx context.Context, path string) (factory.OperationalStore, error) { return store.Open(ctx, path) },
		GitHub:       &resetGitHub{issue: github.Issue{Number: 42, State: "closed"}, statusComment: github.Comment{ID: "status-1"}},
		GitWorkspace: gitWorkspace,
		Worker:       workerRuntime,
		Terminal:     terminalRuntime,
		Now:          func() time.Time { return time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC) },
	})

	result, err := service.Reset(ctx, factory.ResetRequest{Confirm: true})
	if err != nil {
		t.Fatalf("Reset() error = %v", err)
	}
	if len(result.Remaining) != 0 {
		t.Fatalf("remaining targets = %#v, want none", result.Remaining)
	}

	for _, path := range []string{configPath, operationalPath, workspace.Worktree, projection, packetPath, resultPath, result.Plan.CoordinatorLock} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("planned target %q still exists: err = %v", path, err)
		}
	}
	if branches := runIntegrationGit(t, repositoryPath, "branch", "--list", workspace.Branch); strings.TrimSpace(branches) != "" {
		t.Fatalf("local run branch survived reset: %q", branches)
	}
	// Reset retains the source checkout, its tracked files, and its ordinary
	// branches, so the fresh-host journey starts from a real repository.
	for _, path := range []string{repositoryPath, filepath.Join(repositoryPath, "factory.yaml"), filepath.Join(repositoryPath, "README.md")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retained path %q was removed: %v", path, err)
		}
	}
	if branches := runIntegrationGit(t, repositoryPath, "branch", "--list", "main"); strings.TrimSpace(branches) == "" {
		t.Fatal("ordinary main branch was removed by reset")
	}

	// The normal fresh-host journey works again at the same paths.
	if _, err := factory.New(configPath).Init(ctx); err != nil {
		t.Fatalf("Init() after reset error = %v", err)
	}
	registerIntegrationInstallation(t, configPath, repositoryPath, operationalPath)
	if _, err := os.Stat(operationalPath); err != nil {
		t.Fatalf("operational store missing after re-registration: %v", err)
	}
}

// initializeIntegrationRepository creates a real repository with an origin
// remote, so the Git adapter can fetch its target branch.
func initializeIntegrationRepository(t *testing.T, root, repositoryPath string) {
	t.Helper()

	remote := filepath.Join(root, "project-origin.git")
	runIntegrationGit(t, root, "init", "--bare", remote)
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	runIntegrationGit(t, repositoryPath, "init", "-b", "main")
	runIntegrationGit(t, repositoryPath, "config", "user.email", "factory@example.test")
	runIntegrationGit(t, repositoryPath, "config", "user.name", "Factory Test")
	if err := os.WriteFile(filepath.Join(repositoryPath, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	policy, err := yaml.Marshal(validRepositoryConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryPath, "factory.yaml"), policy, 0o644); err != nil {
		t.Fatal(err)
	}
	runIntegrationGit(t, repositoryPath, "add", "README.md", "factory.yaml")
	runIntegrationGit(t, repositoryPath, "commit", "-m", "initial")
	runIntegrationGit(t, repositoryPath, "remote", "add", "origin", remote)
	runIntegrationGit(t, repositoryPath, "push", "origin", "main")
}

// registerIntegrationInstallation performs one real registration at the exact
// configuration, repository, and operational-store paths.
func registerIntegrationInstallation(t *testing.T, configPath, repositoryPath, operationalPath string) {
	t.Helper()

	if _, err := factory.New(configPath).Register(context.Background(), factory.RegisterRequest{
		RepositoryPath:      repositoryPath,
		GitHubOwner:         "example",
		GitHubRepository:    "project",
		AuthorizedUsers:     []string{"alice"},
		OperationalDataPath: operationalPath,
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
}

// saveIntegrationRun persists one terminal run whose targets are the real
// worktree, branch, projection, and generated output directories.
func saveIntegrationRun(t *testing.T, operationalPath, repositoryPath string, workspace gitadapter.Workspace, packetPath, resultPath string) {
	t.Helper()

	packet, err := json.Marshal(factory.SpecificationPacket{Version: 1, RepositoryConfig: validRepositoryConfig()})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, time.February, 1, 12, 0, 0, 0, time.UTC)
	opened, err := store.Open(context.Background(), operationalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if err := opened.SaveRun(context.Background(), store.Run{
		ID:                  workspace.RunID,
		RepositoryPath:      repositoryPath,
		IssueNumber:         42,
		Stage:               store.StageReady,
		Status:              store.StatusComplete,
		Branch:              workspace.Branch,
		Worktree:            workspace.Worktree,
		StatusCommentID:     "status-1",
		SpecificationPacket: string(packet),
		TerminalAt:          at,
		CreatedAt:           at,
		UpdatedAt:           at,
	}); err != nil {
		t.Fatal(err)
	}
	if err := opened.SaveInvocation(context.Background(), store.Invocation{
		ID:                  "invocation-1",
		RunID:               workspace.RunID,
		Harness:             "codex",
		Role:                "implementation",
		Stage:               store.StageImplementation,
		Status:              store.InvocationStatusCompleted,
		InvocationDirectory: packetPath,
		ResultDirectory:     resultPath,
		WorkspaceID:         "workspace-integration",
		CredentialStoreID:   "credential-store",
		CreatedAt:           at,
		UpdatedAt:           at,
	}); err != nil {
		t.Fatal(err)
	}
}

// runIntegrationGit runs one real git command for the integration fixture.
func runIntegrationGit(t *testing.T, directory string, args ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}
