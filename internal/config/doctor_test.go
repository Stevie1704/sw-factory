package config_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/doctor"
)

// TestStartupCheckLoadsTheRegisteredRepositoryPolicy verifies a healthy host
// configuration exposes the registration and repository policy to composition.
func TestStartupCheckLoadsTheRegisteredRepositoryPolicy(t *testing.T) {
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDoctorRepositoryConfig(t, repositoryPath)
	hostPath := filepath.Join(root, "config.yaml")
	host := `schema_version: 1
repositories:
  - path: ` + repositoryPath + `
    github:
      owner: example
      repository: project
    authorized_users: [alice]
    polling:
      interval: 30s
      backoff: 5m
    operational_data_path: ` + filepath.Join(root, "state", "factory.db") + `
    repository_config_path: ` + filepath.Join(repositoryPath, "factory.yaml") + `
`
	if err := os.WriteFile(hostPath, []byte(host), 0o600); err != nil {
		t.Fatal(err)
	}

	state, check := config.StartupCheck(hostPath)
	result := check(context.Background())
	if result.Status != doctor.StatusPassed {
		t.Fatalf("startup result = %#v, want passed", result)
	}
	if state.Registration == nil || state.Repository == nil {
		t.Fatalf("startup state = %#v, want registration and repository policy", state)
	}
	if state.Registration.GitHub.Repository != "project" || state.Repository.TargetBranch != "main" {
		t.Fatalf("startup state = %#v, want registered project and main target", state)
	}
}

// TestStartupCheckReportsAnInvalidRepositoryPolicyWithoutRawDetails verifies
// configuration failures stay actionable without printing file contents.
func TestStartupCheckReportsAnInvalidRepositoryPolicyWithoutRawDetails(t *testing.T) {
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := "do-not-print-this-value"
	if err := os.WriteFile(filepath.Join(repositoryPath, "factory.yaml"), []byte("schema_version: 1\ntarget_branch: \""+secret+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hostPath := filepath.Join(root, "config.yaml")
	host := `schema_version: 1
repositories:
  - path: ` + repositoryPath + `
    github:
      owner: example
      repository: project
    authorized_users: [alice]
    polling:
      interval: 30s
      backoff: 5m
    operational_data_path: ` + filepath.Join(root, "state", "factory.db") + `
    repository_config_path: ` + filepath.Join(repositoryPath, "factory.yaml") + `
`
	if err := os.WriteFile(hostPath, []byte(host), 0o600); err != nil {
		t.Fatal(err)
	}

	_, check := config.StartupCheck(hostPath)
	result := check(context.Background())
	if result.Status != doctor.StatusFailed || !strings.Contains(result.Problem, "repository") {
		t.Fatalf("startup result = %#v, want repository configuration failure", result)
	}
	if strings.Contains(result.Problem+result.Action, secret) {
		t.Fatalf("startup result exposed configuration content: %#v", result)
	}
}

// TestStartupCheckRejectsAGroupReadableHostConfiguration verifies the host
// file containing credential paths remains private to the operator.
func TestStartupCheckRejectsAGroupReadableHostConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("schema_version: 1\nrepositories: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, check := config.StartupCheck(path)
	result := check(context.Background())
	if result.Status != doctor.StatusFailed || !strings.Contains(result.Problem, "private") {
		t.Fatalf("startup result = %#v, want private host configuration failure", result)
	}
}

// TestStartupCheckRejectsASymlinkedRepositoryConfiguration verifies the
// repository policy remains checked-in rather than redirecting diagnosis to a
// host-local file outside the checkout.
func TestStartupCheckRejectsASymlinkedRepositoryConfiguration(t *testing.T) {
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	externalPath := filepath.Join(root, "external-factory.yaml")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(externalPath, []byte("schema_version: 1\ntarget_branch: main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalPath, filepath.Join(repositoryPath, "factory.yaml")); err != nil {
		t.Fatal(err)
	}
	hostPath := filepath.Join(root, "config.yaml")
	host := `schema_version: 1
repositories:
  - path: ` + repositoryPath + `
    github:
      owner: example
      repository: project
    authorized_users: [alice]
    polling:
      interval: 30s
      backoff: 5m
    operational_data_path: ` + filepath.Join(root, "state", "factory.db") + `
    repository_config_path: ` + filepath.Join(repositoryPath, "factory.yaml") + `
`
	if err := os.WriteFile(hostPath, []byte(host), 0o600); err != nil {
		t.Fatal(err)
	}

	_, check := config.StartupCheck(hostPath)
	result := check(context.Background())
	if result.Status != doctor.StatusFailed || (!strings.Contains(result.Problem, "outside") && !strings.Contains(result.Problem, "regular file")) {
		t.Fatalf("startup result = %#v, want symlink refusal", result)
	}
}

// writeDoctorRepositoryConfig writes the smallest valid repository policy for
// configuration startup tests.
func writeDoctorRepositoryConfig(t *testing.T, repositoryPath string) {
	t.Helper()
	contents := `schema_version: 1
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
  test: [gpt-5]
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
`
	if err := os.WriteFile(filepath.Join(repositoryPath, "factory.yaml"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
