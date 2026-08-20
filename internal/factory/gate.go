package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
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
	gitMetadataPath, err := prepareGitMetadataProjection(run.ID, registration.Path, run.Worktree)
	if err != nil {
		return gate.Result{}, fmt.Errorf("prepare worker git metadata: %w", err)
	}
	if err := s.deps.Worker.Start(ctx, worker.StartRequest{
		RunID:           run.ID,
		WorktreePath:    run.Worktree,
		GitMetadataPath: gitMetadataPath,
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

// prepareGitMetadataProjection creates or reuses a sanitized Git metadata
// projection for a run. It copies history and refs without copying repository
// configuration, remotes, hooks, or other files that could carry credentials or
// host paths. The projection is stable for the run so a reused worker keeps the
// same mounted metadata after a coordinator restart. It returns the projection
// path or an error if the inputs or projection are invalid.
func prepareGitMetadataProjection(runID, repositoryPath, worktreePath string) (string, error) {
	if strings.TrimSpace(runID) == "" || filepath.Base(runID) != runID || runID == "." || runID == ".." || strings.ContainsAny(runID, `/\`) {
		return "", fmt.Errorf("run id %q cannot name a git metadata projection", runID)
	}
	if !filepath.IsAbs(worktreePath) {
		return "", errors.New("worktree path must be absolute for a git metadata projection")
	}
	source := filepath.Join(repositoryPath, ".git")
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return "", fmt.Errorf("inspect repository git metadata: %w", err)
	}
	if !sourceInfo.IsDir() {
		return "", errors.New("repository git metadata must be a directory")
	}

	projectionParent := filepath.Join(filepath.Dir(worktreePath), ".factory-git")
	projection := filepath.Join(projectionParent, runID)
	if projectionInfo, statErr := os.Stat(projection); statErr == nil {
		if !projectionInfo.IsDir() {
			return "", fmt.Errorf("git metadata projection %q is not a directory", projection)
		}
		if _, statErr := os.Stat(filepath.Join(projection, "HEAD")); statErr != nil {
			return "", fmt.Errorf("git metadata projection %q is incomplete: %w", projection, statErr)
		}
		if _, statErr := os.Stat(filepath.Join(projection, "config")); statErr == nil {
			return "", fmt.Errorf("git metadata projection %q contains forbidden configuration", projection)
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", fmt.Errorf("inspect git metadata projection configuration: %w", statErr)
		}
		if err := makeGitProjectionReadable(projection); err != nil {
			return "", fmt.Errorf("prepare git metadata projection permissions: %w", err)
		}
		return projection, nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", fmt.Errorf("inspect git metadata projection: %w", statErr)
	}
	if err := os.MkdirAll(projectionParent, 0o700); err != nil {
		return "", fmt.Errorf("create git metadata projection parent: %w", err)
	}
	temporary, err := os.MkdirTemp(projectionParent, "."+runID+".git-projection-")
	if err != nil {
		return "", fmt.Errorf("create temporary git metadata projection: %w", err)
	}
	defer func() { _ = os.RemoveAll(temporary) }()
	if err := copyGitMetadata(source, temporary); err != nil {
		return "", fmt.Errorf("copy git metadata: %w", err)
	}
	if err := overlayWorktreeGitState(worktreePath, temporary); err != nil {
		return "", fmt.Errorf("overlay worktree git state: %w", err)
	}
	if err := makeGitProjectionReadable(temporary); err != nil {
		return "", fmt.Errorf("prepare git metadata projection permissions: %w", err)
	}
	if err := os.Rename(temporary, projection); err != nil {
		return "", fmt.Errorf("publish git metadata projection: %w", err)
	}
	return projection, nil
}

// copyGitMetadata copies permitted regular Git metadata from source to
// destination while omitting local configuration and files that can define
// remotes, commands, or host paths. It rejects symbolic links and other
// non-regular entries.
func copyGitMetadata(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if skipGitMetadata(relative, entry.IsDir()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported git metadata entry %q", relative)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return copyGitMetadataFile(path, target, 0o644)
	})
}

// makeGitProjectionReadable gives the fixed worker identity read and execute
// access to an existing projection without granting it write access.
func makeGitProjectionReadable(projection string) error {
	return filepath.WalkDir(projection, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported git metadata entry %q", path)
		}
		mode := fs.FileMode(0o644)
		if info.IsDir() {
			mode = 0o755
		} else if info.Mode().Perm()&0o111 != 0 {
			mode |= 0o111
		}
		return os.Chmod(path, mode)
	})
}

// skipGitMetadata reports whether a relative Git metadata path should be
// excluded from a projection because it can contain configuration, credentials,
// executable hooks, or host-specific indirection.
func skipGitMetadata(relative string, directory bool) bool {
	parts := strings.Split(filepath.ToSlash(relative), "/")
	for _, part := range parts {
		switch part {
		case "hooks", "modules", "remotes", "branches", "worktrees":
			return true
		}
	}
	if directory {
		return false
	}
	base := filepath.Base(relative)
	return base == "config" || base == "config.worktree" || base == "FETCH_HEAD" || base == "alternates"
}

// copyGitMetadataFile streams one regular Git metadata file into the projection
// while preserving the specified non-secret permission bits.
func copyGitMetadataFile(source, destination string, mode fs.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	if err := os.Chmod(destination, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	chmodErr := output.Chmod(mode)
	closeErr := output.Close()
	return errors.Join(copyErr, chmodErr, closeErr)
}

// overlayWorktreeGitState copies the run worktree's HEAD and index from its
// private worktree admin directory onto the common projected metadata when
// those files are available.
func overlayWorktreeGitState(worktreePath, projection string) error {
	pointer, err := os.ReadFile(filepath.Join(worktreePath, ".git"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	line := strings.TrimSpace(strings.SplitN(string(pointer), "\n", 2)[0])
	if !strings.HasPrefix(line, "gitdir:") {
		return nil
	}
	adminPath := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if adminPath == "" {
		return errors.New("worktree git pointer has no admin path")
	}
	if !filepath.IsAbs(adminPath) {
		adminPath = filepath.Join(worktreePath, adminPath)
	}
	for _, name := range []string{"HEAD", "index"} {
		source := filepath.Join(adminPath, name)
		info, statErr := os.Stat(source)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return statErr
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("worktree git state %q is not a regular file", name)
		}
		if err := copyGitMetadataFile(source, filepath.Join(projection, name), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// decodeSpecificationPacket decodes and validates a serialized frozen specification packet.
// It returns an error when the packet is missing, malformed, or uses an unsupported version.
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

// declaredGate finds a gate with the specified name in the frozen configuration.
// It returns the matching gate and true, or an empty gate configuration and false
// when no match exists.
func declaredGate(packet SpecificationPacket, name string) (config.GateConfig, bool) {
	for _, candidate := range packet.RepositoryConfig.Gates {
		if candidate.Name == name {
			return candidate, true
		}
	}
	return config.GateConfig{}, false
}

// workerCaches converts repository cache configurations into worker cache mounts.
func workerCaches(caches []config.CacheConfig) []worker.CacheMount {
	result := make([]worker.CacheMount, 0, len(caches))
	for _, cache := range caches {
		result = append(result, worker.CacheMount{Name: cache.Name, HostPath: cache.Path, ReadOnly: cache.ReadOnly})
	}
	return result
}
