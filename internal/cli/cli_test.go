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
	code := cli.Run(context.Background(), []string{"init", "--config", configPath, "extra-arg"}, &output, &output)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2, output = %s", code, output.String())
	}
	if !strings.Contains(output.String(), "does not accept positional arguments") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestRunRegisterRejectsPositionalArguments(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	var output bytes.Buffer
	code := cli.Run(context.Background(), []string{"register", "--config", configPath, "extra-arg"}, &output, &output)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2, output = %s", code, output.String())
	}
	if !strings.Contains(output.String(), "does not accept positional arguments") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestRunStatusRejectsPositionalArguments(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	var output bytes.Buffer
	code := cli.Run(context.Background(), []string{"status", "--config", configPath, "extra-arg"}, &output, &output)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2, output = %s", code, output.String())
	}
	if !strings.Contains(output.String(), "does not accept positional arguments") {
		t.Fatalf("output = %q", output.String())
	}
}

// TestRunDraftPullRequestRejectsPositionalArguments verifies the draft-PR
// command keeps run selection explicit and unambiguous.
func TestRunDraftPullRequestRejectsPositionalArguments(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	var output bytes.Buffer
	code := cli.Run(context.Background(), []string{"draft-pr", "extra", "--config", configPath}, &output, &output)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2, output = %s", code, output.String())
	}
	if !strings.Contains(output.String(), "draft-pr does not accept positional arguments") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestRunInitRejectsAnUnknownFlag(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	code := cli.Run(context.Background(), []string{"init", "--bogus-flag"}, &output, &output)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2, output = %s", code, output.String())
	}
}

func TestRunRegisterRequiresTheRequiredFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
	}{
		{
			name: "missing repository",
			args: []string{"register", "--github-owner", "example", "--github-repository", "project", "--authorized-user", "alice"},
		},
		{
			name: "missing github owner",
			args: []string{"register", "--repository", "/tmp/repository", "--github-repository", "project", "--authorized-user", "alice"},
		},
		{
			name: "missing github repository",
			args: []string{"register", "--repository", "/tmp/repository", "--github-owner", "example", "--authorized-user", "alice"},
		},
		{
			name: "missing authorized user",
			args: []string{"register", "--repository", "/tmp/repository", "--github-owner", "example", "--github-repository", "project"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			configPath := filepath.Join(t.TempDir(), "config.yaml")
			var output bytes.Buffer
			code := cli.Run(context.Background(), append([]string{tc.args[0], "--config", configPath}, tc.args[1:]...), &output, &output)
			if code != 2 {
				t.Fatalf("exit code = %d, want 2, output = %s", code, output.String())
			}
			if !strings.Contains(output.String(), "register requires") {
				t.Fatalf("output = %q", output.String())
			}
		})
	}
}

func TestRunRegisterRejectsAnEmptyAuthorizedUserValue(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	var output bytes.Buffer
	code := cli.Run(context.Background(), []string{
		"register",
		"--config", configPath,
		"--repository", "/tmp/repository",
		"--github-owner", "example",
		"--github-repository", "project",
		"--authorized-user", "",
	}, &output, &output)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2, output = %s", code, output.String())
	}
}

func TestRunRegisterAcceptsMultipleAuthorizedUserFlags(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeValidRepositoryConfig(t, repositoryPath)

	var output bytes.Buffer
	code := cli.Run(context.Background(), []string{"init", "--config", configPath}, &output, &output)
	if code != 0 {
		t.Fatalf("init exit code = %d, output = %s", code, output.String())
	}

	output.Reset()
	code = cli.Run(context.Background(), []string{
		"register",
		"--config", configPath,
		"--repository", repositoryPath,
		"--github-owner", "example",
		"--github-repository", "project",
		"--authorized-user", "alice",
		"--authorized-user", "bob",
		"--operational-data", filepath.Join(root, "state", "factory.db"),
	}, &output, &output)
	if code != 0 {
		t.Fatalf("register exit code = %d, output = %s", code, output.String())
	}
	if !strings.Contains(output.String(), "registered repository") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestRunRegisterReportsAnErrorWhenTheRepositoryDoesNotExist(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	var output bytes.Buffer
	if code := cli.Run(context.Background(), []string{"init", "--config", configPath}, &output, &output); code != 0 {
		t.Fatalf("init exit code = %d, output = %s", code, output.String())
	}

	output.Reset()
	code := cli.Run(context.Background(), []string{
		"register",
		"--config", configPath,
		"--repository", filepath.Join(root, "does-not-exist"),
		"--github-owner", "example",
		"--github-repository", "project",
		"--authorized-user", "alice",
	}, &output, &output)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1, output = %s", code, output.String())
	}
	if !strings.HasPrefix(output.String(), "error:") {
		t.Fatalf("output = %q, want it to start with error:", output.String())
	}
}

func TestRunInitReportsAnErrorWhenTheConfigurationAlreadyExists(t *testing.T) {
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
	if !strings.HasPrefix(output.String(), "error:") {
		t.Fatalf("output = %q, want it to start with error:", output.String())
	}
}

func TestRunUsesTheFactoryConfigEnvironmentVariableWhenConfigFlagIsOmitted(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "env-config.yaml")
	t.Setenv("FACTORY_CONFIG", configPath)

	var output bytes.Buffer
	code := cli.Run(context.Background(), []string{"init"}, &output, &output)
	if code != 0 {
		t.Fatalf("exit code = %d, output = %s", code, output.String())
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config file was not created at the FACTORY_CONFIG path: %v", err)
	}
}

func TestRunStatusReportsNoRegisteredRepository(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")

	var output bytes.Buffer
	if code := cli.Run(context.Background(), []string{"init", "--config", configPath}, &output, &output); code != 0 {
		t.Fatalf("init exit code = %d, output = %s", code, output.String())
	}

	output.Reset()
	if code := cli.Run(context.Background(), []string{"status", "--config", configPath}, &output, &output); code != 0 {
		t.Fatalf("status exit code = %d, output = %s", code, output.String())
	}
	if !strings.Contains(output.String(), "repository: none registered") {
		t.Fatalf("output = %q, want no repository registered", output.String())
	}
	if !strings.Contains(output.String(), "config: "+configPath) {
		t.Fatalf("output = %q, want it to report the config path", output.String())
	}
}

func writeValidRepositoryConfig(t *testing.T, repositoryPath string) {
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
