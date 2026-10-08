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
	host := `schema_version: 2
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
	host := `schema_version: 2
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
	if err := os.WriteFile(path, []byte("schema_version: 2\nrepositories: []\n"), 0o644); err != nil {
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
	host := `schema_version: 2
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

// TestCacheStartupCheckAcceptsAMappedRepositoryCache verifies a declared cache
// with a host directory under the cache root passes startup diagnosis.
func TestCacheStartupCheckAcceptsAMappedRepositoryCache(t *testing.T) {
	t.Parallel()

	registration := config.RepositoryRegistration{
		CacheRoot: "/var/lib/factory/caches",
		Caches:    map[string]string{"go-build": "/var/lib/factory/caches/go-build"},
	}
	repository := config.RepositoryConfig{Caches: []config.CacheConfig{{Name: "go-build"}}}

	result := config.CacheStartupCheck(config.DoctorState{Registration: &registration, Repository: &repository})(context.Background())
	if result.Status != doctor.StatusPassed {
		t.Fatalf("cache diagnosis = %#v, want passed", result)
	}
}

// TestCacheStartupCheckBlocksADeclaredCacheWithNoHostMapping verifies an
// unmapped cache is a blocking finding rather than a silent default path.
func TestCacheStartupCheckBlocksADeclaredCacheWithNoHostMapping(t *testing.T) {
	t.Parallel()

	registration := config.RepositoryRegistration{CacheRoot: "/var/lib/factory/caches"}
	repository := config.RepositoryConfig{Caches: []config.CacheConfig{{Name: "go-build"}}}

	result := config.CacheStartupCheck(config.DoctorState{Registration: &registration, Repository: &repository})(context.Background())
	if result.Status != doctor.StatusFailed {
		t.Fatalf("cache diagnosis = %#v, want failed", result)
	}
	if !strings.Contains(result.Problem, "go-build") || !strings.Contains(result.Action, "caches.go-build") {
		t.Fatalf("cache diagnosis = %#v, want the unmapped cache name and host field", result)
	}
}

// TestCacheStartupCheckBlocksAHostMappingForAnUndeclaredCache verifies host
// configuration cannot mount a cache the repository never declared.
func TestCacheStartupCheckBlocksAHostMappingForAnUndeclaredCache(t *testing.T) {
	t.Parallel()

	registration := config.RepositoryRegistration{
		CacheRoot: "/var/lib/factory/caches",
		Caches:    map[string]string{"npm": "/var/lib/factory/caches/npm"},
	}
	repository := config.RepositoryConfig{}

	result := config.CacheStartupCheck(config.DoctorState{Registration: &registration, Repository: &repository})(context.Background())
	if result.Status != doctor.StatusFailed {
		t.Fatalf("cache diagnosis = %#v, want failed", result)
	}
	if !strings.Contains(result.Problem, "npm") {
		t.Fatalf("cache diagnosis = %#v, want the undeclared cache name", result)
	}
}

// TestCacheStartupCheckBlocksAHostPathOutsideTheCacheRoot verifies a
// hand-edited host file cannot escape the operator's cache root.
func TestCacheStartupCheckBlocksAHostPathOutsideTheCacheRoot(t *testing.T) {
	t.Parallel()

	registration := config.RepositoryRegistration{
		CacheRoot: "/var/lib/factory/caches",
		Caches:    map[string]string{"go-build": "/Users/maintainer/.ssh"},
	}
	repository := config.RepositoryConfig{Caches: []config.CacheConfig{{Name: "go-build"}}}

	result := config.CacheStartupCheck(config.DoctorState{Registration: &registration, Repository: &repository})(context.Background())
	if result.Status != doctor.StatusFailed {
		t.Fatalf("cache diagnosis = %#v, want failed", result)
	}
	if !strings.Contains(result.Problem, "cache root") {
		t.Fatalf("cache diagnosis = %#v, want a cache-root containment problem", result)
	}
}

// TestCacheStartupCheckStaysSilentWithoutConfiguration verifies the cache
// diagnosis does not repeat a configuration failure another check reports.
func TestCacheStartupCheckStaysSilentWithoutConfiguration(t *testing.T) {
	t.Parallel()

	result := config.CacheStartupCheck(config.DoctorState{})(context.Background())
	if result.Status != doctor.StatusPassed {
		t.Fatalf("cache diagnosis = %#v, want passed without configuration", result)
	}
}

// TestStartupCheckNamesTheInvalidRepositoryField verifies a semantic
// repository-policy failure names the field and the validator reason so an
// operator can repair factory.yaml without guessing.
func TestStartupCheckNamesTheInvalidRepositoryField(t *testing.T) {
	result := diagnoseRepositoryConfig(t, func(contents string) string {
		return strings.Replace(contents, "  test: [gpt-5]", "  test: []", 1)
	})
	if result.Status != doctor.StatusFailed {
		t.Fatalf("startup result = %#v, want failed", result)
	}
	if !strings.Contains(result.Problem, "repository configuration") || !strings.Contains(result.Problem, "model_options.test: must declare at least one model") {
		t.Fatalf("startup result = %#v, want the field and reason", result)
	}
	if !strings.Contains(result.Action, "repair the repository factory.yaml") {
		t.Fatalf("startup result = %#v, want the repository repair action", result)
	}
}

// TestStartupCheckNamesAnInvalidDigestWithoutEchoingIt verifies a rejected
// worker digest names the field and format without printing the value or any
// unrelated repository scalar.
func TestStartupCheckNamesAnInvalidDigestWithoutEchoingIt(t *testing.T) {
	supplied := "sha256:not-a-digest-sentinel"
	result := diagnoseRepositoryConfig(t, func(contents string) string {
		contents = strings.Replace(contents, "target_branch: main", "target_branch: main-sentinel", 1)
		contents = strings.Replace(contents, "command: go test ./...", "command: go test ./... # sentinel", 1)
		return strings.Replace(contents, "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", supplied, 1)
	})
	want := "worker_build.digest: must be a sha256 digest with 64 hexadecimal characters"
	if result.Status != doctor.StatusFailed || !strings.Contains(result.Problem, want) {
		t.Fatalf("startup result = %#v, want %q", result, want)
	}
	if strings.Contains(result.Problem+result.Action, "sentinel") {
		t.Fatalf("startup result exposed repository values: %#v", result)
	}
}

// TestStartupCheckNamesAFactoryOwnedWorkflowDeclaration verifies a repository
// attempt to declare workflow roles yields the typed policy finding.
func TestStartupCheckNamesAFactoryOwnedWorkflowDeclaration(t *testing.T) {
	result := diagnoseRepositoryConfig(t, func(contents string) string {
		return contents + "roles: [secret-role-sentinel]\n"
	})
	want := "roles: workflow declarations are factory-owned and cannot be supplied by repository configuration"
	if result.Status != doctor.StatusFailed || !strings.Contains(result.Problem, "repository policy") || !strings.Contains(result.Problem, want) {
		t.Fatalf("startup result = %#v, want the policy field and reason", result)
	}
	if strings.Contains(result.Problem+result.Action, "sentinel") {
		t.Fatalf("startup result exposed configuration content: %#v", result)
	}
}

// TestStartupCheckNamesANewerRepositorySchema verifies an unsupported schema
// names schema_version and the version mismatch.
func TestStartupCheckNamesANewerRepositorySchema(t *testing.T) {
	result := diagnoseRepositoryConfig(t, func(contents string) string {
		return strings.Replace(contents, "schema_version: 1", "schema_version: 99", 1)
	})
	want := "schema_version 99 is newer than supported version 1"
	if result.Status != doctor.StatusFailed || !strings.Contains(result.Problem, "repository configuration") || !strings.Contains(result.Problem, want) {
		t.Fatalf("startup result = %#v, want %q", result, want)
	}
}

// TestStartupCheckCollapsesUnknownRoleKeys verifies an unknown role key in any
// role map reports only the parent map and the safe reason, so secret-like or
// terminal-control keys never reach the operator's terminal.
func TestStartupCheckCollapsesUnknownRoleKeys(t *testing.T) {
	const reason = ": role must be declared by the factory-owned workflow registry"
	keys := map[string]string{
		"secret-like": `"sk-live-sentinel-token"`,
		"newline":     `"evil\nsentinel"`,
		"terminal":    `"\e[31msentinel\e[0m"`,
		"indexed":     `"test[0]"`,
		"long":        strings.Repeat("sentinel", 64),
	}
	maps := map[string]func(contents, key string) string{
		"role_craft": func(contents, key string) string {
			return contents + "role_craft:\n  " + key + ": docs/craft.md\n"
		},
		"role_harness_defaults": func(contents, key string) string {
			return strings.Replace(contents, "role_harness_defaults:\n", "role_harness_defaults:\n  "+key+": codex\n", 1)
		},
		"model_options": func(contents, key string) string {
			return strings.Replace(contents, "model_options:\n", "model_options:\n  "+key+": [gpt-5]\n", 1)
		},
		"reasoning_effort_options": func(contents, key string) string {
			return contents + "reasoning_effort_options:\n  " + key + ": [high]\n"
		},
	}
	for field, insert := range maps {
		for name, key := range keys {
			t.Run(field+"/"+name, func(t *testing.T) {
				result := diagnoseRepositoryConfig(t, func(contents string) string { return insert(contents, key) })
				if result.Status != doctor.StatusFailed || !strings.Contains(result.Problem, field+reason) {
					t.Fatalf("startup result = %#v, want %q", result, field+reason)
				}
				if strings.ContainsAny(result.Problem+result.Action, "\n\x1b") || strings.Contains(result.Problem+result.Action, "sentinel") || strings.Contains(result.Problem, field+".") {
					t.Fatalf("startup result exposed the role key: %#v", result)
				}
			})
		}
	}
}

// TestStartupCheckKeepsAKnownRoleAndListIndex verifies a field under a
// factory-declared role keeps the role and the list index the operator needs.
func TestStartupCheckKeepsAKnownRoleAndListIndex(t *testing.T) {
	result := diagnoseRepositoryConfig(t, func(contents string) string {
		return strings.Replace(contents, "  test: [gpt-5]", `  test: [""]`, 1)
	})
	want := "model_options.test[0]: must not be empty"
	if result.Status != doctor.StatusFailed || !strings.Contains(result.Problem, want) {
		t.Fatalf("startup result = %#v, want %q", result, want)
	}
}

// TestStartupCheckKeepsParseFailuresGeneric verifies YAML syntax, type, and
// unknown-field failures keep the generic finding, because their decoder
// diagnostics can quote repository content.
func TestStartupCheckKeepsParseFailuresGeneric(t *testing.T) {
	const sentinel = "zqleak"
	cases := map[string]func(string) string{
		"syntax": func(contents string) string { return contents + "target_branch: [" + sentinel + "\n" },
		"type": func(contents string) string {
			return contents + "review_units:\n  max_units: " + sentinel + "\n"
		},
		"unknown field": func(contents string) string { return contents + sentinel + ": true\n" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			result := diagnoseRepositoryConfig(t, mutate)
			if result.Status != doctor.StatusFailed || result.Problem != "checked-in repository configuration is missing or invalid" {
				t.Fatalf("startup result = %#v, want the generic repository finding", result)
			}
			if strings.Contains(result.Problem+result.Action, sentinel) {
				t.Fatalf("startup result exposed configuration content: %#v", result)
			}
		})
	}
}

// TestRepositoryTypeErrorQuotesTheInputValue proves the type-error fixture in
// TestStartupCheckKeepsParseFailuresGeneric exercises a decoder error that
// really contains the sentinel, so the generic fallback is meaningful.
func TestRepositoryTypeErrorQuotesTheInputValue(t *testing.T) {
	repositoryPath := t.TempDir()
	writeDoctorRepositoryConfig(t, repositoryPath)
	path := filepath.Join(repositoryPath, "factory.yaml")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(contents, []byte("review_units:\n  max_units: zqleak\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = config.LoadRepository(path)
	if err == nil || !strings.Contains(err.Error(), "zqleak") {
		t.Fatalf("LoadRepository() error = %v, want a decoder error quoting the sentinel", err)
	}
}

// diagnoseRepositoryConfig runs the configuration startup check against a
// private host registration and the smallest valid repository policy after
// mutate rewrites the policy text.
func diagnoseRepositoryConfig(t *testing.T, mutate func(string) string) doctor.Result {
	t.Helper()
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDoctorRepositoryConfig(t, repositoryPath)
	repositoryConfigPath := filepath.Join(repositoryPath, "factory.yaml")
	contents, err := os.ReadFile(repositoryConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	mutated := mutate(string(contents))
	if mutated == string(contents) {
		t.Fatal("mutation did not change the repository configuration")
	}
	if err := os.WriteFile(repositoryConfigPath, []byte(mutated), 0o600); err != nil {
		t.Fatal(err)
	}
	hostPath := filepath.Join(root, "config.yaml")
	if err := config.SaveHost(hostPath, config.HostConfig{
		SchemaVersion: config.CurrentHostSchemaVersion,
		Repositories: []config.RepositoryRegistration{{
			Path: repositoryPath, GitHub: config.GitHubConfig{Owner: "example", Repository: "project"},
			AuthorizedUsers: []string{"alice"}, Polling: config.PollingConfig{Interval: "30s", Backoff: "5m"},
			OperationalDataPath: filepath.Join(root, "state", "factory.db"), RepositoryConfigPath: repositoryConfigPath,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	state, check := config.StartupCheck(hostPath)
	result := check(context.Background())
	if result.Status == doctor.StatusFailed && state.Repository != nil {
		t.Fatalf("startup state = %#v, want no repository projection on failure", state)
	}
	return result
}

// TestWorkerLimitsStartupCheckReportsTheEffectiveLimits verifies startup
// diagnosis names the limits every worker container receives.
func TestWorkerLimitsStartupCheckReportsTheEffectiveLimits(t *testing.T) {
	t.Parallel()

	registration := config.RepositoryRegistration{WorkerLimits: config.WorkerLimitsConfig{Memory: "2g", PIDs: "512"}}

	result := config.WorkerLimitsStartupCheck(config.DoctorState{Registration: &registration})(context.Background())
	if result.Status != doctor.StatusPassed || result.Name != "worker limits" {
		t.Fatalf("worker limits diagnosis = %#v, want a passed worker limits check", result)
	}
	if result.Detail != "memory 2g, swap 2g, cpus 4, pids 512, log 3 x 10m" {
		t.Fatalf("worker limits detail = %q, want the effective limits", result.Detail)
	}
}

// TestWorkerLimitsStartupCheckReportsNothingWithoutARegistration verifies the
// check leaves an unreadable configuration to the configuration check.
func TestWorkerLimitsStartupCheckReportsNothingWithoutARegistration(t *testing.T) {
	t.Parallel()

	result := config.WorkerLimitsStartupCheck(config.DoctorState{})(context.Background())
	if result.Status != doctor.StatusPassed || result.Detail != "" {
		t.Fatalf("worker limits diagnosis = %#v, want a passed check with no detail", result)
	}
}
