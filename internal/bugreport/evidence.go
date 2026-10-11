// Package bugreport prepares a local suspected-Factory-defect report and
// publishes an operator-reviewed report to Factory's upstream tracker. Draft
// preparation is offline; only Submit contacts the upstream tracker, and only
// through the Upstream port.
package bugreport

import "github.com/Stevie1704/sw-factory/internal/report"

// Availability tells whether one local evidence source could be read.
type Availability string

const (
	// AvailabilityAvailable means the source was read.
	AvailabilityAvailable Availability = "available"
	// AvailabilityUnavailable means the source could not be read.
	AvailabilityUnavailable Availability = "unavailable"
	// AvailabilityNotRequested means the report did not need the source.
	AvailabilityNotRequested Availability = "not_requested"
)

// Evidence is the allowlisted local context of one draft. Every field is a
// fixed vocabulary value or a Factory-generated identifier, except the
// worker-authored proposals, which the draft marks for operator review.
// Collection never copies configuration or store objects, paths, repository
// identities, branch names, or error text.
type Evidence struct {
	// Configuration tells whether the host configuration loaded.
	Configuration Availability
	// Store tells whether the operational store could be read.
	Store Availability
	// Provider is the registered tracker kind, for example github or
	// azure_devops, or empty when unknown.
	Provider string
	// Run is the selected Run, or nil when the report has no Run.
	Run *RunEvidence
	// Limitations are fixed sentences that name evidence the collection
	// could not gather.
	Limitations []string
}

// RunEvidence is the allowlisted projection of one Run.
type RunEvidence struct {
	// ID is the Factory-generated Run identifier.
	ID string
	// Stage is the workflow stage.
	Stage string
	// Status is the Run status.
	Status string
	// FailedGates are the phase and outcome of failed blocking gates, without
	// repository-defined gate names.
	FailedGates []GateEvidence
	// Invocations are the Run's harness Invocations.
	Invocations []InvocationEvidence
	// DefectProposals are worker-authored suspected-defect proposals from
	// retained, validated reports.
	DefectProposals []report.FactoryDefectProposal
}

// GateEvidence is one failed blocking gate without its repository-defined name.
type GateEvidence struct {
	// Phase is the gate phase, for example checkpoint.
	Phase string
	// Outcome is the gate outcome, for example failed.
	Outcome string
}

// InvocationEvidence is the allowlisted projection of one Invocation.
type InvocationEvidence struct {
	// ID is the Factory-generated Invocation identifier.
	ID string
	// Role is the workflow role.
	Role string
	// Stage is the workflow stage.
	Stage string
	// Harness is the configured harness name.
	Harness string
	// Status is the Invocation status.
	Status string
	// PromptVersion is the Factory role prompt version.
	PromptVersion string
}
