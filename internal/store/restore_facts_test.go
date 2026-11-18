package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestInspectForRestoreTreatsOnlyAMissingOrDamagedStoreAsReplaceable
// verifies that only a missing store or a damaged file skips precondition
// 6. Every other store that does not open is unknown evidence and gives the
// store.unreadable finding.
func TestInspectForRestoreTreatsOnlyAMissingOrDamagedStoreAsReplaceable(t *testing.T) {
	t.Parallel()
	missing, err := store.InspectForRestore(context.Background(), filepath.Join(privateDirectory(t), "factory.db"))
	if err != nil || missing.Exists || missing.Damaged || len(missing.Findings) != 0 {
		t.Fatalf("missing store facts = %+v, %v; want nothing to check", missing, err)
	}
	for name, content := range map[string][]byte{"not a database": []byte("damaged"), "empty": {}} {
		path := filepath.Join(privateDirectory(t), "factory.db")
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		facts, err := store.InspectForRestore(context.Background(), path)
		if err != nil || !facts.Exists || !facts.Damaged || facts.Readable || facts.Unreadable == "" || len(facts.Findings) != 0 {
			t.Fatalf("%s store facts = %+v, %v; want a damaged store with its reason", name, facts, err)
		}
	}
	for name, change := range map[string]func(t *testing.T, path string){
		"not private": func(t *testing.T, path string) {
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"newer schema": func(t *testing.T, path string) {
			if _, err := openRaw(t, path).Exec("UPDATE schema_metadata SET version = ?", store.CurrentSchemaVersion+1); err != nil {
				t.Fatal(err)
			}
		},
		"older schema": func(t *testing.T, path string) {
			if _, err := openRaw(t, path).Exec("UPDATE schema_metadata SET version = ?", store.CurrentSchemaVersion-1); err != nil {
				t.Fatal(err)
			}
		},
	} {
		path := quiescentStore(t, completedRun("run-1", 7))
		change(t, path)
		facts, err := store.InspectForRestore(context.Background(), path)
		if err != nil {
			t.Fatalf("%s: InspectForRestore() error = %v", name, err)
		}
		if facts.Damaged || facts.Readable || len(facts.Findings) != 1 || facts.Findings[0].Code != "store.unreadable" {
			t.Fatalf("%s store facts = %+v; want one store.unreadable finding", name, facts)
		}
	}
}
