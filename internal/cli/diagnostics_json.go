package cli

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/Stevie1704/sw-factory/internal/doctor"
)

// diagnosticsSchemaVersion is the version of the `doctor --json` and
// `status --json` documents. Additive fields keep the version; a removed or
// renamed field, or a changed meaning, increments it.
const diagnosticsSchemaVersion = 1

// Document outcomes shared by the JSON views.
const (
	outcomeReady   = "ready"
	outcomeBlocked = "blocked"
	outcomeOK      = "ok"
	outcomeError   = "error"
)

// maxErrorMessageBytes bounds an operational error message in a document.
const maxErrorMessageBytes = 512

// errorDocument is the structured operational failure of a JSON command.
type errorDocument struct {
	Code         string        `json:"code"`
	LikelyOrigin doctor.Origin `json:"likely_origin"`
	Message      string        `json:"message"`
}

// newErrorDocument bounds the message of one classified operational failure.
func newErrorDocument(code string, origin doctor.Origin, err error) *errorDocument {
	message := err.Error()
	if len(message) > maxErrorMessageBytes {
		message = message[:maxErrorMessageBytes]
	}
	return &errorDocument{Code: code, LikelyOrigin: origin, Message: message}
}

// doctorDocument is the `factory doctor --json` view of a doctor report.
type doctorDocument struct {
	SchemaVersion int                  `json:"schema_version"`
	Command       string               `json:"command"`
	Outcome       string               `json:"outcome"`
	Ready         bool                 `json:"ready"`
	Complete      bool                 `json:"complete"`
	Diagnostics   []diagnosticDocument `json:"diagnostics"`
	Error         *errorDocument       `json:"error,omitempty"`
}

// diagnosticDocument is the JSON view of one doctor result.
type diagnosticDocument struct {
	Code          string          `json:"code"`
	Subsystem     string          `json:"subsystem"`
	Check         string          `json:"check"`
	Status        doctor.Status   `json:"status"`
	BlocksStartup bool            `json:"blocks_startup"`
	Observed      bool            `json:"observed"`
	Problem       string          `json:"problem,omitempty"`
	Detail        string          `json:"detail,omitempty"`
	Facts         []factDocument  `json:"facts"`
	LikelyOrigin  doctor.Origin   `json:"likely_origin,omitempty"`
	Action        *actionDocument `json:"action,omitempty"`
}

// factDocument is one bounded supporting observation.
type factDocument struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// actionDocument is the suggested corrective action of one diagnostic.
type actionDocument struct {
	Summary       string        `json:"summary"`
	Effect        doctor.Effect `json:"effect,omitempty"`
	Preconditions []string      `json:"preconditions"`
	GuideTopic    string        `json:"guide_topic,omitempty"`
}

// newDoctorDocument projects a doctor report without reinterpreting it.
func newDoctorDocument(report doctor.Report) doctorDocument {
	document := doctorDocument{
		SchemaVersion: diagnosticsSchemaVersion,
		Command:       "doctor",
		Outcome:       outcomeBlocked,
		Ready:         report.Ready(),
		Complete:      report.Complete(),
		Diagnostics:   make([]diagnosticDocument, 0, len(report.Results)),
	}
	if document.Ready {
		document.Outcome = outcomeReady
	}
	for _, result := range report.Results {
		document.Diagnostics = append(document.Diagnostics, newDiagnosticDocument(result))
	}
	return document
}

// newDoctorErrorDocument represents a doctor run that produced no report.
// Configuration problems are diagnostics, not errors. The message is fixed
// because the error text is not known to be free of secrets.
func newDoctorErrorDocument() doctorDocument {
	return doctorDocument{
		SchemaVersion: diagnosticsSchemaVersion,
		Command:       "doctor",
		Outcome:       outcomeError,
		Diagnostics:   []diagnosticDocument{},
		Error:         newErrorDocument("doctor.operational_failure", doctor.OriginUnknown, errors.New("the diagnosis did not produce a report")),
	}
}

// newDiagnosticDocument projects one doctor result and its condition.
func newDiagnosticDocument(result doctor.Result) diagnosticDocument {
	document := diagnosticDocument{
		Code:          result.Code,
		Subsystem:     result.Subsystem(),
		Check:         result.Name,
		Status:        result.Status,
		BlocksStartup: result.BlocksStartup(),
		Observed:      !result.Unobserved,
		Problem:       result.Problem,
		Detail:        result.Detail,
		Facts:         make([]factDocument, 0, len(result.Facts)),
		LikelyOrigin:  result.Origin,
	}
	for _, fact := range result.Facts {
		document.Facts = append(document.Facts, factDocument(fact))
	}
	if result.Action != "" {
		document.Action = &actionDocument{
			Summary:       result.Action,
			Effect:        result.Effect,
			Preconditions: append([]string{}, result.Preconditions...),
			GuideTopic:    result.Topic,
		}
	}
	return document
}

// writeJSONDocument writes one JSON document and a newline to output, and
// reports a write failure on errorsOutput.
func writeJSONDocument(output, errorsOutput io.Writer, document any) bool {
	encoded, err := json.Marshal(document)
	if err != nil {
		writeError(errorsOutput, err)
		return false
	}
	return writeOutput(output, errorsOutput, "%s\n", encoded)
}
