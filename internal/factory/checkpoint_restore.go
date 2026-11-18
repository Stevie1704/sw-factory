package factory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// crash stops the restore at a named point; a test sets it.
	crash checkpoint.CrashPoint
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
	if refusals, err = restoreRefusals(ctx, request, storePath, manifest); err != nil || len(refusals) != 0 {
		return refusedRestore(refusals), err
	}
	plan, err := newRestorePlan(request, storePath, manifest)
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
	if err := applyRestore(ctx, request, plan); err != nil {
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
// the unused work directory.
func restoreRefusals(ctx context.Context, request RestoreRequest, storePath string, manifest checkpoint.Manifest) ([]store.UpgradeFinding, error) {
	var refusals []store.UpgradeFinding
	workDirectory := filepath.Join(filepath.Dir(storePath), ".restore-"+manifest.CheckpointID)
	if _, err := os.Lstat(workDirectory); !errors.Is(err, os.ErrNotExist) {
		refusals = append(refusals, store.UpgradeFinding{Code: RestoreWorkDirectoryExists, Detail: workDirectory + " exists; it can hold the original of an earlier restore; move it away after you review it"})
	}
	local, err := liveStoreRefusals(ctx, request, storePath, manifest)
	if err != nil {
		return nil, err
	}
	refusals = append(refusals, local...)
	divergence := checkpoint.CompareExternalState(ctx, request.External, request.Owned, manifest.ExternalState)
	for _, detail := range divergence.Diverged {
		refusals = append(refusals, store.UpgradeFinding{Code: RestoreExternalDivergence, Detail: detail})
	}
	for _, detail := range divergence.Unverified {
		refusals = append(refusals, store.UpgradeFinding{Code: RestoreDivergenceUnverified, Detail: detail})
	}
	return refusals, nil
}

// liveStoreRefusals checks preconditions 5 and 6. The workers come from the
// live store, or from the snapshot when the live store is not readable.
func liveStoreRefusals(ctx context.Context, request RestoreRequest, storePath string, manifest checkpoint.Manifest) ([]store.UpgradeFinding, error) {
	facts, err := store.InspectForRestore(ctx, storePath)
	if err != nil {
		return nil, err
	}
	processes := facts.WorkerProcesses
	if !facts.Readable {
		if processes, err = store.ReadSnapshotWorkerProcesses(ctx, filepath.Join(request.Path, checkpoint.StoreFile)); err != nil {
			return nil, err
		}
	}
	var refusals []store.UpgradeFinding
	for _, finding := range WorkerActivityFindings(ctx, processes, request.Workers) {
		refusals = append(refusals, store.UpgradeFinding{Code: RestoreWorkerActive, Detail: finding.Code + ": " + finding.Detail})
	}
	for _, finding := range facts.Findings {
		code := RestoreNotQuiescent
		if strings.HasPrefix(finding.Code, "session_invocation.") {
			code = RestoreWorkerActive
		}
		refusals = append(refusals, store.UpgradeFinding{Code: code, Detail: finding.Code + ": " + finding.Detail})
	}
	if facts.Readable {
		for _, newer := range facts.Watermarks.NewerThan(manifest.Watermarks) {
			refusals = append(refusals, store.UpgradeFinding{Code: RestoreNotQuiescent, Detail: "store.newer_than_snapshot: the live store has newer " + newer})
		}
	}
	return refusals, nil
}

// newRestorePlan returns the plan with its steps and its digest. The digest
// covers the file plan and the size and SHA-256 of each live file, so any
// change after the preview needs a new preview.
func newRestorePlan(request RestoreRequest, storePath string, manifest checkpoint.Manifest) (RestorePlan, error) {
	files, err := checkpoint.NewRestorePlan(request.Path, manifest, storePath)
	if err != nil {
		return RestorePlan{}, err
	}
	plan := RestorePlan{Steps: []string{}, PreservedPaths: []string{}, files: files}
	directory := filepath.Dir(storePath)
	if _, err := os.Stat(directory); errors.Is(err, os.ErrNotExist) {
		plan.Steps = append(plan.Steps, "create the missing store directory "+directory+" with mode 0700; the registration must be the one of the checkpoint")
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

// liveFileIdentity is the size and SHA-256 of one live file.
type liveFileIdentity struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// planDigest returns the SHA-256 of the plan and the live file identities.
func planDigest(plan RestorePlan) (string, error) {
	identities := make([]liveFileIdentity, 0, len(plan.files.LiveFiles))
	for _, name := range plan.files.LiveFiles {
		size, digest, err := fileIdentity(filepath.Join(filepath.Dir(plan.files.StorePath), name))
		if err != nil {
			return "", err
		}
		identities = append(identities, liveFileIdentity{Name: name, Size: size, SHA256: digest})
	}
	encoded, err := json.Marshal(struct {
		Files checkpoint.RestorePlan `json:"files"`
		Steps []string               `json:"steps"`
		Live  []liveFileIdentity     `json:"live"`
	}{plan.files, plan.Steps, identities})
	if err != nil {
		return "", fmt.Errorf("encode the restore plan: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// fileIdentity returns the size and the hexadecimal SHA-256 of one file.
func fileIdentity(path string) (int64, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, "", fmt.Errorf("open %s for its checksum: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return 0, "", fmt.Errorf("read %s for its checksum: %w", path, err)
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

// applyRestore creates a missing store directory and runs the journaled
// file sequence.
func applyRestore(ctx context.Context, request RestoreRequest, plan RestorePlan) error {
	if err := os.MkdirAll(filepath.Dir(plan.files.StorePath), 0o700); err != nil {
		return fmt.Errorf("create the store directory: %w", err)
	}
	build := checkpoint.Factory{Version: request.Build.Version, Build: request.Build.Build, Revision: request.Build.Revision, Modified: request.Build.Modified}
	return checkpoint.ApplyRestore(ctx, plan.files, build, request.crash)
}
