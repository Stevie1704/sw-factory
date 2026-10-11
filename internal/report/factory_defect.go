package report

import (
	"errors"
	"fmt"
	"strings"
)

// maxDefectTextRunes bounds each text field of a defect proposal.
const maxDefectTextRunes = 1000

// FactoryDefectProposal is a worker's bounded proposal that the host operator
// prepare a suspected Software Factory defect report. It is not a workflow
// outcome: it does not change how the coordinator accepts the report, and it
// authorizes no upstream publication.
type FactoryDefectProposal struct {
	// InvocationID binds the proposal to the invocation that reports it.
	InvocationID string `json:"invocation_id"`
	// Code is a stable diagnostic identifier, for example report.handoff_rejected.
	Code string `json:"code"`
	// Expected is the documented behavior the worker relied on.
	Expected string `json:"expected"`
	// Observed is the behavior the worker saw instead.
	Observed string `json:"observed"`
	// Reason tells why the cause appears to be Factory, not the repository or
	// the environment.
	Reason string `json:"reason"`
}

// ValidateFactoryDefectProposal checks the identity binding and the bounds of
// an optional defect proposal.
func ValidateFactoryDefectProposal(value FactoryDefectProposal, invocationID string) error {
	if value.InvocationID != invocationID {
		return fmt.Errorf("suspected_factory_defect.invocation_id %q does not match expected %q", value.InvocationID, invocationID)
	}
	if strings.TrimSpace(value.Code) == "" || !IsSafeIdentifier(value.Code) {
		return errors.New("suspected_factory_defect.code is required and must be a safe identifier")
	}
	fields := []struct{ name, value string }{
		{"expected", value.Expected},
		{"observed", value.Observed},
		{"reason", value.Reason},
	}
	for _, field := range fields {
		if err := validateText("suspected_factory_defect."+field.name, field.value, maxDefectTextRunes, true); err != nil {
			return err
		}
	}
	return nil
}
