package effect_test

import (
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/effect"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestReadResultAcceptanceRequiresTheRecordedReport rejects a pre-baseline
// payload that would substitute a mutable disk artifact for accepted evidence.
func TestReadResultAcceptanceRequiresTheRecordedReport(t *testing.T) {
	t.Parallel()
	pending := store.PendingEffect{RunID: "run-1", ID: "acceptance-without-snapshot", Kind: store.PendingEffectKindResultAcceptance, Payload: `{"Invocation":{"ID":"inv-1","RunID":"run-1"}}`}
	if _, err := effect.ReadResultAcceptance(pending); err == nil || !strings.Contains(err.Error(), "no accepted report") {
		t.Fatalf("ReadResultAcceptance() = %v, want missing snapshot refusal", err)
	}
}
