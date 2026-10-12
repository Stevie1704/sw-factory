package swfactory

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPublishStagedPreservesAFileThatAppearedAtTheDestination proves that
// publication never deletes a file that another process created at the
// destination while the export was staged.
func TestPublishStagedPreservesAFileThatAppearedAtTheDestination(t *testing.T) {
	parent := t.TempDir()
	staging := filepath.Join(parent, ".resources.partial-1")
	destination := filepath.Join(parent, "resources")
	if err := os.Mkdir(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("operator notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := publishStaged(staging, destination); err == nil {
		t.Error("publication over a file succeeded")
	}
	if got, err := os.ReadFile(destination); err != nil || string(got) != "operator notes\n" {
		t.Errorf("destination = %q (%v), want the operator's file unchanged", got, err)
	}
}
