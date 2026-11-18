package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/checkpoint"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// setCheckpointManifest rewrites the release, store schema, and creation
// time of a created checkpoint. The manifest is not checksummed, so the
// checkpoint still verifies when the schema is the one of its snapshot.
func setCheckpointManifest(t *testing.T, directory, version string, schema int, createdAt int64) {
	t.Helper()
	path := filepath.Join(directory, checkpoint.ManifestFile)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest checkpoint.Manifest
	if err := json.Unmarshal(content, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Factory.Version, manifest.Factory.Revision = version, "abc123"
	manifest.StoreSchemaVersion = schema
	manifest.CreatedAt = time.Unix(createdAt, 0).UTC()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.WriteManifest(directory, manifest); err != nil {
		t.Fatal(err)
	}
}

// listedCheckpoint is the part of the checkpoint of an upgrade plan that
// the tests read.
type listedCheckpoint struct {
	Path     string `json:"path"`
	Manifest struct {
		CheckpointID string `json:"checkpoint_id"`
	} `json:"manifest"`
}

// TestUpgradePlanListsTheNewestMatchingCheckpoint verifies that the plan
// names the newest checkpoint of this registration and of the live store
// schema in the directory that --checkpoints names: the one that the
// installed release v1.3.0 created before the migration and can restore,
// not one of the target release v1.4.0 that runs the plan. It skips a newer
// checkpoint of another schema and names none without the flag.
func TestUpgradePlanListsTheNewestMatchingCheckpoint(t *testing.T) {
	fixture := newUpgradeFixture(t, store.StatusComplete, fakeUpgradeImages{release: "v1.3.0"})
	useCheckpointExternal(t, cliExternal{})
	parent := filepath.Dir(checkpointOutput(t))
	installed := filepath.Join(parent, "installed")
	otherSchema := filepath.Join(parent, "other-schema")
	for _, output := range []string{installed, otherSchema} {
		var created checkpointCreateDocument
		if code := runCheckpointCommand(t, &created, "create", "--config", fixture.hostConfig, "--output", output, "--json"); code != 0 {
			t.Fatalf("checkpoint create %s exit = %d, want 0", output, code)
		}
	}
	setCheckpointManifest(t, installed, "v1.3.0", store.CurrentSchemaVersion, 100)
	setCheckpointManifest(t, otherSchema, "v1.4.0", store.CurrentSchemaVersion+1, 200)

	var withoutFlag struct {
		Checkpoint *listedCheckpoint `json:"checkpoint"`
	}
	if code := runUpgradeCommand(t, &withoutFlag, "plan", "--config", fixture.hostConfig, "--json"); code != 0 || withoutFlag.Checkpoint != nil {
		t.Fatalf("plan without --checkpoints = exit %d, checkpoint %+v; want null", code, withoutFlag.Checkpoint)
	}

	var document struct {
		Checkpoint *listedCheckpoint `json:"checkpoint"`
	}
	runUpgradeCommand(t, &document, "plan", "--config", fixture.hostConfig, "--checkpoints", parent, "--json")
	if document.Checkpoint == nil || document.Checkpoint.Path != installed || document.Checkpoint.Manifest.CheckpointID == "" {
		t.Fatalf("plan checkpoint = %+v, want %s", document.Checkpoint, installed)
	}
}
