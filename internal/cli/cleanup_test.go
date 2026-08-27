package cli_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/cli"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestCleanupCommandPrintsTargetsBeforeConfirmation verifies that the CLI
// exposes every local target and leaves the run untouched without --confirm.
func TestCleanupCommandPrintsTargetsBeforeConfirmation(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "host", "config.yaml")
	repositoryPath := filepath.Join(root, "repository")
	worktreePath := filepath.Join(root, "worktrees", "run-cli-cleanup")
	operationalPath := filepath.Join(root, "state", "factory.db")
	for _, path := range []string{repositoryPath, worktreePath, filepath.Join(root, "worktrees", ".factory-agents", "run-cli-cleanup"), filepath.Join(root, "worktrees", ".factory-git", "run-cli-cleanup")} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
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
	now := time.Date(2026, time.January, 20, 12, 0, 0, 0, time.UTC)
	opened, err := store.Open(context.Background(), operationalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.SaveRun(context.Background(), store.Run{
		ID:             "run-cli-cleanup",
		RepositoryPath: repositoryPath,
		IssueNumber:    23,
		Stage:          store.StageReady,
		Status:         store.StatusComplete,
		Branch:         "factory/run-cli-cleanup",
		Worktree:       worktreePath,
		TerminalAt:     now.Add(-8 * 24 * time.Hour),
		CreatedAt:      now.Add(-9 * 24 * time.Hour),
		UpdatedAt:      now.Add(-8 * 24 * time.Hour),
	}); err != nil {
		_ = opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	code := cli.Run(context.Background(), []string{"cleanup", "--config", configPath}, &output, &output)
	if code != 2 {
		t.Fatalf("cleanup exit code = %d, want 2; output = %s", code, output.String())
	}
	for _, fragment := range []string{
		"cleanup worktree: " + worktreePath,
		"cleanup branch: factory/run-cli-cleanup (local only; remote retained)",
		"cleanup container: run=run-cli-cleanup",
		"cleanup stored output:",
		"cleanup requires --confirm; no resources removed",
	} {
		if !strings.Contains(output.String(), fragment) {
			t.Fatalf("cleanup output = %q, missing %q", output.String(), fragment)
		}
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("worktree after preview: %v", err)
	}
	reopened, err := store.Open(context.Background(), operationalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if run, err := reopened.LatestRun(context.Background()); err != nil {
		t.Fatal(err)
	} else if run == nil || run.ID != "run-cli-cleanup" {
		t.Fatalf("LatestRun() = %#v, want retained run", run)
	}
	if _, err := os.Stat(filepath.Join(root, "worktrees", ".factory-agents", "run-cli-cleanup")); errors.Is(err, os.ErrNotExist) {
		t.Fatal("preview removed stored outputs")
	}
}
