package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/Stevie1704/sw-factory/internal/workflow"
	"gopkg.in/yaml.v3"
)

const (
	CurrentSchemaVersion     = 1
	RepositoryConfigFileName = "factory.yaml"
)

type HostConfig struct {
	SchemaVersion int                      `yaml:"schema_version"`
	Repositories  []RepositoryRegistration `yaml:"repositories"`
}

type RepositoryRegistration struct {
	Path            string        `yaml:"path"`
	GitHub          GitHubConfig  `yaml:"github"`
	AuthorizedUsers []string      `yaml:"authorized_users"`
	Polling         PollingConfig `yaml:"polling"`
	Cmux            CmuxConfig    `yaml:"cmux"`
	// Authentication contains optional narrowly scoped host credential sources.
	Authentication       AuthenticationConfig `yaml:"authentication"`
	OperationalDataPath  string               `yaml:"operational_data_path"`
	RepositoryConfigPath string               `yaml:"repository_config_path"`
}

type GitHubConfig struct {
	Owner      string `yaml:"owner"`
	Repository string `yaml:"repository"`
}

type PollingConfig struct {
	Interval string `yaml:"interval"`
	Backoff  string `yaml:"backoff"`
}

type CmuxConfig struct {
	SocketPath       string `yaml:"socket_path"`
	ControlWorkspace string `yaml:"control_workspace"`
}

// AuthenticationConfig identifies narrowly scoped host credential sources.
// The factory stores the path, never the credential contents.
type AuthenticationConfig struct {
	// CodexAuthPath is an optional host path to one Codex auth.json file.
	CodexAuthPath string `yaml:"codex_auth_path"`
	// ClaudeAuthPath is an optional host path to one Claude Code credential
	// file. A macOS host keeps this credential in the login Keychain rather
	// than a file, so the path is declared only where a file exists.
	ClaudeAuthPath string `yaml:"claude_auth_path"`
}

type RepositoryConfig struct {
	SchemaVersion int    `yaml:"schema_version"`
	TargetBranch  string `yaml:"target_branch"`
	Setup         string `yaml:"setup"`
	// SetupFiles identifies checked-in manifests and lockfiles used to fingerprint setup.
	SetupFiles []string `yaml:"setup_files"`
	// SetupEnvironmentPolicy selects the checked-in worker environment for setup.
	SetupEnvironmentPolicy EnvironmentPolicy  `yaml:"setup_environment_policy"`
	Gates                  []GateConfig       `yaml:"gates"`
	RoleHarnessDefaults    map[string]Harness `yaml:"role_harness_defaults"`
	// RoleCraft maps factory-declared role names to repository-relative Markdown
	// files whose contents replace only that role's embedded craft section.
	RoleCraft    map[string]string   `yaml:"role_craft"`
	ModelOptions map[string][]string `yaml:"model_options"`
	// ReasoningEffortOptions declares the validated reasoning-effort values a
	// role may use. A role that declares none accepts no reasoning-effort
	// selection at all.
	ReasoningEffortOptions map[string][]string `yaml:"reasoning_effort_options"`
	Timeouts               TimeoutConfig       `yaml:"timeouts"`
	RetryLimits            RetryLimits         `yaml:"retry_limits"`
	TestPolicy             TestPolicy          `yaml:"test_policy"`
	AllowedOverrides       []OverrideName      `yaml:"allowed_overrides"`
	Caches                 []CacheConfig       `yaml:"caches"`
	WorkerBuild            WorkerBuildConfig   `yaml:"worker_build"`
	BaseSynchronization    BaseSynchronization `yaml:"base_synchronization"`
	// Evaluation controls explicit local evaluation-summary retention. A blank
	// value retains summaries until a deliberate deletion command is run.
	Evaluation EvaluationConfig `yaml:"evaluation"`
}

// EvaluationConfig contains repository-declared retention policy for the
// separate content-free local evaluation projection.
type EvaluationConfig struct {
	// Retention is a positive Go duration used by operators when selecting
	// deliberate deletion cutoffs; the coordinator never deletes automatically.
	Retention string `yaml:"retention"`
}

type EnvironmentPolicy string

const (
	EnvironmentPolicyClean EnvironmentPolicy = "clean"
	EnvironmentPolicyRole  EnvironmentPolicy = "role"
)

type GateConfig struct {
	Name              string            `yaml:"name"`
	Command           string            `yaml:"command"`
	Timeout           string            `yaml:"timeout"`
	Blocking          bool              `yaml:"blocking"`
	DependsOn         []string          `yaml:"depends_on"`
	EnvironmentPolicy EnvironmentPolicy `yaml:"environment_policy"`
}

type TimeoutConfig struct {
	Setup  string `yaml:"setup"`
	Agent  string `yaml:"agent"`
	Gate   string `yaml:"gate"`
	Review string `yaml:"review"`
}

type RetryLimits struct {
	CheckRepair  int `yaml:"check_repair"`
	ReviewRepair int `yaml:"review_repair"`
	TestRevision int `yaml:"test_revision"`
}

type TestMode string

const (
	TestModeRequired TestMode = "required"
	TestModeAdvisory TestMode = "advisory"
)

type TestPolicy struct {
	Mode                    TestMode `yaml:"mode"`
	AllowHumanExemption     bool     `yaml:"allow_human_exemption"`
	AllowTechnicalExemption bool     `yaml:"allow_technical_exemption"`
	// AllowAutomatedObjections is the explicit evidence-gate switch for the
	// bounded implementation-versus-test revision loop. It remains disabled
	// until the measured pilot authorizes automation.
	AllowAutomatedObjections bool `yaml:"allow_automated_objections"`
	// TestPaths adds repository-relative prefixes where the test role may edit.
	TestPaths []string `yaml:"test_paths"`
	// InfrastructurePaths adds essential test-support prefixes owned by the test role.
	InfrastructurePaths []string `yaml:"infrastructure_paths"`
}

type CacheConfig struct {
	Name     string `yaml:"name"`
	Path     string `yaml:"path"`
	ReadOnly bool   `yaml:"read_only"`
}

type WorkerBuildConfig struct {
	Image      string `yaml:"image"`
	Digest     string `yaml:"digest"`
	Definition string `yaml:"definition"`
}

type BaseSynchronizationMode string

const (
	BaseSynchronizationNever       BaseSynchronizationMode = "never"
	BaseSynchronizationBeforeReady BaseSynchronizationMode = "before_ready"
)

type BaseSynchronization struct {
	Mode   BaseSynchronizationMode `yaml:"mode"`
	Branch string                  `yaml:"branch"`
}

type Harness string

const (
	HarnessCodex  Harness = "codex"
	HarnessClaude Harness = "claude"
)

type OverrideName string

const (
	OverrideModel           OverrideName = "model"
	OverrideReasoningEffort OverrideName = "reasoning_effort"
	OverrideHarness         OverrideName = "harness"
)

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("configuration invalid: %s: %s", e.Field, e.Message)
}

// PolicyError reports a repository configuration attempt to take ownership of
// factory-owned workflow policy such as roles, prompts, stages, or transitions.
type PolicyError struct {
	// Field identifies the repository configuration field that crossed the
	// factory/repository authority boundary.
	Field string
	// Message explains the policy boundary without echoing untrusted content.
	Message string
}

// Error returns the stable typed repository-policy diagnostic.
func (e *PolicyError) Error() string {
	return fmt.Sprintf("repository policy rejected: %s: %s", e.Field, e.Message)
}

// RepositoryPolicyError is the descriptive compatibility name for PolicyError.
type RepositoryPolicyError = PolicyError

type UnknownSchemaVersionError struct {
	Kind      string
	Field     string
	Version   int
	Supported int
}

func (e *UnknownSchemaVersionError) Error() string {
	field := e.Field
	if field == "" {
		field = "schema_version"
	}
	return fmt.Sprintf("%s configuration invalid: %s %d is newer than supported version %d", e.Kind, field, e.Version, e.Supported)
}

type ConfigFileError struct {
	Path string
	Err  error
}

func (e *ConfigFileError) Error() string {
	return fmt.Sprintf("read configuration %q: %v", e.Path, e.Err)
}

func (e *ConfigFileError) Unwrap() error { return e.Err }

// NewHostConfig creates an empty host configuration using the current schema version.
func NewHostConfig() HostConfig {
	return HostConfig{SchemaVersion: CurrentSchemaVersion, Repositories: []RepositoryRegistration{}}
}

// DefaultHostConfigPath returns the host configuration path from FACTORY_CONFIG when set,
// or the default factory/config.yaml path in the user's configuration directory. It returns
// an error if the configured path or user configuration directory cannot be resolved.
func DefaultHostConfigPath() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("FACTORY_CONFIG")); configured != "" {
		return filepath.Abs(configured)
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(configDir, "factory", "config.yaml"), nil
}

// DefaultOperationalDataPath returns the default operational data file path relative to the host configuration file.
func DefaultOperationalDataPath(hostConfigPath string) string {
	return filepath.Join(filepath.Dir(hostConfigPath), "data", "factory.db")
}

// DefaultRepositoryConfigPath returns the path to the repository configuration file within the specified repository directory.
func DefaultRepositoryConfigPath(repositoryPath string) string {
	return filepath.Join(repositoryPath, RepositoryConfigFileName)
}

// LoadHost loads and validates a host configuration from a YAML file.
// It returns the configuration or an error if the file cannot be loaded or the configuration is invalid.
func LoadHost(path string) (HostConfig, error) {
	var config HostConfig
	if err := loadYAML(path, &config); err != nil {
		return HostConfig{}, err
	}
	if err := ValidateHost(config); err != nil {
		return HostConfig{}, err
	}
	return config, nil
}

// LoadRepository reads, decodes, and validates a repository configuration from a YAML file.
func LoadRepository(path string) (RepositoryConfig, error) {
	var config RepositoryConfig
	if err := rejectFactoryOwnedWorkflowFields(path); err != nil {
		return RepositoryConfig{}, err
	}
	if err := loadYAML(path, &config); err != nil {
		return RepositoryConfig{}, err
	}
	if err := ValidateRepository(config); err != nil {
		return RepositoryConfig{}, err
	}
	return config, nil
}

// SaveHost validates and atomically writes a host configuration to the specified path.
func SaveHost(path string, config HostConfig) error {
	if err := ValidateHost(config); err != nil {
		return err
	}
	data, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("encode host configuration: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create configuration directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".factory-config-*")
	if err != nil {
		return fmt.Errorf("create temporary configuration: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect temporary configuration: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write configuration: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync configuration: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close configuration: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace configuration: %w", err)
	}
	return nil
}

// CreateHost creates and saves a default host configuration at path.
// It returns an error if the path already exists or cannot be checked or written.
func CreateHost(path string) (HostConfig, error) {
	if _, err := os.Stat(path); err == nil {
		return HostConfig{}, fmt.Errorf("configuration already exists at %q", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return HostConfig{}, fmt.Errorf("check configuration path: %w", err)
	}
	config := NewHostConfig()
	if err := SaveHost(path, config); err != nil {
		return HostConfig{}, err
	}
	return config, nil
}

// ValidateHost validates a host configuration, including its schema version and repository registrations.
func ValidateHost(config HostConfig) error {
	if err := validateSchema("host", config.SchemaVersion); err != nil {
		return err
	}
	if len(config.Repositories) > 1 {
		return validation("repositories", "version one supports one registered repository")
	}
	for index, repository := range config.Repositories {
		prefix := fmt.Sprintf("repositories[%d]", index)
		if err := validateRegistration(prefix, repository); err != nil {
			return err
		}
	}
	return nil
}

// ValidateRepository validates a repository configuration and reports the first invalid field.
// It returns nil when the configuration satisfies all supported repository requirements.
func ValidateRepository(config RepositoryConfig) error {
	if err := validateSchema("repository", config.SchemaVersion); err != nil {
		return err
	}
	if strings.TrimSpace(config.TargetBranch) == "" {
		return validation("target_branch", "is required")
	}
	if strings.ContainsAny(config.TargetBranch, "\r\n") {
		return validation("target_branch", "must be a single line")
	}
	if strings.TrimSpace(config.Setup) == "" {
		return validation("setup", "is required")
	}
	if err := validateSetupFiles(config.SetupFiles); err != nil {
		return err
	}
	if config.SetupEnvironmentPolicy != EnvironmentPolicyClean && config.SetupEnvironmentPolicy != EnvironmentPolicyRole {
		return validation("setup_environment_policy", "must be clean or role")
	}
	if len(config.Gates) == 0 {
		return validation("gates", "must declare at least one ordered gate")
	}
	seenGates := make(map[string]struct{}, len(config.Gates))
	for index, gate := range config.Gates {
		prefix := fmt.Sprintf("gates[%d]", index)
		if strings.TrimSpace(gate.Name) == "" {
			return validation(prefix+".name", "is required")
		}
		if _, exists := seenGates[gate.Name]; exists {
			return validation(prefix+".name", "must be unique")
		}
		seenGates[gate.Name] = struct{}{}
		if strings.TrimSpace(gate.Command) == "" {
			return validation(prefix+".command", "is required")
		}
		if err := validateDuration(prefix+".timeout", gate.Timeout, false); err != nil {
			return err
		}
		if gate.EnvironmentPolicy != EnvironmentPolicyClean && gate.EnvironmentPolicy != EnvironmentPolicyRole {
			return validation(prefix+".environment_policy", "must be clean or role")
		}
		for dependencyIndex, dependency := range gate.DependsOn {
			if dependency == gate.Name {
				return validation(fmt.Sprintf("%s.depends_on[%d]", prefix, dependencyIndex), "must not reference the gate itself")
			}
			if _, exists := seenGates[dependency]; !exists {
				return validation(fmt.Sprintf("%s.depends_on[%d]", prefix, dependencyIndex), "must reference an earlier gate")
			}
		}
	}
	if len(config.RoleHarnessDefaults) == 0 {
		return validation("role_harness_defaults", "must declare at least one role")
	}
	workflowRegistry := workflow.DefaultRegistry()
	if err := validateRoleCraft(config.RoleCraft, workflowRegistry); err != nil {
		return err
	}
	for role, harness := range config.RoleHarnessDefaults {
		if strings.TrimSpace(role) == "" {
			return validation("role_harness_defaults", "role names must not be empty")
		}
		if _, exists := workflowRegistry.Role(role); !exists {
			return policy("role_harness_defaults."+role, "role must be declared by the factory-owned workflow registry")
		}
		if harness != HarnessCodex && harness != HarnessClaude {
			return validation("role_harness_defaults."+role, "must be codex or claude")
		}
	}
	if len(config.ModelOptions) == 0 {
		return validation("model_options", "must declare at least one role")
	}
	for role, models := range config.ModelOptions {
		if _, exists := workflowRegistry.Role(role); !exists {
			return policy("model_options."+role, "role must be declared by the factory-owned workflow registry")
		}
		if len(models) == 0 {
			return validation("model_options."+role, "must declare at least one model")
		}
		for index, model := range models {
			if strings.TrimSpace(model) == "" {
				return validation(fmt.Sprintf("model_options.%s[%d]", role, index), "must not be empty")
			}
		}
	}
	for role := range config.RoleHarnessDefaults {
		if _, exists := config.ModelOptions[role]; !exists {
			return validation("model_options."+role, "must declare a model option for every harness role")
		}
	}
	for role := range config.ModelOptions {
		if _, exists := config.RoleHarnessDefaults[role]; !exists {
			return validation("role_harness_defaults."+role, "must declare a harness for every model role")
		}
	}
	for role, efforts := range config.ReasoningEffortOptions {
		if _, exists := workflowRegistry.Role(role); !exists {
			return policy("reasoning_effort_options."+role, "role must be declared by the factory-owned workflow registry")
		}
		if _, exists := config.RoleHarnessDefaults[role]; !exists {
			return validation("reasoning_effort_options."+role, "must reference a role with a declared harness")
		}
		if len(efforts) == 0 {
			return validation("reasoning_effort_options."+role, "must declare at least one value or be omitted")
		}
		seen := make(map[string]struct{}, len(efforts))
		for index, effort := range efforts {
			field := fmt.Sprintf("reasoning_effort_options.%s[%d]", role, index)
			if strings.TrimSpace(effort) == "" || strings.ContainsAny(effort, "\x00\r\n \t") {
				return validation(field, "must be a nonempty single-token value")
			}
			if _, exists := seen[effort]; exists {
				return validation(field, "must be unique")
			}
			seen[effort] = struct{}{}
			// Claude Code accepts only low, medium, high, xhigh, or max.
			// Validate Claude roles against these supported values.
			if harness := config.RoleHarnessDefaults[role]; harness == HarnessClaude {
				switch effort {
				case "low", "medium", "high", "xhigh", "max":
					// Supported value
				default:
					return validation(field, "must be low, medium, high, xhigh, or max for Claude Code")
				}
			}
		}
	}
	if config.TestPolicy.Mode != TestModeRequired && config.TestPolicy.Mode != TestModeAdvisory {
		return validation("test_policy.mode", "must be required or advisory")
	}
	if config.TestPolicy.Mode == TestModeRequired {
		if _, exists := config.RoleHarnessDefaults["test"]; !exists {
			return validation("role_harness_defaults.test", "must declare the mandatory test role in required mode")
		}
		if _, exists := config.ModelOptions["test"]; !exists {
			return validation("model_options.test", "must declare a model for the mandatory test role in required mode")
		}
	}
	for field, value := range map[string]string{
		"timeouts.setup":  config.Timeouts.Setup,
		"timeouts.agent":  config.Timeouts.Agent,
		"timeouts.gate":   config.Timeouts.Gate,
		"timeouts.review": config.Timeouts.Review,
	} {
		if err := validateDuration(field, value, false); err != nil {
			return err
		}
	}
	for field, value := range map[string]int{
		"retry_limits.check_repair":  config.RetryLimits.CheckRepair,
		"retry_limits.review_repair": config.RetryLimits.ReviewRepair,
		"retry_limits.test_revision": config.RetryLimits.TestRevision,
	} {
		if value < 1 {
			return validation(field, "must be greater than zero")
		}
	}
	if err := validateTestPolicyPaths("test_policy.test_paths", config.TestPolicy.TestPaths); err != nil {
		return err
	}
	if err := validateTestPolicyPaths("test_policy.infrastructure_paths", config.TestPolicy.InfrastructurePaths); err != nil {
		return err
	}
	if err := validateOptionalOverrides(config.AllowedOverrides); err != nil {
		return err
	}
	seenCaches := make(map[string]struct{}, len(config.Caches))
	for index, cache := range config.Caches {
		prefix := fmt.Sprintf("caches[%d]", index)
		if strings.TrimSpace(cache.Name) == "" {
			return validation(prefix+".name", "is required")
		}
		if _, exists := seenCaches[cache.Name]; exists {
			return validation(prefix+".name", "must be unique")
		}
		seenCaches[cache.Name] = struct{}{}
		if strings.TrimSpace(cache.Path) == "" {
			return validation(prefix+".path", "is required")
		}
	}
	if strings.TrimSpace(config.WorkerBuild.Image) == "" {
		return validation("worker_build.image", "is required")
	}
	if strings.TrimSpace(config.WorkerBuild.Digest) == "" {
		return validation("worker_build.digest", "is required")
	}
	if !validSHA256Digest(config.WorkerBuild.Digest) {
		return validation("worker_build.digest", "must be a sha256 digest with 64 hexadecimal characters")
	}
	if strings.TrimSpace(config.WorkerBuild.Definition) == "" {
		return validation("worker_build.definition", "is required")
	}
	if config.BaseSynchronization.Mode != BaseSynchronizationNever && config.BaseSynchronization.Mode != BaseSynchronizationBeforeReady {
		return validation("base_synchronization.mode", "must be never or before_ready")
	}
	if config.BaseSynchronization.Mode == BaseSynchronizationBeforeReady && strings.TrimSpace(config.BaseSynchronization.Branch) == "" {
		return validation("base_synchronization.branch", "is required when synchronization is enabled")
	}
	if strings.TrimSpace(config.Evaluation.Retention) != "" {
		if err := validateDuration("evaluation.retention", config.Evaluation.Retention, false); err != nil {
			return err
		}
	}
	return nil
}

// validateSetupFiles validates optional repository-relative manifest and
// lockfile paths whose contents identify the dependency graph used by setup.
func validateSetupFiles(values []string) error {
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		field := fmt.Sprintf("setup_files[%d]", index)
		if strings.TrimSpace(value) == "" {
			return validation(field, "must not be empty")
		}
		if strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return validation(field, "must not contain control characters")
		}
		if filepath.IsAbs(value) {
			return validation(field, "must be relative to the repository checkout")
		}
		normalized := filepath.Clean(filepath.FromSlash(value))
		if normalized == ".." || strings.HasPrefix(normalized, ".."+string(filepath.Separator)) {
			return validation(field, "must remain inside the repository checkout")
		}
		if _, exists := seen[normalized]; exists {
			return validation(field, "must be unique")
		}
		seen[normalized] = struct{}{}
	}
	return nil
}

// validateRoleCraft validates repository-selected craft files without allowing
// role ownership or a path outside the repository to cross the configuration
// boundary.
func validateRoleCraft(values map[string]string, registry workflow.Registry) error {
	roles := make([]string, 0, len(values))
	for role := range values {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		field := "role_craft." + role
		if strings.TrimSpace(role) == "" {
			return validation(field, "role names must not be empty")
		}
		if _, exists := registry.Role(role); !exists {
			return validation(field, "role must be declared by the factory-owned workflow registry")
		}
		value := values[role]
		if strings.TrimSpace(value) == "" {
			return validation(field, "must name a repository-relative Markdown file")
		}
		if strings.IndexFunc(value, unicode.IsControl) >= 0 || strings.ContainsAny(value, "\\\x00\r\n") || filepath.IsAbs(value) {
			return validation(field, "must be a safe repository-relative path")
		}
		if !strings.EqualFold(filepath.Ext(value), ".md") {
			return validation(field, "must name a repository-relative Markdown file")
		}
		normalized := filepath.ToSlash(value)
		segments := strings.Split(normalized, "/")
		for _, segment := range segments {
			if segment == ".." {
				return validation(field, "must not contain parent traversal segments")
			}
		}
		clean := filepath.ToSlash(filepath.Clean(value))
		if clean == "." || clean == ".git" || strings.HasPrefix(clean, ".git/") {
			return validation(field, "must remain inside the repository checkout")
		}
	}
	return nil
}

// validateTestPolicyPaths validates optional repository-relative test-role
// prefixes used to authorize essential test infrastructure edits.
func validateTestPolicyPaths(field string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		entryField := fmt.Sprintf("%s[%d]", field, index)
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return validation(entryField, "must not be empty")
		}
		if trimmed == "." {
			return validation(entryField, "must remain inside the repository checkout")
		}
		if strings.IndexFunc(value, unicode.IsControl) >= 0 || filepath.IsAbs(value) {
			return validation(entryField, "must be a safe repository-relative path")
		}
		normalized := filepath.Clean(filepath.FromSlash(value))
		if normalized == "." || normalized == ".." || strings.HasPrefix(normalized, ".."+string(filepath.Separator)) || normalized == ".git" || strings.HasPrefix(normalized, ".git"+string(filepath.Separator)) {
			return validation(entryField, "must remain inside the repository checkout")
		}
		if _, exists := seen[normalized]; exists {
			return validation(entryField, "must be unique")
		}
		seen[normalized] = struct{}{}
	}
	return nil
}

// validateRegistration validates a repository registration and its associated paths, metadata, users, polling settings, and optional cmux socket.
func validateRegistration(prefix string, repository RepositoryRegistration) error {
	if strings.TrimSpace(repository.Path) == "" {
		return validation(prefix+".path", "is required")
	}
	if !filepath.IsAbs(repository.Path) {
		return validation(prefix+".path", "must be absolute")
	}
	if strings.TrimSpace(repository.GitHub.Owner) == "" {
		return validation(prefix+".github.owner", "is required")
	}
	if strings.TrimSpace(repository.GitHub.Repository) == "" {
		return validation(prefix+".github.repository", "is required")
	}
	if err := validateUniqueStrings(prefix+".authorized_users", repository.AuthorizedUsers); err != nil {
		return err
	}
	if err := validateDuration(prefix+".polling.interval", repository.Polling.Interval, false); err != nil {
		return err
	}
	if err := validateDuration(prefix+".polling.backoff", repository.Polling.Backoff, false); err != nil {
		return err
	}
	if strings.TrimSpace(repository.OperationalDataPath) == "" {
		return validation(prefix+".operational_data_path", "is required")
	}
	if !filepath.IsAbs(repository.OperationalDataPath) {
		return validation(prefix+".operational_data_path", "must be absolute")
	}
	if repository.Cmux.SocketPath != "" && !filepath.IsAbs(repository.Cmux.SocketPath) {
		return validation(prefix+".cmux.socket_path", "must be absolute when set")
	}
	for field, path := range map[string]string{
		prefix + ".authentication.codex_auth_path":  repository.Authentication.CodexAuthPath,
		prefix + ".authentication.claude_auth_path": repository.Authentication.ClaudeAuthPath,
	} {
		if path == "" {
			continue
		}
		if strings.ContainsAny(path, "\x00\r\n") {
			return validation(field, "must not contain control characters")
		}
		if !filepath.IsAbs(path) {
			return validation(field, "must be absolute when set")
		}
	}
	if strings.TrimSpace(repository.RepositoryConfigPath) == "" {
		return validation(prefix+".repository_config_path", "is required")
	}
	if !filepath.IsAbs(repository.RepositoryConfigPath) {
		return validation(prefix+".repository_config_path", "must be absolute")
	}
	return nil
}

// validateSchema validates a configuration schema version and reports an error for missing,
// non-positive, or unsupported versions.
func validateSchema(kind string, version int) error {
	if version == 0 {
		return validation("schema_version", "is required")
	}
	if version > CurrentSchemaVersion {
		return &UnknownSchemaVersionError{Kind: kind, Field: "schema_version", Version: version, Supported: CurrentSchemaVersion}
	}
	if version < 1 {
		return validation("schema_version", "must be positive")
	}
	return nil
}

// validateDuration validates that a duration value is positive, or empty when allowEmpty is true.
func validateDuration(field, value string, allowEmpty bool) error {
	if strings.TrimSpace(value) == "" {
		if allowEmpty {
			return nil
		}
		return validation(field, "is required")
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return validation(field, "must be a positive duration such as 30s or 5m")
	}
	return nil
}

// validateUniqueStrings requires at least one nonempty, unique string in values.
func validateUniqueStrings(field string, values []string) error {
	if len(values) == 0 {
		return validation(field, "must contain at least one value")
	}
	return validateOptionalUniqueStrings(field, values)
}

// validateOptionalUniqueStrings validates that each value is nonempty and unique; an empty slice is valid.
func validateOptionalUniqueStrings(field string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		if strings.TrimSpace(value) == "" {
			return validation(fmt.Sprintf("%s[%d]", field, index), "must not be empty")
		}
		if _, exists := seen[value]; exists {
			return validation(fmt.Sprintf("%s[%d]", field, index), "must be unique")
		}
		seen[value] = struct{}{}
	}
	return nil
}

// validateOptionalOverrides validates allowed override names when provided, including their supported values and uniqueness.
func validateOptionalOverrides(values []OverrideName) error {
	seen := make(map[OverrideName]struct{}, len(values))
	for index, value := range values {
		if value != OverrideModel && value != OverrideReasoningEffort && value != OverrideHarness {
			return validation(fmt.Sprintf("allowed_overrides[%d]", index), "must be model, reasoning_effort, or harness")
		}
		if _, exists := seen[value]; exists {
			return validation(fmt.Sprintf("allowed_overrides[%d]", index), "must be unique")
		}
		seen[value] = struct{}{}
	}
	return nil
}

// validation creates an error describing an invalid configuration field and the reason it is invalid.
func validation(field, message string) error {
	return &ValidationError{Field: field, Message: message}
}

// policy creates a typed repository-policy rejection.
func policy(field, message string) error {
	return &PolicyError{Field: field, Message: message}
}

// rejectFactoryOwnedWorkflowFields rejects YAML fields that would let a
// repository redefine coordinator-owned roles, prompts, stages, or edges.
func rejectFactoryOwnedWorkflowFields(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return &ConfigFileError{Path: path, Err: err}
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return &ConfigFileError{Path: path, Err: err}
	}
	if len(document.Content) == 0 {
		return nil
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(root.Content); index += 2 {
		field := root.Content[index].Value
		if _, forbidden := factoryOwnedWorkflowFields[field]; forbidden {
			return policy(field, "workflow declarations are factory-owned and cannot be supplied by repository configuration")
		}
	}
	return nil
}

// factoryOwnedWorkflowFields names repository YAML fields that are never
// accepted as alternate sources of coordinator authority.
var factoryOwnedWorkflowFields = map[string]struct{}{
	"role":              {},
	"craft":             {},
	"role_registry":     {},
	"roles":             {},
	"stage":             {},
	"stage_registry":    {},
	"stages":            {},
	"prompt":            {},
	"prompt_registry":   {},
	"prompts":           {},
	"transition":        {},
	"transitions":       {},
	"workflow":          {},
	"workflow_registry": {},
}

// loadYAML reads a single YAML document from path into destination and rejects unknown fields or additional documents. Errors are wrapped in ConfigFileError.
func loadYAML(path string, destination any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return &ConfigFileError{Path: path, Err: err}
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(destination); err != nil {
		return &ConfigFileError{Path: path, Err: err}
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return &ConfigFileError{Path: path, Err: errors.New("configuration contains more than one YAML document")}
		}
		return &ConfigFileError{Path: path, Err: err}
	}
	return nil
}

// validSHA256Digest reports whether value is a lowercase hexadecimal SHA-256 digest with the "sha256:" prefix.
func validSHA256Digest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
