package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/cli"
)

func TestRunInitializesRegistersAndReportsStatus(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "host", "config.yaml")
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryPath, "factory.yaml"), []byte(`schema_version: 1
target_branch: main
setup: go mod download
gates:
  - name: test
    command: go test ./...
    timeout: 5m
    blocking: true
    environment_policy: clean
role_harness_defaults:
  implementation: codex
model_options:
  implementation: [gpt-5]
timeouts:
  setup: 5m
  agent: 30m
  gate: 5m
  review: 10m
retry_limits:
  check_repair: 3
  review_repair: 2
  test_revision: 2
test_policy:
  mode: required
allowed_overrides: [model]
worker_build:
  image: ghcr.io/example/factory-worker
  digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  definition: worker/Dockerfile
base_synchronization:
  mode: never
`), 0o600); err != nil {
		t.Fatal(err)
	}
	operationalPath := filepath.Join(root, "state", "factory.db")

	var output bytes.Buffer
	if code := cli.Run(context.Background(), []string{"init", "--config", configPath}, &output, &output); code != 0 {
		t.Fatalf("init exit code = %d, output = %s", code, output.String())
	}
	if !strings.Contains(output.String(), "created host configuration") {
		t.Fatalf("init output = %q", output.String())
	}

	output.Reset()
	if code := cli.Run(context.Background(), []string{
		"register",
		"--config", configPath,
		"--repository", repositoryPath,
		"--github-owner", "example",
		"--github-repository", "project",
		"--authorized-user", "alice",
		"--operational-data", operationalPath,
	}, &output, &output); code != 0 {
		t.Fatalf("register exit code = %d, output = %s", code, output.String())
	}

	output.Reset()
	if code := cli.Run(context.Background(), []string{"status", "--config", configPath}, &output, &output); code != 0 {
		t.Fatalf("status exit code = %d, output = %s", code, output.String())
	}
	if !strings.Contains(output.String(), "active run: none") {
		t.Fatalf("status output = %q", output.String())
	}
}

func TestRunRejectsAnUnknownCommand(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	if code := cli.Run(context.Background(), []string{"unknown"}, &output, &output); code == 0 {
		t.Fatal("unknown command returned success")
	}
	if !strings.Contains(output.String(), "unknown command") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestRunRequiresACommand(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	code := cli.Run(context.Background(), []string{}, &output, &output)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(output.String(), "a command is required") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestRunInitRejectsPositionalArguments(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	var output bytes.Buffer
	code := cli.Run(context.Background(), []string{"init", "--config", configPath, "extra"}, &output, &output)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2, output = %s", code, output.String())
	}
	if !strings.Contains(output.String(), "does not accept positional arguments") {
		t.Fatalf("output = %q", output.String())
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("config file was created despite the rejected arguments: %v", err)
	}
}

func TestRunInitFailsWhenConfigurationAlreadyExists(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	var output bytes.Buffer
	if code := cli.Run(context.Background(), []string{"init", "--config", configPath}, &output, &output); code != 0 {
		t.Fatalf("first init exit code = %d, output = %s", code, output.String())
	}

	output.Reset()
	code := cli.Run(context.Background(), []string{"init", "--config", configPath}, &output, &output)
	if code != 1 {
		t.Fatalf("second init exit code = %d, want 1, output = %s", code, output.String())
	}
	if !strings.Contains(output.String(), "already exists") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestRunRegisterRequiresTheRepositoryOwnerRepositoryAndAnAuthorizedUser(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	var output bytes.Buffer
	if code := cli.Run(context.Background(), []string{"init", "--config", configPath}, &output, &output); code != 0 {
		t.Fatalf("init exit code = %d, output = %s", code, output.String())
	}

	output.Reset()
	code := cli.Run(context.Background(), []string{"register", "--config", configPath}, &output, &output)
	if code != 2 {
		t.Fatalf("register exit code = %d, want 2, output = %s", code, output.String())
	}
	if !strings.Contains(output.String(), "register requires --repository, --github-owner, --github-repository, and at least one --authorized-user") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestRunRegisterRejectsPositionalArguments(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	code := cli.Run(context.Background(), []string{"register", "extra"}, &output, &output)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2, output = %s", code, output.String())
	}
	if !strings.Contains(output.String(), "does not accept positional arguments") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestRunRegisterRejectsAnEmptyAuthorizedUser(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	code := cli.Run(context.Background(), []string{
		"register",
		"--repository", "/tmp/repository",
		"--github-owner", "example",
		"--github-repository", "project",
		"--authorized-user", "",
	}, &output, &output)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2, output = %s", code, output.String())
	}
}

func TestRunRegisterAcceptsMultipleAuthorizedUsers(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "host", "config.yaml")
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalRepositoryConfig(t, repositoryPath)

	var output bytes.Buffer
	if code := cli.Run(context.Background(), []string{"init", "--config", configPath}, &output, &output); code != 0 {
		t.Fatalf("init exit code = %d, output = %s", code, output.String())
	}

	output.Reset()
	code := cli.Run(context.Background(), []string{
		"register",
		"--config", configPath,
		"--repository", repositoryPath,
		"--github-owner", "example",
		"--github-repository", "project",
		"--authorized-user", "alice",
		"--authorized-user", "bob",
	}, &output, &output)
	if code != 0 {
		t.Fatalf("register exit code = %d, output = %s", code, output.String())
	}
}

func TestRunStatusRejectsPositionalArguments(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	code := cli.Run(context.Background(), []string{"status", "extra"}, &output, &output)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2, output = %s", code, output.String())
	}
	if !strings.Contains(output.String(), "does not accept positional arguments") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestRunStatusReportsAnErrorWhenTheConfigurationCannotBeLoaded(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "missing", "config.yaml")
	var output bytes.Buffer
	code := cli.Run(context.Background(), []string{"status", "--config", configPath}, &output, &output)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1, output = %s", code, output.String())
	}
	if !strings.Contains(output.String(), "error:") {
		t.Fatalf("output = %q", output.String())
	}
}

func writeMinimalRepositoryConfig(t *testing.T, repositoryPath string) {
	t.Helper()
	contents := `schema_version: 1
target_branch: main
setup: go mod download
gates:
  - name: test
    command: go test ./...
    timeout: 5m
    blocking: true
    environment_policy: clean
role_harness_defaults:
  implementation: codex
model_options:
  implementation: [gpt-5]
timeouts:
  setup: 5m
  agent: 30m
  gate: 5m
  review: 10m
retry_limits:
  check_repair: 3
  review_repair: 2
  test_revision: 2
test_policy:
  mode: required
allowed_overrides: [model]
worker_build:
  image: ghcr.io/example/factory-worker
  digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  definition: worker/Dockerfile
base_synchronization:
  mode: never
`
	if err := os.WriteFile(filepath.Join(repositoryPath, "factory.yaml"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
