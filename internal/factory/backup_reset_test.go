package factory_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/factory"
)

// TestResetRemovesPreMigrationBackupsAndTheirProvenanceOfTheExactDatabase
// verifies that a reset removes each backup of its own database together with
// the provenance file, and leaves a backup of another database untouched.
func TestResetRemovesPreMigrationBackupsAndTheirProvenanceOfTheExactDatabase(t *testing.T) {
	fixture := newHeadlessRemovalFixture(t, "codex")
	backup := fixture.databasePath + ".bak-20260101T000000.000000000Z"
	provenance := backup + ".json"
	other := filepath.Join(filepath.Dir(fixture.databasePath), "other.db.bak-20260101T000000.000000000Z")
	for _, path := range []string{backup, provenance, other} {
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	preview, err := fixture.service.Reset(t.Context(), factory.ResetRequest{})
	var confirmation *factory.ResetConfirmationRequiredError
	if !errors.As(err, &confirmation) {
		t.Fatalf("Reset(preview) error = %v, want a confirmation request", err)
	}
	planned := basenamesForTest(preview.Plan.MigrationBackups)
	if want := []string{filepath.Base(backup), filepath.Base(provenance)}; !slices.Equal(planned, want) {
		t.Fatalf("planned backups = %v, want %v", planned, want)
	}

	if _, err := fixture.service.Reset(t.Context(), factory.ResetRequest{Confirm: true}); err != nil {
		t.Fatalf("Reset(confirm) error = %v", err)
	}
	for _, path := range []string{backup, provenance} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("backup file %q error = %v, want removed", path, err)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("backup of another database error = %v, want kept", err)
	}
}

// basenamesForTest returns the file names of paths, because the reset plan
// resolves the store directory through the file system.
func basenamesForTest(paths []string) []string {
	names := make([]string, 0, len(paths))
	for _, path := range paths {
		names = append(names, filepath.Base(path))
	}
	return names
}
