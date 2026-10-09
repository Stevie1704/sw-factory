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
)

// TestRunDoctorRendersTheInvalidRepositoryField verifies factory doctor prints
// the field-level repository finding with its corrective action and exits
// nonzero. Stub executables replace gh, git, and docker so the diagnosis needs
// no live GitHub account, Docker daemon, or credentials.
func TestRunDoctorRendersTheInvalidRepositoryField(t *testing.T) {
	configPath := doctorFixture(t, func(registration config.RepositoryRegistration) config.RepositoryRegistration { return registration })

	var output, errorsOutput bytes.Buffer
	code := cli.Run(context.Background(), []string{"doctor", "--config", configPath}, &output, &errorsOutput)
	if code == 0 {
		t.Fatalf("doctor exit code = 0, want failure, stdout=%q stderr=%q", output.String(), errorsOutput.String())
	}
	reported := output.String()
	for _, want := range []string{
		"doctor: configuration: failed",
		"model_options.test: must declare at least one model",
		"action: repair the repository factory.yaml and its declared workflow policy",
		"doctor: worker limits: passed (memory 8g, swap 8g, cpus 4, pids 4096, log 3 x 10m)",
		"doctor: blocked",
	} {
		if !strings.Contains(reported, want) {
			t.Fatalf("doctor stdout = %q, want it to contain %q", reported, want)
		}
	}
}

// doctorFixture writes a host configuration whose repository policy has an
// invalid model list, with stub executables in place of gh, az, git, docker,
// and the harnesses. mutate adjusts the registration before it is saved.
func doctorFixture(t *testing.T, mutate func(config.RepositoryRegistration) config.RepositoryRegistration) string {
	t.Helper()
	stubs := t.TempDir()
	for _, name := range []string{"gh", "az", "git", "docker", "codex", "claude"} {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", stubs)

	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	repositoryConfigPath := filepath.Join(repositoryPath, config.RepositoryConfigFileName)
	if err := os.WriteFile(repositoryConfigPath, []byte(`schema_version: 1
target_branch: main
setup: go mod download
setup_environment_policy: clean
gates:
  - name: test
    command: go test ./...
    timeout: 5m
    blocking: true
    environment_policy: clean
role_harness_defaults:
  test: codex
  implementation: codex
model_options:
  test: []
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
worker_build:
  image: ghcr.io/example/factory-worker
  digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  definition: worker/Dockerfile
base_synchronization:
  mode: never
`), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "host", "config.yaml")
	registration := config.RepositoryRegistration{
		Path: repositoryPath, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
		AuthorizedUsers: []string{"alice"}, Polling: config.PollingConfig{Interval: "30s", Backoff: "5m"},
		OperationalDataPath: filepath.Join(root, "state", "factory.db"), RepositoryConfigPath: repositoryConfigPath,
	}
	if err := config.SaveHost(configPath, config.HostConfig{
		SchemaVersion: config.CurrentHostSchemaVersion,
		Repositories:  []config.RepositoryRegistration{mutate(registration)},
	}); err != nil {
		t.Fatal(err)
	}
	return configPath

}

// TestRunDoctorSelectsTheAzureDevOpsAdapter verifies that an Azure DevOps
// registration makes the composition root run the Azure DevOps checks
// instead of the GitHub checks.
func TestRunDoctorSelectsTheAzureDevOpsAdapter(t *testing.T) {
	configPath := doctorFixture(t, func(registration config.RepositoryRegistration) config.RepositoryRegistration {
		registration.GitHub = config.GitHubConfig{}
		registration.AzureDevOps = config.AzureDevOpsConfig{Organization: "contoso", Project: "Factory Pilot", Repository: "service"}
		return registration
	})

	var output, errorsOutput bytes.Buffer
	cli.Run(context.Background(), []string{"doctor", "--config", configPath}, &output, &errorsOutput)
	reported := output.String()
	if !strings.Contains(reported, "doctor: azure devops authentication: failed") || strings.Contains(reported, "github authentication") {
		t.Fatalf("doctor stdout = %q, want the Azure DevOps checks only", reported)
	}
}
