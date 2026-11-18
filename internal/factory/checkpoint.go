package factory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/checkpoint"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/onboarding"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/version"
)

// Refusal codes of factory checkpoint create (ADR 0032).
const (
	// CheckpointCoordinatorRunning reports a held coordinator lock.
	CheckpointCoordinatorRunning = "checkpoint.coordinator_running"
	// CheckpointMigrationRequired reports a store with an older schema.
	CheckpointMigrationRequired = "checkpoint.migration_required"
	// CheckpointNotQuiescent reports one unmet full-quiescence condition.
	CheckpointNotQuiescent = "checkpoint.not_quiescent"
	// CheckpointExternalUnverified reports one external object that could
	// not be read.
	CheckpointExternalUnverified = "checkpoint.external_unverified"
	// CheckpointStoreMissing reports a missing operational store.
	CheckpointStoreMissing = "checkpoint.store_missing"
	// CheckpointDestinationRefused reports an unsafe or existing destination.
	CheckpointDestinationRefused = "checkpoint.destination_refused"
)

// CheckpointRequest is one factory checkpoint create.
type CheckpointRequest struct {
	// ConfigPath is the absolute host configuration path.
	ConfigPath string
	// Registration is the registration whose store is captured.
	Registration config.RepositoryRegistration
	// Output is the new checkpoint directory. It must not exist.
	Output string
	// TargetBranch is the target branch of the repository policy.
	TargetBranch string
	// WorkerImage is the pinned worker image@digest.
	WorkerImage string
	// Build identifies the running Factory build.
	Build version.Identity
	// External reads the state of the external objects.
	External checkpoint.ExternalReader
	// Workers reads Docker for the worker-activity condition of full
	// quiescence.
	Workers WorkerProbe
	// Now returns the current time.
	Now func() time.Time
	// afterSnapshot runs after VACUUM INTO and before the manifest write. A
	// test uses it to observe the lock or to stop the process there.
	afterSnapshot func()
}

// CheckpointResult is the outcome of a checkpoint create.
type CheckpointResult struct {
	// Path is the created checkpoint directory.
	Path string
	// Manifest is the manifest of the created checkpoint; it is nil when a
	// refusal exists.
	Manifest *checkpoint.Manifest
	// Refusals are the reasons why nothing was written.
	Refusals []store.UpgradeFinding
}

// errNotQuiescent ends a snapshot whose full-quiescence check found work.
var errNotQuiescent = errors.New("the installation is not in full quiescence")

// CreateCheckpoint creates a maintenance checkpoint (ADR 0032). It refuses
// an unsafe destination, then holds the coordinator lock to the end, takes
// the online store snapshot under the SQLite write lock after the
// full-quiescence check, reads the state of every known external object,
// and writes the manifest. All files go into a private staging directory
// beside the destination, which is renamed into place only when it is
// complete; any refusal, error, or crash leaves no directory at the
// destination. A refusal is a result, not an error.
func CreateCheckpoint(ctx context.Context, request CheckpointRequest) (CheckpointResult, error) {
	output, refusal, err := checkDestination(request)
	if err != nil || refusal != nil {
		return refused(refusal), err
	}
	lock, err := acquireCoordinatorLock(coordinatorLockPath(request.Registration), LockHolderCheckpoint)
	if errors.Is(err, ErrCoordinatorAlreadyRunning) {
		return refused(&store.UpgradeFinding{Code: CheckpointCoordinatorRunning, Detail: "a live coordinator or another maintenance command holds the repository lock"}), nil
	}
	if err != nil {
		return CheckpointResult{}, err
	}
	defer func() { _ = lock.release() }()
	if _, err := os.Lstat(request.Registration.OperationalDataPath); errors.Is(err, os.ErrNotExist) {
		return refused(&store.UpgradeFinding{Code: CheckpointStoreMissing, Detail: "no operational store exists at " + request.Registration.OperationalDataPath}), nil
	}
	staging, err := os.MkdirTemp(filepath.Dir(output), "."+filepath.Base(output)+".staging-")
	if err != nil {
		return CheckpointResult{}, fmt.Errorf("create the checkpoint staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	result, err := fillCheckpoint(ctx, request, staging)
	if err != nil || len(result.Refusals) != 0 {
		return result, err
	}
	if err := publishCheckpoint(staging, output); err != nil {
		return CheckpointResult{}, err
	}
	result.Path = output
	return result, nil
}

// refused returns the result of one refusal, or an empty result.
func refused(refusal *store.UpgradeFinding) CheckpointResult {
	if refusal == nil {
		return CheckpointResult{}
	}
	return CheckpointResult{Refusals: []store.UpgradeFinding{*refusal}}
}

// fillCheckpoint writes the snapshot and the manifest into staging.
func fillCheckpoint(ctx context.Context, request CheckpointRequest, staging string) (CheckpointResult, error) {
	var findings []store.UpgradeFinding
	var createdAt time.Time
	check := func(ctx context.Context, view store.Querier) error {
		createdAt = request.Now()
		var err error
		if findings, err = store.FullQuiescenceFindings(ctx, view); err != nil {
			return err
		}
		processes, err := store.ReadWorkerProcesses(ctx, view)
		if err != nil {
			return err
		}
		findings = append(findings, WorkerActivityFindings(ctx, processes, request.Workers)...)
		if len(findings) != 0 {
			return errNotQuiescent
		}
		return nil
	}
	snapshot, err := store.Snapshot(ctx, request.Registration.OperationalDataPath, filepath.Join(staging, checkpoint.StoreFile), check)
	var migration *store.MigrationRequiredError
	switch {
	case errors.As(err, &migration):
		return refused(&store.UpgradeFinding{Code: CheckpointMigrationRequired, Detail: migration.Error()}), nil
	case errors.Is(err, errNotQuiescent):
		return CheckpointResult{Refusals: notQuiescentRefusals(findings)}, nil
	case err != nil:
		return CheckpointResult{}, err
	}
	if request.afterSnapshot != nil {
		request.afterSnapshot()
	}
	external, unread := checkpoint.ReadExternalState(ctx, request.External, request.TargetBranch, snapshot.KnownRuns)
	if len(unread) != 0 {
		result := CheckpointResult{}
		for _, detail := range unread {
			result.Refusals = append(result.Refusals, store.UpgradeFinding{Code: CheckpointExternalUnverified, Detail: detail})
		}
		return result, nil
	}
	manifest, err := checkpointManifest(request, createdAt, snapshot, external)
	if err != nil {
		return CheckpointResult{}, err
	}
	if err := checkpoint.WriteManifest(staging, manifest); err != nil {
		return CheckpointResult{}, err
	}
	return CheckpointResult{Manifest: &manifest}, nil
}

// notQuiescentRefusals names each unmet full-quiescence condition.
func notQuiescentRefusals(findings []store.UpgradeFinding) []store.UpgradeFinding {
	refusals := make([]store.UpgradeFinding, 0, len(findings))
	for _, finding := range findings {
		refusals = append(refusals, store.UpgradeFinding{Code: CheckpointNotQuiescent, Detail: finding.Code + ": " + finding.Detail})
	}
	return refusals
}

// checkpointManifest returns the complete manifest of one snapshot.
func checkpointManifest(request CheckpointRequest, createdAt time.Time, snapshot store.SnapshotInspection, external checkpoint.ExternalState) (checkpoint.Manifest, error) {
	id, err := newCheckpointID()
	if err != nil {
		return checkpoint.Manifest{}, err
	}
	identity, err := onboarding.RegistrationIdentity(request.ConfigPath, request.Registration)
	if err != nil {
		return checkpoint.Manifest{}, err
	}
	manifest := checkpoint.NewManifest(id, createdAt, snapshot)
	manifest.Factory = checkpointFactory(request.Build)
	manifest.RegistrationIdentity = identity
	manifest.RegistrationSummary = registrationSummary(request.Registration)
	manifest.WorkerImage = request.WorkerImage
	manifest.TargetBranchHead = external.TargetBranch.Head
	manifest.ExternalState = external
	return manifest, nil
}

// registrationSummary returns the non-secret registration summary. It never
// copies a credential path.
func registrationSummary(registration config.RepositoryRegistration) checkpoint.RegistrationSummary {
	repository := registeredRepository(registration)
	return checkpoint.RegistrationSummary{
		RepositoryPath: registration.Path,
		Tracker:        string(registration.EffectiveTracker()),
		CodeHost:       string(registration.EffectiveCodeHost()),
		Owner:          repository.Owner,
		Project:        repository.Project,
		Repository:     repository.Name,
		InstallationID: registration.LocalOperator.InstallationID,
	}
}

// newCheckpointID returns a random checkpoint identifier.
func newCheckpointID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("create the checkpoint identifier: %w", err)
	}
	return "checkpoint-" + hex.EncodeToString(random), nil
}

// checkDestination returns the absolute destination, or a refusal when it
// exists, its parent is not a private directory, or it lies inside the
// registered checkout or the store directory.
func checkDestination(request CheckpointRequest) (string, *store.UpgradeFinding, error) {
	refuse := func(detail string) (string, *store.UpgradeFinding, error) {
		return "", &store.UpgradeFinding{Code: CheckpointDestinationRefused, Detail: detail}, nil
	}
	if strings.TrimSpace(request.Output) == "" {
		return refuse("the checkpoint needs a new output directory")
	}
	absolute, err := filepath.Abs(request.Output)
	if err != nil {
		return "", nil, fmt.Errorf("resolve the checkpoint destination: %w", err)
	}
	if _, err := os.Lstat(absolute); !errors.Is(err, os.ErrNotExist) {
		return refuse(absolute + " already exists; a checkpoint needs a new directory")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return refuse("the parent of " + absolute + " is not an existing directory")
	}
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() {
		return refuse("the parent of " + absolute + " is not an existing directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return refuse(parent + " is accessible by group or other; a checkpoint is private data")
	}
	resolved := filepath.Join(parent, filepath.Base(absolute))
	for name, directory := range map[string]string{"the registered checkout": request.Registration.Path, "the store directory": filepath.Dir(request.Registration.OperationalDataPath)} {
		if within(resolved, directory) {
			return refuse(absolute + " is inside " + name)
		}
	}
	return absolute, nil, nil
}

// within reports whether path lies inside directory after symbolic links of
// directory are resolved.
func within(path, directory string) bool {
	if resolved, err := filepath.EvalSymlinks(directory); err == nil {
		directory = resolved
	}
	relative, err := filepath.Rel(filepath.Clean(directory), path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// publishCheckpoint syncs the complete staging directory and renames it to
// the destination, then syncs the parent so the rename is durable.
func publishCheckpoint(staging, output string) error {
	if err := syncDirectory(staging); err != nil {
		return err
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checkpoint destination %s appeared during the checkpoint", output)
	}
	if err := os.Rename(staging, output); err != nil {
		return fmt.Errorf("move the checkpoint into place: %w", err)
	}
	return syncDirectory(filepath.Dir(output))
}

// syncDirectory flushes the entries of one directory to disk.
func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s to sync it: %w", path, err)
	}
	err = directory.Sync()
	if closeErr := directory.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	return nil
}

// checkpointFactory returns the build identity that a checkpoint manifest
// or a restore journal records.
func checkpointFactory(build version.Identity) checkpoint.Factory {
	return checkpoint.Factory{Version: build.Version, Build: build.Build, Revision: build.Revision, Modified: build.Modified}
}
