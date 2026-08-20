package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
)

func TestLoadRepositoryConfigValidatesAndPreservesTheDeclaredWorkflow(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "factory.yaml")
	contents := `schema_version: 1
target_branch: main
setup: go mod download
gates:
  - name: format
    command: gofmt -l .
    timeout: 30s
    blocking: true
    environment_policy: clean
  - name: test
    command: go test ./...
    timeout: 2m
    blocking: true
    depends_on: [format]
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
  allow_human_exemption: true
  allow_technical_exemption: true
allowed_overrides: [model, reasoning_effort]
caches:
  - name: go-build
    path: /tmp/factory-cache
    read_only: false
worker_build:
  image: ghcr.io/example/factory-worker
  digest: sha256:0123456789abcdef
  definition: worker/Dockerfile
base_synchronization:
  mode: before_ready
  branch: main
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := config.LoadRepository(path)
	if err != nil {
		t.Fatalf("LoadRepository() error = %v", err)
	}
	if got.TargetBranch != "main" {
		t.Fatalf("TargetBranch = %q, want main", got.TargetBranch)
	}
	if len(got.Gates) != 2 || got.Gates[1].DependsOn[0] != "format" {
		t.Fatalf("Gates = %#v, want ordered dependency", got.Gates)
	}
	if got.RetryLimits.CheckRepair != 3 || got.TestPolicy.Mode != "required" {
		t.Fatalf("workflow policy was not preserved: %#v %#v", got.RetryLimits, got.TestPolicy)
	}
}

func TestLoadRepositoryUnknownSchemaFailsClosed(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "factory.yaml")
	if err := os.WriteFile(path, []byte("schema_version: 99\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := config.LoadRepository(path)
	var schemaErr *config.UnknownSchemaVersionError
	if !errors.As(err, &schemaErr) {
		t.Fatalf("error = %v, want UnknownSchemaVersionError", err)
	}
	if schemaErr.Version != 99 || schemaErr.Supported != config.CurrentSchemaVersion {
		t.Fatalf("schema error = %#v", schemaErr)
	}
}

func TestLoadHostRejectsTheOffendingField(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := `schema_version: 1
repositories:
  - path: relative/repository
    github:
      owner: example
      repository: project
    authorized_users: [alice]
    operational_data_path: /tmp/factory.db
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := config.LoadHost(path)
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %v, want ValidationError", err)
	}
	if validationErr.Field != "repositories[0].path" {
		t.Fatalf("field = %q, want repositories[0].path", validationErr.Field)
	}
}
