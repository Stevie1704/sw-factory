// Package command parses the small, structured command language accepted in
// GitHub supervision comments.
package command

import (
	"fmt"
	"strings"
	"unicode"
)

// Kind identifies a command understood by the coordinator.
type Kind string

const (
	// Status requests a fresh rendering of the current run status.
	Status Kind = "status"
	// Retry asks the coordinator to retry a failed run at its current stage.
	Retry Kind = "retry"
	// Refresh asks the coordinator to refresh the supervision projection.
	Refresh Kind = "refresh"
	// ConfigureHarness changes the selected harness for a later invocation.
	ConfigureHarness Kind = "configure_harness"
)

// Harness identifies a harness value accepted by the command grammar.
type Harness string

const (
	// HarnessCodex selects Codex for a later invocation.
	HarnessCodex Harness = "codex"
	// HarnessClaude selects Claude for a later invocation.
	HarnessClaude Harness = "claude"
)

// Request is the typed command produced by Parse.
type Request struct {
	// Kind identifies the coordinator operation.
	Kind Kind
	// Harness is set only for ConfigureHarness.
	Harness Harness
}

// ParseResult distinguishes ordinary discussion from a structured command.
type ParseResult struct {
	// Command is populated when Recognized is true and parsing succeeds.
	Command Request
	// Recognized reports whether the comment used the factory command marker.
	Recognized bool
}

// ParseErrorCode identifies the policy problem found in a recognized command.
type ParseErrorCode string

const (
	// ParseErrorEmpty means the command marker had no verb.
	ParseErrorEmpty ParseErrorCode = "empty_command"
	// ParseErrorMultiline means a command was mixed with other comment text.
	ParseErrorMultiline ParseErrorCode = "multiline_command"
	// ParseErrorUnknownVerb means the verb is not registered by the factory.
	ParseErrorUnknownVerb ParseErrorCode = "unknown_command"
	// ParseErrorUnexpectedArgument means a known verb received extra arguments.
	ParseErrorUnexpectedArgument ParseErrorCode = "unexpected_argument"
	// ParseErrorMissingHarness means a harness configuration had no value.
	ParseErrorMissingHarness ParseErrorCode = "missing_harness"
	// ParseErrorInvalidHarnessArgument means the harness key was malformed.
	ParseErrorInvalidHarnessArgument ParseErrorCode = "invalid_harness_argument"
	// ParseErrorUnsupportedHarness means the harness is outside the grammar.
	ParseErrorUnsupportedHarness ParseErrorCode = "unsupported_harness"
	// ParseErrorControlCharacter means the command contains unsafe controls.
	ParseErrorControlCharacter ParseErrorCode = "control_character"
)

// ParseError is a typed rejection for a recognized but malformed command.
type ParseError struct {
	// Code is stable machine-readable policy vocabulary.
	Code ParseErrorCode
	// Problem is a concise operator-facing explanation.
	Problem string
}

// Error returns the typed policy rejection in a human-readable form.
func (e *ParseError) Error() string {
	if e == nil {
		return "malformed factory command"
	}
	return fmt.Sprintf("malformed factory command (%s): %s", e.Code, e.Problem)
}

// Parse recognizes one complete /factory comment. Ordinary discussion is not
// an error; a recognized marker with malformed content returns ParseError.
func Parse(body string) (ParseResult, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" || !hasMarker(trimmed) {
		return ParseResult{}, nil
	}
	result := ParseResult{Recognized: true}
	if strings.ContainsAny(trimmed, "\r\n") {
		return result, &ParseError{Code: ParseErrorMultiline, Problem: "the command must occupy the complete comment on one line"}
	}
	if strings.ContainsAny(trimmed, "\x00") {
		return result, &ParseError{Code: ParseErrorControlCharacter, Problem: "the command contains a NUL character"}
	}
	fields := strings.Fields(trimmed)
	if len(fields) < 2 {
		return result, &ParseError{Code: ParseErrorEmpty, Problem: "a command verb is required after /factory"}
	}
	switch fields[1] {
	case string(Status):
		return parseFixedArity(result, fields, Status)
	case string(Retry):
		return parseFixedArity(result, fields, Retry)
	case string(Refresh):
		return parseFixedArity(result, fields, Refresh)
	case "config", "configure", "harness":
		harness, parseErr := parseHarness(fields[2:])
		if parseErr != nil {
			return result, parseErr
		}
		result.Command = Request{Kind: ConfigureHarness, Harness: harness}
		return result, nil
	default:
		return result, &ParseError{Code: ParseErrorUnknownVerb, Problem: fmt.Sprintf("command %q is not supported", fields[1])}
	}
}

// parseFixedArity parses a registered verb that accepts no arguments.
func parseFixedArity(result ParseResult, fields []string, kind Kind) (ParseResult, error) {
	if len(fields) != 2 {
		return result, unexpectedArgument(fields[1])
	}
	result.Command = Request{Kind: kind}
	return result, nil
}

// hasMarker reports whether trimmed is explicitly addressed to the factory.
func hasMarker(trimmed string) bool {
	if !strings.HasPrefix(trimmed, "/factory") {
		return false
	}
	if len(trimmed) == len("/factory") {
		return true
	}
	return unicode.IsSpace(rune(trimmed[len("/factory")]))
}

// unexpectedArgument builds the stable parser rejection for a fixed-arity verb.
func unexpectedArgument(verb string) *ParseError {
	return &ParseError{Code: ParseErrorUnexpectedArgument, Problem: fmt.Sprintf("%s does not accept arguments", verb)}
}

// parseHarness validates the one supported harness argument shape.
func parseHarness(arguments []string) (Harness, *ParseError) {
	if len(arguments) == 0 {
		return "", &ParseError{Code: ParseErrorMissingHarness, Problem: "configure requires harness=codex, harness=claude, or a harness name"}
	}
	if len(arguments) != 1 {
		return "", &ParseError{Code: ParseErrorUnexpectedArgument, Problem: "configure accepts exactly one harness value"}
	}
	value := arguments[0]
	if key, configured, found := strings.Cut(value, "="); found {
		if key != "harness" || configured == "" {
			return "", &ParseError{Code: ParseErrorInvalidHarnessArgument, Problem: "the configuration key must be harness with a nonempty value"}
		}
		value = configured
	}
	harness := Harness(value)
	if harness != HarnessCodex && harness != HarnessClaude {
		return "", &ParseError{Code: ParseErrorUnsupportedHarness, Problem: fmt.Sprintf("harness %q is not supported", value)}
	}
	return harness, nil
}
