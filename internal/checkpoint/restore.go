package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// RestoreJournalSchemaVersion is the only restore journal schema this build
// reads.
const RestoreJournalSchemaVersion = 1

// Restore journal states (ADR 0032, "Restore plan and interruption"). The
// journal holds the state of the step that runs next; it is written before
// that step.
const (
	// StatePlanned: create the work directory and stage the replacement.
	StatePlanned = "planned"
	// StatePreserving: rename each live file into original/.
	StatePreserving = "preserving"
	// StateReplacing: rename the staged replacement to the live path.
	StateReplacing = "replacing"
	// StateVerifying: verify the replacement at the live path.
	StateVerifying = "verifying"
	// StateVerified: the replacement is verified; only the journal remains.
	StateVerified = "verified"
)

// Continuation results.
const (
	// ContinuationRolledBack means that the original live file set is back.
	ContinuationRolledBack = "rolled_back"
	// ContinuationCompleted means that the verified replacement is live.
	ContinuationCompleted = "completed"
)

// sidecarSuffixes are the SQLite sidecars of a live store, in rename order.
var sidecarSuffixes = []string{"-journal", "-wal", "-shm"}

// Replacement identifies the store snapshot that a restore puts live.
type Replacement struct {
	SHA256        string           `json:"sha256"`
	Size          int64            `json:"size"`
	SchemaVersion int              `json:"schema_version"`
	Watermarks    store.Watermarks `json:"watermarks"`
}

// RestorePlan is the file part of one restore: what moves where.
type RestorePlan struct {
	// CheckpointID is the identifier of the restored checkpoint.
	CheckpointID string `json:"checkpoint_id"`
	// CheckpointPath is the checkpoint directory.
	CheckpointPath string `json:"checkpoint_path"`
	// StorePath is the absolute live store path.
	StorePath string `json:"store"`
	// WorkDirectory is <store directory>/.restore-<checkpoint_id>.
	WorkDirectory string `json:"work_directory"`
	// LiveFiles are the names of the live store and each of its sidecars
	// that exist, the store first.
	LiveFiles []string `json:"live_files"`
	// Replacement identifies the snapshot from the manifest.
	Replacement Replacement `json:"replacement"`
}

// RestoreJournal is <store>.restore.json: the plan and the state of the step
// that runs next.
type RestoreJournal struct {
	SchemaVersion int    `json:"schema_version"`
	State         string `json:"state"`
	// Factory is the build that wrote the journal; only the same release
	// continues it.
	Factory Factory `json:"factory"`
	RestorePlan
}

// RestoreContinuation reports one continuation of an interrupted restore.
type RestoreContinuation struct {
	// State is the journal state that the continuation found.
	State string `json:"state"`
	// Result is ContinuationRolledBack or ContinuationCompleted.
	Result string `json:"result"`
	// Journal is the journal that the continuation read.
	Journal RestoreJournal `json:"-"`
}

// NewRestorePlan returns the plan to put the store snapshot of the verified
// checkpoint at directory live at storePath. It lists the live file set that
// exists now.
func NewRestorePlan(directory string, manifest Manifest, storePath string) (RestorePlan, error) {
	absolute, err := filepath.Abs(storePath)
	if err != nil {
		return RestorePlan{}, fmt.Errorf("resolve the store path: %w", err)
	}
	checkpointPath, err := filepath.Abs(directory)
	if err != nil {
		return RestorePlan{}, fmt.Errorf("resolve the checkpoint path: %w", err)
	}
	plan := RestorePlan{
		CheckpointID:   manifest.CheckpointID,
		CheckpointPath: checkpointPath,
		StorePath:      absolute,
		WorkDirectory:  filepath.Join(filepath.Dir(absolute), ".restore-"+manifest.CheckpointID),
		LiveFiles:      []string{},
		Replacement:    Replacement{SchemaVersion: manifest.StoreSchemaVersion, Watermarks: manifest.Watermarks},
	}
	for _, file := range manifest.Files {
		if file.Path == StoreFile {
			plan.Replacement.SHA256, plan.Replacement.Size = file.SHA256, file.Size
		}
	}
	for _, suffix := range append([]string{""}, sidecarSuffixes...) {
		name := filepath.Base(absolute) + suffix
		_, err := os.Lstat(filepath.Join(filepath.Dir(absolute), name))
		switch {
		case err == nil:
			plan.LiveFiles = append(plan.LiveFiles, name)
		case !errors.Is(err, os.ErrNotExist):
			return RestorePlan{}, fmt.Errorf("inspect the live file %s: %w", name, err)
		}
	}
	return plan, nil
}

// ReadRestoreJournal reads the restore journal of the store at storePath. It
// returns an error that wraps os.ErrNotExist when no journal exists.
func ReadRestoreJournal(storePath string) (RestoreJournal, error) {
	content, err := os.ReadFile(store.RestoreJournalPath(storePath))
	if err != nil {
		return RestoreJournal{}, fmt.Errorf("read the restore journal: %w", err)
	}
	var journal RestoreJournal
	if err := json.Unmarshal(content, &journal); err != nil {
		return RestoreJournal{}, fmt.Errorf("decode the restore journal: %w", err)
	}
	if journal.SchemaVersion != RestoreJournalSchemaVersion {
		return RestoreJournal{}, fmt.Errorf("the restore journal has schema %d; this build reads %d", journal.SchemaVersion, RestoreJournalSchemaVersion)
	}
	return journal, nil
}

// crashPoint lets a test stop a restore at one named point, as a process
// crash does: an error from it stops the restore at once, without cleanup.
// Production uses nil, which never stops. export_test.go sets it.
type crashPoint func(point string) error

// restorer runs one restore with an optional crash point.
type restorer struct {
	journal RestoreJournal
	crash   crashPoint
}

// ApplyRestore runs the restore plan of ADR 0032. It writes the journal
// before each step, stages and verifies the replacement in new/, renames
// each live file into original/, renames the replacement into place, and
// verifies it. On a failed step, such as a replacement that does not
// verify, it rolls back, so the original live file set is back. A crash
// leaves the journal for ContinueRestore. The caller holds the coordinator
// lock.
func ApplyRestore(ctx context.Context, plan RestorePlan, build Factory) error {
	return applyRestore(ctx, plan, build, nil)
}

// applyRestore is ApplyRestore with a crash point; crash is nil outside
// tests.
func applyRestore(ctx context.Context, plan RestorePlan, build Factory, crash crashPoint) error {
	return (&restorer{journal: RestoreJournal{SchemaVersion: RestoreJournalSchemaVersion, Factory: build, RestorePlan: plan}, crash: crash}).apply(ctx)
}

// apply runs each step of the restore after its journal write.
func (r *restorer) apply(ctx context.Context) error {
	steps := []struct {
		state string
		run   func(context.Context) error
	}{
		{StatePlanned, r.stage},
		{StatePreserving, r.preserve},
		{StateReplacing, r.replace},
		{StateVerifying, r.verify},
	}
	for _, step := range steps {
		if err := r.enter(step.state); err != nil {
			return err
		}
		err := step.run(ctx)
		if err != nil && !errors.Is(err, errStopped) {
			if rollbackErr := r.rollback(); rollbackErr != nil {
				return errors.Join(err, rollbackErr)
			}
			return err
		}
		if err != nil {
			return err
		}
	}
	if err := r.enter(StateVerified); err != nil {
		return err
	}
	return r.finish()
}

// errStopped marks a stop at a crash point, after which nothing runs.
var errStopped = errors.New("the restore stopped at a crash point")

// at stops the restore at point when the crash point says so.
func (r *restorer) at(point string) error {
	if r.crash == nil {
		return nil
	}
	if err := r.crash(point); err != nil {
		return fmt.Errorf("%w: %w", errStopped, err)
	}
	return nil
}

// enter writes the journal with state, then passes the crash point after
// that write.
func (r *restorer) enter(state string) error {
	r.journal.State = state
	if err := writeJournal(r.journal); err != nil {
		return err
	}
	return r.at("journal " + state)
}

// stage creates the work directory, copies the snapshot to new/, and
// verifies the copy against the manifest.
func (r *restorer) stage(ctx context.Context) error {
	plan := r.journal.RestorePlan
	if err := os.Mkdir(plan.WorkDirectory, 0o700); err != nil {
		return fmt.Errorf("create the restore work directory: %w", err)
	}
	for _, name := range []string{"new", "original"} {
		if err := os.Mkdir(filepath.Join(plan.WorkDirectory, name), 0o700); err != nil {
			return fmt.Errorf("create the restore work directory: %w", err)
		}
	}
	staged := filepath.Join(plan.WorkDirectory, "new", StoreFile)
	if err := copyFile(filepath.Join(plan.CheckpointPath, StoreFile), staged); err != nil {
		return err
	}
	if err := syncFile(staged); err != nil {
		return err
	}
	if err := syncDirectories(filepath.Join(plan.WorkDirectory, "new"), plan.WorkDirectory, filepath.Dir(plan.StorePath)); err != nil {
		return err
	}
	if err := verifyReplacement(ctx, staged, plan.Replacement); err != nil {
		return err
	}
	return r.at("after stage")
}

// preserve renames each live file into original/, the store first, and
// syncs both directories after each rename.
func (r *restorer) preserve(context.Context) error {
	plan := r.journal.RestorePlan
	for _, name := range plan.LiveFiles {
		if err := r.at("before preserve " + name); err != nil {
			return err
		}
		if err := renameDurably(filepath.Join(filepath.Dir(plan.StorePath), name), filepath.Join(plan.WorkDirectory, "original", name)); err != nil {
			return err
		}
		if err := r.at("after preserve " + name); err != nil {
			return err
		}
	}
	return nil
}

// replace renames the staged replacement to the live store path.
func (r *restorer) replace(context.Context) error {
	plan := r.journal.RestorePlan
	name := filepath.Base(plan.StorePath)
	if err := r.at("before replace " + name); err != nil {
		return err
	}
	if err := renameDurably(filepath.Join(plan.WorkDirectory, "new", StoreFile), plan.StorePath); err != nil {
		return err
	}
	return r.at("after replace " + name)
}

// verify checks the replacement at the live path.
func (r *restorer) verify(ctx context.Context) error {
	return verifyReplacement(ctx, r.journal.StorePath, r.journal.Replacement)
}

// finish removes the journal of a verified restore and the empty new/. The
// original stays in original/ until the operator removes it.
func (r *restorer) finish() error {
	plan := r.journal.RestorePlan
	if err := os.RemoveAll(filepath.Join(plan.WorkDirectory, "new")); err != nil {
		return fmt.Errorf("remove the staged replacement directory: %w", err)
	}
	return removeJournal(plan.StorePath)
}

// ContinueRestore continues the interrupted restore of the store at
// storePath as its journal state says: before verified, it puts the
// original live file set back unchanged; in verifying, it repeats the
// verification and rolls back on failure; in verified, it removes the
// journal. It returns an error that wraps os.ErrNotExist when no journal
// exists. The caller holds the coordinator lock and runs the release that
// wrote the journal.
func ContinueRestore(ctx context.Context, storePath string) (RestoreContinuation, error) {
	return continueRestore(ctx, storePath, nil)
}

// continueRestore is ContinueRestore with a crash point; crash is nil
// outside tests.
func continueRestore(ctx context.Context, storePath string, crash crashPoint) (RestoreContinuation, error) {
	absolute, err := filepath.Abs(storePath)
	if err != nil {
		return RestoreContinuation{}, fmt.Errorf("resolve the store path: %w", err)
	}
	journal, err := ReadRestoreJournal(absolute)
	if err != nil {
		return RestoreContinuation{}, err
	}
	continuation := RestoreContinuation{State: journal.State, Journal: journal}
	r := &restorer{journal: journal, crash: crash}
	switch journal.State {
	case StateVerified:
		continuation.Result = ContinuationCompleted
		return continuation, r.finish()
	case StateVerifying:
		if verifyReplacement(ctx, journal.StorePath, journal.Replacement) == nil {
			if err := r.enter(StateVerified); err != nil {
				return continuation, err
			}
			continuation.Result = ContinuationCompleted
			return continuation, r.finish()
		}
	case StatePlanned, StatePreserving, StateReplacing:
	default:
		return continuation, fmt.Errorf("the restore journal has the unknown state %q", journal.State)
	}
	continuation.Result = ContinuationRolledBack
	return continuation, r.rollback()
}

// rollback puts the original live file set back and removes every restore
// file. Before it removes a file at the live store path, it proves that the
// complete original is in original/, so it never removes an original.
func (r *restorer) rollback() error {
	plan := r.journal.RestorePlan
	original := filepath.Join(plan.WorkDirectory, "original")
	if r.journal.State == StateReplacing || r.journal.State == StateVerifying {
		for _, name := range plan.LiveFiles {
			if _, err := os.Lstat(filepath.Join(original, name)); err != nil {
				return fmt.Errorf("the original %s is not in %s; nothing was removed: %w", name, original, err)
			}
		}
		if err := os.Remove(plan.StorePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove the replacement at the live path: %w", err)
		}
	}
	for _, name := range plan.LiveFiles {
		preserved := filepath.Join(original, name)
		if _, err := os.Lstat(preserved); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := renameDurably(preserved, filepath.Join(filepath.Dir(plan.StorePath), name)); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(filepath.Join(plan.WorkDirectory, "new")); err != nil {
		return fmt.Errorf("remove the staged replacement directory: %w", err)
	}
	for _, directory := range []string{original, plan.WorkDirectory} {
		if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", directory, err)
		}
	}
	return removeJournal(plan.StorePath)
}

// verifyReplacement runs the integrity check on path, reads its schema
// version and watermarks, and compares them and its SHA-256 with the
// manifest. It opens the file immutable, so it creates no sidecar.
func verifyReplacement(ctx context.Context, path string, expected Replacement) error {
	inspection, err := store.InspectSnapshot(ctx, path)
	if err != nil {
		return fmt.Errorf("verify the replacement store: %w", err)
	}
	switch {
	case inspection.File.SHA256 != expected.SHA256 || inspection.File.Size != expected.Size:
		return errors.New("the replacement store does not match the checksum of the manifest")
	case inspection.File.SchemaVersion != expected.SchemaVersion:
		return fmt.Errorf("the replacement store has schema %d; the manifest records %d", inspection.File.SchemaVersion, expected.SchemaVersion)
	case inspection.Watermarks != expected.Watermarks:
		return errors.New("the replacement store does not have the watermarks of the manifest")
	}
	return nil
}

// writeJournal writes the journal to a temporary file, syncs it, renames it
// into place, and syncs the store directory.
func writeJournal(journal RestoreJournal) error {
	encoded, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the restore journal: %w", err)
	}
	path := store.RestoreJournalPath(journal.StorePath)
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create the restore journal: %w", err)
	}
	_, err = file.Write(append(encoded, '\n'))
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write the restore journal: %w", err)
	}
	return renameDurably(temporary, path)
}

// removeJournal removes the restore journal and syncs the store directory.
func removeJournal(storePath string) error {
	if err := os.Remove(store.RestoreJournalPath(storePath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the restore journal: %w", err)
	}
	return syncDirectories(filepath.Dir(storePath))
}

// renameDurably renames source to destination and syncs both directories.
func renameDurably(source, destination string) error {
	if err := os.Rename(source, destination); err != nil {
		return fmt.Errorf("rename %s to %s: %w", source, destination, err)
	}
	return syncDirectories(filepath.Dir(source), filepath.Dir(destination))
}

// syncFile flushes one file to disk.
func syncFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s to sync it: %w", path, err)
	}
	err = file.Sync()
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	return nil
}

// syncDirectories flushes the entries of each directory to disk.
func syncDirectories(directories ...string) error {
	for _, directory := range directories {
		if err := syncFile(directory); err != nil {
			return err
		}
	}
	return nil
}
