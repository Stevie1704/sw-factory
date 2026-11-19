package checkpoint

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Release names a Factory release by version and revision. Only the release
// with this version and revision restores a checkpoint that it created.
type Release struct {
	Version  string `json:"version"`
	Revision string `json:"revision"`
}

// Match is the registration and store schema that a checkpoint must carry
// to be offered in an upgrade plan, and the installed release that the plan
// prefers.
type Match struct {
	// RegistrationIdentity is onboarding.RegistrationIdentity of the
	// installation.
	RegistrationIdentity string
	// StoreSchemaVersion is the schema of the live store. Only the release
	// that created a checkpoint restores it, and that release reads only its
	// own schema, so a checkpoint of another schema cannot be restored
	// before the store migration.
	StoreSchemaVersion int
	// InstalledRelease is the release that the installation runs before the
	// upgrade, or empty when it is not known. A checkpoint of this release
	// wins over a newer one of another release with the same schema.
	InstalledRelease Release
}

// Listed is a checkpoint directory that verifies, with its manifest.
type Listed struct {
	Path     string   `json:"path"`
	Manifest Manifest `json:"manifest"`
	// RestoreRelease is the only release that restores the checkpoint: the
	// one that created it.
	RestoreRelease Release `json:"restore_release"`
	// OfInstalledRelease reports that RestoreRelease is the installed
	// release of the match.
	OfInstalledRelease bool `json:"of_installed_release"`
}

// FindNewest returns the newest checkpoint that matches, among the direct
// subdirectories of root. A checkpoint of the installed release wins; only
// when none exists is the newest checkpoint of the schema returned, and
// RestoreRelease names the other release that restores it. It does not
// search deeper, and it keeps no registry: the operator names the
// directory. A subdirectory that does not verify is not a checkpoint and is
// skipped; it never counts as a match. It returns nil when no checkpoint
// matches, and an error only when root cannot be read or the context ends.
func FindNewest(ctx context.Context, root string, match Match) (*Listed, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read checkpoint directory: %w", err)
	}
	var newest, newestInstalled *Listed
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		listed, ok, err := matchingCheckpoint(ctx, filepath.Join(root, entry.Name()), match)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		newest = newerListed(newest, listed)
		if listed.OfInstalledRelease {
			newestInstalled = newerListed(newestInstalled, listed)
		}
	}
	if newestInstalled != nil {
		return newestInstalled, nil
	}
	return newest, nil
}

// newerListed returns the newer of current and candidate by creation time.
func newerListed(current *Listed, candidate Listed) *Listed {
	if current == nil || candidate.Manifest.CreatedAt.After(current.Manifest.CreatedAt) {
		return &candidate
	}
	return current
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
	restore := Release{Version: manifest.Factory.Version, Revision: manifest.Factory.Revision}
	installed := match.InstalledRelease.Version != "" && restore == match.InstalledRelease
	return Listed{Path: directory, Manifest: manifest, RestoreRelease: restore, OfInstalledRelease: installed}, true, nil
}
