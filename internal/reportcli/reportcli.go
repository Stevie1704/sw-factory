// Package reportcli implements the factory-report command available inside a
// worker image.
package reportcli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/report"
)

// Request contains the worker command inputs and its explicit environment.
type Request struct {
	// Args are command-line arguments after the report command name.
	Args []string
	// Environment contains coordinator-provided identity and result variables.
	Environment map[string]string
	// Now supplies the report timestamp.
	Now func() time.Time
	// Output receives the successful report path.
	Output io.Writer
	// ErrorsOutput receives usage and operational errors.
	ErrorsOutput io.Writer
}

// Run validates command arguments, builds exactly one report outcome, and
// atomically writes it into the coordinator-provided invocation result path.
func Run(request Request) int {
	if request.Output == nil {
		request.Output = io.Discard
	}
	if request.ErrorsOutput == nil {
		request.ErrorsOutput = io.Discard
	}
	if request.Now == nil {
		request.Now = func() time.Time { return time.Now().UTC() }
	}
	environment := request.Environment
	lookup := func(name string) string {
		if environment != nil {
			return environment[name]
		}
		return os.Getenv(name)
	}
	flags := flag.NewFlagSet("factory-report", flag.ContinueOnError)
	flags.SetOutput(request.ErrorsOutput)
	outcome := flags.String("outcome", "", "completed, needs_clarification, or cannot_proceed")
	summary := flags.String("summary", "", "short observable summary")
	changeSummary := flags.String("change-summary", "", "completed handoff change summary")
	acceptance := pairList{}
	flags.Var(&acceptance, "acceptance", "criterion=evidence; may be repeated")
	productionFiles := stringList{}
	flags.Var(&productionFiles, "production-file", "repository-relative production file; may be repeated")
	focusedCommands := stringList{}
	flags.Var(&focusedCommands, "focused-command", "focused command run; may be repeated")
	limitations := stringList{}
	flags.Var(&limitations, "known-limitation", "known limitation; may be repeated")
	questions := pairList{}
	flags.Var(&questions, "question", "question-id=question text; may be repeated")
	evidence := pairList{}
	flags.Var(&evidence, "evidence", "kind=detail; may be repeated")
	nativeSessionID := flags.String("native-session-id", "", "harness-native session identifier")
	if err := flags.Parse(request.Args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		writeError(request.ErrorsOutput, errors.New("factory-report does not accept positional arguments"))
		return 2
	}
	identity := map[string]string{
		"invocation_id": lookup("FACTORY_INVOCATION_ID"),
		"run_id":        lookup("FACTORY_RUN_ID"),
		"harness":       lookup("FACTORY_HARNESS"),
		"role":          lookup("FACTORY_ROLE"),
		"stage":         lookup("FACTORY_STAGE"),
	}
	for field, value := range identity {
		if strings.TrimSpace(value) == "" {
			writeError(request.ErrorsOutput, fmt.Errorf("%s environment value is required", field))
			return 1
		}
	}
	resultDirectory := lookup("FACTORY_RESULT_DIR")
	if strings.TrimSpace(resultDirectory) == "" {
		writeError(request.ErrorsOutput, errors.New("FACTORY_RESULT_DIR environment value is required"))
		return 1
	}
	value := report.Report{
		SchemaVersion:   report.SchemaVersion,
		InvocationID:    identity["invocation_id"],
		RunID:           identity["run_id"],
		Harness:         identity["harness"],
		Role:            identity["role"],
		Stage:           identity["stage"],
		Outcome:         report.Outcome(*outcome),
		Summary:         *summary,
		NativeSessionID: *nativeSessionID,
		ReportedAt:      request.Now().UTC(),
	}
	for _, pair := range acceptance {
		value.Handoff = ensureHandoff(value.Handoff)
		value.Handoff.AcceptanceMapping = append(value.Handoff.AcceptanceMapping, report.AcceptanceMapping{Criterion: pair.Key, Evidence: pair.Value})
	}
	if *changeSummary != "" || len(productionFiles) > 0 || len(focusedCommands) > 0 || len(limitations) > 0 {
		value.Handoff = ensureHandoff(value.Handoff)
		value.Handoff.ChangeSummary = *changeSummary
		value.Handoff.ProductionFilesChanged = append([]string(nil), productionFiles...)
		value.Handoff.FocusedCommands = append([]string(nil), focusedCommands...)
		value.Handoff.KnownLimitations = append([]string(nil), limitations...)
	}
	for _, pair := range questions {
		value.Questions = append(value.Questions, report.Question{ID: pair.Key, Prompt: pair.Value})
	}
	for _, pair := range evidence {
		value.Evidence = append(value.Evidence, report.Evidence{Kind: pair.Key, Detail: pair.Value})
	}
	path, err := report.WriteAtomicForInvocation(resultDirectory, value.InvocationID, value)
	if err != nil {
		writeError(request.ErrorsOutput, err)
		return 1
	}
	if _, err := fmt.Fprintf(request.Output, "report written: %s\n", path); err != nil {
		writeError(request.ErrorsOutput, err)
		return 1
	}
	return 0
}

// ensureHandoff returns a mutable handoff value for repeated completion flags.
func ensureHandoff(value *report.Handoff) *report.Handoff {
	if value != nil {
		return value
	}
	return &report.Handoff{}
}

// writeError writes one CLI error without exposing credentials or transcripts.
func writeError(output io.Writer, err error) {
	if err != nil {
		_, _ = fmt.Fprintf(output, "error: %v\n", err)
	}
}

// stringList collects repeatable nonempty string flags.
type stringList []string

// String returns the comma-separated flag value.
func (value *stringList) String() string { return strings.Join(*value, ",") }

// Set appends one nonempty string flag value.
func (value *stringList) Set(input string) error {
	if strings.TrimSpace(input) == "" {
		return errors.New("value must not be empty")
	}
	*value = append(*value, input)
	return nil
}

// pair is one key=value flag parsed without shell interpretation.
type pair struct {
	// Key is the left side of the pair.
	Key string
	// Value is the right side of the pair.
	Value string
}

// pairList collects repeatable key=value flags.
type pairList []pair

// String returns the comma-separated pair value.
func (value *pairList) String() string {
	parts := make([]string, 0, len(*value))
	for _, item := range *value {
		parts = append(parts, item.Key+"="+item.Value)
	}
	return strings.Join(parts, ",")
}

// Set parses one nonempty key=value flag.
func (value *pairList) Set(input string) error {
	key, item, ok := strings.Cut(input, "=")
	if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(item) == "" {
		return errors.New("value must use nonempty key=value form")
	}
	*value = append(*value, pair{Key: key, Value: item})
	return nil
}
