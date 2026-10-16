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
		"reset credential volume: " + repositoryPath,
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
	if strings.Contains(output.String(), "workspace-cli-reset") || strings.Contains(output.String(), "factory-control") {
		t.Fatalf("reset output = %q, want no removed local-UI projection", output.String())
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
		SchemaVersion: config.CurrentHostSchemaVersion,
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
		CredentialStoreID: repositoryPath,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestResetCommandPrintsSessionRoleResources verifies that the reset plan
// names the worker, role, and snapshot targets of an ideation session role,
// so the operator sees them before confirmation.
func TestResetCommandPrintsSessionRoleResources(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "host", "config.yaml")
	repositoryPath := filepath.Join(root, "repository")
	snapshotPath := filepath.Join(root, "worktrees", "invocation-1")
	operationalPath := filepath.Join(root, "state", "factory.db")
	for _, path := range []string{repositoryPath, snapshotPath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	saveResetHost(t, configPath, repositoryPath, operationalPath)
	saveResetSessionRole(t, operationalPath, repositoryPath, snapshotPath)

	var output bytes.Buffer
	if code := cli.Run(context.Background(), []string{"reset", "--config", configPath}, &output, &output); code != 2 {
		t.Fatalf("reset exit code = %d, want 2; output = %s", code, output.String())
	}
	for _, fragment := range []string{
		"reset sessions: 1",
		"reset session: session-1",
		"reset session worker: session-session-1-ideation",
		"reset session worker role: ideation",
		"reset session snapshot: " + snapshotPath,
	} {
		if !strings.Contains(output.String(), fragment) {
			t.Fatalf("reset output = %q, missing %q", output.String(), fragment)
		}
	}
}

// saveResetSessionRole stores session-1 with an ideation invocation that was
// active when the coordinator stopped.
func saveResetSessionRole(t *testing.T, operationalPath, repositoryPath, snapshotPath string) {
	t.Helper()
	opened, err := store.Open(context.Background(), operationalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	session, err := opened.CreateIdeationSession(context.Background(), store.IdeationSession{
		ID: "session-1", RepositoryPath: repositoryPath, State: store.IdeationStateAwaitingUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, _, err = opened.AppendIdeationTurn(context.Background(), store.IdeationWrite{
		SessionID: session.ID, ExpectedRevision: session.Revision, State: store.IdeationStateAwaitingUser,
	}, store.IdeationTurn{ID: "turn-1", Author: store.IdeationTurnAuthorUser, Body: "Export run history."})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := opened.StartSessionInvocation(context.Background(), store.IdeationWrite{
		SessionID: session.ID, ExpectedRevision: session.Revision, State: store.IdeationStateRoleActive,
	}, store.SessionInvocation{
		ID: "invocation-1", Role: "ideation", TriggerID: "turn-1", ContextRevision: "0123456789abcdef0123456789abcdef01234567",
		Harness: "codex", Model: "gpt", PromptVersion: "ideation-v2", PromptSHA256: "b2c88fe7a651a61953d3c9f1d0d5ea219cdfa0cd4a5090e4ef0440e19bbc134d",
		Image: "ghcr.io/example/worker", ImageDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		WorkerID: "session-session-1-ideation", SnapshotPath: snapshotPath,
	}); err != nil {
		t.Fatal(err)
	}
}
