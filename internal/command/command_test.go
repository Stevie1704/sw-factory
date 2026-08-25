package command_test

import (
	"errors"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/command"
)

// TestParseDistinguishesOrdinaryDiscussionFromStructuredCommands verifies the
// explicit marker keeps ordinary GitHub discussion outside the command surface.
func TestParseDistinguishesOrdinaryDiscussionFromStructuredCommands(t *testing.T) {
	t.Parallel()

	ordinary, err := command.Parse("Could we refresh the documentation before retrying?")
	if err != nil {
		t.Fatalf("Parse() ordinary discussion error = %v", err)
	}
	if ordinary.Recognized {
		t.Fatal("ordinary discussion was recognized as a factory command")
	}

	parsed, err := command.Parse("  /factory status  ")
	if err != nil {
		t.Fatalf("Parse() status error = %v", err)
	}
	if !parsed.Recognized || parsed.Command.Kind != command.Status {
		t.Fatalf("parsed status = %#v, want recognized status command", parsed)
	}

	nonBreakingSpace, err := command.Parse("/factory\u00a0status")
	if err != nil {
		t.Fatalf("Parse() non-breaking-space status error = %v", err)
	}
	if !nonBreakingSpace.Recognized || nonBreakingSpace.Command.Kind != command.Status {
		t.Fatalf("parsed non-breaking-space status = %#v, want recognized status command", nonBreakingSpace)
	}
}

// TestAnswerCommandCarriesItsQuestionIdentifierAndText verifies the structured
// answer payload is distinguishable from ordinary discussion.
func TestAnswerCommandCarriesItsQuestionIdentifierAndText(t *testing.T) {
	parsed, err := command.Parse("/factory answer clarification-1 use the existing JSON format")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !parsed.Recognized || parsed.Command.Kind != command.Answer {
		t.Fatalf("Parse() = %#v, want recognized answer command", parsed)
	}
	if parsed.Command.QuestionID != "clarification-1" || parsed.Command.Answer != "use the existing JSON format" {
		t.Fatalf("answer command = %#v, want identifier and text", parsed.Command)
	}
}

// TestAnswerCommandRejectsAnUnsafeQuestionIdentifier verifies malformed
// structured input fails closed before it can reach workflow policy.
func TestAnswerCommandRejectsAnUnsafeQuestionIdentifier(t *testing.T) {
	parsed, err := command.Parse("/factory answer question-id=clarification/1 yes")
	if !parsed.Recognized || err == nil {
		t.Fatalf("Parse() = %#v/%v, want recognized typed rejection", parsed, err)
	}
	var parseErr *command.ParseError
	if !errors.As(err, &parseErr) || parseErr.Code != command.ParseErrorInvalidQuestionArgument {
		t.Fatalf("Parse() error = %v, want invalid question argument", err)
	}
}

// TestParseReturnsTypedRejectionsForMalformedCommands verifies malformed
// structured comments expose stable parser rejection codes.
func TestParseReturnsTypedRejectionsForMalformedCommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		code command.ParseErrorCode
	}{
		{name: "unknown verb", body: "/factory deploy", code: command.ParseErrorUnknownVerb},
		{name: "unexpected argument", body: "/factory status now", code: command.ParseErrorUnexpectedArgument},
		{name: "missing harness", body: "/factory config", code: command.ParseErrorMissingHarness},
		{name: "unsupported harness", body: "/factory config harness=llama", code: command.ParseErrorUnsupportedHarness},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			parsed, err := command.Parse(test.body)
			if err == nil {
				t.Fatal("Parse() error = nil, want typed rejection")
			}
			if !parsed.Recognized {
				t.Fatal("malformed command was not recognized")
			}
			parseErr, ok := err.(*command.ParseError)
			if !ok {
				t.Fatalf("Parse() error type = %T, want *command.ParseError", err)
			}
			if parseErr.Code != test.code {
				t.Fatalf("Parse() error code = %q, want %q", parseErr.Code, test.code)
			}
			if parseErr.Problem == "" {
				t.Fatal("Parse() rejection has no human-readable problem")
			}
		})
	}
}

// TestParseSupportsHarnessConfigurationForms verifies the extensible harness
// configuration verb accepts its documented equivalent forms.
func TestParseSupportsHarnessConfigurationForms(t *testing.T) {
	t.Parallel()

	for _, body := range []string{
		"/factory config harness=codex",
		"/factory configure harness=claude",
		"/factory harness codex",
	} {
		parsed, err := command.Parse(body)
		if err != nil {
			t.Fatalf("Parse(%q) error = %v", body, err)
		}
		if parsed.Command.Kind != command.ConfigureHarness || parsed.Command.Harness == "" {
			t.Fatalf("Parse(%q) = %#v, want harness configuration", body, parsed.Command)
		}
	}
}

// TestParseRecognizesTheAuthorizedCancelCommand verifies cancellation uses the
// same explicit, single-comment command grammar as the existing controls.
func TestParseRecognizesTheAuthorizedCancelCommand(t *testing.T) {
	t.Parallel()

	parsed, err := command.Parse("/factory cancel")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if !parsed.Recognized || parsed.Command.Kind != command.Cancel {
		t.Fatalf("parsed cancel = %#v, want recognized cancel command", parsed)
	}
}
