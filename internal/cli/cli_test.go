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
