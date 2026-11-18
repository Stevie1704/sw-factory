package checkpoint

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Match is the registration and store schema that a checkpoint must carry
// to be offered in an upgrade plan.
type Match struct {
	// RegistrationIdentity is onboarding.RegistrationIdentity of the
	// installation.
	RegistrationIdentity string
	// StoreSchemaVersion is the schema of the live store. Only the release
	// that created a checkpoint restores it, and that release reads only its
	// own schema, so a checkpoint of another schema cannot be restored
	// before the store migration.
	StoreSchemaVersion int
}

// Listed is a checkpoint directory that verifies, with its manifest.
type Listed struct {
	Path     string   `json:"path"`
	Manifest Manifest `json:"manifest"`
}

// FindNewest returns the newest checkpoint that matches, among the direct
// subdirectories of root. It does not search deeper, and it keeps no
// registry: the operator names the directory. A subdirectory that does not
// verify is not a checkpoint and is skipped; it never counts as a match. It
// returns nil when no checkpoint matches, and an error only when root cannot
// be read or the context ends.
func FindNewest(ctx context.Context, root string, match Match) (*Listed, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read checkpoint directory: %w", err)
	}
	var newest *Listed
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		listed, ok, err := matchingCheckpoint(ctx, filepath.Join(root, entry.Name()), match)
		if err != nil {
			return nil, err
		}
		if ok && (newest == nil || listed.Manifest.CreatedAt.After(newest.Manifest.CreatedAt)) {
			newest = &listed
		}
	}
	return newest, nil
}

// matchingCheckpoint verifies one candidate directory and reports whether its
// manifest carries the registration and store schema of match.
func matchingCheckpoint(ctx context.Context, directory string, match Match) (Listed, bool, error) {
	manifest, err := Verify(ctx, directory)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Listed{}, false, ctxErr
	}
	if err != nil {
		return Listed{}, false, nil
	}
	if manifest.RegistrationIdentity != match.RegistrationIdentity || manifest.StoreSchemaVersion != match.StoreSchemaVersion {
		return Listed{}, false, nil
	}
	return Listed{Path: directory, Manifest: manifest}, true, nil
}
