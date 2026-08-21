package factory_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

const factoryGateCheckpoint = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestRunGateStartsThePinnedWorkerAndUsesTheFrozenGate verifies the
// coordinator composes the claim packet, worker seam, and exact-SHA status
// publisher without re-reading mutable repository policy.
func TestRunGateStartsThePinnedWorkerAndUsesTheFrozenGate(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repositoryPath := filepath.Join(root, "repository")
	worktreePath := filepath.Join(root, "worktree")
	gitMetadataPath := filepath.Join(repositoryPath, ".git")
	for _, path := range []string{repositoryPath, worktreePath, gitMetadataPath} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(gitMetadataPath, "config"), []byte("[remote \"origin\"]\n\turl = https://user:secret@example.invalid/project.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitMetadataPath, "HEAD"), []byte("ref: refs/heads/factory/run-gate-coordinator\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	policy := validRepositoryConfig()
	policy.Setup = "setup-from-frozen-packet"
	policy.Gates[0].Command = "gate-from-frozen-packet"
	packetData, err := json.Marshal(factory.SpecificationPacket{
		Version:          1,
		RepositoryConfig: policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	host := config.HostConfig{SchemaVersion: 1, Repositories: []config.RepositoryRegistration{{
		Path:                 repositoryPath,
		GitHub:               config.GitHubConfig{Owner: "example", Repository: "project"},
		AuthorizedUsers:      []string{"alice"},
		Polling:              config.PollingConfig{Interval: "30s", Backoff: "5m"},
		OperationalDataPath:  filepath.Join(root, "state", "factory.db"),
		RepositoryConfigPath: filepath.Join(repositoryPath, "factory.yaml"),
	}}}
	run := store.Run{
		ID:                  "run-gate-coordinator",
		RepositoryPath:      repositoryPath,
		IssueNumber:         5,
		Stage:               store.StageClaim,
		Status:              store.StatusActive,
		Worktree:            worktreePath,
		CheckpointSHA:       factoryGateCheckpoint,
		ImageDigest:         policy.WorkerBuild.Digest,
		SpecificationPacket: string(packetData),
	}
	runtime := &gateWorker{results: []worker.CommandResult{{ExitCode: 0}, {ExitCode: 0}}}
	statuses := &gateStatuses{}
	service := factory.NewWithDependencies("/host/config.yaml", factory.Dependencies{
		Config: &fakeConfig{value: host},
		OpenStore: func(context.Context, string) (factory.OperationalStore, error) {
			return &fakeRunStore{saved: []store.Run{run}}, nil
		},
		GitHub:         &fakeGitHub{},
		Worker:         runtime,
		CommitStatuses: statuses,
	})

	result, err := service.RunGate(context.Background(), factory.RunGateRequest{
		RunID:    run.ID,
		GateName: policy.Gates[0].Name,
	})
	if err != nil {
		t.Fatalf("RunGate() error = %v", err)
	}
	if result.Status.State != github.CommitStatusSuccess || result.Status.SHA != factoryGateCheckpoint {
		t.Fatalf("RunGate() status = %#v, want exact-SHA success", result.Status)
	}
	if len(runtime.starts) != 1 {
		t.Fatalf("worker starts = %#v, want one start", runtime.starts)
	}
	started := runtime.starts[0]
	if started.Image != policy.WorkerBuild.Image || started.ImageDigest != policy.WorkerBuild.Digest || started.WorktreePath != worktreePath || started.GitMetadataPath == gitMetadataPath {
		t.Fatalf("worker start = %#v, want frozen worker identity and stable paths", started)
	}
	if started.Role != "gate" || started.CredentialStoreID != "" {
		t.Fatalf("gate worker start = %#v, want gate role without credentials", started)
	}
	if _, err := os.Stat(filepath.Join(started.GitMetadataPath, "config")); !os.IsNotExist(err) {
		t.Fatalf("worker Git projection contains a configuration file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(started.GitMetadataPath, "HEAD")); err != nil {
		t.Fatalf("worker Git projection did not preserve HEAD metadata: %v", err)
	}
	if info, err := os.Stat(started.GitMetadataPath); err != nil {
		t.Fatalf("stat worker Git projection: %v", err)
	} else if got := info.Mode().Perm(); got&0o050 != 0o050 || got&0o007 != 0 {
		t.Fatalf("worker Git projection permissions = %#o, want group read/execute without other access", got)
	}
	if info, err := os.Stat(filepath.Join(started.GitMetadataPath, "HEAD")); err != nil {
		t.Fatalf("stat worker Git projection HEAD: %v", err)
	} else if got := info.Mode().Perm(); got&0o040 != 0o040 || got&0o007 != 0 {
		t.Fatalf("worker Git projection HEAD permissions = %#o, want group read without other access", got)
	}
	if len(runtime.commands) != 2 || runtime.commands[0].Command != policy.Setup || runtime.commands[1].Command != policy.Gates[0].Command {
		t.Fatalf("worker commands = %#v, want frozen setup then gate", runtime.commands)
	}
	if len(statuses.values) != 1 || statuses.repositories[0] != (github.Repository{Owner: "example", Name: "project"}) || statuses.values[0].SHA != factoryGateCheckpoint || statuses.values[0].Context != "factory/gate/test" {
		t.Fatalf("published statuses = %#v, want exact stable status", statuses.values)
	}
}

// gateWorker is a WorkerRuntime adapter that records coordinator requests.
type gateWorker struct {
	starts   []worker.StartRequest
	commands []worker.CommandRequest
	results  []worker.CommandResult
}

// Start records the frozen worker start request.
func (w *gateWorker) Start(_ context.Context, request worker.StartRequest) error {
	w.starts = append(w.starts, request)
	return nil
}

// Resume implements WorkerRuntime for the coordinator gate test.
func (w *gateWorker) Resume(context.Context, worker.ResumeRequest) error { return nil }

// RunCommand records and returns one deterministic fixture.
func (w *gateWorker) RunCommand(_ context.Context, request worker.CommandRequest) (worker.CommandResult, error) {
	w.commands = append(w.commands, request)
	result := w.results[0]
	w.results = w.results[1:]
	return result, nil
}

// Stop implements WorkerRuntime for the coordinator gate test.
func (w *gateWorker) Stop(context.Context, string) error { return nil }

// Inspect implements WorkerRuntime for the coordinator gate test.
func (w *gateWorker) Inspect(context.Context, string) (worker.Inspection, error) {
	return worker.Inspection{Exists: true, Running: true}, nil
}

// gateStatuses records exact-SHA statuses published by the coordinator.
type gateStatuses struct {
	repositories []github.Repository
	values       []github.CommitStatus
}

// CreateCommitStatus records one status publication.
func (s *gateStatuses) CreateCommitStatus(_ context.Context, repository github.Repository, status github.CommitStatus) error {
	s.repositories = append(s.repositories, repository)
	s.values = append(s.values, status)
	return nil
}

var _ worker.WorkerRuntime = (*gateWorker)(nil)
var _ github.CommitStatusPublisher = (*gateStatuses)(nil)
