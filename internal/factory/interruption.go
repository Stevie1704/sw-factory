package factory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/harness"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// ResumeRequest selects the active run whose persisted worker and native
// harness session should be resumed by an operator.
type ResumeRequest struct {
	// RunID optionally selects one opaque factory run identifier.
	RunID string
}

// ResumeResult reports the invocation restored by an explicit resume command.
type ResumeResult struct {
	// Run is the resulting durable run projection.
	Run store.Run
	// Invocation is the resumed or newly launched invocation.
	Invocation store.Invocation
}

// AuthRefreshRequest selects the harness credential source to reseed into the
// factory-managed worker credential store.
type AuthRefreshRequest struct {
	// RunID optionally selects one opaque factory run identifier.
	RunID string
	// Harness selects codex or claude. Empty uses the invocation's harness.
	Harness config.Harness
	// Resume continues the affected native session after the credential source
	// has been projected. It is deliberately opt-in so the default refresh
	// operation remains a credential-only maintenance action.
	Resume bool
}

// AuthRefreshResult reports the credential store identity after a refresh.
type AuthRefreshResult struct {
	// Run is unchanged workflow state after credential refresh.
	Run store.Run
	// Invocation is the invocation whose worker credential store was reseeded.
	Invocation store.Invocation
	// Harness identifies the refreshed adapter.
	Harness config.Harness
	// Resumed reports whether the explicit native session was continued.
	Resumed bool
}

// latestInvocationForAuthRefresh selects the active invocation or the latest
// superseded attempt that still carries the worker identity needed to reseed.
func latestInvocationForAuthRefresh(ctx context.Context, runStore RunStore, run store.Run) (*store.Invocation, error) {
	activeValues, supported, err := activeInvocationsForRun(ctx, runStore, run.ID)
	if err != nil {
		return nil, fmt.Errorf("read active invocation for auth refresh: %w", err)
	}
	if supported && len(activeValues) > 0 {
		active := activeValues[len(activeValues)-1]
		return &active, nil
	}
	latestStore, ok := runStore.(LatestInvocationStore)
	if !ok {
		return nil, errors.New("operational store does not support latest invocation lookup")
	}
	latest, err := latestStore.LatestInvocation(ctx, run.ID)
	if err != nil {
		return nil, fmt.Errorf("read latest invocation for auth refresh: %w", err)
	}
	if latest == nil {
		return nil, errors.New("run has no invocation to refresh")
	}
	return latest, nil
}

// classifyHarnessRuntimeErrorForInvocation preserves typed harness failures
// while avoiding any attempt to expose adapter output for known categories.
func classifyHarnessRuntimeErrorForInvocation(invocation store.Invocation, err error) error {
	return harness.ClassifyError(err, invocation.Harness)
}

// validateOptionalRunID rejects command-line control characters before they
// reach a store lookup or an external adapter.
func validateOptionalRunID(runID string) error {
	if strings.ContainsAny(runID, "\x00\r\n") {
		return errors.New("run id contains control characters")
	}
	return nil
}

// resetStartupStateProjection clears a cached diagnosis after infrastructure changes.
func (s *Service) resetStartupStateProjection() {
	s.startupMu.Lock()
	defer s.startupMu.Unlock()
	s.startupChecked = false
	s.startupErr = nil
}
