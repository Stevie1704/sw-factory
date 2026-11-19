package guide_test

import (
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/guide"
)

// TestMaintenanceGuideNamesTheOptionalCheckpointStep verifies that the
// upgrade guide names the optional checkpoint before the store migration,
// the extra full-quiescence condition beyond the migration gate, the
// --checkpoints flag, and the plan field that lists the checkpoint. It also
// verifies that the guide no longer says that no supported restore exists.
func TestMaintenanceGuideNamesTheOptionalCheckpointStep(t *testing.T) {
	body, err := guide.Topic("maintenance")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"checkpoint before the store migration",
		"publication.in_progress",
		"--checkpoints",
		"`checkpoint`",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("maintenance guide does not contain %q", want)
		}
	}
	if strings.Contains(strings.ToLower(body), "no supported restore") {
		t.Error("maintenance guide still states that no supported restore exists")
	}
}
