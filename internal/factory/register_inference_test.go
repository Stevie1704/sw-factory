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
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
)

// stubDiscoverer answers repository discovery with a fixed result and records
// the working directory registration asked about.
type stubDiscoverer struct {
	discovery gitadapter.RepositoryDiscovery
	err       error
	directory string
	callCount int
}

// DiscoverRepository returns the configured discovery answer.
func (s *stubDiscoverer) DiscoverRepository(_ context.Context, workingDirectory string) (gitadapter.RepositoryDiscovery, error) {
	s.callCount++
	s.directory = workingDirectory
	return s.discovery, s.err
}

// stubAccount answers the authenticated GitHub login with a fixed result.
type stubAccount struct {
	login     string
	err       error
	callCount int
}

// AuthenticatedLogin returns the configured account answer.
func (s *stubAccount) AuthenticatedLogin(context.Context) (string, error) {
	s.callCount++
	return s.login, s.err
}

// inferenceFixture is one prepared installation with injected discovery and
// account seams.
type inferenceFixture struct {
	service     *factory.Service
	configPath  string
	repository  string
	discoverer  *stubDiscoverer
	account     *stubAccount
	stateFolder string
}

// newInferenceFixture initializes a host configuration and a checkout whose
// discovery and account answers are controlled by the test.
func newInferenceFixture(t *testing.T) *inferenceFixture {
	t.Helper()

	root := t.TempDir()
	configPath := filepath.Join(root, "host", "config.yaml")
	repositoryPath := filepath.Join(root, "repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRepositoryConfig(t, repositoryPath)
	discoverer := &stubDiscoverer{discovery: gitadapter.RepositoryDiscovery{Root: repositoryPath, Owner: "example", Repository: "project"}}
	account := &stubAccount{login: "alice"}
	service := factory.NewWithDependencies(configPath, factory.Dependencies{
		RepositoryDiscoverer: discoverer,
		GitHubAccount:        account,
	})
	if _, err := service.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &inferenceFixture{
		service:     service,
		configPath:  configPath,
		repository:  repositoryPath,
		discoverer:  discoverer,
		account:     account,
		stateFolder: filepath.Join(root, "state"),
	}
}

// TestRegisterInfersEveryMissingValueFromASubdirectory verifies a bare
// registration resolves the checkout root, GitHub identity, and authorized user.
func TestRegisterInfersEveryMissingValueFromASubdirectory(t *testing.T) {
	t.Parallel()

	fixture := newInferenceFixture(t)
	subdirectory := filepath.Join(fixture.repository, "internal", "cli")
	result, err := fixture.service.Register(context.Background(), factory.RegisterRequest{
		WorkingDirectory:    subdirectory,
		OperationalDataPath: filepath.Join(fixture.stateFolder, "factory.db"),
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if fixture.discoverer.directory != subdirectory {
		t.Fatalf("discovery directory = %q, want %q", fixture.discoverer.directory, subdirectory)
	}
	if result.RepositoryPath != fixture.repository {
		t.Fatalf("RepositoryPath = %q, want %q", result.RepositoryPath, fixture.repository)
	}
	if !filepath.IsAbs(result.RepositoryPath) {
		t.Fatalf("RepositoryPath = %q, want an absolute path", result.RepositoryPath)
	}
	registration := loadRegistration(t, fixture.configPath)
	if registration.GitHub.Owner != "example" || registration.GitHub.Repository != "project" {
		t.Fatalf("registered identity = %q/%q, want example/project", registration.GitHub.Owner, registration.GitHub.Repository)
	}
	if strings.Join(registration.AuthorizedUsers, ",") != "alice" {
		t.Fatalf("registered authorized users = %v, want [alice]", registration.AuthorizedUsers)
	}
	if registration.RepositoryConfigPath != filepath.Join(fixture.repository, "factory.yaml") {
		t.Fatalf("registered repository config = %q, want the checkout's factory.yaml", registration.RepositoryConfigPath)
	}
	want := []string{"repository=" + fixture.repository, "github-owner=example", "github-repository=project", "authorized-user=alice"}
	if strings.Join(inferredPairs(result), ",") != strings.Join(want, ",") {
		t.Fatalf("inferred = %v, want %v", inferredPairs(result), want)
	}
}

// TestRegisterLetsExplicitFlagsOverrideInferenceIndependently verifies each
// explicit registration value replaces exactly one inferred value.
func TestRegisterLetsExplicitFlagsOverrideInferenceIndependently(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mutate   func(*factory.RegisterRequest)
		inferred []string
		verify   func(*testing.T, config.RepositoryRegistration)
	}{
		{
			name:     "explicit owner",
			mutate:   func(request *factory.RegisterRequest) { request.GitHubOwner = "other" },
			inferred: []string{"repository", "github-repository", "authorized-user"},
			verify: func(t *testing.T, registration config.RepositoryRegistration) {
				if registration.GitHub.Owner != "other" || registration.GitHub.Repository != "project" {
					t.Fatalf("registered identity = %q/%q, want other/project", registration.GitHub.Owner, registration.GitHub.Repository)
				}
			},
		},
		{
			name:     "explicit repository name",
			mutate:   func(request *factory.RegisterRequest) { request.GitHubRepository = "fork" },
			inferred: []string{"repository", "github-owner", "authorized-user"},
			verify: func(t *testing.T, registration config.RepositoryRegistration) {
				if registration.GitHub.Owner != "example" || registration.GitHub.Repository != "fork" {
					t.Fatalf("registered identity = %q/%q, want example/fork", registration.GitHub.Owner, registration.GitHub.Repository)
				}
			},
		},
		{
			name:     "explicit authorized users",
			mutate:   func(request *factory.RegisterRequest) { request.AuthorizedUsers = []string{"bob", "carol"} },
			inferred: []string{"repository", "github-owner", "github-repository"},
			verify: func(t *testing.T, registration config.RepositoryRegistration) {
				if strings.Join(registration.AuthorizedUsers, ",") != "bob,carol" {
					t.Fatalf("registered authorized users = %v, want [bob carol]", registration.AuthorizedUsers)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fixture := newInferenceFixture(t)
			request := factory.RegisterRequest{
				WorkingDirectory:    fixture.repository,
				OperationalDataPath: filepath.Join(fixture.stateFolder, "factory.db"),
			}
			tc.mutate(&request)
			result, err := fixture.service.Register(context.Background(), request)
			if err != nil {
				t.Fatalf("Register() error = %v", err)
			}
			if strings.Join(inferredFlags(result), ",") != strings.Join(tc.inferred, ",") {
				t.Fatalf("inferred flags = %v, want %v", inferredFlags(result), tc.inferred)
			}
			tc.verify(t, loadRegistration(t, fixture.configPath))
		})
	}
}

// TestRegisterUsesTheExplicitRepositoryAsTheDiscoveryDirectory verifies an
// explicit checkout keeps its path while the remaining values are inferred
// from that checkout rather than the working directory.
func TestRegisterUsesTheExplicitRepositoryAsTheDiscoveryDirectory(t *testing.T) {
	t.Parallel()

	fixture := newInferenceFixture(t)
	result, err := fixture.service.Register(context.Background(), factory.RegisterRequest{
		WorkingDirectory:    t.TempDir(),
		RepositoryPath:      fixture.repository,
		OperationalDataPath: filepath.Join(fixture.stateFolder, "factory.db"),
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if fixture.discoverer.directory != fixture.repository {
		t.Fatalf("discovery directory = %q, want %q", fixture.discoverer.directory, fixture.repository)
	}
	if result.RepositoryPath != fixture.repository {
		t.Fatalf("RepositoryPath = %q, want %q", result.RepositoryPath, fixture.repository)
	}
}

// TestRegisterSkipsInferenceWhenEveryValueIsExplicit verifies a complete
// registration request never reads Git or GitHub metadata.
func TestRegisterSkipsInferenceWhenEveryValueIsExplicit(t *testing.T) {
	t.Parallel()

	fixture := newInferenceFixture(t)
	result, err := fixture.service.Register(context.Background(), factory.RegisterRequest{
		RepositoryPath:      fixture.repository,
		GitHubOwner:         "example",
		GitHubRepository:    "project",
		AuthorizedUsers:     []string{"alice"},
		OperationalDataPath: filepath.Join(fixture.stateFolder, "factory.db"),
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if fixture.discoverer.callCount != 0 || fixture.account.callCount != 0 {
		t.Fatalf("inference ran %d discovery and %d account calls, want none", fixture.discoverer.callCount, fixture.account.callCount)
	}
	if len(result.Inferred) != 0 {
		t.Fatalf("inferred = %v, want none", result.Inferred)
	}
}

// TestRegisterSkipsTheAccountProbeWhenUsersAreExplicit verifies the GitHub
// account is read only when no authorized user was supplied.
func TestRegisterSkipsTheAccountProbeWhenUsersAreExplicit(t *testing.T) {
	t.Parallel()

	fixture := newInferenceFixture(t)
	if _, err := fixture.service.Register(context.Background(), factory.RegisterRequest{
		WorkingDirectory:    fixture.repository,
		AuthorizedUsers:     []string{"bob"},
		OperationalDataPath: filepath.Join(fixture.stateFolder, "factory.db"),
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if fixture.account.callCount != 0 {
		t.Fatalf("account probe ran %d times, want none", fixture.account.callCount)
	}
}

// TestRegisterFailsClosedWhenInferenceCannotResolveAValue verifies unsafe or
// missing metadata leaves no registration and no operational store behind.
func TestRegisterFailsClosedWhenInferenceCannotResolveAValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prepare func(*inferenceFixture)
		want    string
	}{
		{
			name: "outside a checkout",
			prepare: func(fixture *inferenceFixture) {
				fixture.discoverer.err = errors.New("not inside a Git checkout")
			},
			want: "not inside a Git checkout",
		},
		{
			name: "unauthenticated github account",
			prepare: func(fixture *inferenceFixture) {
				fixture.account.err = errors.New("run gh auth login")
			},
			want: "gh auth login",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fixture := newInferenceFixture(t)
			tc.prepare(fixture)
			operationalPath := filepath.Join(fixture.stateFolder, "factory.db")
			_, err := fixture.service.Register(context.Background(), factory.RegisterRequest{
				WorkingDirectory:    fixture.repository,
				OperationalDataPath: operationalPath,
			})
			if err == nil {
				t.Fatal("Register() error = nil, want a fail-closed diagnosis")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
			if _, statErr := os.Stat(operationalPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("operational store stat error = %v, want it to be absent", statErr)
			}
			status, statusErr := fixture.service.Status(context.Background())
			if statusErr != nil {
				t.Fatalf("Status() error = %v", statusErr)
			}
			if status.RepositoryPath != "" {
				t.Fatalf("RepositoryPath = %q, want no registration", status.RepositoryPath)
			}
		})
	}
}

// TestRegisterRequiresAnInferenceDirectoryWhenValuesAreMissing verifies a
// missing working directory is reported instead of silently probing the host.
func TestRegisterRequiresAnInferenceDirectoryWhenValuesAreMissing(t *testing.T) {
	t.Parallel()

	fixture := newInferenceFixture(t)
	_, err := fixture.service.Register(context.Background(), factory.RegisterRequest{
		OperationalDataPath: filepath.Join(fixture.stateFolder, "factory.db"),
	})
	if err == nil {
		t.Fatal("Register() error = nil, want a rejected inference request")
	}
	if fixture.discoverer.callCount != 0 {
		t.Fatalf("discovery ran %d times, want none", fixture.discoverer.callCount)
	}
}

// TestRegisterReportsAMissingHostConfigurationBeforeInference verifies the
// first-run error names factory init and that no Git or GitHub probe runs.
func TestRegisterReportsAMissingHostConfigurationBeforeInference(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	discoverer := &stubDiscoverer{}
	account := &stubAccount{}
	service := factory.NewWithDependencies(filepath.Join(root, "config.yaml"), factory.Dependencies{
		RepositoryDiscoverer: discoverer,
		GitHubAccount:        account,
	})
	_, err := service.Register(context.Background(), factory.RegisterRequest{WorkingDirectory: root})
	if err == nil {
		t.Fatal("Register() error = nil, want a missing host configuration")
	}
	if !strings.Contains(err.Error(), "factory init") {
		t.Fatalf("error = %q, want it to name factory init", err.Error())
	}
	if discoverer.callCount != 0 || account.callCount != 0 {
		t.Fatal("inference ran before the host configuration was loaded")
	}
}

// loadRegistration reads the single repository registration a register call
// persisted, so assertions observe the stored schema rather than a result echo.
func loadRegistration(t *testing.T, configPath string) config.RepositoryRegistration {
	t.Helper()

	host, err := config.LoadHost(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(host.Repositories) != 1 {
		t.Fatalf("registered repositories = %d, want 1", len(host.Repositories))
	}
	return host.Repositories[0]
}

// inferredFlags lists the register flags a result reports as inferred.
func inferredFlags(result factory.RegisterResult) []string {
	flags := make([]string, 0, len(result.Inferred))
	for _, value := range result.Inferred {
		flags = append(flags, value.Flag)
	}
	return flags
}

// inferredPairs lists each inferred register flag with the value it resolved to.
func inferredPairs(result factory.RegisterResult) []string {
	pairs := make([]string, 0, len(result.Inferred))
	for _, value := range result.Inferred {
		pairs = append(pairs, value.Flag+"="+value.Value)
	}
	return pairs
}
