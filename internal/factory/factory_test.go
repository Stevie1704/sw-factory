package factory_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestFactoryRegisterRejectsASecondRegistrationInVersionOne(t *testing.T) {
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
	if err == nil || !strings.Contains(err.Error(), "already has a registered repository") {
		t.Fatalf("second Register() error = %v, want an already-registered error", err)
	}
}

func TestFactoryRegisterFailsWhenTheRepositoryPathDoesNotExist(t *testing.T) {
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
	host, loadErr := config.LoadHost(configPath)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(host.Repositories) != 0 {
		t.Fatalf("host repositories = %#v, want none after a failed registration", host.Repositories)
	}
}

func TestFactoryRegisterAppliesDefaultPollingAndRepositoryConfigPathWhenOmitted(t *testing.T) {
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
	if len(host.Repositories) != 1 {
		t.Fatalf("host repositories = %#v, want one registration", host.Repositories)
	}
	registration := host.Repositories[0]
	if registration.Polling.Interval != "30s" || registration.Polling.Backoff != "5m" {
		t.Fatalf("Polling = %#v, want default 30s/5m", registration.Polling)
	}
	wantRepositoryConfigPath := filepath.Join(repositoryPath, config.RepositoryConfigFileName)
	if registration.RepositoryConfigPath != wantRepositoryConfigPath {
		t.Fatalf("RepositoryConfigPath = %q, want %q", registration.RepositoryConfigPath, wantRepositoryConfigPath)
	}
}

func TestFactoryStatusReturnsAnEmptyResultWhenNoRepositoryIsRegistered(t *testing.T) {
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

func TestFactoryRegisterPropagatesAConfigLoadFailure(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom: could not load host configuration")
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config: &failingConfigRepository{loadErr: wantErr},
		OpenStore: func(ctx context.Context, path string) (factory.OperationalStore, error) {
			t.Fatal("OpenStore should not be called when the configuration cannot be loaded")
			return nil, nil
		},
		CheckRepository: func(string) error {
			t.Fatal("CheckRepository should not be called when the configuration cannot be loaded")
			return nil
		},
	})

	_, err := service.Register(context.Background(), factory.RegisterRequest{
		RepositoryPath:      "/work/repository",
		GitHubOwner:         "example",
		GitHubRepository:    "project",
		AuthorizedUsers:     []string{"alice"},
		OperationalDataPath: "/work/state/factory.db",
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Register() error = %v, want %v", err, wantErr)
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

type fakeConfigRepository struct {
	value config.HostConfig
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

type failingConfigRepository struct {
	loadErr error
}

func (f *failingConfigRepository) Load(string) (config.HostConfig, error) {
	return config.HostConfig{}, f.loadErr
}

func (f *failingConfigRepository) Save(string, config.HostConfig) error {
	return errors.New("Save should not be called")
}

func (f *failingConfigRepository) Create(string) (config.HostConfig, error) {
	return config.HostConfig{}, errors.New("Create should not be called")
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
