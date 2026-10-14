package cli

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/Stevie1704/sw-factory/internal/doctor"
	"github.com/Stevie1704/sw-factory/internal/factory"
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

// actionDocument is one suggested action. Summary, Effect, Preconditions,
// and GuideTopic are the schema-1 fields; the typed fields are additive. Argv
// is set only when the action needs no human input.
type actionDocument struct {
	Summary       string                      `json:"summary"`
	Effect        doctor.Effect               `json:"effect,omitempty"`
	Preconditions []string                    `json:"preconditions"`
	GuideTopic    string                      `json:"guide_topic,omitempty"`
	ID            string                      `json:"id,omitempty"`
	Kind          factory.ActionKind          `json:"kind,omitempty"`
	Authorization factory.ActionAuthorization `json:"authorization,omitempty"`
	Command       []string                    `json:"command,omitempty"`
	Arguments     []argumentDocument          `json:"arguments,omitempty"`
	Argv          []string                    `json:"argv,omitempty"`
	Target        *targetDocument             `json:"target,omitempty"`
	Requires      []conditionDocument         `json:"requires,omitempty"`
	Postcondition *conditionDocument          `json:"postcondition,omitempty"`
	Verify        []string                    `json:"verify,omitempty"`
}

// argumentDocument is one flag of an action command. A human_input flag has
// no value; a human supplies it.
type argumentDocument struct {
	Flag       string `json:"flag"`
	Value      string `json:"value,omitempty"`
	HumanInput bool   `json:"human_input"`
}

// targetDocument is the exact state an action applies to.
type targetDocument struct {
	RunID         string `json:"run_id,omitempty"`
	Revision      int64  `json:"revision,omitempty"`
	CheckpointSHA string `json:"checkpoint_sha,omitempty"`
	InvocationID  string `json:"invocation_id,omitempty"`
	EffectID      string `json:"effect_id,omitempty"`
}

// conditionDocument is one structured precondition or postcondition.
type conditionDocument struct {
	Code        string `json:"code"`
	Description string `json:"description"`
}

// newRecommendedActionDocument projects one typed recovery action. The
// schema-1 preconditions repeat the condition descriptions.
func newRecommendedActionDocument(action factory.RecommendedAction) actionDocument {
	target := targetDocument(action.Target)
	document := actionDocument{
		Summary: action.Summary, Effect: action.Effect, Preconditions: []string{}, GuideTopic: "debug",
		ID: string(action.ID), Kind: action.Kind, Authorization: action.Authorization,
		Command: append([]string(nil), action.Command...), Target: &target,
		Verify: append([]string(nil), action.Verify...),
	}
	if argv, complete := action.Argv(); complete {
		document.Argv = argv
	}
	for _, argument := range action.Arguments {
		document.Arguments = append(document.Arguments, argumentDocument(argument))
	}
	for _, condition := range action.Preconditions {
		document.Preconditions = append(document.Preconditions, condition.Description)
		document.Requires = append(document.Requires, conditionDocument(condition))
	}
	if action.Postcondition.Code != "" {
		postcondition := conditionDocument(action.Postcondition)
		document.Postcondition = &postcondition
	}
	return document
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
		document.Action.Kind, document.Action.Authorization = doctorActionAuthority(result.Effect)

	}
	return document
}

// doctorActionAuthority classifies a doctor action. Its summary is prose, not
// a command, so a repair is a human decision that the operator authorizes.
func doctorActionAuthority(effect doctor.Effect) (factory.ActionKind, factory.ActionAuthorization) {
	if effect == doctor.EffectInspection {
		return factory.ActionKindInspection, factory.AuthorizationNone
	}
	return factory.ActionKindHumanDecision, factory.AuthorizationOperator
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
