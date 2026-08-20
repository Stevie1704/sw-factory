package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/gate"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// RunGateRequest selects one declared gate for the active run. An empty RunID
// addresses the repository's only active run; a gate name is always required
// so the coordinator never invents or silently changes acceptance criteria.
type RunGateRequest struct {
	// RunID optionally identifies the active run.
	RunID string
	// GateName identifies the declared gate to execute.
	GateName string
}

// RunGate starts the active run's pinned worker and executes repository setup
// plus exactly one gate from its frozen specification packet. The resulting
// status is attached to the run's exact checkpoint SHA.
func (s *Service) RunGate(ctx context.Context, request RunGateRequest) (gate.Result, error) {
	if strings.TrimSpace(request.GateName) == "" {
		return gate.Result{}, errors.New("gate name is required")
	}
	registration, runStore, run, err := s.openActiveRunStore(ctx)
	if err != nil {
		return gate.Result{}, err
	}
	defer func() { _ = runStore.Close() }()
	if run == nil {
		return gate.Result{}, errors.New("no active run")
	}
	if request.RunID != "" && request.RunID != run.ID {
		return gate.Result{}, fmt.Errorf("active run is %s, not %s", run.ID, request.RunID)
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return gate.Result{}, err
	}
	if run.ImageDigest != packet.RepositoryConfig.WorkerBuild.Digest {
		return gate.Result{}, fmt.Errorf("run %q image digest %q differs from frozen packet digest %q", run.ID, run.ImageDigest, packet.RepositoryConfig.WorkerBuild.Digest)
	}
	declaredGate, ok := declaredGate(packet, request.GateName)
	if !ok {
		return gate.Result{}, fmt.Errorf("gate %q is not declared in the frozen specification packet", request.GateName)
	}
	if s.deps.CommitStatuses == nil {
		return gate.Result{}, errors.New("GitHub client does not support Commit Statuses")
	}
	if err := s.deps.Worker.Start(ctx, worker.StartRequest{
		RunID:           run.ID,
		WorktreePath:    run.Worktree,
		GitMetadataPath: filepath.Join(registration.Path, ".git"),
		Image:           packet.RepositoryConfig.WorkerBuild.Image,
		ImageDigest:     run.ImageDigest,
		Caches:          workerCaches(packet.RepositoryConfig.Caches),
	}); err != nil {
		return gate.Result{}, err
	}
	return (gate.Runner{
		Runtime:    s.deps.Worker,
		Repository: github.Repository{Owner: registration.GitHub.Owner, Name: registration.GitHub.Repository},
		Statuses:   s.deps.CommitStatuses,
	}).Run(ctx, gate.Request{
		RunID:         run.ID,
		CheckpointSHA: run.CheckpointSHA,
		Setup:         packet.RepositoryConfig.Setup,
		SetupTimeout:  packet.RepositoryConfig.Timeouts.Setup,
		Gate:          declaredGate,
	})
}

// decodeSpecificationPacket loads the immutable packet retained by issue #4.
func decodeSpecificationPacket(serialized string) (SpecificationPacket, error) {
	if strings.TrimSpace(serialized) == "" {
		return SpecificationPacket{}, errors.New("active run has no frozen specification packet")
	}
	var packet SpecificationPacket
	if err := json.Unmarshal([]byte(serialized), &packet); err != nil {
		return SpecificationPacket{}, fmt.Errorf("decode frozen specification packet: %w", err)
	}
	if packet.Version != specificationPacketVersion {
		return SpecificationPacket{}, fmt.Errorf("unsupported frozen specification packet version %d", packet.Version)
	}
	return packet, nil
}

// declaredGate resolves one gate by its frozen configuration name.
func declaredGate(packet SpecificationPacket, name string) (config.GateConfig, bool) {
	for _, candidate := range packet.RepositoryConfig.Gates {
		if candidate.Name == name {
			return candidate, true
		}
	}
	return config.GateConfig{}, false
}

// workerCaches maps repository configuration caches to the worker seam.
func workerCaches(caches []config.CacheConfig) []worker.CacheMount {
	result := make([]worker.CacheMount, 0, len(caches))
	for _, cache := range caches {
		result = append(result, worker.CacheMount{Name: cache.Name, HostPath: cache.Path, ReadOnly: cache.ReadOnly})
	}
	return result
}
