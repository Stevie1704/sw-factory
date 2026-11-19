package checkpoint_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/checkpoint"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// sealedCheckpoint writes a checkpoint directory with a snapshot of a new
// current store and its manifest, and returns the directory.
func sealedCheckpoint(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(root, "data", "factory.db")
	opened, err := store.Open(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "checkpoint")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(ctx, source, filepath.Join(directory, checkpoint.StoreFile), nil)
	if err != nil {
		t.Fatal(err)
	}
	manifest := checkpoint.NewManifest("checkpoint-1", time.Unix(100, 0).UTC(), snapshot)
	manifest.RegistrationIdentity = "registration-identity"
	if err := checkpoint.WriteManifest(directory, manifest); err != nil {
		t.Fatalf("WriteManifest() error = %v", err)
	}
	return directory
}

// editManifest rewrites the manifest of a checkpoint directory as a generic
// JSON object, applies edit to it, and keeps the manifest mode private, so a
// test can damage one manifest field at a time.
func editManifest(t *testing.T, directory string, edit func(fields map[string]any)) {
	t.Helper()
	path := filepath.Join(directory, checkpoint.ManifestFile)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(content, &fields); err != nil {
		t.Fatal(err)
	}
	edit(fields)
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestVerifyAcceptsASealedCheckpoint verifies that a sealed checkpoint has
// the private modes of ADR 0032 and that its checksums verify.
func TestVerifyAcceptsASealedCheckpoint(t *testing.T) {
	t.Parallel()
	directory := sealedCheckpoint(t)
	for _, name := range []string{checkpoint.StoreFile, checkpoint.ManifestFile} {
		info, err := os.Stat(filepath.Join(directory, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, %v; want 0600", name, info, err)
		}
	}
	manifest, err := checkpoint.Verify(context.Background(), directory)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if manifest.SchemaVersion != checkpoint.ManifestSchemaVersion || manifest.Kind != checkpoint.Kind || manifest.StoreSchemaVersion != store.CurrentSchemaVersion || !manifest.PrivateData.Private {
		t.Fatalf("manifest = %+v, want a schema 1 maintenance checkpoint of the current store that is private data", manifest)
	}
}

// TestVerifyRefusesADamagedCheckpoint verifies that a changed byte, a
// missing or extra file, and an unknown manifest schema each refuse with
// the code that restore reports.
func TestVerifyRefusesADamagedCheckpoint(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		damage func(t *testing.T, directory string)
		code   string
	}{
		"changed byte": {func(t *testing.T, directory string) {
			path := filepath.Join(directory, checkpoint.StoreFile)
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			content[len(content)-1] ^= 0xff
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
		}, checkpoint.CodeCorrupt},
		"missing store": {func(t *testing.T, directory string) {
			if err := os.Remove(filepath.Join(directory, checkpoint.StoreFile)); err != nil {
				t.Fatal(err)
			}
		}, checkpoint.CodeIncomplete},
		"missing manifest": {func(t *testing.T, directory string) {
			if err := os.Remove(filepath.Join(directory, checkpoint.ManifestFile)); err != nil {
				t.Fatal(err)
			}
		}, checkpoint.CodeIncomplete},
		"extra file": {func(t *testing.T, directory string) {
			if err := os.WriteFile(filepath.Join(directory, "notes.txt"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, checkpoint.CodeCorrupt},
		"unknown manifest schema": {func(t *testing.T, directory string) {
			if err := os.WriteFile(filepath.Join(directory, checkpoint.ManifestFile), []byte(`{"schema_version":2}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}, checkpoint.CodeCorrupt},
		"missing registration identity": {func(t *testing.T, directory string) {
			editManifest(t, directory, func(fields map[string]any) {
				delete(fields, "registration_identity")
			})
		}, checkpoint.CodeCorrupt},
		"not private data": {func(t *testing.T, directory string) {
			editManifest(t, directory, func(fields map[string]any) {
				fields["private_data"] = map[string]any{"private": false, "reason": "none"}
			})
		}, checkpoint.CodeCorrupt},
		"manifest mode 0644": {func(t *testing.T, directory string) {
			if err := os.Chmod(filepath.Join(directory, checkpoint.ManifestFile), 0o644); err != nil {
				t.Fatal(err)
			}
		}, checkpoint.CodeCorrupt},
		"store mode 0644": {func(t *testing.T, directory string) {
			if err := os.Chmod(filepath.Join(directory, checkpoint.StoreFile), 0o644); err != nil {
				t.Fatal(err)
			}
		}, checkpoint.CodeCorrupt},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			directory := sealedCheckpoint(t)
			test.damage(t, directory)
			_, err := checkpoint.Verify(context.Background(), directory)
			var refusal *checkpoint.VerificationError
			if !errors.As(err, &refusal) || refusal.Code != test.code {
				t.Fatalf("Verify() error = %v, want %s", err, test.code)
			}
		})
	}
}
