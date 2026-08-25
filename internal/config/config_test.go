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
setup_environment_policy: clean
setup_files: [go.mod, go.sum]
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
	if len(got.SetupFiles) != 2 || got.SetupFiles[0] != "go.mod" || got.SetupFiles[1] != "go.sum" {
		t.Fatalf("SetupFiles = %#v, want configured manifest and lockfile", got.SetupFiles)
	}
	if got.SetupEnvironmentPolicy != config.EnvironmentPolicyClean {
		t.Fatalf("SetupEnvironmentPolicy = %q, want clean", got.SetupEnvironmentPolicy)
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

// TestValidateRepositoryRejectsUnsafeSetupFiles verifies setup dependency
// inputs remain repository-relative and unique.
func TestValidateRepositoryRejectsUnsafeSetupFiles(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		files []string
		field string
	}{
		{name: "absolute", files: []string{"/tmp/go.mod"}, field: "setup_files[0]"},
		{name: "parent traversal", files: []string{"../go.mod"}, field: "setup_files[0]"},
		{name: "duplicate", files: []string{"go.mod", "go.mod"}, field: "setup_files[1]"},
		{name: "tab control character", files: []string{"go\t.mod"}, field: "setup_files[0]"},
		{name: "unicode control character", files: []string{"go\u0085.mod"}, field: "setup_files[0]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := validRepositoryConfig()
			policy.SetupFiles = tc.files
			err := config.ValidateRepository(policy)
			var validationErr *config.ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("ValidateRepository() error = %v, want ValidationError", err)
			}
			if validationErr.Field != tc.field {
				t.Fatalf("field = %q, want %q", validationErr.Field, tc.field)
			}
		})
	}
}

// TestValidateRepositoryRejectsInvalidSetupEnvironmentPolicy verifies setup
// cannot silently select an unconfigured worker environment.
func TestValidateRepositoryRejectsInvalidSetupEnvironmentPolicy(t *testing.T) {
	t.Parallel()

	policy := validRepositoryConfig()
	policy.SetupEnvironmentPolicy = "host"
	err := config.ValidateRepository(policy)
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "setup_environment_policy" {
		t.Fatalf("ValidateRepository() error = %v, want setup_environment_policy validation", err)
	}
}

// TestValidateRepositoryAcceptsExplicitEvaluationRetention verifies local
// evaluation retention is optional but, when present, must be a positive
// duration.
func TestValidateRepositoryAcceptsExplicitEvaluationRetention(t *testing.T) {
	t.Parallel()

	policy := validRepositoryConfig()
	policy.Evaluation.Retention = "720h"
	if err := config.ValidateRepository(policy); err != nil {
		t.Fatalf("ValidateRepository() error = %v, want valid evaluation retention", err)
	}

	policy.Evaluation.Retention = "0s"
	err := config.ValidateRepository(policy)
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != "evaluation.retention" {
		t.Fatalf("ValidateRepository() error = %v, want evaluation.retention validation", err)
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

func TestNewHostConfigUsesTheCurrentSchemaVersionAndNoRepositories(t *testing.T) {
	t.Parallel()

	got := config.NewHostConfig()
	if got.SchemaVersion != config.CurrentSchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", got.SchemaVersion, config.CurrentSchemaVersion)
	}
	if len(got.Repositories) != 0 {
		t.Fatalf("Repositories = %#v, want none", got.Repositories)
	}
}

func TestDefaultHostConfigPathPrefersTheFactoryConfigEnvironmentVariable(t *testing.T) {
	t.Setenv("FACTORY_CONFIG", "relative/config.yaml")

	got, err := config.DefaultHostConfigPath()
	if err != nil {
		t.Fatalf("DefaultHostConfigPath() error = %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("DefaultHostConfigPath() = %q, want an absolute path", got)
	}
	if !strings.HasSuffix(got, filepath.Join("relative", "config.yaml")) {
		t.Fatalf("DefaultHostConfigPath() = %q, want it derived from FACTORY_CONFIG", got)
	}
}

func TestDefaultOperationalDataPathIsRelativeToTheHostConfigDirectory(t *testing.T) {
	t.Parallel()

	got := config.DefaultOperationalDataPath("/home/user/.config/factory/config.yaml")
	want := filepath.Join("/home/user/.config/factory", "data", "factory.db")
	if got != want {
		t.Fatalf("DefaultOperationalDataPath() = %q, want %q", got, want)
	}
}

func TestDefaultRepositoryConfigPathJoinsTheRepositoryRoot(t *testing.T) {
	t.Parallel()

	got := config.DefaultRepositoryConfigPath("/work/repository")
	want := filepath.Join("/work/repository", config.RepositoryConfigFileName)
	if got != want {
		t.Fatalf("DefaultRepositoryConfigPath() = %q, want %q", got, want)
	}
}

func TestCreateHostWritesAPrivateDefaultConfigurationAndRejectsAnExistingPath(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "host", "config.yaml")
	created, err := config.CreateHost(path)
	if err != nil {
		t.Fatalf("CreateHost() error = %v", err)
	}
	if created.SchemaVersion != config.CurrentSchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", created.SchemaVersion, config.CurrentSchemaVersion)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config permissions = %#o, want 0600", got)
	}

	if _, err := config.CreateHost(path); err == nil {
		t.Fatal("CreateHost() succeeded for an existing configuration path")
	}
}

func TestSaveHostRejectsAnInvalidConfigurationWithoutWriting(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	invalid := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{
		{Path: "relative/repository"},
	}}
	err := config.SaveHost(path, invalid)
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("SaveHost() error = %v, want ValidationError", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("SaveHost() wrote a file despite validation failure: %v", statErr)
	}
}

func TestSaveHostThenLoadHostRoundTripsARegisteredRepository(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "host", "config.yaml")
	registration := config.RepositoryRegistration{
		Path:                 "/work/repository",
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		AuthorizedUsers:      []string{"alice", "bob"},
		Polling:              config.PollingConfig{Interval: "30s", Backoff: "5m"},
		OperationalDataPath:  "/var/lib/factory/factory.db",
		RepositoryConfigPath: "/work/repository/factory.yaml",
	}
	want := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{registration}}
	if err := config.SaveHost(path, want); err != nil {
		t.Fatalf("SaveHost() error = %v", err)
	}

	got, err := config.LoadHost(path)
	if err != nil {
		t.Fatalf("LoadHost() error = %v", err)
	}
	if len(got.Repositories) != 1 || got.Repositories[0].Path != registration.Path {
		t.Fatalf("LoadHost() = %#v, want round-tripped registration", got)
	}
	if len(got.Repositories[0].AuthorizedUsers) != 2 {
		t.Fatalf("AuthorizedUsers = %#v, want two entries", got.Repositories[0].AuthorizedUsers)
	}
}

func TestValidateHostAcceptsNoRegisteredRepositories(t *testing.T) {
	t.Parallel()

	if err := config.ValidateHost(config.NewHostConfig()); err != nil {
		t.Fatalf("ValidateHost() error = %v, want no error for zero repositories", err)
	}
}

func TestValidateHostRejectsMoreThanOneRegisteredRepository(t *testing.T) {
	t.Parallel()

	registration := config.RepositoryRegistration{
		Path:                 "/work/repository",
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		AuthorizedUsers:      []string{"alice"},
		Polling:              config.PollingConfig{Interval: "30s", Backoff: "5m"},
		OperationalDataPath:  "/var/lib/factory/factory.db",
		RepositoryConfigPath: "/work/repository/factory.yaml",
	}
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

func TestValidateHostRejectsMissingGitHubMetadataAndDuplicateAuthorizedUsers(t *testing.T) {
	t.Parallel()

	base := config.RepositoryRegistration{
		Path:                 "/work/repository",
		AuthorizedUsers:      []string{"alice"},
		Polling:              config.PollingConfig{Interval: "30s", Backoff: "5m"},
		OperationalDataPath:  "/var/lib/factory/factory.db",
		RepositoryConfigPath: "/work/repository/factory.yaml",
	}

	tests := []struct {
		name   string
		mutate func(config.RepositoryRegistration) config.RepositoryRegistration
		field  string
	}{
		{
			name: "missing owner",
			mutate: func(r config.RepositoryRegistration) config.RepositoryRegistration {
				r.GitHub = config.GitHubConfig{Repository: "project"}
				return r
			},
			field: "repositories[0].github.owner",
		},
		{
			name: "missing repository",
			mutate: func(r config.RepositoryRegistration) config.RepositoryRegistration {
				r.GitHub = config.GitHubConfig{Owner: "example"}
				return r
			},
			field: "repositories[0].github.repository",
		},
		{
			name: "duplicate authorized users",
			mutate: func(r config.RepositoryRegistration) config.RepositoryRegistration {
				r.GitHub = config.GitHubConfig{Owner: "example", Repository: "project"}
				r.AuthorizedUsers = []string{"alice", "alice"}
				return r
			},
			field: "repositories[0].authorized_users[1]",
		},
		{
			name: "relative cmux socket path",
			mutate: func(r config.RepositoryRegistration) config.RepositoryRegistration {
				r.GitHub = config.GitHubConfig{Owner: "example", Repository: "project"}
				r.Cmux = config.CmuxConfig{SocketPath: "relative/socket"}
				return r
			},
			field: "repositories[0].cmux.socket_path",
		},
		{
			name: "invalid polling interval",
			mutate: func(r config.RepositoryRegistration) config.RepositoryRegistration {
				r.GitHub = config.GitHubConfig{Owner: "example", Repository: "project"}
				r.Polling.Interval = "not-a-duration"
				return r
			},
			field: "repositories[0].polling.interval",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{tc.mutate(base)}}
			err := config.ValidateHost(host)
			var validationErr *config.ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("ValidateHost() error = %v, want ValidationError", err)
			}
			if validationErr.Field != tc.field {
				t.Fatalf("field = %q, want %q", validationErr.Field, tc.field)
			}
		})
	}
}

func TestValidateRepositoryAcceptsAnEmptyAllowedOverridesList(t *testing.T) {
	t.Parallel()

	policy := validRepositoryConfig()
	policy.AllowedOverrides = []config.OverrideName{}
	if err := config.ValidateRepository(policy); err != nil {
		t.Fatalf("ValidateRepository() error = %v, want no error for an empty allowed_overrides list", err)
	}
}

func TestValidateRepositoryRejectsInvalidFieldsOneAtATime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(config.RepositoryConfig) config.RepositoryConfig
		field  string
	}{
		{
			name: "missing schema version",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.SchemaVersion = 0
				return c
			},
			field: "schema_version",
		},
		{
			name: "negative schema version",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.SchemaVersion = -1
				return c
			},
			field: "schema_version",
		},
		{
			name: "empty target branch",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.TargetBranch = "  "
				return c
			},
			field: "target_branch",
		},
		{
			name: "multiline target branch",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.TargetBranch = "main\nfeature"
				return c
			},
			field: "target_branch",
		},
		{
			name: "empty setup",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.Setup = ""
				return c
			},
			field: "setup",
		},
		{
			name: "no gates",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.Gates = nil
				return c
			},
			field: "gates",
		},
		{
			name: "duplicate gate names",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.Gates = append(c.Gates, c.Gates[0])
				return c
			},
			field: "gates[1].name",
		},
		{
			name: "gate depends on a later gate",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.Gates = []config.GateConfig{
					{Name: "test", Command: "go test ./...", Timeout: "5m", Blocking: true, EnvironmentPolicy: config.EnvironmentPolicyClean, DependsOn: []string{"build"}},
					{Name: "build", Command: "go build ./...", Timeout: "5m", Blocking: true, EnvironmentPolicy: config.EnvironmentPolicyClean},
				}
				return c
			},
			field: "gates[0].depends_on[0]",
		},
		{
			name: "gate depends on itself",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.Gates[0].DependsOn = []string{"test"}
				return c
			},
			field: "gates[0].depends_on[0]",
		},
		{
			name: "invalid gate environment policy",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.Gates[0].EnvironmentPolicy = "dirty"
				return c
			},
			field: "gates[0].environment_policy",
		},
		{
			name: "invalid gate timeout",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.Gates[0].Timeout = "not-a-duration"
				return c
			},
			field: "gates[0].timeout",
		},
		{
			name: "invalid harness value",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.RoleHarnessDefaults["implementation"] = "unsupported-harness"
				return c
			},
			field: "role_harness_defaults.implementation",
		},
		{
			name: "invalid retry limit",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.RetryLimits.CheckRepair = 0
				return c
			},
			field: "retry_limits.check_repair",
		},
		{
			name: "check repair limit above hard ceiling",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.RetryLimits.CheckRepair = config.MaxCheckRepairAttempts + 1
				return c
			},
			field: "retry_limits.check_repair",
		},
		{
			name: "invalid test policy mode",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.TestPolicy.Mode = "sometimes"
				return c
			},
			field: "test_policy.mode",
		},
		{
			name: "duplicate allowed overrides",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.AllowedOverrides = []config.OverrideName{config.OverrideModel, config.OverrideModel}
				return c
			},
			field: "allowed_overrides[1]",
		},
		{
			name: "duplicate cache names",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.Caches = []config.CacheConfig{
					{Name: "go-build", Path: "/tmp/a"},
					{Name: "go-build", Path: "/tmp/b"},
				}
				return c
			},
			field: "caches[1].name",
		},
		{
			name: "empty cache path",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.Caches = []config.CacheConfig{{Name: "go-build", Path: ""}}
				return c
			},
			field: "caches[0].path",
		},
		{
			name: "missing worker image",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.WorkerBuild.Image = ""
				return c
			},
			field: "worker_build.image",
		},
		{
			name: "digest missing prefix",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.WorkerBuild.Digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
				return c
			},
			field: "worker_build.digest",
		},
		{
			name: "digest too short",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.WorkerBuild.Digest = "sha256:0123"
				return c
			},
			field: "worker_build.digest",
		},
		{
			name: "digest with uppercase hex",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.WorkerBuild.Digest = "sha256:0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef"
				return c
			},
			field: "worker_build.digest",
		},
		{
			name: "missing worker definition",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.WorkerBuild.Definition = ""
				return c
			},
			field: "worker_build.definition",
		},
		{
			name: "invalid base synchronization mode",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.BaseSynchronization.Mode = "sometimes"
				return c
			},
			field: "base_synchronization.mode",
		},
		{
			name: "before_ready without a branch",
			mutate: func(c config.RepositoryConfig) config.RepositoryConfig {
				c.BaseSynchronization = config.BaseSynchronization{Mode: config.BaseSynchronizationBeforeReady}
				return c
			},
			field: "base_synchronization.branch",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := config.ValidateRepository(tc.mutate(validRepositoryConfig()))
			var validationErr *config.ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("ValidateRepository() error = %v, want ValidationError", err)
			}
			if validationErr.Field != tc.field {
				t.Fatalf("field = %q, want %q", validationErr.Field, tc.field)
			}
		})
	}
}

func TestLoadRepositoryRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "factory.yaml")
	if err := os.WriteFile(path, []byte("schema_version: 1\nunexpected_field: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := config.LoadRepository(path)
	var fileErr *config.ConfigFileError
	if !errors.As(err, &fileErr) {
		t.Fatalf("LoadRepository() error = %v, want ConfigFileError", err)
	}
	if fileErr.Path != path {
		t.Fatalf("ConfigFileError.Path = %q, want %q", fileErr.Path, path)
	}
	if fileErr.Unwrap() == nil {
		t.Fatal("ConfigFileError.Unwrap() = nil, want the underlying decode error")
	}
}

func TestLoadRepositoryRejectsMultipleYAMLDocuments(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "factory.yaml")
	if err := os.WriteFile(path, []byte("schema_version: 1\n---\nschema_version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := config.LoadRepository(path)
	var fileErr *config.ConfigFileError
	if !errors.As(err, &fileErr) {
		t.Fatalf("LoadRepository() error = %v, want ConfigFileError", err)
	}
	if !strings.Contains(err.Error(), "more than one YAML document") {
		t.Fatalf("error = %v, want a multiple-document diagnostic", err)
	}
}

func TestLoadRepositoryReportsAMissingFileAsAConfigFileError(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "missing.yaml")
	_, err := config.LoadRepository(path)
	var fileErr *config.ConfigFileError
	if !errors.As(err, &fileErr) {
		t.Fatalf("LoadRepository() error = %v, want ConfigFileError", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want it to unwrap to os.ErrNotExist", err)
	}
}

func TestUnknownSchemaVersionErrorMessageUsesTheProvidedKindAndField(t *testing.T) {
	t.Parallel()

	err := &config.UnknownSchemaVersionError{Kind: "host", Field: "schema_version", Version: 7, Supported: 1}
	got := err.Error()
	if !strings.Contains(got, "host") || !strings.Contains(got, "7") || !strings.Contains(got, "1") {
		t.Fatalf("Error() = %q, want it to mention the kind and both versions", got)
	}
}

func validRepositoryConfig() config.RepositoryConfig {
	return config.RepositoryConfig{
		SchemaVersion:          1,
		TargetBranch:           "main",
		Setup:                  "go mod download",
		SetupEnvironmentPolicy: config.EnvironmentPolicyClean,
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
