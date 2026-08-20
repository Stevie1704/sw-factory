package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
	Path                 string        `yaml:"path"`
	GitHub               GitHubConfig  `yaml:"github"`
	AuthorizedUsers      []string      `yaml:"authorized_users"`
	Polling              PollingConfig `yaml:"polling"`
	Cmux                 CmuxConfig    `yaml:"cmux"`
	OperationalDataPath  string        `yaml:"operational_data_path"`
	RepositoryConfigPath string        `yaml:"repository_config_path"`
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

type RepositoryConfig struct {
	SchemaVersion       int                 `yaml:"schema_version"`
	TargetBranch        string              `yaml:"target_branch"`
	Setup               string              `yaml:"setup"`
	Gates               []GateConfig        `yaml:"gates"`
	RoleHarnessDefaults map[string]string   `yaml:"role_harness_defaults"`
	ModelOptions        map[string][]string `yaml:"model_options"`
	Timeouts            TimeoutConfig       `yaml:"timeouts"`
	RetryLimits         RetryLimits         `yaml:"retry_limits"`
	TestPolicy          TestPolicy          `yaml:"test_policy"`
	AllowedOverrides    []string            `yaml:"allowed_overrides"`
	Caches              []CacheConfig       `yaml:"caches"`
	WorkerBuild         WorkerBuildConfig   `yaml:"worker_build"`
	BaseSynchronization BaseSynchronization `yaml:"base_synchronization"`
}

type GateConfig struct {
	Name              string   `yaml:"name"`
	Command           string   `yaml:"command"`
	Timeout           string   `yaml:"timeout"`
	Blocking          bool     `yaml:"blocking"`
	DependsOn         []string `yaml:"depends_on"`
	EnvironmentPolicy string   `yaml:"environment_policy"`
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

type TestPolicy struct {
	Mode                    string `yaml:"mode"`
	AllowHumanExemption     bool   `yaml:"allow_human_exemption"`
	AllowTechnicalExemption bool   `yaml:"allow_technical_exemption"`
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

type BaseSynchronization struct {
	Mode   string `yaml:"mode"`
	Branch string `yaml:"branch"`
}

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("configuration invalid: %s: %s", e.Field, e.Message)
}

type UnknownSchemaVersionError struct {
	Kind      string
	Version   int
	Supported int
}

func (e *UnknownSchemaVersionError) Error() string {
	return fmt.Sprintf("%s configuration schema version %d is newer than supported version %d", e.Kind, e.Version, e.Supported)
}

type ConfigFileError struct {
	Path string
	Err  error
}

func (e *ConfigFileError) Error() string {
	return fmt.Sprintf("read configuration %q: %v", e.Path, e.Err)
}

func (e *ConfigFileError) Unwrap() error { return e.Err }

func NewHostConfig() HostConfig {
	return HostConfig{SchemaVersion: CurrentSchemaVersion, Repositories: []RepositoryRegistration{}}
}

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

func DefaultOperationalDataPath(hostConfigPath string) string {
	return filepath.Join(filepath.Dir(hostConfigPath), "data", "factory.db")
}

func DefaultRepositoryConfigPath(repositoryPath string) string {
	return filepath.Join(repositoryPath, RepositoryConfigFileName)
}

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

func LoadRepository(path string) (RepositoryConfig, error) {
	var config RepositoryConfig
	if err := loadYAML(path, &config); err != nil {
		return RepositoryConfig{}, err
	}
	if err := ValidateRepository(config); err != nil {
		return RepositoryConfig{}, err
	}
	return config, nil
}

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
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("protect temporary configuration: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write configuration: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
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
		if gate.EnvironmentPolicy != "clean" && gate.EnvironmentPolicy != "role" {
			return validation(prefix+".environment_policy", "must be clean or role")
		}
		for dependencyIndex, dependency := range gate.DependsOn {
			if _, exists := seenGates[dependency]; !exists {
				return validation(fmt.Sprintf("%s.depends_on[%d]", prefix, dependencyIndex), "must reference an earlier gate")
			}
		}
	}
	if len(config.RoleHarnessDefaults) == 0 {
		return validation("role_harness_defaults", "must declare at least one role")
	}
	for role, harness := range config.RoleHarnessDefaults {
		if strings.TrimSpace(role) == "" {
			return validation("role_harness_defaults", "role names must not be empty")
		}
		if harness != "codex" && harness != "claude" {
			return validation("role_harness_defaults."+role, "must be codex or claude")
		}
	}
	if len(config.ModelOptions) == 0 {
		return validation("model_options", "must declare at least one role")
	}
	for role, models := range config.ModelOptions {
		if len(models) == 0 {
			return validation("model_options."+role, "must declare at least one model")
		}
		for index, model := range models {
			if strings.TrimSpace(model) == "" {
				return validation(fmt.Sprintf("model_options.%s[%d]", role, index), "must not be empty")
			}
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
	if config.TestPolicy.Mode != "required" && config.TestPolicy.Mode != "advisory" && config.TestPolicy.Mode != "disabled" {
		return validation("test_policy.mode", "must be required, advisory, or disabled")
	}
	if err := validateUniqueStrings("allowed_overrides", config.AllowedOverrides); err != nil {
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
	if config.BaseSynchronization.Mode != "never" && config.BaseSynchronization.Mode != "before_ready" {
		return validation("base_synchronization.mode", "must be never or before_ready")
	}
	if config.BaseSynchronization.Mode == "before_ready" && strings.TrimSpace(config.BaseSynchronization.Branch) == "" {
		return validation("base_synchronization.branch", "is required when synchronization is enabled")
	}
	return nil
}

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
	if strings.TrimSpace(repository.RepositoryConfigPath) == "" {
		return validation(prefix+".repository_config_path", "is required")
	}
	if !filepath.IsAbs(repository.RepositoryConfigPath) {
		return validation(prefix+".repository_config_path", "must be absolute")
	}
	return nil
}

func validateSchema(kind string, version int) error {
	if version == 0 {
		return validation("schema_version", "is required")
	}
	if version > CurrentSchemaVersion {
		return &UnknownSchemaVersionError{Kind: kind, Version: version, Supported: CurrentSchemaVersion}
	}
	if version < 1 {
		return validation("schema_version", "must be positive")
	}
	return nil
}

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

func validateUniqueStrings(field string, values []string) error {
	if len(values) == 0 {
		return validation(field, "must contain at least one value")
	}
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

func validation(field, message string) error {
	return &ValidationError{Field: field, Message: message}
}

func loadYAML(path string, destination any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return &ConfigFileError{Path: path, Err: err}
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
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

func SortedRoles(values map[string]string) []string {
	roles := make([]string, 0, len(values))
	for role := range values {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles
}
