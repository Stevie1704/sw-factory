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

func TestValidateRepositoryRequiresAtLeastOneGate(t *testing.T) {
	t.Parallel()

	policy := validRepositoryConfig()
	policy.Gates = nil
	err := config.ValidateRepository(policy)
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateRepository() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "gates" {
		t.Fatalf("field = %q, want gates", validationErr.Field)
	}
}

func TestValidateRepositoryRejectsDuplicateGateNames(t *testing.T) {
	t.Parallel()

	policy := validRepositoryConfig()
	policy.Gates = append(policy.Gates, policy.Gates[0])
	err := config.ValidateRepository(policy)
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateRepository() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "gates[1].name" {
		t.Fatalf("field = %q, want gates[1].name", validationErr.Field)
	}
}

func TestValidateRepositoryRejectsAGateDependingOnAnUnknownGate(t *testing.T) {
	t.Parallel()

	policy := validRepositoryConfig()
	policy.Gates[0].DependsOn = []string{"nonexistent"}
	err := config.ValidateRepository(policy)
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateRepository() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "gates[0].depends_on[0]" {
		t.Fatalf("field = %q, want gates[0].depends_on[0]", validationErr.Field)
	}
}

func TestValidateRepositoryRejectsAnInvalidEnvironmentPolicy(t *testing.T) {
	t.Parallel()

	policy := validRepositoryConfig()
	policy.Gates[0].EnvironmentPolicy = "dirty"
	err := config.ValidateRepository(policy)
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateRepository() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "gates[0].environment_policy" {
		t.Fatalf("field = %q, want gates[0].environment_policy", validationErr.Field)
	}
}

func TestValidateRepositoryRejectsAGateTimeoutThatIsNotAPositiveDuration(t *testing.T) {
	t.Parallel()

	policy := validRepositoryConfig()
	policy.Gates[0].Timeout = "not-a-duration"
	err := config.ValidateRepository(policy)
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateRepository() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "gates[0].timeout" {
		t.Fatalf("field = %q, want gates[0].timeout", validationErr.Field)
	}
}

func TestValidateRepositoryRejectsAMissingTimeout(t *testing.T) {
	t.Parallel()

	policy := validRepositoryConfig()
	policy.Timeouts.Agent = ""
	err := config.ValidateRepository(policy)
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateRepository() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "timeouts.agent" {
		t.Fatalf("field = %q, want timeouts.agent", validationErr.Field)
	}
}

func TestValidateRepositoryRejectsNonPositiveRetryLimits(t *testing.T) {
	t.Parallel()

	policy := validRepositoryConfig()
	policy.RetryLimits.ReviewRepair = 0
	err := config.ValidateRepository(policy)
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateRepository() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "retry_limits.review_repair" {
		t.Fatalf("field = %q, want retry_limits.review_repair", validationErr.Field)
	}
}

func TestValidateRepositoryRejectsAnInvalidTestPolicyMode(t *testing.T) {
	t.Parallel()

	policy := validRepositoryConfig()
	policy.TestPolicy.Mode = "sometimes"
	err := config.ValidateRepository(policy)
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateRepository() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "test_policy.mode" {
		t.Fatalf("field = %q, want test_policy.mode", validationErr.Field)
	}
}

func TestValidateRepositoryRejectsDuplicateAllowedOverrides(t *testing.T) {
	t.Parallel()

	policy := validRepositoryConfig()
	policy.AllowedOverrides = []config.OverrideName{config.OverrideModel, config.OverrideModel}
	err := config.ValidateRepository(policy)
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateRepository() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "allowed_overrides[1]" {
		t.Fatalf("field = %q, want allowed_overrides[1]", validationErr.Field)
	}
}

func TestValidateRepositoryAllowsAnEmptyAllowedOverridesList(t *testing.T) {
	t.Parallel()

	policy := validRepositoryConfig()
	policy.AllowedOverrides = nil
	if err := config.ValidateRepository(policy); err != nil {
		t.Fatalf("ValidateRepository() error = %v, want nil for an empty override list", err)
	}
}

func TestValidateRepositoryRejectsDuplicateCacheNames(t *testing.T) {
	t.Parallel()

	policy := validRepositoryConfig()
	policy.Caches = []config.CacheConfig{
		{Name: "go-build", Path: "/tmp/one"},
		{Name: "go-build", Path: "/tmp/two"},
	}
	err := config.ValidateRepository(policy)
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateRepository() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "caches[1].name" {
		t.Fatalf("field = %q, want caches[1].name", validationErr.Field)
	}
}

func TestValidateRepositoryRejectsAMalformedWorkerBuildDigest(t *testing.T) {
	t.Parallel()

	policy := validRepositoryConfig()
	policy.WorkerBuild.Digest = "sha256:UPPERCASE0123456789abcdef0123456789abcdef0123456789abcdef012345"
	err := config.ValidateRepository(policy)
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateRepository() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "worker_build.digest" {
		t.Fatalf("field = %q, want worker_build.digest", validationErr.Field)
	}
}

func TestValidateRepositoryRequiresABranchWhenBaseSynchronizationRunsBeforeReady(t *testing.T) {
	t.Parallel()

	policy := validRepositoryConfig()
	policy.BaseSynchronization = config.BaseSynchronization{Mode: config.BaseSynchronizationBeforeReady}
	err := config.ValidateRepository(policy)
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateRepository() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "base_synchronization.branch" {
		t.Fatalf("field = %q, want base_synchronization.branch", validationErr.Field)
	}
}

func TestValidateRepositoryRejectsAnUnsupportedBaseSynchronizationMode(t *testing.T) {
	t.Parallel()

	policy := validRepositoryConfig()
	policy.BaseSynchronization = config.BaseSynchronization{Mode: "always"}
	err := config.ValidateRepository(policy)
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateRepository() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "base_synchronization.mode" {
		t.Fatalf("field = %q, want base_synchronization.mode", validationErr.Field)
	}
}

func TestValidateHostRejectsMoreThanOneRegisteredRepository(t *testing.T) {
	t.Parallel()

	registration := validRegistration(t)
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{registration, registration}}
	err := config.ValidateHost(host)
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateHost() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "repositories" {
		t.Fatalf("field = %q, want repositories", validationErr.Field)
	}
}

func TestValidateHostRejectsARegistrationWithNoAuthorizedUsers(t *testing.T) {
	t.Parallel()

	registration := validRegistration(t)
	registration.AuthorizedUsers = nil
	err := config.ValidateHost(config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{registration}})
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateHost() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "repositories[0].authorized_users" {
		t.Fatalf("field = %q, want repositories[0].authorized_users", validationErr.Field)
	}
}

func TestValidateHostRejectsDuplicateAuthorizedUsers(t *testing.T) {
	t.Parallel()

	registration := validRegistration(t)
	registration.AuthorizedUsers = []string{"alice", "alice"}
	err := config.ValidateHost(config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{registration}})
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateHost() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "repositories[0].authorized_users[1]" {
		t.Fatalf("field = %q, want repositories[0].authorized_users[1]", validationErr.Field)
	}
}

func TestValidateHostRejectsARelativeOperationalDataPath(t *testing.T) {
	t.Parallel()

	registration := validRegistration(t)
	registration.OperationalDataPath = "relative/factory.db"
	err := config.ValidateHost(config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{registration}})
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateHost() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "repositories[0].operational_data_path" {
		t.Fatalf("field = %q, want repositories[0].operational_data_path", validationErr.Field)
	}
}

func TestValidateHostRejectsARelativeRepositoryConfigPath(t *testing.T) {
	t.Parallel()

	registration := validRegistration(t)
	registration.RepositoryConfigPath = "relative/factory.yaml"
	err := config.ValidateHost(config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{registration}})
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateHost() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "repositories[0].repository_config_path" {
		t.Fatalf("field = %q, want repositories[0].repository_config_path", validationErr.Field)
	}
}

func TestValidateHostRejectsARelativeCmuxSocketPath(t *testing.T) {
	t.Parallel()

	registration := validRegistration(t)
	registration.Cmux.SocketPath = "relative/factory.sock"
	err := config.ValidateHost(config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{registration}})
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateHost() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "repositories[0].cmux.socket_path" {
		t.Fatalf("field = %q, want repositories[0].cmux.socket_path", validationErr.Field)
	}
}

func TestValidateHostRejectsAnInvalidPollingBackoff(t *testing.T) {
	t.Parallel()

	registration := validRegistration(t)
	registration.Polling.Backoff = "not-a-duration"
	err := config.ValidateHost(config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{registration}})
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateHost() error = %v, want ValidationError", err)
	}
	if validationErr.Field != "repositories[0].polling.backoff" {
		t.Fatalf("field = %q, want repositories[0].polling.backoff", validationErr.Field)
	}
}

func TestSaveHostThenLoadHostRoundTripsTheRegisteredRepository(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	registration := validRegistration(t)
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{registration}}
	if err := config.SaveHost(path, host); err != nil {
		t.Fatalf("SaveHost() error = %v", err)
	}

	got, err := config.LoadHost(path)
	if err != nil {
		t.Fatalf("LoadHost() error = %v", err)
	}
	if len(got.Repositories) != 1 || got.Repositories[0].Path != registration.Path {
		t.Fatalf("Repositories = %#v, want %#v", got.Repositories, []config.RepositoryRegistration{registration})
	}
	if got.Repositories[0].GitHub.Owner != "example" {
		t.Fatalf("GitHub.Owner = %q, want example", got.Repositories[0].GitHub.Owner)
	}
}

func TestCreateHostFailsWhenTheConfigurationAlreadyExists(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := config.CreateHost(path); err != nil {
		t.Fatalf("CreateHost() error = %v", err)
	}

	_, err := config.CreateHost(path)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second CreateHost() error = %v, want an already-exists error", err)
	}
}

func TestDefaultHostConfigPathPrefersTheFactoryConfigEnvironmentVariable(t *testing.T) {
	relative := "relative-config.yaml"
	t.Setenv("FACTORY_CONFIG", relative)

	got, err := config.DefaultHostConfigPath()
	if err != nil {
		t.Fatalf("DefaultHostConfigPath() error = %v", err)
	}
	want, err := filepath.Abs(relative)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("DefaultHostConfigPath() = %q, want %q", got, want)
	}
}

func TestDefaultOperationalDataPathIsRelativeToTheHostConfigDirectory(t *testing.T) {
	t.Parallel()

	hostConfigPath := filepath.Join("/etc", "factory", "config.yaml")
	got := config.DefaultOperationalDataPath(hostConfigPath)
	want := filepath.Join("/etc", "factory", "data", "factory.db")
	if got != want {
		t.Fatalf("DefaultOperationalDataPath() = %q, want %q", got, want)
	}
}

func TestDefaultRepositoryConfigPathIsInsideTheRepository(t *testing.T) {
	t.Parallel()

	got := config.DefaultRepositoryConfigPath("/work/repository")
	want := "/work/repository/factory.yaml"
	if got != want {
		t.Fatalf("DefaultRepositoryConfigPath() = %q, want %q", got, want)
	}
}

func TestLoadRepositoryRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "factory.yaml")
	if err := os.WriteFile(path, []byte("schema_version: 1\nunknown_field: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := config.LoadRepository(path)
	var fileErr *config.ConfigFileError
	if !errors.As(err, &fileErr) {
		t.Fatalf("error = %v, want ConfigFileError", err)
	}
}

func TestLoadRepositoryRejectsMultipleYAMLDocuments(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "factory.yaml")
	contents := "schema_version: 1\n---\nschema_version: 1\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := config.LoadRepository(path)
	var fileErr *config.ConfigFileError
	if !errors.As(err, &fileErr) {
		t.Fatalf("error = %v, want ConfigFileError", err)
	}
	if !strings.Contains(fileErr.Error(), "more than one YAML document") {
		t.Fatalf("error = %q, want more-than-one-document diagnostic", fileErr.Error())
	}
}

func TestLoadRepositoryWrapsTheUnderlyingFileError(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "missing.yaml")
	_, err := config.LoadRepository(path)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want a wrapped os.ErrNotExist", err)
	}
	var fileErr *config.ConfigFileError
	if !errors.As(err, &fileErr) || fileErr.Path != path {
		t.Fatalf("error = %#v, want ConfigFileError for %q", err, path)
	}
}

func TestValidationErrorMessageIncludesTheFieldAndMessage(t *testing.T) {
	t.Parallel()

	err := config.ValidateRepository(config.RepositoryConfig{SchemaVersion: 1})
	if !strings.Contains(err.Error(), "target_branch") || !strings.Contains(err.Error(), "is required") {
		t.Fatalf("error = %q, want it to name the offending field and reason", err.Error())
	}
}

func TestUnknownSchemaVersionErrorDefaultsToTheSchemaVersionField(t *testing.T) {
	t.Parallel()

	err := &config.UnknownSchemaVersionError{Kind: "host", Version: 2, Supported: 1}
	if !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("error = %q, want it to default to the schema_version field", err.Error())
	}
}

func validRegistration(t *testing.T) config.RepositoryRegistration {
	t.Helper()
	root := t.TempDir()
	return config.RepositoryRegistration{
		Path:                 filepath.Join(root, "repository"),
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		AuthorizedUsers:      []string{"alice"},
		Polling:              config.PollingConfig{Interval: "30s", Backoff: "5m"},
		Cmux:                 config.CmuxConfig{ControlWorkspace: "factory-control"},
		OperationalDataPath:  filepath.Join(root, "data", "factory.db"),
		RepositoryConfigPath: filepath.Join(root, "repository", "factory.yaml"),
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
