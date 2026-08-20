package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
  digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
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
	if schemaErr.Field != "schema_version" {
		t.Fatalf("schema field = %q, want schema_version", schemaErr.Field)
	}
	if !strings.Contains(schemaErr.Error(), "schema_version") {
		t.Fatalf("schema error = %q, want schema_version field", schemaErr.Error())
	}
}

func TestValidateRepositoryRequiresMatchingHarnessAndModelRoles(t *testing.T) {
	t.Parallel()

	policy := validRepositoryConfig()
	policy.ModelOptions = map[string][]string{"test": {"gpt-5"}}
	err := config.ValidateRepository(policy)
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateRepository() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "model_options.implementation" {
		t.Fatalf("field = %q, want model_options.implementation", validationErr.Field)
	}
}

func TestValidateRepositoryRejectsUnsupportedOverrides(t *testing.T) {
	t.Parallel()

	policy := validRepositoryConfig()
	policy.AllowedOverrides = []config.OverrideName{"shell_args"}
	err := config.ValidateRepository(policy)
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateRepository() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "allowed_overrides[0]" {
		t.Fatalf("field = %q, want allowed_overrides[0]", validationErr.Field)
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

func TestProjectRepositoryConfigIsValid(t *testing.T) {
	t.Parallel()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() failed")
	}
	path := filepath.Join(filepath.Dir(filename), "..", "..", config.RepositoryConfigFileName)
	loaded, err := config.LoadRepository(path)
	if err != nil {
		t.Fatalf("LoadRepository(%q) error = %v", path, err)
	}
	if len(loaded.Gates) < 4 {
		t.Fatalf("project gates = %d, want format, vet, test, and build", len(loaded.Gates))
	}
}

func validRepositoryConfig() config.RepositoryConfig {
	return config.RepositoryConfig{
		SchemaVersion: 1,
		TargetBranch:  "main",
		Setup:         "go mod download",
		Gates: []config.GateConfig{{
			Name:              "test",
			Command:           "go test ./...",
			Timeout:           "5m",
			Blocking:          true,
			EnvironmentPolicy: config.EnvironmentPolicyClean,
		}},
		RoleHarnessDefaults: map[string]config.Harness{"implementation": config.HarnessCodex},
		ModelOptions:        map[string][]string{"implementation": {"gpt-5"}},
		Timeouts: config.TimeoutConfig{
			Setup: "5m", Agent: "30m", Gate: "5m", Review: "10m",
		},
		RetryLimits: config.RetryLimits{CheckRepair: 3, ReviewRepair: 2, TestRevision: 2},
		TestPolicy:  config.TestPolicy{Mode: config.TestModeRequired},
		WorkerBuild: config.WorkerBuildConfig{
			Image: "ghcr.io/example/factory-worker", Digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Definition: "worker/Dockerfile",
		},
		BaseSynchronization: config.BaseSynchronization{Mode: config.BaseSynchronizationNever},
	}
}
