// Package command parses the small, structured command language accepted in
// GitHub supervision comments.
package command

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Kind identifies a command understood by the coordinator.
type Kind string

const (
	// Status requests a fresh rendering of the current run status.
	Status Kind = "status"
	// Retry asks the coordinator to retry a failed run at its current stage.
	Retry Kind = "retry"
	// Cancel asks the coordinator to stop an active run while retaining its work.
	Cancel Kind = "cancel"
	// Refresh asks the coordinator to refresh the supervision projection.
	Refresh Kind = "refresh"
	// Revision asks the coordinator to amend a ready pull request's frozen
	// specification and restart implementation on the existing worktree.
	Revision Kind = "revision"
	// Answer supplies an authorized answer to one pending clarification question.
	Answer Kind = "answer"
	// Repair supplies one maintainer instruction as a repair packet and
	// resumes implementation from the run's current checkpoint.
	Repair Kind = "repair"
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
	// QuestionID is set only for Answer and identifies one pending question.
	QuestionID string
	// Answer is set only for Answer and contains the maintainer's response.
	Answer string
	// Instruction is set only for Repair and contains the requested change.
	Instruction string
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
	// ParseErrorMissingAnswer means an answer command omitted required content.
	ParseErrorMissingAnswer ParseErrorCode = "missing_answer"
	// ParseErrorMissingInstruction means a repair command omitted its instruction.
	ParseErrorMissingInstruction ParseErrorCode = "missing_instruction"
	// ParseErrorInvalidQuestionArgument means an answer question identifier was malformed.
	ParseErrorInvalidQuestionArgument ParseErrorCode = "invalid_question_argument"
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
	case string(Cancel):
		return parseFixedArity(result, fields, Cancel)
	case string(Refresh):
		return parseFixedArity(result, fields, Refresh)
	case string(Revision), "revise":
		return parseFixedArity(result, fields, Revision)
	case string(Answer):
		return parseAnswer(result, fields[2:])
	case string(Repair):
		return parseRepair(result, fields[2:])
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

// parseAnswer parses the question identifier and answer text from an answer
// command while preserving the answer's word boundaries for the coordinator.
func parseAnswer(result ParseResult, arguments []string) (ParseResult, error) {
	if len(arguments) < 2 {
		return result, &ParseError{Code: ParseErrorMissingAnswer, Problem: "answer requires a question identifier and answer text"}
	}
	questionID := arguments[0]
	if key, value, found := strings.Cut(questionID, "="); found {
		if key != "question" && key != "question-id" && key != "id" {
			return result, &ParseError{Code: ParseErrorInvalidQuestionArgument, Problem: "the first answer argument must be a question identifier"}
		}
		questionID = value
	}
	if !safeArgument(questionID) {
		return result, &ParseError{Code: ParseErrorInvalidQuestionArgument, Problem: "the question identifier must be a nonempty safe value"}
	}
	answer := strings.Join(arguments[1:], " ")
	if key, value, found := strings.Cut(answer, "answer="); found && key == "" {
		answer = value
	}
	if strings.TrimSpace(answer) == "" {
		return result, &ParseError{Code: ParseErrorMissingAnswer, Problem: "answer text must not be empty"}
	}
	if strings.ContainsAny(answer, "\x00\r\n") {
		return result, &ParseError{Code: ParseErrorControlCharacter, Problem: "the answer contains a control character"}
	}
	result.Command = Request{Kind: Answer, QuestionID: questionID, Answer: answer}
	return result, nil
}

// parseRepair parses one maintainer instruction from a repair command,
// preserving its word boundaries for the repair finding it becomes.
func parseRepair(result ParseResult, arguments []string) (ParseResult, error) {
	instruction := strings.TrimSpace(strings.Join(arguments, " "))
	if instruction == "" {
		return result, &ParseError{Code: ParseErrorMissingInstruction, Problem: "repair requires the requested change as text"}
	}
	if strings.ContainsAny(instruction, "\x00\r\n") {
		return result, &ParseError{Code: ParseErrorControlCharacter, Problem: "the instruction contains a control character"}
	}
	result.Command = Request{Kind: Repair, Instruction: instruction}
	return result, nil
}

// safeArgument reports whether a command identifier contains only a bounded
// single-line token that cannot alter command parsing.
func safeArgument(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsDigit(character) || strings.ContainsRune("._-", character) {
			continue
		}
		return false
	}
	return true
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
	next, _ := utf8.DecodeRuneInString(trimmed[len("/factory"):])
	return unicode.IsSpace(next)
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
