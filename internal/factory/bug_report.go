package factory

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/Stevie1704/sw-factory/internal/bugreport"
	"github.com/Stevie1704/sw-factory/internal/config"
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

// invocationLister lists the Invocations of one Run.
type invocationLister interface {
	Invocations(context.Context, string) ([]store.Invocation, error)
}

// retainedDefectProposals returns the valid defect proposals of a Run's
// retained reports, and ignores a report it cannot use.
func retainedDefectProposals(ctx context.Context, lister invocationLister, run store.Run) ([]report.FactoryDefectProposal, error) {
	invocations, err := lister.Invocations(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	var proposals []report.FactoryDefectProposal
	for _, invocation := range invocations {
		if proposal, _ := retainedDefectProposal(run, invocation); proposal != nil {
			proposals = append(proposals, *proposal)
		}
	}
	return proposals, nil
}

// BugReportEvidence collects the allowlisted local evidence of a suspected
// Factory defect draft. It never fails for an unavailable source: the
// evidence states the source as unavailable and names the limitation, so a
// broken installation still produces a useful partial draft. An empty runID
// requests no Run. Collection is read-only and makes no external requests.
func (s *Service) BugReportEvidence(ctx context.Context, runID string) (bugreport.Evidence, error) {
	evidence := bugreport.Evidence{Configuration: bugreport.AvailabilityAvailable, Store: bugreport.AvailabilityNotRequested}
	registration, err := s.bugReportRegistration()
	if err != nil {
		evidence.Configuration = bugreport.AvailabilityUnavailable
		evidence.Limitations = append(evidence.Limitations, "The host configuration could not be loaded or has no valid registration; run `factory doctor` for the field-level diagnosis.")
		if runID != "" {
			evidence.Store = bugreport.AvailabilityUnavailable
			evidence.Limitations = append(evidence.Limitations, "The Run could not be read because the operational store location is unknown.")
		}
		return evidence, nil
	}
	evidence.Provider = string(providerOf(registration))
	if runID == "" {
		return evidence, nil
	}
	var limitations []string
	evidence.Run, evidence.Store, limitations = s.storeRunEvidence(ctx, registration, runID)
	evidence.Limitations = append(evidence.Limitations, limitations...)
	return evidence, nil
}

// bugReportRegistration loads the one registration whose store a draft may
// read. Its error text is never shown, because it can quote configuration.
func (s *Service) bugReportRegistration() (config.RepositoryRegistration, error) {
	host, err := s.deps.Config.Load(s.configPath)
	if err != nil {
		return config.RepositoryRegistration{}, err
	}
	if len(host.Repositories) == 0 {
		return config.RepositoryRegistration{}, errors.New("no repository is registered")
	}
	registration := host.Repositories[0]
	if err := validateOperationalPath(registration.Path, registration.OperationalDataPath); err != nil {
		return config.RepositoryRegistration{}, err
	}
	return registration, nil
}

// storeRunEvidence reads one Run from the registration's store, and states
// the store's availability and the collection limitations.
func (s *Service) storeRunEvidence(ctx context.Context, registration config.RepositoryRegistration, runID string) (*bugreport.RunEvidence, bugreport.Availability, []string) {
	opened, err := s.deps.OpenStoreReadOnly(ctx, registration.OperationalDataPath)
	if err != nil {
		return nil, bugreport.AvailabilityUnavailable, []string{"The operational store could not be opened."}
	}
	defer func() { _ = opened.Close() }()
	reader, ok := opened.(bugReportRunReader)
	if !ok {
		return nil, bugreport.AvailabilityUnavailable, []string{"The operational store does not provide the Run read model."}
	}
	run, limitations, err := collectRunEvidence(ctx, reader, runID)
	if err != nil {
		return nil, bugreport.AvailabilityUnavailable, append(limitations, "The operational store could not be read.")
	}
	return run, bugreport.AvailabilityAvailable, limitations
}

// collectRunEvidence projects one Run onto the allowlist and returns the
// collection limitations. A missing Run is a limitation; a store read
// failure is an error.
func collectRunEvidence(ctx context.Context, reader bugReportRunReader, runID string) (*bugreport.RunEvidence, []string, error) {
	run, err := reader.Run(ctx, runID)
	if err != nil {
		return nil, nil, err
	}
	if run == nil {
		return nil, []string{fmt.Sprintf("Run %s is not in the operational store.", safeEvidenceIdentifier(runID))}, nil
	}
	gates, err := reader.RunGateResults(ctx, run.ID)
	if err != nil {
		return nil, nil, err
	}
	invocations, err := reader.Invocations(ctx, run.ID)
	if err != nil {
		return nil, nil, err
	}
	evidence := &bugreport.RunEvidence{ID: run.ID, Stage: string(run.Stage), Status: string(run.Status)}
	for _, gate := range failedGatesAt(gates, run.CheckpointSHA) {
		evidence.FailedGates = append(evidence.FailedGates, bugreport.GateEvidence{Phase: string(gate.Phase), Outcome: string(gate.Outcome)})
	}
	var limitations []string
	for _, invocation := range invocations {
		evidence.Invocations = append(evidence.Invocations, bugreport.InvocationEvidence{
			ID: invocation.ID, Role: invocation.Role, Stage: string(invocation.Stage),
			Harness: invocation.Harness, Status: string(invocation.Status), PromptVersion: invocation.PromptVersion,
		})
		proposal, limitation := retainedDefectProposal(*run, invocation)
		if limitation != "" {
			limitations = append(limitations, limitation)
		}
		if proposal != nil {
			evidence.DefectProposals = append(evidence.DefectProposals, *proposal)
		}
	}
	return evidence, limitations, nil
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
