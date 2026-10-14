package cli

import (
	"fmt"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/hostcmd"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestOperationDocumentReportsAnUnansweredCallAsPending verifies a timed-out
// host call is an unknown external outcome, not a completion or a refusal,
// and that its message does not promise a journal replay.
func TestOperationDocumentReportsAnUnansweredCallAsPending(t *testing.T) {
	err := fmt.Errorf("publish label: %w", &hostcmd.TimeoutError{Operation: "gh api", Timeout: time.Minute})

	document := newOperationDocument("draft-pr", err)

	if document.Outcome != operationPending || !document.Attempted || document.Reason == nil || document.Reason.Code != "external.outcome_unknown" || document.Error != nil {
		t.Fatalf("document = %+v, want an attempted pending outcome", document)
	}
	if document.Reason.Message != messageOutcomeUnknown {
		t.Fatalf("message = %q, want the generic unknown-outcome message", document.Reason.Message)
	}
}

// TestReconcileDocumentReportsAReservedReplayAsPending verifies a replay that
// timed out keeps its effect reserved and names that effect.
func TestReconcileDocumentReportsAReservedReplayAsPending(t *testing.T) {
	run := store.Run{ID: "run-replay", Revision: 7, Stage: store.StageCheck, Status: store.StatusActive}
	result := factory.RecoveryResult{
		Run:       &run,
		Outcome:   factory.RecoveryOutcomeRetryable,
		Diagnosis: factory.RecoveryDiagnosis{RunID: run.ID, PendingEffect: &store.PendingEffect{ID: "effect-label", Kind: store.PendingEffectKindLabelTransition}},
	}

	document := newReconcileDocument(result, &hostcmd.TimeoutError{Operation: "gh api", Timeout: time.Minute})

	if document.Outcome != operationPending || document.Reason == nil || document.Reason.Message != messageReplayReserved || document.Identities.EffectID != "effect-label" || document.Identities.Revision != 7 {
		t.Fatalf("document = %+v, want the reserved replay as pending", document)
	}
}
