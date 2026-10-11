package factory

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/Stevie1704/sw-factory/internal/bugreport"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// bugReportRunReader is the read model a suspected-defect draft needs from
// the operational store.
type bugReportRunReader interface {
	Run(context.Context, string) (*store.Run, error)
	Invocations(context.Context, string) ([]store.Invocation, error)
	RunGateResults(context.Context, string) ([]store.GateResult, error)
}

// BugReportEvidence collects the allowlisted local evidence of a suspected
// Factory defect draft. It never fails for an unavailable source: the
// evidence states the source as unavailable and names the limitation, so a
// broken installation still produces a useful partial draft. An empty runID
// requests no Run. Collection is read-only and makes no external requests.
func (s *Service) BugReportEvidence(ctx context.Context, runID string) (bugreport.Evidence, error) {
	evidence := bugreport.Evidence{Configuration: bugreport.AvailabilityAvailable, Store: bugreport.AvailabilityNotRequested}
	host, err := s.deps.Config.Load(s.configPath)
	if err == nil && len(host.Repositories) == 0 {
		err = errors.New("no repository is registered")
	}
	if err == nil {
		err = validateOperationalPath(host.Repositories[0].Path, host.Repositories[0].OperationalDataPath)
	}
	if err != nil {
		evidence.Configuration = bugreport.AvailabilityUnavailable
		evidence.Limitations = append(evidence.Limitations, "The host configuration could not be loaded or has no valid registration; run `factory doctor` for the field-level diagnosis.")
		if runID != "" {
			evidence.Store = bugreport.AvailabilityUnavailable
			evidence.Limitations = append(evidence.Limitations, "The Run could not be read because the operational store location is unknown.")
		}
		return evidence, nil
	}
	registration := host.Repositories[0]
	evidence.Provider = string(providerOf(registration))
	if runID == "" {
		return evidence, nil
	}
	evidence.Store = bugreport.AvailabilityUnavailable
	opened, err := s.deps.OpenStoreReadOnly(ctx, registration.OperationalDataPath)
	if err != nil {
		evidence.Limitations = append(evidence.Limitations, "The operational store could not be opened.")
		return evidence, nil
	}
	defer func() { _ = opened.Close() }()
	reader, ok := opened.(bugReportRunReader)
	if !ok {
		evidence.Limitations = append(evidence.Limitations, "The operational store does not provide the Run read model.")
		return evidence, nil
	}
	run, err := collectRunEvidence(ctx, reader, runID, &evidence.Limitations)
	if err != nil {
		evidence.Limitations = append(evidence.Limitations, "The operational store could not be read.")
		return evidence, nil
	}
	evidence.Store = bugreport.AvailabilityAvailable
	evidence.Run = run
	return evidence, nil
}

// collectRunEvidence projects one Run onto the allowlist. A missing Run is a
// limitation; a store read failure is an error.
func collectRunEvidence(ctx context.Context, reader bugReportRunReader, runID string, limitations *[]string) (*bugreport.RunEvidence, error) {
	run, err := reader.Run(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		*limitations = append(*limitations, fmt.Sprintf("Run %s is not in the operational store.", safeEvidenceIdentifier(runID)))
		return nil, nil
	}
	gates, err := reader.RunGateResults(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	invocations, err := reader.Invocations(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	evidence := &bugreport.RunEvidence{ID: run.ID, Stage: string(run.Stage), Status: string(run.Status)}
	for _, gate := range failedGatesAt(gates, run.CheckpointSHA) {
		evidence.FailedGates = append(evidence.FailedGates, bugreport.GateEvidence{Phase: string(gate.Phase), Outcome: string(gate.Outcome)})
	}
	for _, invocation := range invocations {
		evidence.Invocations = append(evidence.Invocations, bugreport.InvocationEvidence{
			ID: invocation.ID, Role: invocation.Role, Stage: string(invocation.Stage),
			Harness: invocation.Harness, Status: string(invocation.Status), PromptVersion: invocation.PromptVersion,
		})
		proposal, limitation := retainedDefectProposal(*run, invocation)
		if limitation != "" {
			*limitations = append(*limitations, limitation)
		}
		if proposal != nil {
			evidence.DefectProposals = append(evidence.DefectProposals, *proposal)
		}
	}
	return evidence, nil
}

// retainedDefectProposal reads the defect proposal of one reported
// Invocation. It returns a limitation instead of a proposal when the retained
// report is unavailable or does not belong to the Invocation.
func retainedDefectProposal(run store.Run, invocation store.Invocation) (*report.FactoryDefectProposal, string) {
	switch invocation.Status {
	case store.InvocationStatusCompleted, store.InvocationStatusWaitingForHuman, store.InvocationStatusCannotProceed:
	default:
		return nil, ""
	}
	value, err := report.ReadEnvelope(reportPath(invocation))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Sprintf("The retained report of Invocation %s is unavailable.", invocation.ID)
	}
	if err != nil {
		return nil, fmt.Sprintf("The retained report of Invocation %s could not be read.", invocation.ID)
	}
	if value.InvocationID != invocation.ID || value.RunID != run.ID {
		return nil, fmt.Sprintf("The retained report of Invocation %s names another Invocation and was ignored.", invocation.ID)
	}
	if value.SuspectedFactoryDefect == nil {
		return nil, ""
	}
	if err := report.ValidateFactoryDefectProposal(*value.SuspectedFactoryDefect, invocation.ID); err != nil {
		return nil, fmt.Sprintf("The defect proposal of Invocation %s was rejected: it is malformed or names another Invocation.", invocation.ID)
	}
	return value.SuspectedFactoryDefect, ""
}

// safeEvidenceIdentifier returns an operator-supplied identifier when it is a
// safe protocol identifier, and a placeholder otherwise, so a limitation never
// echoes arbitrary input.
func safeEvidenceIdentifier(value string) string {
	if !report.IsSafeIdentifier(value) {
		return "<unsafe identifier>"
	}
	return value
}
