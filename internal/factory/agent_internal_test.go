package factory

import (
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestPromptForPersistedInvocationRejectsUnsupportedSchemaVersion verifies
// recovery refuses invocation packets it cannot interpret without building a
// degraded role prompt.
func TestPromptForPersistedInvocationRejectsUnsupportedSchemaVersion(t *testing.T) {
	for _, test := range []struct {
		name    string
		version int
	}{
		{name: "legacy", version: invocationPacketVersion - 1},
		{name: "future", version: invocationPacketVersion + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			run := store.Run{ID: "run-prompt", SpecificationPacket: "{}"}
			invocation := store.Invocation{
				ID: "inv-prompt", RunID: run.ID, Role: "implementation", Stage: store.StageImplementation,
				InvocationDirectory: directory,
			}
			if err := writeInvocationPacket(directory, InvocationPacket{
				SchemaVersion: test.version,
				InvocationID:  invocation.ID,
				RunID:         run.ID,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := promptForPersistedInvocation(run, invocation, SpecificationPacket{}); err == nil || !strings.Contains(err.Error(), "unsupported persisted invocation packet schema version") {
				t.Fatalf("promptForPersistedInvocation() error = %v, want schema-version refusal", err)
			}
		})
	}
}
