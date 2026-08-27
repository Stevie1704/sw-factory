package factory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/github"
)

// TestCoordinatorLockPreventsASecondOwner verifies the host lock is enforced
// by the kernel and becomes reusable after the first owner releases it.
func TestCoordinatorLockPreventsASecondOwner(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state", "factory.lock")
	first, err := acquireCoordinatorLock(path)
	if err != nil {
		t.Fatalf("first acquireCoordinatorLock() error = %v", err)
	}
	defer func() { _ = first.release() }()
	if _, err := acquireCoordinatorLock(path); !errors.Is(err, ErrCoordinatorAlreadyRunning) {
		t.Fatalf("second acquireCoordinatorLock() error = %v, want ErrCoordinatorAlreadyRunning", err)
	}
	if err := first.release(); err != nil {
		t.Fatalf("release() error = %v", err)
	}
	second, err := acquireCoordinatorLock(path)
	if err != nil {
		t.Fatalf("reacquireCoordinatorLock() error = %v", err)
	}
	if err := second.release(); err != nil {
		t.Fatalf("second release() error = %v", err)
	}
}

// TestStartAcquiresTheCoordinatorLockBeforeReconciliation verifies a losing
// process cannot open the operational store or replay durable mutations owned
// by the process that already holds the repository lock.
func TestStartAcquiresTheCoordinatorLockBeforeReconciliation(t *testing.T) {
	t.Parallel()

	registration := config.RepositoryRegistration{
		Path:                 filepath.Join(t.TempDir(), "repository"),
		OperationalDataPath:  filepath.Join(t.TempDir(), "factory.db"),
		RepositoryConfigPath: filepath.Join(t.TempDir(), "factory.yaml"),
		Polling:              config.PollingConfig{Interval: "1s", Backoff: "1s"},
	}
	owner, err := acquireCoordinatorLock(coordinatorLockPath(registration))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.release() }()
	openCalls := 0
	service := &Service{configPath: "/host/config.yaml", deps: Dependencies{
		Config: lockTestConfig{host: config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{registration}}},
		OpenStore: func(context.Context, string) (OperationalStore, error) {
			openCalls++
			return nil, errors.New("operational store must remain unopened")
		},
		LoadRepository: func(string) (config.RepositoryConfig, error) {
			return config.RepositoryConfig{TargetBranch: "main"}, nil
		},
		IssuePoller: lockTestIssuePoller{},
		Lease:       lockTestLease{},
		StartupDiagnosis: func(context.Context) (DoctorResult, error) {
			return DoctorResult{Report: doctor.Report{Results: []doctor.Result{doctor.Success("startup")}}}, nil
		},
	}}

	err = service.Start(context.Background())
	if !errors.Is(err, ErrCoordinatorAlreadyRunning) {
		t.Fatalf("Start() error = %v, want ErrCoordinatorAlreadyRunning", err)
	}
	if openCalls != 0 {
		t.Fatalf("operational store opens = %d, want zero before lock ownership", openCalls)
	}
}

// TestRequestCoordinatorStopReportsNoOwner verifies an idle stop is safe and
// leaves no misleading success result for the CLI.
func TestRequestCoordinatorStopReportsNoOwner(t *testing.T) {
	t.Parallel()

	result, err := requestCoordinatorStop(filepath.Join(t.TempDir(), "missing.lock"))
	if !errors.Is(err, ErrCoordinatorNotRunning) {
		t.Fatalf("requestCoordinatorStop() error = %v, want ErrCoordinatorNotRunning", err)
	}
	if result.Running || result.PID != 0 {
		t.Fatalf("stop result = %#v, want no running coordinator", result)
	}
}

// TestCoordinatorLockPathUsesTheRepositoryIdentity verifies separate host
// configurations for one checkout cannot bypass the coordinator lock by
// choosing different operational-store paths.
func TestCoordinatorLockPathUsesTheRepositoryIdentity(t *testing.T) {
	t.Parallel()

	first := config.RepositoryRegistration{
		Path:                filepath.Join(t.TempDir(), "repository"),
		OperationalDataPath: filepath.Join(t.TempDir(), "first.db"),
	}
	second := first
	second.OperationalDataPath = filepath.Join(t.TempDir(), "second.db")
	if got, want := coordinatorLockPath(first), coordinatorLockPath(second); got != want {
		t.Fatalf("lock paths = %q and %q, want shared path for one repository", got, want)
	}
}

// TestCoordinatorLockRejectsSymlinkedLockFiles verifies lock ownership and
// stop inspection cannot follow a path replacement into an unrelated file.
func TestCoordinatorLockRejectsSymlinkedLockFiles(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "coordinator.lock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireCoordinatorLock(link); err == nil {
		t.Fatal("acquireCoordinatorLock() followed a symlink")
	}
	if _, err := requestCoordinatorStop(link); err == nil {
		t.Fatal("requestCoordinatorStop() followed a symlink")
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "123\n" {
		t.Fatalf("symlink target content = %q, want unchanged PID marker", content)
	}
}

// lockTestConfig supplies the one repository used by the lock-order fixture.
type lockTestConfig struct {
	host config.HostConfig
}

// Load returns the configured host registration.
func (c lockTestConfig) Load(string) (config.HostConfig, error) { return c.host, nil }

// Save satisfies ConfigRepository for the read-only fixture.
func (lockTestConfig) Save(string, config.HostConfig) error { return nil }

// Create returns the configured host registration.
func (c lockTestConfig) Create(string) (config.HostConfig, error) { return c.host, nil }

// lockTestIssuePoller satisfies the queue seam without making a request.
type lockTestIssuePoller struct{}

// ListEligibleIssues returns an empty queue.
func (lockTestIssuePoller) ListEligibleIssues(context.Context, github.Repository) ([]github.Issue, error) {
	return nil, nil
}

// lockTestLease satisfies the lease seam without publishing a heartbeat.
type lockTestLease struct{}

// RenewLease accepts the fixture heartbeat.
func (lockTestLease) RenewLease(context.Context, github.Repository, github.Lease) error { return nil }
