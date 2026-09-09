package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/cli"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestResetCommandPrintsRemovedAndRetainedResourcesBeforeConfirmation verifies
// that the CLI displays the complete plan, distinguishes removal from
// retention, and changes nothing without --confirm.
func TestResetCommandPrintsRemovedAndRetainedResourcesBeforeConfirmation(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "host", "config.yaml")
	repositoryPath := filepath.Join(root, "repository")
	worktreePath := filepath.Join(root, "worktrees", "run-cli-reset")
	operationalPath := filepath.Join(root, "state", "factory.db")
	for _, path := range []string{repositoryPath, worktreePath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	saveResetHost(t, configPath, repositoryPath, operationalPath)
	saveResetRun(t, operationalPath, repositoryPath, worktreePath)

	var output bytes.Buffer
	code := cli.Run(context.Background(), []string{"reset", "--config", configPath}, &output, &output)
	if code != 2 {
		t.Fatalf("reset exit code = %d, want 2; output = %s", code, output.String())
	}
	for _, fragment := range []string{
		"reset repository: " + repositoryPath,
		"reset run: run-cli-reset",
		"reset worktree: " + worktreePath,
		"reset branch: factory/run-cli-reset (local only; remote retained)",
		"reset git projection: ",
		"reset terminal workspace: workspace-cli-reset",
		"reset credential volume: " + repositoryPath,
		"reset control workspace: factory-control",
		"reset coordinator lock: ",
		"reset database: " + operationalPath,
		"reset config: " + configPath + " (removed last)",
		"reset retains: remote factory/* branches",
		"reset requires --confirm; no resources removed",
	} {
		if !strings.Contains(output.String(), fragment) {
			t.Fatalf("reset output = %q, missing %q", output.String(), fragment)
		}
	}
	for _, path := range []string{configPath, operationalPath, worktreePath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("preview removed %q: %v", path, err)
		}
	}
}

// TestResetCommandRequiresAnExplicitConfigPath verifies that a command able to
// destroy a whole installation never defaults to the operator's real
// configuration.
func TestResetCommandRequiresAnExplicitConfigPath(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	code := cli.Run(context.Background(), []string{"reset"}, &output, &output)
	if code != 2 {
		t.Fatalf("reset exit code = %d, want 2; output = %s", code, output.String())
	}
	if !strings.Contains(output.String(), "reset requires an explicit --config path") {
		t.Fatalf("reset output = %q, want the explicit configuration requirement", output.String())
	}
}

// saveResetHost writes one registered host configuration for the CLI test.
func saveResetHost(t *testing.T, configPath, repositoryPath, operationalPath string) {
	t.Helper()

	if err := config.SaveHost(configPath, config.HostConfig{
		SchemaVersion: 1,
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
}

// saveResetRun persists one terminal run whose local resources the plan names.
func saveResetRun(t *testing.T, operationalPath, repositoryPath, worktreePath string) {
	t.Helper()

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
		ID:             "run-cli-reset",
		RepositoryPath: repositoryPath,
		IssueNumber:    23,
		Stage:          store.StageReady,
		Status:         store.StatusComplete,
		Branch:         "factory/run-cli-reset",
		Worktree:       worktreePath,
	}); err != nil {
		t.Fatal(err)
	}
	if err := opened.SaveInvocation(context.Background(), store.Invocation{
		ID:                "invocation-cli-reset",
		RunID:             "run-cli-reset",
		Harness:           "codex",
		Role:              "implementation",
		Stage:             store.StageImplementation,
		Status:            store.InvocationStatusCompleted,
		WorkspaceID:       "workspace-cli-reset",
		CredentialStoreID: repositoryPath,
	}); err != nil {
		t.Fatal(err)
	}
}
