package factory_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/store"
)

var _ factory.Factory = (*factory.Service)(nil)

func TestFactoryInitializesRegistersAndReportsAnEmptyRunStore(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "host", "config.yaml")
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRepositoryConfig(t, repositoryPath)
	operationalPath := filepath.Join(root, "state", "factory.db")
	service := factory.New(configPath)

	initialized, err := service.Init(context.Background())
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if initialized.ConfigPath != configPath {
		t.Fatalf("ConfigPath = %q, want %q", initialized.ConfigPath, configPath)
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config file was not created: %v", err)
	}

	registered, err := service.Register(context.Background(), factory.RegisterRequest{
		RepositoryPath:       repositoryPath,
		GitHubOwner:          "example",
		GitHubRepository:     "project",
		AuthorizedUsers:      []string{"alice"},
		OperationalDataPath:  operationalPath,
		PollingInterval:      "30s",
		PollingBackoff:       "5m",
		CmuxControlWorkspace: "factory-control",
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if registered.OperationalDataPath != operationalPath {
		t.Fatalf("OperationalDataPath = %q, want %q", registered.OperationalDataPath, operationalPath)
	}

	status, err := service.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.RepositoryPath != repositoryPath {
		t.Fatalf("RepositoryPath = %q, want %q", status.RepositoryPath, repositoryPath)
	}
	if status.ActiveRun != nil {
		t.Fatalf("ActiveRun = %#v, want empty store", status.ActiveRun)
	}
}

func TestFactoryRefusesAnInvalidRegistrationBeforePersistingIt(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	service := factory.New(configPath)
	if _, err := service.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, err := service.Register(context.Background(), factory.RegisterRequest{
		RepositoryPath:      "relative/repository",
		GitHubOwner:         "example",
		GitHubRepository:    "project",
		AuthorizedUsers:     []string{"alice"},
		OperationalDataPath: filepath.Join(t.TempDir(), "factory.db"),
		PollingInterval:     "30s",
		PollingBackoff:      "5m",
	})
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %v, want ValidationError", err)
	}
	if validationErr.Field != "repositories[0].path" {
		t.Fatalf("field = %q, want repositories[0].path", validationErr.Field)
	}
}

func TestFactoryRefusesAnOperationalStoreInsideTheRepositoryCheckout(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "host", "config.yaml")
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRepositoryConfig(t, repositoryPath)
	service := factory.New(configPath)
	if _, err := service.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, err := service.Register(context.Background(), factory.RegisterRequest{
		RepositoryPath:      repositoryPath,
		GitHubOwner:         "example",
		GitHubRepository:    "project",
		AuthorizedUsers:     []string{"alice"},
		OperationalDataPath: filepath.Join(repositoryPath, ".factory", "factory.db"),
	})
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %v, want ValidationError", err)
	}
	if validationErr.Field != "repositories[0].operational_data_path" {
		t.Fatalf("field = %q, want repositories[0].operational_data_path", validationErr.Field)
	}
}

func TestFactoryRefusesASymlinkedOperationalStoreInsideTheRepositoryCheckout(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "host", "config.yaml")
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRepositoryConfig(t, repositoryPath)
	aliasPath := filepath.Join(root, "repository-alias")
	if err := os.Symlink(repositoryPath, aliasPath); err != nil {
		t.Fatal(err)
	}
	service := factory.New(configPath)
	if _, err := service.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, err := service.Register(context.Background(), factory.RegisterRequest{
		RepositoryPath:      repositoryPath,
		GitHubOwner:         "example",
		GitHubRepository:    "project",
		AuthorizedUsers:     []string{"alice"},
		OperationalDataPath: filepath.Join(aliasPath, "factory.db"),
	})
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %v, want ValidationError", err)
	}
	if validationErr.Field != "repositories[0].operational_data_path" {
		t.Fatalf("field = %q, want repositories[0].operational_data_path", validationErr.Field)
	}
}

func TestFactoryStatusRefusesAHandEditedOperationalStoreInsideTheRepositoryCheckout(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "host", "config.yaml")
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	registration := config.RepositoryRegistration{
		Path:                 repositoryPath,
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		AuthorizedUsers:      []string{"alice"},
		Polling:              config.PollingConfig{Interval: "30s", Backoff: "5m"},
		OperationalDataPath:  filepath.Join(repositoryPath, "factory.db"),
		RepositoryConfigPath: filepath.Join(repositoryPath, config.RepositoryConfigFileName),
	}
	if err := config.SaveHost(configPath, config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{registration}}); err != nil {
		t.Fatal(err)
	}

	_, err := factory.New(configPath).Status(context.Background())
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %v, want ValidationError", err)
	}
	if validationErr.Field != "repositories[0].operational_data_path" {
		t.Fatalf("field = %q, want repositories[0].operational_data_path", validationErr.Field)
	}
}

func TestFactoryRefusesAnInvalidRepositoryConfigurationBeforePersistingRegistration(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "host", "config.yaml")
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryPath, config.RepositoryConfigFileName), []byte("schema_version: 99\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := factory.New(configPath)
	if _, err := service.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	operationalPath := filepath.Join(root, "state", "factory.db")
	_, err := service.Register(context.Background(), factory.RegisterRequest{
		RepositoryPath:      repositoryPath,
		GitHubOwner:         "example",
		GitHubRepository:    "project",
		AuthorizedUsers:     []string{"alice"},
		OperationalDataPath: operationalPath,
	})
	var schemaErr *config.UnknownSchemaVersionError
	if !errors.As(err, &schemaErr) {
		t.Fatalf("error = %v, want UnknownSchemaVersionError", err)
	}
	if _, err := os.Stat(operationalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("operational store exists after invalid registration: %v", err)
	}
	host, err := config.LoadHost(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(host.Repositories) != 0 {
		t.Fatalf("host repositories = %#v, want none", host.Repositories)
	}
}

func TestFactoryStatusUsesTheHighLevelSeamWithARealSQLiteStoreAndFakeConfigAdapter(t *testing.T) {
	t.Parallel()

	operationalPath := filepath.Join(t.TempDir(), "data", "factory.db")
	fake := &fakeConfigRepository{value: config.HostConfig{
		SchemaVersion: 1,
		Repositories: []config.RepositoryRegistration{{
			Path:                 "/work/repository",
			GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
			AuthorizedUsers:      []string{"alice"},
			Polling:              config.PollingConfig{Interval: "30s", Backoff: "5m"},
			OperationalDataPath:  operationalPath,
			RepositoryConfigPath: "/work/repository/factory.yaml",
		}},
	}}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config: fake,
		OpenStore: func(ctx context.Context, path string) (factory.OperationalStore, error) {
			return store.Open(ctx, path)
		},
		CheckRepository: func(string) error { return nil },
	})

	status, err := service.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.ActiveRun != nil {
		t.Fatalf("ActiveRun = %#v, want empty SQLite store", status.ActiveRun)
	}
	if status.RepositoryPath != "/work/repository" {
		t.Fatalf("RepositoryPath = %q", status.RepositoryPath)
	}
}

// TestFactoryStatusReportsTheLatestTerminalRun verifies status retains the
// branch and worktree after a run reaches a terminal state.
func TestFactoryStatusReportsTheLatestTerminalRun(t *testing.T) {
	t.Parallel()

	operationalPath := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(context.Background(), operationalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.SaveRun(context.Background(), store.Run{
		ID:             "run-complete",
		RepositoryPath: "/work/repository",
		IssueNumber:    42,
		Stage:          store.StageReady,
		Status:         store.StatusComplete,
		Branch:         "factory/run-complete",
		Worktree:       "/worktrees/run-complete",
		CreatedAt:      time.Unix(100, 0).UTC(),
		UpdatedAt:      time.Unix(200, 0).UTC(),
	}); err != nil {
		_ = opened.Close()
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config: &fakeConfigRepository{value: config.HostConfig{
			SchemaVersion: 1,
			Repositories: []config.RepositoryRegistration{{
				Path:                "/work/repository",
				OperationalDataPath: operationalPath,
			}},
		}},
		OpenStore: func(ctx context.Context, path string) (factory.OperationalStore, error) {
			return store.Open(ctx, path)
		},
	})
	status, err := service.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.ActiveRun == nil || status.ActiveRun.Status != store.StatusComplete || status.ActiveRun.Branch != "factory/run-complete" || status.ActiveRun.Worktree != "/worktrees/run-complete" {
		t.Fatalf("status.ActiveRun = %#v, want latest terminal run details", status.ActiveRun)
	}
}

func TestServiceInitFailsWhenTheConfigurationAlreadyExists(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	service := factory.New(configPath)
	if _, err := service.Init(context.Background()); err != nil {
		t.Fatalf("first Init() error = %v", err)
	}

	if _, err := service.Init(context.Background()); err == nil {
		t.Fatal("second Init() succeeded, want an error because the configuration already exists")
	}
}

func TestServiceRegisterFailsWhenARepositoryIsAlreadyRegistered(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "host", "config.yaml")
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRepositoryConfig(t, repositoryPath)
	service := factory.New(configPath)
	if _, err := service.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	request := factory.RegisterRequest{
		RepositoryPath:      repositoryPath,
		GitHubOwner:         "example",
		GitHubRepository:    "project",
		AuthorizedUsers:     []string{"alice"},
		OperationalDataPath: filepath.Join(root, "state", "factory.db"),
	}
	if _, err := service.Register(context.Background(), request); err != nil {
		t.Fatalf("first Register() error = %v", err)
	}

	_, err := service.Register(context.Background(), request)
	if err == nil {
		t.Fatal("second Register() succeeded, want an error because a repository is already registered")
	}
}

func TestServiceRegisterUsesDefaultOperationalAndRepositoryConfigPathsWhenNotProvided(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "host", "config.yaml")
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRepositoryConfig(t, repositoryPath)
	service := factory.New(configPath)
	if _, err := service.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	result, err := service.Register(context.Background(), factory.RegisterRequest{
		RepositoryPath:   repositoryPath,
		GitHubOwner:      "example",
		GitHubRepository: "project",
		AuthorizedUsers:  []string{"alice"},
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	wantOperationalPath := config.DefaultOperationalDataPath(configPath)
	if result.OperationalDataPath != wantOperationalPath {
		t.Fatalf("OperationalDataPath = %q, want %q", result.OperationalDataPath, wantOperationalPath)
	}
	host, err := config.LoadHost(configPath)
	if err != nil {
		t.Fatal(err)
	}
	wantRepositoryConfigPath := config.DefaultRepositoryConfigPath(repositoryPath)
	if host.Repositories[0].RepositoryConfigPath != wantRepositoryConfigPath {
		t.Fatalf("RepositoryConfigPath = %q, want %q", host.Repositories[0].RepositoryConfigPath, wantRepositoryConfigPath)
	}
}

func TestServiceRegisterAppliesDefaultPollingValuesWhenOmitted(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "host", "config.yaml")
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRepositoryConfig(t, repositoryPath)
	service := factory.New(configPath)
	if _, err := service.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	if _, err := service.Register(context.Background(), factory.RegisterRequest{
		RepositoryPath:      repositoryPath,
		GitHubOwner:         "example",
		GitHubRepository:    "project",
		AuthorizedUsers:     []string{"alice"},
		OperationalDataPath: filepath.Join(root, "state", "factory.db"),
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	host, err := config.LoadHost(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if host.Repositories[0].Polling.Interval != "30s" || host.Repositories[0].Polling.Backoff != "5m" {
		t.Fatalf("Polling = %#v, want defaults of 30s/5m", host.Repositories[0].Polling)
	}
}

func TestServiceRegisterRejectsARepositoryConfigPathOutsideTheRepositoryCheckout(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "host", "config.yaml")
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRepositoryConfig(t, repositoryPath)
	outsidePath := filepath.Join(root, "outside", "factory.yaml")
	if err := os.MkdirAll(filepath.Dir(outsidePath), 0o755); err != nil {
		t.Fatal(err)
	}
	service := factory.New(configPath)
	if _, err := service.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, err := service.Register(context.Background(), factory.RegisterRequest{
		RepositoryPath:       repositoryPath,
		GitHubOwner:          "example",
		GitHubRepository:     "project",
		AuthorizedUsers:      []string{"alice"},
		OperationalDataPath:  filepath.Join(root, "state", "factory.db"),
		RepositoryConfigPath: outsidePath,
	})
	var validationErr *config.ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("error = %v, want ValidationError", err)
	}
	if validationErr.Field != "repositories[0].repository_config_path" {
		t.Fatalf("field = %q, want repositories[0].repository_config_path", validationErr.Field)
	}
}

func TestServiceRegisterRejectsANonexistentRepositoryPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "host", "config.yaml")
	service := factory.New(configPath)
	if _, err := service.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, err := service.Register(context.Background(), factory.RegisterRequest{
		RepositoryPath:      filepath.Join(root, "does-not-exist"),
		GitHubOwner:         "example",
		GitHubRepository:    "project",
		AuthorizedUsers:     []string{"alice"},
		OperationalDataPath: filepath.Join(root, "state", "factory.db"),
	})
	if err == nil {
		t.Fatal("Register() succeeded for a nonexistent repository path")
	}
}

func TestServiceRegisterRejectsARepositoryPathThatIsAFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "host", "config.yaml")
	filePath := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := factory.New(configPath)
	if _, err := service.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, err := service.Register(context.Background(), factory.RegisterRequest{
		RepositoryPath:      filePath,
		GitHubOwner:         "example",
		GitHubRepository:    "project",
		AuthorizedUsers:     []string{"alice"},
		OperationalDataPath: filepath.Join(root, "state", "factory.db"),
	})
	if err == nil {
		t.Fatal("Register() succeeded for a repository path that is a file")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("error = %v, want a not-a-directory diagnostic", err)
	}
}

func TestServiceRegisterDoesNotPersistWhenTheOperationalStoreFailsToOpen(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "host", "config.yaml")
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRepositoryConfig(t, repositoryPath)
	service := factory.NewWithDependencies(configPath, factory.Dependencies{
		OpenStore: func(context.Context, string) (factory.OperationalStore, error) {
			return nil, errors.New("open store boom")
		},
	})
	if _, err := service.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, err := service.Register(context.Background(), factory.RegisterRequest{
		RepositoryPath:      repositoryPath,
		GitHubOwner:         "example",
		GitHubRepository:    "project",
		AuthorizedUsers:     []string{"alice"},
		OperationalDataPath: filepath.Join(root, "state", "factory.db"),
	})
	if err == nil || !strings.Contains(err.Error(), "open store boom") {
		t.Fatalf("error = %v, want it to include the store open failure", err)
	}
	host, loadErr := config.LoadHost(configPath)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(host.Repositories) != 0 {
		t.Fatalf("host repositories = %#v, want none persisted after a store open failure", host.Repositories)
	}
}

func TestServiceRegisterReturnsAnErrorWhenTheOperationalStoreFailsToClose(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "host", "config.yaml")
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRepositoryConfig(t, repositoryPath)
	service := factory.NewWithDependencies(configPath, factory.Dependencies{
		OpenStore: func(context.Context, string) (factory.OperationalStore, error) {
			return closeFailingStore{}, nil
		},
	})
	if _, err := service.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, err := service.Register(context.Background(), factory.RegisterRequest{
		RepositoryPath:      repositoryPath,
		GitHubOwner:         "example",
		GitHubRepository:    "project",
		AuthorizedUsers:     []string{"alice"},
		OperationalDataPath: filepath.Join(root, "state", "factory.db"),
	})
	if err == nil || !strings.Contains(err.Error(), "close boom") {
		t.Fatalf("error = %v, want it to include the store close failure", err)
	}
	host, loadErr := config.LoadHost(configPath)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(host.Repositories) != 0 {
		t.Fatalf("host repositories = %#v, want none persisted after a store close failure", host.Repositories)
	}
}

func TestServiceRegisterIncludesTheOperationalPathWhenHostSaveFails(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configPath := filepath.Join(root, "host", "config.yaml")
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRepositoryConfig(t, repositoryPath)
	configRepository := &saveFailingConfigRepository{}
	service := factory.NewWithDependencies(configPath, factory.Dependencies{Config: configRepository})
	if _, err := service.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	operationalPath := filepath.Join(root, "state", "factory.db")
	_, err := service.Register(context.Background(), factory.RegisterRequest{
		RepositoryPath:      repositoryPath,
		GitHubOwner:         "example",
		GitHubRepository:    "project",
		AuthorizedUsers:     []string{"alice"},
		OperationalDataPath: operationalPath,
	})
	if err == nil || !strings.Contains(err.Error(), operationalPath) || !strings.Contains(err.Error(), "save boom") {
		t.Fatalf("error = %v, want the save failure and created operational path", err)
	}
}

func TestServiceStatusReturnsAnEmptyResultWhenNoRepositoryIsRegistered(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	service := factory.New(configPath)
	if _, err := service.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	status, err := service.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.ConfigPath != configPath {
		t.Fatalf("ConfigPath = %q, want %q", status.ConfigPath, configPath)
	}
	if status.RepositoryPath != "" {
		t.Fatalf("RepositoryPath = %q, want empty", status.RepositoryPath)
	}
	if status.ActiveRun != nil {
		t.Fatalf("ActiveRun = %#v, want nil", status.ActiveRun)
	}
}

func TestNewWithDependenciesDefaultsAllMissingDependencies(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	service := factory.NewWithDependencies(configPath, factory.Dependencies{})
	if _, err := service.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config file was not created via default dependencies: %v", err)
	}
}

type closeFailingStore struct{}

func (closeFailingStore) CurrentRun(context.Context) (*store.Run, error) { return nil, nil }

func (closeFailingStore) Close() error { return errors.New("close boom") }

type fakeConfigRepository struct {
	value config.HostConfig
}

type saveFailingConfigRepository struct {
	fakeConfigRepository
}

func (*saveFailingConfigRepository) Save(string, config.HostConfig) error {
	return errors.New("save boom")
}

func (f *fakeConfigRepository) Load(string) (config.HostConfig, error) { return f.value, nil }

func (f *fakeConfigRepository) Save(_ string, value config.HostConfig) error {
	f.value = value
	return nil
}

func (f *fakeConfigRepository) Create(string) (config.HostConfig, error) {
	f.value = config.NewHostConfig()
	return f.value, nil
}

func writeRepositoryConfig(t *testing.T, repositoryPath string) {
	t.Helper()
	contents := `schema_version: 1
target_branch: main
setup: go mod download
gates:
  - name: test
    command: go test ./...
    timeout: 5m
    blocking: true
    environment_policy: clean
role_harness_defaults:
  implementation: codex
model_options:
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
allowed_overrides: [model]
worker_build:
  image: ghcr.io/example/factory-worker
  digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  definition: worker/Dockerfile
base_synchronization:
  mode: never
`
	if err := os.WriteFile(filepath.Join(repositoryPath, config.RepositoryConfigFileName), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
