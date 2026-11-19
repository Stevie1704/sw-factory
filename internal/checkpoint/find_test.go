package checkpoint_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/checkpoint"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// placeCheckpoint moves a sealed checkpoint to name below parent, after
// edit changed its manifest. The manifest is not checksummed, so the
// sealed files still verify.
func placeCheckpoint(t *testing.T, parent, name string, edit func(*checkpoint.Manifest)) string {
	t.Helper()
	directory := sealedCheckpoint(t)
	manifestPath := filepath.Join(directory, checkpoint.ManifestFile)
	content, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest checkpoint.Manifest
	if err := json.Unmarshal(content, &manifest); err != nil {
		t.Fatal(err)
	}
	edit(&manifest)
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.WriteManifest(directory, manifest); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(parent, name)
	if err := os.Rename(directory, destination); err != nil {
		t.Fatal(err)
	}
	return destination
}

// TestFindNewestPicksTheNewestMatchingCheckpoint verifies that the scan
// returns the newest checkpoint of the registration and store schema, of
// any release, and skips a checkpoint of another registration or schema and
// a damaged one.
func TestFindNewestPicksTheNewestMatchingCheckpoint(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	match := checkpoint.Match{RegistrationIdentity: "sha256:mine", StoreSchemaVersion: store.CurrentSchemaVersion}
	mine := func(at int64) func(*checkpoint.Manifest) {
		return func(m *checkpoint.Manifest) {
			m.RegistrationIdentity, m.Factory.Version, m.Factory.Revision = "sha256:mine", "v1.3.0", "abc123"
			m.CreatedAt = time.Unix(at, 0).UTC()
		}
	}
	older := placeCheckpoint(t, parent, "older", mine(100))
	newest := placeCheckpoint(t, parent, "newest", func(m *checkpoint.Manifest) {
		mine(300)(m)
		m.Factory.Version = "v1.2.0"
	})
	placeCheckpoint(t, parent, "other-registration", func(m *checkpoint.Manifest) {
		mine(500)(m)
		m.RegistrationIdentity = "sha256:other"
	})
	placeCheckpoint(t, parent, "other-schema", func(m *checkpoint.Manifest) {
		mine(600)(m)
		m.StoreSchemaVersion = store.CurrentSchemaVersion - 1
	})
	damaged := placeCheckpoint(t, parent, "damaged", mine(700))
	corruptStore(t, damaged)

	listed, err := checkpoint.FindNewest(context.Background(), parent, match)
	if err != nil || listed == nil {
		t.Fatalf("FindNewest() = %v, %v; want the newest matching checkpoint", listed, err)
	}
	if listed.Path != newest {
		t.Fatalf("FindNewest() path = %s, want %s (older was %s)", listed.Path, newest, older)
	}
}

// TestFindNewestPrefersTheCheckpointOfTheInstalledRelease verifies that,
// among checkpoints of the same registration and schema, the newest one of
// the installed release and revision wins over a newer one of another
// release, and that without such a checkpoint the newest schema match is
// returned and marked as not of the installed release.
func TestFindNewestPrefersTheCheckpointOfTheInstalledRelease(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	release := func(version, revision string, at int64) func(*checkpoint.Manifest) {
		return func(m *checkpoint.Manifest) {
			m.RegistrationIdentity, m.Factory.Version, m.Factory.Revision = "sha256:mine", version, revision
			m.CreatedAt = time.Unix(at, 0).UTC()
		}
	}
	installed := placeCheckpoint(t, parent, "installed", release("v1.3.0", "abc123", 100))
	placeCheckpoint(t, parent, "other-revision", release("v1.3.0", "def456", 200))
	newerOtherRelease := placeCheckpoint(t, parent, "other-release", release("v1.2.0", "abc123", 300))
	match := checkpoint.Match{RegistrationIdentity: "sha256:mine", StoreSchemaVersion: store.CurrentSchemaVersion, InstalledRelease: checkpoint.Release{Version: "v1.3.0", Revision: "abc123"}}

	listed, err := checkpoint.FindNewest(context.Background(), parent, match)
	if err != nil || listed == nil || listed.Path != installed || !listed.OfInstalledRelease {
		t.Fatalf("FindNewest() = %+v, %v; want %s of the installed release", listed, err, installed)
	}

	match.InstalledRelease = checkpoint.Release{Version: "v1.4.0", Revision: "fff000"}
	listed, err = checkpoint.FindNewest(context.Background(), parent, match)
	if err != nil || listed == nil || listed.Path != newerOtherRelease || listed.OfInstalledRelease {
		t.Fatalf("FindNewest() = %+v, %v; want the newest schema match %s, not of the installed release", listed, err, newerOtherRelease)
	}
	if listed.RestoreRelease != (checkpoint.Release{Version: "v1.2.0", Revision: "abc123"}) {
		t.Fatalf("restore release = %+v, want v1.2.0 (abc123)", listed.RestoreRelease)
	}
}

// corruptStore changes the last byte of the store snapshot of a checkpoint.
func corruptStore(t *testing.T, directory string) {
	t.Helper()
	path := filepath.Join(directory, checkpoint.StoreFile)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content[len(content)-1] ^= 0xff
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestFindNewestIgnoresNestedDirectoriesAndFiles verifies that only the
// direct subdirectories of the named directory are candidates, and that a
// checkpoint with no match yields no result without an error.
func TestFindNewestIgnoresNestedDirectoriesAndFiles(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	nested := filepath.Join(parent, "group")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	placeCheckpoint(t, nested, "too-deep", func(m *checkpoint.Manifest) { m.RegistrationIdentity = "sha256:mine" })
	if err := os.WriteFile(filepath.Join(parent, "notes.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	listed, err := checkpoint.FindNewest(context.Background(), parent, checkpoint.Match{RegistrationIdentity: "sha256:mine", StoreSchemaVersion: store.CurrentSchemaVersion})
	if err != nil || listed != nil {
		t.Fatalf("FindNewest() = %+v, %v; want no checkpoint and no error", listed, err)
	}
}

// TestFindNewestRefusesAnUnreadableDirectory verifies that a directory that
// cannot be read is an error, not an empty result.
func TestFindNewestRefusesAnUnreadableDirectory(t *testing.T) {
	t.Parallel()
	_, err := checkpoint.FindNewest(context.Background(), filepath.Join(t.TempDir(), "missing"), checkpoint.Match{})
	if err == nil {
		t.Fatal("FindNewest() error = nil for a missing directory; want an error")
	}
}
