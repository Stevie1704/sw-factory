package factory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/store"
)

type Factory interface {
	Init(context.Context) (InitResult, error)
	Register(context.Context, RegisterRequest) (RegisterResult, error)
	Status(context.Context) (StatusResult, error)
}

type ConfigRepository interface {
	Load(string) (config.HostConfig, error)
	Save(string, config.HostConfig) error
	Create(string) (config.HostConfig, error)
}

type OperationalStore interface {
	CurrentRun(context.Context) (*store.Run, error)
	Close() error
}

type StoreOpener func(context.Context, string) (OperationalStore, error)
type RepositoryChecker func(string) error

type Dependencies struct {
	Config          ConfigRepository
	OpenStore       StoreOpener
	CheckRepository RepositoryChecker
}

type Service struct {
	configPath string
	deps       Dependencies
}

type InitResult struct {
	ConfigPath string
}

type RegisterRequest struct {
	RepositoryPath       string
	GitHubOwner          string
	GitHubRepository     string
	AuthorizedUsers      []string
	OperationalDataPath  string
	PollingInterval      string
	PollingBackoff       string
	CmuxSocketPath       string
	CmuxControlWorkspace string
	RepositoryConfigPath string
}

type RegisterResult struct {
	RepositoryPath      string
	OperationalDataPath string
}

type StatusResult struct {
	ConfigPath     string
	RepositoryPath string
	ActiveRun      *store.Run
}

type fileConfigRepository struct{}

func (fileConfigRepository) Load(path string) (config.HostConfig, error) {
	return config.LoadHost(path)
}

func (fileConfigRepository) Save(path string, value config.HostConfig) error {
	return config.SaveHost(path, value)
}

func (fileConfigRepository) Create(path string) (config.HostConfig, error) {
	return config.CreateHost(path)
}

// New creates a Service configured with the specified host configuration path and default dependencies.
func New(configPath string) *Service {
	return NewWithDependencies(configPath, Dependencies{
		Config: fileConfigRepository{},
		OpenStore: func(ctx context.Context, path string) (OperationalStore, error) {
			return store.Open(ctx, path)
		},
		CheckRepository: checkRepository,
	})
}

// NewWithDependencies creates a Service with the specified configuration path and dependencies.
// Missing dependencies are replaced with their default implementations.
func NewWithDependencies(configPath string, dependencies Dependencies) *Service {
	if dependencies.Config == nil {
		dependencies.Config = fileConfigRepository{}
	}
	if dependencies.OpenStore == nil {
		dependencies.OpenStore = func(ctx context.Context, path string) (OperationalStore, error) {
			return store.Open(ctx, path)
		}
	}
	if dependencies.CheckRepository == nil {
		dependencies.CheckRepository = checkRepository
	}
	return &Service{configPath: configPath, deps: dependencies}
}

func (s *Service) Init(_ context.Context) (InitResult, error) {
	if s.configPath == "" {
		return InitResult{}, errors.New("host configuration path is required")
	}
	if _, err := s.deps.Config.Create(s.configPath); err != nil {
		return InitResult{}, err
	}
	return InitResult{ConfigPath: s.configPath}, nil
}

func (s *Service) Register(ctx context.Context, request RegisterRequest) (RegisterResult, error) {
	if s.configPath == "" {
		return RegisterResult{}, errors.New("host configuration path is required")
	}
	host, err := s.deps.Config.Load(s.configPath)
	if err != nil {
		return RegisterResult{}, err
	}
	if len(host.Repositories) != 0 {
		return RegisterResult{}, errors.New("version one already has a registered repository")
	}
	if err := s.deps.CheckRepository(request.RepositoryPath); err != nil {
		return RegisterResult{}, err
	}
	repositoryPath, err := filepath.Abs(request.RepositoryPath)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("resolve repository path: %w", err)
	}
	operationalPath := request.OperationalDataPath
	if operationalPath == "" {
		operationalPath = config.DefaultOperationalDataPath(s.configPath)
	}
	operationalPath, err = filepath.Abs(operationalPath)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("resolve operational data path: %w", err)
	}
	repositoryConfigPath := request.RepositoryConfigPath
	if repositoryConfigPath == "" {
		repositoryConfigPath = config.DefaultRepositoryConfigPath(repositoryPath)
	}
	repositoryConfigPath, err = filepath.Abs(repositoryConfigPath)
	if err != nil {
		return RegisterResult{}, fmt.Errorf("resolve repository configuration path: %w", err)
	}
	if err := validateOperationalPath(repositoryPath, operationalPath); err != nil {
		return RegisterResult{}, err
	}
	if !pathWithin(repositoryPath, repositoryConfigPath) {
		return RegisterResult{}, &config.ValidationError{Field: "repositories[0].repository_config_path", Message: "must be inside the registered repository checkout"}
	}
	if _, err := config.LoadRepository(repositoryConfigPath); err != nil {
		return RegisterResult{}, err
	}
	registration := config.RepositoryRegistration{
		Path:                 repositoryPath,
		GitHub:               config.GitHubConfig{Owner: strings.TrimSpace(request.GitHubOwner), Repository: strings.TrimSpace(request.GitHubRepository)},
		AuthorizedUsers:      request.AuthorizedUsers,
		Polling:              config.PollingConfig{Interval: defaultString(request.PollingInterval, "30s"), Backoff: defaultString(request.PollingBackoff, "5m")},
		Cmux:                 config.CmuxConfig{SocketPath: request.CmuxSocketPath, ControlWorkspace: request.CmuxControlWorkspace},
		OperationalDataPath:  operationalPath,
		RepositoryConfigPath: repositoryConfigPath,
	}
	host.Repositories = []config.RepositoryRegistration{registration}
	if err := config.ValidateHost(host); err != nil {
		return RegisterResult{}, err
	}
	opened, err := s.deps.OpenStore(ctx, operationalPath)
	if err != nil {
		return RegisterResult{}, err
	}
	if err := opened.Close(); err != nil {
		return RegisterResult{}, fmt.Errorf("close operational store after initialization: %w", err)
	}
	if err := s.deps.Config.Save(s.configPath, host); err != nil {
		return RegisterResult{}, fmt.Errorf("save host configuration after creating operational store %q: %w", operationalPath, err)
	}
	return RegisterResult{RepositoryPath: repositoryPath, OperationalDataPath: operationalPath}, nil
}

func (s *Service) Status(ctx context.Context) (StatusResult, error) {
	if s.configPath == "" {
		return StatusResult{}, errors.New("host configuration path is required")
	}
	host, err := s.deps.Config.Load(s.configPath)
	if err != nil {
		return StatusResult{}, err
	}
	result := StatusResult{ConfigPath: s.configPath}
	if len(host.Repositories) == 0 {
		return result, nil
	}
	registration := host.Repositories[0]
	result.RepositoryPath = registration.Path
	if err := validateOperationalPath(registration.Path, registration.OperationalDataPath); err != nil {
		return StatusResult{}, err
	}
	opened, err := s.deps.OpenStore(ctx, registration.OperationalDataPath)
	if err != nil {
		return StatusResult{}, err
	}
	defer func() { _ = opened.Close() }()
	result.ActiveRun, err = opened.CurrentRun(ctx)
	if err != nil {
		return StatusResult{}, err
	}
	return result, nil
}

// checkRepository validates that path is an absolute path to an existing directory.
func checkRepository(path string) error {
	if strings.TrimSpace(path) == "" {
		return &config.ValidationError{Field: "repositories[0].path", Message: "is required"}
	}
	if !filepath.IsAbs(path) {
		return &config.ValidationError{Field: "repositories[0].path", Message: "must be absolute"}
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect repository path %q: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("repository path %q is not a directory", path)
	}
	return nil
}

// defaultString returns fallback when value is blank; otherwise, it returns value.
func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// pathWithin reports whether target is equal to or contained within root after resolving both paths.
func pathWithin(root, target string) bool {
	relative, err := filepath.Rel(resolvePath(root), resolvePath(target))
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

// validateOperationalPath verifies that the operational data path is outside the registered repository checkout.
func validateOperationalPath(repositoryPath, operationalPath string) error {
	if pathWithin(repositoryPath, operationalPath) {
		return &config.ValidationError{Field: "repositories[0].operational_data_path", Message: "must be outside the registered repository checkout"}
	}
	return nil
}

// resolvePath returns an absolute, cleaned path with symlinks resolved where possible.
// It preserves unresolved trailing components and falls back to the absolute path when
// resolution fails.
func resolvePath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	suffix := []string{}
	current := abs
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return abs
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}
