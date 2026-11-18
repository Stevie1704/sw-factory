// Package checkpoint holds the maintenance checkpoint of ADR 0032: its
// manifest (schema 1), the manifest verification, and the observed state of
// the external objects that a checkpoint knows. Creating a checkpoint and
// restoring one both use these types; neither redefines them.
package checkpoint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

const (
	// ManifestSchemaVersion is the only manifest schema this build reads.
	ManifestSchemaVersion = 1
	// Kind is the kind of every checkpoint manifest.
	Kind = "maintenance_checkpoint"
	// StoreFile is the name of the store snapshot in a checkpoint.
	StoreFile = "store.db"
	// ManifestFile is the name of the manifest in a checkpoint.
	ManifestFile = "manifest.json"
	// privateDataReason says why a checkpoint is private data.
	privateDataReason = "the snapshot holds issue and specification text"
)

// Verification refusal codes. Restore reports them unchanged.
const (
	// CodeIncomplete reports a missing manifest or a missing listed file.
	CodeIncomplete = "restore.checkpoint_incomplete"
	// CodeCorrupt reports a checksum, integrity, file set, or manifest-schema
	// failure.
	CodeCorrupt = "restore.checkpoint_corrupt"
)

// Manifest is manifest.json of a checkpoint, schema 1.
type Manifest struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	// CheckpointID is a random identifier.
	CheckpointID string `json:"checkpoint_id"`
	// CreatedAt is the instant of the snapshot transaction.
	CreatedAt time.Time `json:"created_at"`
	// Factory identifies the creating build.
	Factory Factory `json:"factory"`
	// StoreSchemaVersion is the schema version read from the snapshot.
	StoreSchemaVersion int `json:"store_schema_version"`
	// RegistrationIdentity is the SHA-256 identity of the host configuration
	// path and the registration, as the onboarding plan computes it.
	RegistrationIdentity string `json:"registration_identity"`
	// RegistrationSummary is the non-secret summary of the registration.
	RegistrationSummary RegistrationSummary `json:"registration_summary"`
	// WorkerImage is the pinned image@digest.
	WorkerImage string `json:"worker_image"`
	// TargetBranchHead is the remote head of the target branch.
	TargetBranchHead string `json:"target_branch_head"`
	// Watermarks are the highest identities that the snapshot knows.
	Watermarks store.Watermarks `json:"watermarks"`
	// ExternalState is the observed state of every known external object.
	ExternalState ExternalState `json:"external_state"`
	// Files lists every checkpoint file except the manifest itself.
	Files []File `json:"files"`
	// PrivateData marks the checkpoint as private data.
	PrivateData PrivateData `json:"private_data"`
}

// Factory identifies the build that created a checkpoint.
type Factory struct {
	Version  string `json:"version"`
	Build    string `json:"build"`
	Revision string `json:"revision"`
	Modified bool   `json:"modified"`
}

// RegistrationSummary is the non-secret part of a registration. It holds no
// credential path.
type RegistrationSummary struct {
	RepositoryPath string `json:"repository_path"`
	Tracker        string `json:"tracker"`
	CodeHost       string `json:"code_host"`
	Owner          string `json:"owner"`
	Project        string `json:"project,omitempty"`
	Repository     string `json:"repository"`
	InstallationID string `json:"installation_id,omitempty"`
}

// File is one checkpoint file with its identity.
type File struct {
	// Path is the path relative to the checkpoint directory.
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Mode   string `json:"mode"`
	SHA256 string `json:"sha256"`
}

// PrivateData says that a checkpoint is private data and why.
type PrivateData struct {
	Private bool   `json:"private"`
	Reason  string `json:"reason"`
}

// NewManifest returns the manifest of a verified store snapshot, with the
// fields that the snapshot decides. The caller sets the build, the
// registration, the worker image, and the external state.
func NewManifest(id string, createdAt time.Time, snapshot store.SnapshotInspection) Manifest {
	return Manifest{
		SchemaVersion:      ManifestSchemaVersion,
		Kind:               Kind,
		CheckpointID:       id,
		CreatedAt:          createdAt.UTC(),
		StoreSchemaVersion: snapshot.File.SchemaVersion,
		Watermarks:         snapshot.Watermarks,
		Files:              []File{{Path: StoreFile, Size: snapshot.File.Size, Mode: "0600", SHA256: snapshot.File.SHA256}},
		PrivateData:        PrivateData{Private: true, Reason: privateDataReason},
	}
}

// WriteManifest writes manifest.json with mode 0600 into directory and
// syncs it. It never replaces an existing manifest.
func WriteManifest(directory string, manifest Manifest) error {
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode checkpoint manifest: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(directory, ManifestFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create checkpoint manifest: %w", err)
	}
	_, err = file.Write(append(encoded, '\n'))
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write checkpoint manifest: %w", err)
	}
	return nil
}

// VerificationError is a typed refusal of a checkpoint that does not verify.
type VerificationError struct {
	// Code is CodeIncomplete or CodeCorrupt.
	Code string
	// Detail names what failed.
	Detail string
}

// Error returns the code and the detail.
func (e *VerificationError) Error() string {
	return e.Code + ": " + e.Detail
}

// Verify checks a checkpoint directory without changing it: the manifest
// schema and kind, the exact file set, the size, mode, and SHA-256 of every
// listed file, and PRAGMA integrity_check and the schema version of the
// store snapshot on a temporary read-only copy. It returns the manifest of
// a checkpoint that verifies, or a *VerificationError.
func Verify(ctx context.Context, directory string) (Manifest, error) {
	manifest, err := readManifest(directory)
	if err != nil {
		return Manifest{}, err
	}
	if err := verifyFileSet(directory, manifest); err != nil {
		return Manifest{}, err
	}
	for _, file := range manifest.Files {
		if err := verifyFile(directory, file); err != nil {
			return Manifest{}, err
		}
	}
	if err := verifySnapshotCopy(ctx, directory, manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// readManifest reads and strictly decodes manifest.json.
func readManifest(directory string) (Manifest, error) {
	content, err := os.ReadFile(filepath.Join(directory, ManifestFile))
	if errors.Is(err, os.ErrNotExist) {
		return Manifest{}, &VerificationError{Code: CodeIncomplete, Detail: "the checkpoint has no " + ManifestFile}
	}
	if err != nil {
		return Manifest{}, fmt.Errorf("read checkpoint manifest: %w", err)
	}
	var version struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(content, &version); err != nil || version.SchemaVersion != ManifestSchemaVersion {
		return Manifest{}, &VerificationError{Code: CodeCorrupt, Detail: fmt.Sprintf("the manifest schema is not %d", ManifestSchemaVersion)}
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, &VerificationError{Code: CodeCorrupt, Detail: "the manifest does not decode: " + err.Error()}
	}
	if manifest.Kind != Kind {
		return Manifest{}, &VerificationError{Code: CodeCorrupt, Detail: "the manifest kind is not " + Kind}
	}
	if !slices.ContainsFunc(manifest.Files, func(file File) bool { return file.Path == StoreFile }) {
		return Manifest{}, &VerificationError{Code: CodeCorrupt, Detail: "the manifest does not list " + StoreFile}
	}
	return manifest, nil
}

// verifyFileSet refuses a missing listed file and any file that the
// manifest does not list.
func verifyFileSet(directory string, manifest Manifest) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("list checkpoint directory: %w", err)
	}
	present := map[string]bool{}
	for _, entry := range entries {
		present[entry.Name()] = true
	}
	expected := map[string]bool{ManifestFile: true}
	for _, file := range manifest.Files {
		if file.Path == ManifestFile || file.Path != filepath.Base(file.Path) || strings.HasPrefix(file.Path, ".") {
			return &VerificationError{Code: CodeCorrupt, Detail: fmt.Sprintf("the manifest lists the invalid file path %q", file.Path)}
		}
		if !present[file.Path] {
			return &VerificationError{Code: CodeIncomplete, Detail: "the checkpoint has no " + file.Path}
		}
		expected[file.Path] = true
	}
	for name := range present {
		if !expected[name] {
			return &VerificationError{Code: CodeCorrupt, Detail: "the checkpoint holds the unlisted file " + name}
		}
	}
	return nil
}

// verifyFile compares one file with its manifest entry.
func verifyFile(directory string, file File) error {
	path := filepath.Join(directory, file.Path)
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect checkpoint file %s: %w", file.Path, err)
	}
	if !info.Mode().IsRegular() || fmt.Sprintf("%04o", info.Mode().Perm()) != file.Mode {
		return &VerificationError{Code: CodeCorrupt, Detail: fmt.Sprintf("%s is not a regular file with mode %s", file.Path, file.Mode)}
	}
	size, digest, err := store.FileDigest(path)
	if err != nil {
		return err
	}
	if size != file.Size || digest != file.SHA256 {
		return &VerificationError{Code: CodeCorrupt, Detail: "the checksum of " + file.Path + " does not match the manifest"}
	}
	return nil
}

// verifySnapshotCopy copies the store snapshot to a private temporary
// directory and checks the copy, so the check cannot change the checkpoint.
func verifySnapshotCopy(ctx context.Context, directory string, manifest Manifest) error {
	temporary, err := os.MkdirTemp("", "factory-checkpoint-verify-")
	if err != nil {
		return fmt.Errorf("create the verification directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(temporary) }()
	copied := filepath.Join(temporary, StoreFile)
	if err := copyFile(filepath.Join(directory, StoreFile), copied); err != nil {
		return err
	}
	inspection, err := store.InspectSnapshot(ctx, copied)
	if err != nil {
		return &VerificationError{Code: CodeCorrupt, Detail: "the store snapshot does not verify: " + err.Error()}
	}
	if inspection.File.SchemaVersion != manifest.StoreSchemaVersion {
		return &VerificationError{Code: CodeCorrupt, Detail: fmt.Sprintf("the store snapshot has schema %d; the manifest records %d", inspection.File.SchemaVersion, manifest.StoreSchemaVersion)}
	}
	return nil
}

// copyFile copies source to a new 0600 file at destination.
func copyFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open %s: %w", source, err)
	}
	defer func() { _ = input.Close() }()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", destination, err)
	}
	_, err = io.Copy(output, input)
	if closeErr := output.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("copy %s: %w", source, err)
	}
	return nil
}
