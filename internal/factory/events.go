package factory

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// EventKind identifies the coordinator observation represented by an event.
type EventKind string

const (
	// EventPollPass reports one completed queue observation.
	EventPollPass EventKind = "poll_pass"
	// EventClaim reports a newly claimed issue.
	EventClaim EventKind = "claim"
	// EventProgressionPass reports why one unattended progression pass stopped.
	EventProgressionPass EventKind = "progression_pass"
	// EventProgressionStep reports one selected unattended transition.
	EventProgressionStep EventKind = "progression_step"
	// EventRetry reports a retryable coordinator failure and its attempt.
	EventRetry EventKind = "retry"
	// EventCommandFailure reports a swallowed command-polling failure.
	EventCommandFailure EventKind = "command_failure"
	// EventStageTransition reports a persisted change of run stage.
	EventStageTransition EventKind = "stage_transition"
)

// CoordinatorEvent is a content-free, structured observation from the
// persistent coordinator. Only identifiers, outcomes, reasons, counts, and
// stage or retry metadata belong in an event; issue and comment content,
// credentials, and credential paths do not.
type CoordinatorEvent struct {
	// Timestamp is the coordinator clock observation at emission time.
	Timestamp time.Time `json:"timestamp"`
	// Kind identifies which event fields are populated.
	Kind EventKind `json:"kind"`
	// RunID identifies the run associated with the observation, when known.
	RunID string `json:"run_id,omitempty"`
	// Outcome is the poll or progression outcome, when applicable.
	Outcome string `json:"outcome,omitempty"`
	// IssueNumber identifies a claimed issue, when applicable.
	IssueNumber int `json:"issue_number,omitempty"`
	// Step is the human-readable coordinator transition name, when applicable.
	Step string `json:"step,omitempty"`
	// Steps is the number of transitions completed in a progression pass.
	Steps int `json:"steps,omitempty"`
	// Reason is a bounded coordinator reason when it is safe to publish.
	Reason string `json:"reason,omitempty"`
	// Operation identifies the retrying coordinator operation.
	Operation string `json:"operation,omitempty"`
	// Attempt is the one-based retry attempt number.
	Attempt int `json:"attempt,omitempty"`
	// MaxAttempts is the configured retry ceiling, when one exists.
	MaxAttempts int `json:"max_attempts,omitempty"`
	// Code identifies a stable command failure code, when applicable.
	Code string `json:"code,omitempty"`
	// FromStage is the stage before a stage transition.
	FromStage store.Stage `json:"from_stage,omitempty"`
	// ToStage is the stage after a stage transition.
	ToStage store.Stage `json:"to_stage,omitempty"`
}

// Event is an alias for the public coordinator event value.
type Event = CoordinatorEvent

// EventSink receives structured coordinator events at the poll-loop seam.
type EventSink interface {
	Emit(CoordinatorEvent)
}

// EventSinkFunc adapts a function to the coordinator event sink interface.
type EventSinkFunc func(CoordinatorEvent)

// Emit sends one event to the adapted function.
func (f EventSinkFunc) Emit(event CoordinatorEvent) {
	if f != nil {
		f(event)
	}
}

// DiscardEventSink is the default sink for non-verbose coordinator runs.
type DiscardEventSink struct{}

// Emit discards one coordinator event.
func (DiscardEventSink) Emit(CoordinatorEvent) {}

// NewTextEventSink creates a sink that renders safe coordinator events to a
// writer. The renderer is intentionally kept at the edge so other consumers
// can observe the same structured values without parsing text.
func NewTextEventSink(output io.Writer) EventSink {
	return textEventSink{output: output}
}

// textEventSink is the CLI's line-oriented event adapter.
type textEventSink struct {
	output io.Writer
}

// Emit renders one coordinator event as a single timestamped, content-free
// line. Writer failures are intentionally ignored so observability cannot
// change coordinator ownership or progression behavior.
func (s textEventSink) Emit(event CoordinatorEvent) {
	if s.output == nil {
		return
	}
	fields := []string{event.Timestamp.UTC().Format(time.RFC3339Nano), string(event.Kind)}
	runID := event.RunID
	if runID == "" {
		runID = "-"
	}
	fields = append(fields, "run="+singleLineEventValue(runID))
	if event.Outcome != "" {
		fields = append(fields, "outcome="+singleLineEventValue(event.Outcome))
	}
	if event.IssueNumber != 0 {
		fields = append(fields, fmt.Sprintf("issue=#%d", event.IssueNumber))
	}
	if event.Step != "" {
		fields = append(fields, "step="+strconv.Quote(singleLineEventValue(event.Step)))
	}
	if event.Kind == EventProgressionPass {
		fields = append(fields, fmt.Sprintf("steps=%d", event.Steps))
	}
	if event.Reason != "" {
		fields = append(fields, "reason="+strconv.Quote(singleLineEventValue(event.Reason)))
	}
	if event.Operation != "" {
		fields = append(fields, "operation="+singleLineEventValue(event.Operation))
	}
	if event.Attempt != 0 {
		if event.MaxAttempts != 0 {
			fields = append(fields, fmt.Sprintf("attempt=%d/%d", event.Attempt, event.MaxAttempts))
		} else {
			fields = append(fields, fmt.Sprintf("attempt=%d", event.Attempt))
		}
	}
	if event.Code != "" {
		fields = append(fields, "code="+singleLineEventValue(event.Code))
	}
	if event.FromStage != "" || event.ToStage != "" {
		from := string(event.FromStage)
		if from == "" {
			from = "-"
		}
		to := string(event.ToStage)
		if to == "" {
			to = "-"
		}
		fields = append(fields, "from_stage="+singleLineEventValue(from), "to_stage="+singleLineEventValue(to))
	}
	_, _ = io.WriteString(s.output, strings.Join(fields, " ")+"\n")
}

// singleLineEventValue prevents an event value from changing the CLI's line
// structure if a future coordinator field is sourced from external input.
func singleLineEventValue(value string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\x00', '\r', '\n', '\t':
			return ' '
		default:
			return r
		}
	}, value)
}

// coordinatorEventSink selects the first supplied sink or the discard sink.
func coordinatorEventSink(sinks []EventSink) EventSink {
	if len(sinks) > 0 && sinks[0] != nil {
		return sinks[0]
	}
	return DiscardEventSink{}
}

// emitCoordinatorEvent attaches the coordinator clock to one event before it
// crosses the event sink seam.
func (s *Service) emitCoordinatorEvent(sink EventSink, event CoordinatorEvent) {
	event.Timestamp = s.deps.Now().UTC()
	sink.Emit(event)
}

// emitStageTransition publishes a changed workflow stage observed around one
// progression action.
func (s *Service) emitStageTransition(sink EventSink, runID string, from, to store.Stage) {
	if from == to {
		return
	}
	s.emitCoordinatorEvent(sink, CoordinatorEvent{
		Kind:      EventStageTransition,
		RunID:     runID,
		FromStage: from,
		ToStage:   to,
	})
}

// progressionOutcomeForEvent converts an errored progression pass into a
// stable observation without copying arbitrary adapter error text into events.
func progressionOutcomeForEvent(result progressionResult, err error) string {
	if err != nil {
		return "error"
	}
	return string(result.Outcome)
}

// progressionReasonForEvent keeps only reasons produced by the progression
// planner. Failure causes and arbitrary lifecycle reasons can contain adapter
// paths or external content, so they are intentionally not copied into the
// event.
func progressionReasonForEvent(result progressionResult) string {
	switch result.Outcome {
	case progressionIdle:
		if result.Reason == "no run to drive" || strings.HasPrefix(result.Reason, "run disappeared after ") {
			return result.Reason
		}
	case progressionInvocationActive:
		if strings.HasPrefix(result.Reason, "invocations ") {
			return result.Reason
		}
	case progressionUnsupported:
		return "operational store has no durable invocation history"
	case progressionDraftPullRequest:
		if strings.HasPrefix(result.Reason, "draft pull request #") {
			return result.Reason
		}
	case progressionTerminal:
		if strings.HasPrefix(result.Reason, "run is ") {
			return result.Reason
		}
	case progressionWaiting:
		// Waiting reasons may come from a persisted lifecycle projection. The
		// named planner step is enough to identify this safe observation.
		if result.Step == "agent deadline" || result.Step == "pull-request readiness" {
			return result.Reason
		}
	}
	return ""
}

// commandFailureCode converts a swallowed command-polling error into stable
// content-free event metadata without copying its potentially unsafe message.
func commandFailureCode(err error) string {
	var rejection *PolicyRejection
	if errors.As(err, &rejection) {
		return string(rejection.Code)
	}
	var transportErr *pollingTransportError
	if errors.As(err, &transportErr) {
		return "transport"
	}
	return "polling_failed"
}
