package sessioncommand

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/localissues"
	"github.com/Stevie1704/sw-factory/internal/store"
)

const (
	// KindApproveStart requests separate exact execution authority and content publication.
	KindApproveStart Kind = "approve_start"
	// KindPreviewExecution renders committed backlog content without session mutation.
	KindPreviewExecution Kind = "preview_execution"
	// KindAuthorizeExecution approves a displayed exact backlog preview.
	KindAuthorizeExecution Kind = "authorize_execution"
	// KindAnswerRun answers a bound displayed Run question.
	KindAnswerRun Kind = "answer_run"
	// KindCancelRun invokes the supported Run cancellation semantics.
	KindCancelRun Kind = "cancel_run"
	// KindRetryRun invokes the supported Run retry semantics.
	KindRetryRun Kind = "retry_run"
	// KindResumeRun invokes the supported Run resume semantics.
	KindResumeRun Kind = "resume_run"
)

// ApproveStartPayload binds publication approval plus an explicit executable subset.
type ApproveStartPayload struct {
	ApprovePayload
	SelectedIssueIDs []string `json:"selected_issue_ids"`
}

// ExecutionPreviewPayload selects exact committed executable work to review.
type ExecutionPreviewPayload struct {
	SelectedIssueIDs []string `json:"selected_issue_ids"`
}

// ExecutionAuthorizationPayload binds one durable exact execution preview.
type ExecutionAuthorizationPayload struct {
	PreviewID                 string   `json:"preview_id"`
	PayloadSHA256             string   `json:"payload_sha256"`
	RelationshipMappingSHA256 string   `json:"relationship_mapping_sha256"`
	GraphDigest               string   `json:"graph_digest"`
	SelectedIssueIDs          []string `json:"selected_issue_ids"`
}

// RunOperatorPayload binds a supported operation to the displayed frozen state.
type RunOperatorPayload struct {
	PacketVersion  int    `json:"packet_version"`
	QuestionID     string `json:"question_id,omitempty"`
	QuestionDigest string `json:"question_digest,omitempty"`
	Answer         string `json:"answer,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

// OperatorCommands is the coordinator-only extension of the existing dispatcher.
type OperatorCommands interface {
	ApplyOperatorCommand(ctx context.Context, command store.IdeationCommand) (Outcome, error)
}

// validateSelected rejects duplicated, fabricated and unbounded selection IDs.
func validateSelected(ids []string) error {
	if len(ids) == 0 || len(ids) > localissues.MaxIssues {
		return errors.New("select at least one bounded executable local issue")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !localissues.ValidID(id) || seen[id] {
			return errors.New("selected_issue_ids must contain unique stable local issue identities")
		}
		seen[id] = true
	}
	return nil
}

// validate checks both exact publication digests and selected execution scope.
func (p *ApproveStartPayload) validate() error {
	if err := p.ApprovePayload.validate(); err != nil {
		return err
	}
	return validateSelected(p.SelectedIssueIDs)
}

// validate checks the explicit committed issue selection.
func (p *ExecutionPreviewPayload) validate() error { return validateSelected(p.SelectedIssueIDs) }

// validate checks durable preview identity and exact displayed digests.
func (p *ExecutionAuthorizationPayload) validate() error {
	if !safeIdentifier(p.PreviewID) {
		return errors.New("preview_id must name a displayed execution preview")
	}
	for _, digest := range []string{p.PayloadSHA256, p.RelationshipMappingSHA256, p.GraphDigest} {
		if !strings.HasPrefix(digest, "sha256:") || !sha256Hex(strings.TrimPrefix(digest, "sha256:")) {
			return errors.New("execution digests must be sha256: followed by lowercase SHA-256")
		}
	}
	return validateSelected(p.SelectedIssueIDs)
}

// validate bounds the displayed Run packet/question and operator text.
func (p *RunOperatorPayload) validate() error {
	if p.PacketVersion < 1 {
		return errors.New("packet_version must be the displayed Specification version")
	}
	if p.QuestionID != "" && !safeIdentifier(p.QuestionID) {
		return errors.New("question_id must name the displayed pending question")
	}
	if p.QuestionDigest != "" && (!strings.HasPrefix(p.QuestionDigest, "sha256:") || !sha256Hex(strings.TrimPrefix(p.QuestionDigest, "sha256:"))) {
		return errors.New("question_digest must bind the displayed question")
	}
	if err := boundedText("answer", p.Answer, store.MaxIdeationTurnBytes, false); err != nil {
		return err
	}
	return boundedText("reason", p.Reason, store.MaxIdeationReasonBytes, false)
}

// commandScope returns the declaration's allowed subject type.
func commandScope(kind Kind) string {
	switch kind {
	case KindPreviewExecution, KindAuthorizeExecution:
		return "backlog"
	case KindAnswerRun, KindCancelRun, KindRetryRun, KindResumeRun:
		return "run"
	default:
		return "session"
	}
}

// EffectiveScope preserves all old unscoped session envelopes.
func EffectiveScope(command store.IdeationCommand) string {
	if command.Scope == "" {
		return "session"
	}
	return command.Scope
}

// validateCommandScope refuses cross-subject commands and stale-less mutations.
func validateCommandScope(envelope request) error {
	scope := envelope.Scope
	if scope == "" {
		scope = "session"
	}
	if scope != commandScope(envelope.Kind) {
		return invalid("%s command requires %s scope", envelope.Kind, commandScope(envelope.Kind))
	}
	if scope == "session" {
		if envelope.SubjectID != "" || !safeIdentifier(envelope.SessionID) {
			return invalid("session commands require session_id only")
		}
	} else {
		if envelope.SessionID != "" || !safeIdentifier(envelope.SubjectID) {
			return invalid("backlog/run commands require subject_id and no session_id")
		}
		if scope == "backlog" && envelope.SubjectID != "repository" {
			return invalid("backlog subject_id must be repository")
		}
	}
	if envelope.ExpectedRevision < 0 {
		return invalid("expected_revision cannot be negative")
	}
	if envelope.Kind != KindCreateSession && envelope.Kind != KindPreviewExecution && envelope.ExpectedRevision < 1 {
		return invalid("expected_revision must be the displayed subject revision")
	}
	if envelope.Kind == KindCreateSession && envelope.ExpectedRevision != 0 {
		return invalid("create_session must use expected_revision 0")
	}
	return nil
}

// sortedSelection returns deterministic executable identities for authority replay.
func sortedSelection(ids []string) []string {
	result := slices.Clone(ids)
	slices.Sort(result)
	return result
}

// operatorUnavailable is the explicit result when composition lacks local commands.
func operatorUnavailable(kind Kind) Outcome {
	return Outcome{Code: store.IdeationCommandUnsupported, Detail: fmt.Sprintf("this coordinator does not support %s", kind)}
}

// ValidateSubject repeats strict scope/payload validation at trusted service admission.
func ValidateSubject(command store.IdeationCommand) error {
	if err := validateCommandScope(request{RequestID: command.RequestID, Scope: command.Scope, SubjectID: command.SubjectID, SessionID: command.SessionID, Kind: Kind(command.Kind), ExpectedRevision: command.ExpectedRevision}); err != nil {
		return err
	}
	_, err := DecodePayload(Kind(command.Kind), command.Payload)
	return err
}
