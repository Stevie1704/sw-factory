package factory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/checkpoint"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/onboarding"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/version"
)

// Refusal codes of factory checkpoint restore (ADR 0032). The checkpoint
// verification codes are checkpoint.CodeIncomplete and checkpoint.CodeCorrupt.
const (
	// RestoreSchemaNewer reports a snapshot schema newer than this build.
	RestoreSchemaNewer = "restore.schema_newer"
	// RestoreReleaseMismatch reports a snapshot of another release or
	// revision, or of an older schema.
	RestoreReleaseMismatch = "restore.release_mismatch"
	// RestoreRegistrationChanged reports another registration identity.
	RestoreRegistrationChanged = "restore.registration_changed"
	// RestoreCoordinatorRunning reports a held coordinator lock.
	RestoreCoordinatorRunning = "restore.coordinator_running"
	// RestoreWorkerActive reports a running worker or role session, or
	// worker evidence that cannot be read.
	RestoreWorkerActive = "restore.worker_active"
	// RestoreNotQuiescent reports a live store with active work or with a
	// record that the snapshot does not have.
	RestoreNotQuiescent = "restore.not_quiescent"
	// RestoreExternalDivergence reports one external object that changed
	// after the snapshot or that the snapshot does not know.
	RestoreExternalDivergence = "restore.external_divergence"
	// RestoreDivergenceUnverified reports one external object or source
	// that could not be read.
	RestoreDivergenceUnverified = "restore.divergence_unverified"
	// RestorePlanStale reports a --confirm digest that is not the digest of
	// the current plan.
	RestorePlanStale = "restore.plan_stale"
	// RestoreWorkDirectoryExists reports the work directory of an earlier
	// restore of the same checkpoint, which can hold a preserved original.
	RestoreWorkDirectoryExists = "restore.work_directory_exists"
)

// Restore outcomes.
const (
	RestoreOutcomePreview     = "preview"
	RestoreOutcomeRestored    = "restored"
	RestoreOutcomeRefused     = "refused"
	RestoreOutcomeInterrupted = "interrupted"
)

// RestoreRequest is one factory checkpoint restore.
type RestoreRequest struct {
	// ConfigPath is the absolute host configuration path.
	ConfigPath string
	// Registration is the registration whose store is replaced.
	Registration config.RepositoryRegistration
	// Path is the checkpoint directory.
	Path string
	// Confirm is the digest of the previewed plan; empty asks for a preview.
	Confirm string
	// Build identifies the running Factory build.
	Build version.Identity
	// External reads the state of the known external objects.
	External checkpoint.ExternalReader
	// Owned lists the factory-owned external objects.
	Owned checkpoint.OwnedObjectLister
	// Workers reads Docker for the worker-activity precondition.
	Workers WorkerProbe
	// Containers lists every running worker container when the live store
	// cannot name the workers of this store.
	Containers WorkerContainerLister
}

// WorkerContainerLister lists the running worker containers of every
// installation on the Docker host. worker.DockerRuntime implements it.
type WorkerContainerLister interface {
	// RunningWorkerContainers returns the names of the running
	// factory-worker-* containers.
	RunningWorkerContainers(ctx context.Context) ([]string, error)
}

// RestorePlan is the previewed restore that --confirm binds by its digest.
type RestorePlan struct {
	// Steps describe what the confirmed restore does, in order.
	Steps []string `json:"steps"`
	// PreservedPaths are where the live files go; they stay after success.
	PreservedPaths []string `json:"preserved_paths"`
	// Digest binds the plan, the checkpoint, and the live file set.
	Digest string `json:"digest"`
	// files is the file part of the plan.
	files checkpoint.RestorePlan
}

// RestoreResult is the outcome of a restore.
type RestoreResult struct {
	// Outcome is preview, restored, refused, or interrupted.
	Outcome string
	// Plan is the restore plan; it is nil for a refusal before planning.
	Plan *RestorePlan
	// Refusals are the reasons why nothing was written.
	Refusals []store.UpgradeFinding
	// Journal is the continuation of an earlier interrupted restore.
	Journal *checkpoint.RestoreContinuation
}

// RestoreCheckpoint previews or applies the restore of a maintenance
// checkpoint (ADR 0032). A restore journal of an earlier restore is
// continued first, and nothing else runs. Otherwise every precondition must
// be proved: the checkpoint verifies, the build and registration match, the
// coordinator lock is free, no worker or role session runs, the live store
// is quiescent and has nothing newer, and no external object diverged.
// Without Confirm it returns the plan and changes nothing; with the digest
// of the same plan it applies the plan under the coordinator lock. A
// confirmed restore also holds the SQLite write lock of the live store from
// before its checks through the preserve renames, so no open writer, such
// as a command inbox, commits between the checks and the renames. A
// refusal is a result, not an error.
func RestoreCheckpoint(ctx context.Context, request RestoreRequest) (RestoreResult, error) {
	storePath, err := filepath.Abs(request.Registration.OperationalDataPath)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("resolve the store path: %w", err)
	}
	if _, err := os.Lstat(store.RestoreJournalPath(storePath)); err == nil {
		return continueRestore(ctx, request, storePath)
	}
	manifest, refusals, err := verifiedManifest(ctx, request)
	if err != nil || len(refusals) != 0 {
		return refusedRestore(refusals), err
	}
	lock, err := acquireCoordinatorLock(coordinatorLockPath(request.Registration), LockHolderRestore)
	if errors.Is(err, ErrCoordinatorAlreadyRunning) {
		return refusedRestore([]store.UpgradeFinding{coordinatorRunningRefusal()}), nil
	}
	if err != nil {
		return RestoreResult{}, err
	}
	defer func() { _ = lock.release() }()
	writeLock, refusal, err := lockConfirmedStore(ctx, request, storePath)
	if err != nil || refusal != nil {
		return refusedRestore(refusalList(refusal)), err
	}
	defer func() { _ = writeLock.Release() }()
	facts, err := store.InspectForRestore(ctx, storePath)
	if err != nil {
		return RestoreResult{}, err
	}
	if refusals = restoreRefusals(ctx, request, storePath, manifest, facts); len(refusals) != 0 {
		return refusedRestore(refusals), nil
	}
	plan, err := newRestorePlan(request, storePath, manifest, facts, writeLock.FileDigest)
	if err != nil {
		return RestoreResult{}, err
	}
	if request.Confirm == "" {
		return RestoreResult{Outcome: RestoreOutcomePreview, Plan: &plan}, nil
	}
	if request.Confirm != plan.Digest {
		result := refusedRestore([]store.UpgradeFinding{{Code: RestorePlanStale, Detail: "the plan changed after the preview; run the preview again and confirm its digest " + plan.Digest}})
		result.Plan = &plan
		return result, nil
	}
	if err := applyRestore(ctx, request, plan, writeLock.Release); err != nil {
		return RestoreResult{Plan: &plan}, err
	}
	return RestoreResult{Outcome: RestoreOutcomeRestored, Plan: &plan}, nil
}

// refusedRestore returns the result of the refusals.
func refusedRestore(refusals []store.UpgradeFinding) RestoreResult {
	if len(refusals) == 0 {
		return RestoreResult{}
	}
	return RestoreResult{Outcome: RestoreOutcomeRefused, Refusals: refusals}
}

// refusalList returns the one refusal as a list, or nil.
func refusalList(refusal *store.UpgradeFinding) []store.UpgradeFinding {
	if refusal == nil {
		return nil
	}
	return []store.UpgradeFinding{*refusal}
}

// lockConfirmedStore takes the SQLite write lock of the live store for a
// confirmed restore; a preview, a missing store, and a damaged store get no
// lock. A lock that another connection holds past the busy timeout is the
// restore.not_quiescent refusal.
func lockConfirmedStore(ctx context.Context, request RestoreRequest, storePath string) (*store.RestoreWriteLock, *store.UpgradeFinding, error) {
	if request.Confirm == "" {
		return nil, nil, nil
	}
	writeLock, err := store.LockForRestore(ctx, storePath)
	if store.IsTransient(err) {
		return nil, &store.UpgradeFinding{Code: RestoreNotQuiescent, Detail: "store.write_locked: another connection holds the write lock of the live store: " + err.Error()}, nil
	}
	return writeLock, nil, err
}

// coordinatorRunningRefusal is the refusal of a held coordinator lock.
func coordinatorRunningRefusal() store.UpgradeFinding {
	return store.UpgradeFinding{Code: RestoreCoordinatorRunning, Detail: "a live coordinator or another maintenance command holds the repository lock"}
}

// continueRestore continues the restore journal at storePath under the
// coordinator lock, from the release that wrote it only.
func continueRestore(ctx context.Context, request RestoreRequest, storePath string) (RestoreResult, error) {
	lock, err := acquireCoordinatorLock(coordinatorLockPath(request.Registration), LockHolderRestore)
	if errors.Is(err, ErrCoordinatorAlreadyRunning) {
		return refusedRestore([]store.UpgradeFinding{coordinatorRunningRefusal()}), nil
	}
	if err != nil {
		return RestoreResult{}, err
	}
	defer func() { _ = lock.release() }()
	journal, err := checkpoint.ReadRestoreJournal(storePath)
	if err != nil {
		return RestoreResult{}, err
	}
	if journal.Factory.Version != request.Build.Version || journal.Factory.Revision != request.Build.Revision {
		return refusedRestore([]store.UpgradeFinding{{Code: RestoreReleaseMismatch, Detail: fmt.Sprintf("the interrupted restore was started by %s (%s); continue it with that release", journal.Factory.Version, journal.Factory.Revision)}}), nil
	}
	continuation, err := checkpoint.ContinueRestore(ctx, storePath)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("continue the interrupted restore: %w", err)
	}
	return RestoreResult{Outcome: RestoreOutcomeInterrupted, Journal: &continuation}, nil
}

// verifiedManifest verifies the checkpoint and the static preconditions 1
// to 3: integrity, release and schema, and registration identity.
func verifiedManifest(ctx context.Context, request RestoreRequest) (checkpoint.Manifest, []store.UpgradeFinding, error) {
	manifest, err := checkpoint.Verify(ctx, request.Path)
	var verification *checkpoint.VerificationError
	if errors.As(err, &verification) {
		return checkpoint.Manifest{}, []store.UpgradeFinding{{Code: verification.Code, Detail: verification.Detail}}, nil
	}
	if err != nil {
		return checkpoint.Manifest{}, nil, err
	}
	if refusal := releaseRefusal(manifest, request.Build); refusal != nil {
		return manifest, []store.UpgradeFinding{*refusal}, nil
	}
	identity, err := onboarding.RegistrationIdentity(request.ConfigPath, request.Registration)
	if err != nil {
		return checkpoint.Manifest{}, nil, err
	}
	if identity != manifest.RegistrationIdentity {
		return manifest, []store.UpgradeFinding{{Code: RestoreRegistrationChanged, Detail: "the registration or the host configuration path differs from the one that created the checkpoint"}}, nil
	}
	return manifest, nil, nil
}

// releaseRefusal refuses a snapshot of a newer schema, or of another
// release, revision, or older schema.
func releaseRefusal(manifest checkpoint.Manifest, build version.Identity) *store.UpgradeFinding {
	switch {
	case manifest.StoreSchemaVersion > store.CurrentSchemaVersion:
		return &store.UpgradeFinding{Code: RestoreSchemaNewer, Detail: fmt.Sprintf("the snapshot has schema %d; this build supports %d", manifest.StoreSchemaVersion, store.CurrentSchemaVersion)}
	case manifest.Factory.Version != build.Version || manifest.Factory.Revision != build.Revision || manifest.StoreSchemaVersion < store.CurrentSchemaVersion:
		return &store.UpgradeFinding{Code: RestoreReleaseMismatch, Detail: fmt.Sprintf("the checkpoint is of %s (%s), schema %d; restore it with that release, then migrate with factory upgrade migrate-store", manifest.Factory.Version, manifest.Factory.Revision, manifest.StoreSchemaVersion)}
	}
	return nil
}

// restoreRefusals checks preconditions 5 to 8 under the coordinator lock and
// the unused work directory. facts are the restore facts of the live store.
func restoreRefusals(ctx context.Context, request RestoreRequest, storePath string, manifest checkpoint.Manifest, facts store.RestoreFacts) []store.UpgradeFinding {
	var refusals []store.UpgradeFinding
	workDirectory := filepath.Join(filepath.Dir(storePath), ".restore-"+manifest.CheckpointID)
	if _, err := os.Lstat(workDirectory); !errors.Is(err, os.ErrNotExist) {
		refusals = append(refusals, store.UpgradeFinding{Code: RestoreWorkDirectoryExists, Detail: workDirectory + " exists; it can hold the original of an earlier restore; move it away after you review it"})
	}
	refusals = append(refusals, liveStoreRefusals(ctx, request, manifest, facts)...)
	divergence := checkpoint.CompareExternalState(ctx, request.External, request.Owned, manifest.ExternalState)
	for _, detail := range divergence.Diverged {
		refusals = append(refusals, store.UpgradeFinding{Code: RestoreExternalDivergence, Detail: detail})
	}
	for _, detail := range divergence.Unverified {
		refusals = append(refusals, store.UpgradeFinding{Code: RestoreDivergenceUnverified, Detail: detail})
	}
	return refusals
}

// liveStoreRefusals checks preconditions 5 and 6. A readable live store
// names the workers to check; otherwise any running worker container
// refuses.
func liveStoreRefusals(ctx context.Context, request RestoreRequest, manifest checkpoint.Manifest, facts store.RestoreFacts) []store.UpgradeFinding {
	if !facts.Readable {
		return append(unattributedWorkerRefusals(ctx, request.Containers), storeFactRefusals(facts)...)
	}
	var refusals []store.UpgradeFinding
	for _, finding := range WorkerActivityFindings(ctx, facts.WorkerProcesses, request.Workers) {
		refusals = append(refusals, store.UpgradeFinding{Code: RestoreWorkerActive, Detail: finding.Code + ": " + finding.Detail})
	}
	refusals = append(refusals, storeFactRefusals(facts)...)
	for _, newer := range facts.Watermarks.NewerThan(manifest.Watermarks) {
		refusals = append(refusals, store.UpgradeFinding{Code: RestoreNotQuiescent, Detail: "store.newer_than_snapshot: the live store has newer " + newer})
	}
	return refusals
}

// unattributedWorkerRefusals checks precondition 5 without a readable live
// store. A worker launched after the checkpoint can still run, and a
// container name, factory-worker- and a hash of the worker ID, does not
// name its installation, so this check is conservative: any running worker
// container refuses, also an idle one or one of another installation.
// Docker is always asked; a failure is unknown evidence and refuses.
func unattributedWorkerRefusals(ctx context.Context, containers WorkerContainerLister) []store.UpgradeFinding {
	if containers == nil {
		return []store.UpgradeFinding{{Code: RestoreWorkerActive, Detail: FindingDockerUnreachable + ": no Docker view lists the running worker containers"}}
	}
	running, err := containers.RunningWorkerContainers(ctx)
	if err != nil {
		return []store.UpgradeFinding{{Code: RestoreWorkerActive, Detail: FindingDockerUnreachable + ": Docker cannot list the running worker containers: " + err.Error()}}
	}
	if len(running) == 0 {
		return nil
	}
	return []store.UpgradeFinding{{Code: RestoreWorkerActive, Detail: "worker.unattributed: the live store does not open, so restore cannot tell whether these running worker containers belong to it; stop them first: " + strings.Join(running, ", ")}}
}

// storeFactRefusals maps the store findings of the restore facts to restore
// refusals.
func storeFactRefusals(facts store.RestoreFacts) []store.UpgradeFinding {
	var refusals []store.UpgradeFinding
	for _, finding := range facts.Findings {
		code := RestoreNotQuiescent
		if strings.HasPrefix(finding.Code, "session_invocation.") {
			code = RestoreWorkerActive
		}
		refusals = append(refusals, store.UpgradeFinding{Code: code, Detail: finding.Code + ": " + finding.Detail})
	}
	return refusals
}

// newRestorePlan returns the plan with its steps and its digest. The digest
// covers the file plan, which holds the size and SHA-256 of each live file
// that digest reads, so any change after the preview needs a new preview. A
// damaged live store gets a step that says why precondition 6 is not
// checked.
func newRestorePlan(request RestoreRequest, storePath string, manifest checkpoint.Manifest, facts store.RestoreFacts, digest checkpoint.FileDigester) (RestorePlan, error) {
	files, err := checkpoint.NewRestorePlan(request.Path, manifest, storePath, digest)
	if err != nil {
		return RestorePlan{}, err
	}
	plan := RestorePlan{Steps: []string{}, PreservedPaths: []string{}, files: files}
	directory := filepath.Dir(storePath)
	if _, err := os.Stat(directory); errors.Is(err, os.ErrNotExist) {
		plan.Steps = append(plan.Steps, "create the missing store directory "+directory+" with mode 0700; the registration must be the one of the checkpoint")
	}
	if facts.Damaged {
		plan.Steps = append(plan.Steps, "replace the damaged live store "+storePath+" without the check for newer records, because it does not open: "+facts.Unreadable)
	}
	journal := store.RestoreJournalPath(storePath)
	plan.Steps = append(plan.Steps,
		"write the restore journal "+journal+" before each step",
		"copy the snapshot to "+filepath.Join(files.WorkDirectory, "new", checkpoint.StoreFile)+" and verify its checksum and integrity",
	)
	for _, name := range files.LiveFiles {
		preserved := filepath.Join(files.WorkDirectory, "original", name)
		plan.PreservedPaths = append(plan.PreservedPaths, preserved)
		plan.Steps = append(plan.Steps, "move "+filepath.Join(directory, name)+" to "+preserved)
	}
	plan.Steps = append(plan.Steps,
		"move the snapshot to "+storePath,
		"verify the schema version, watermarks, checksum, and integrity of the replacement",
		"remove the journal; the original stays in "+filepath.Join(files.WorkDirectory, "original")+" until you remove it",
	)
	if plan.Digest, err = planDigest(plan); err != nil {
		return RestorePlan{}, err
	}
	return plan, nil
}

// planDigest returns the SHA-256 of the file plan and the steps.
func planDigest(plan RestorePlan) (string, error) {
	encoded, err := json.Marshal(struct {
		Files checkpoint.RestorePlan `json:"files"`
		Steps []string               `json:"steps"`
	}{plan.files, plan.Steps})
	if err != nil {
		return "", fmt.Errorf("encode the restore plan: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// applyRestore creates a missing store directory and runs the journaled
// file sequence; release releases the store write lock after the preserve
// renames.
func applyRestore(ctx context.Context, request RestoreRequest, plan RestorePlan, release func() error) error {
	if err := os.MkdirAll(filepath.Dir(plan.files.StorePath), 0o700); err != nil {
		return fmt.Errorf("create the store directory: %w", err)
	}
	return checkpoint.ApplyRestore(ctx, plan.files, checkpointFactory(request.Build), release)
}
