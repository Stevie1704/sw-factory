package factory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// validateClarificationQuestions rejects report questions that would reuse an
// already-resolved or still-pending identifier in the current packet.
func validateClarificationQuestions(packet SpecificationPacket, pending []store.PendingQuestion, questions []report.Question) error {
	if len(questions) == 0 {
		return errors.New("clarification report must contain at least one question")
	}
	seen := make(map[string]struct{}, len(packet.Clarifications)+len(questions))
	for _, clarification := range packet.Clarifications {
		if !clarificationIdentifierSafe(clarification.QuestionID) {
			return fmt.Errorf("specification packet contains unsafe clarification identifier %q", clarification.QuestionID)
		}
		if _, exists := seen[clarification.QuestionID]; exists {
			return fmt.Errorf("specification packet repeats clarification identifier %q", clarification.QuestionID)
		}
		seen[clarification.QuestionID] = struct{}{}
	}
	for _, question := range pending {
		if !clarificationIdentifierSafe(question.ID) {
			return fmt.Errorf("pending clarification identifier %q is unsafe", question.ID)
		}
		if _, exists := seen[question.ID]; exists {
			return fmt.Errorf("pending clarification identifier %q was already resolved", question.ID)
		}
		seen[question.ID] = struct{}{}
	}
	for _, question := range questions {
		if !clarificationIdentifierSafe(question.ID) {
			return fmt.Errorf("clarification question identifier %q is unsafe", question.ID)
		}
		if _, exists := seen[question.ID]; exists {
			return fmt.Errorf("clarification question identifier %q was already resolved", question.ID)
		}
		seen[question.ID] = struct{}{}
	}
	return nil
}

// clarificationIdentifierSafe accepts the bounded identifier vocabulary shared
// by report questions, store projections, and answer commands.
func clarificationIdentifierSafe(value string) bool {
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

// pendingQuestionsFromReport converts the report protocol into the small
// operational projection persisted on the run.
func pendingQuestionsFromReport(questions []report.Question) []store.PendingQuestion {
	pending := make([]store.PendingQuestion, 0, len(questions))
	for _, question := range questions {
		pending = append(pending, store.PendingQuestion{
			ID:     strings.TrimSpace(question.ID),
			Prompt: strings.TrimSpace(question.Prompt),
		})
	}
	return pending
}

// postClarification publishes the current questions on the active supervision
// target, using the pull request once a run has one and the issue beforehand.
func (s *Service) postClarification(ctx context.Context, registration config.RepositoryRegistration, run store.Run, packetVersion int, questions []store.PendingQuestion) (github.Comment, error) {
	target := run.IssueNumber
	if run.PullRequestNumber > 0 {
		target = run.PullRequestNumber
	}
	body := clarificationCommentBody(run, packetVersion, questions)
	comment, err := s.deps.GitHub.CreateIssueComment(ctx, commandRepository(registration), target, body)
	if err != nil {
		return github.Comment{}, fmt.Errorf("post clarification questions on #%d: %w", target, err)
	}
	if strings.TrimSpace(comment.ID) == "" {
		return github.Comment{}, fmt.Errorf("post clarification questions on #%d returned an empty comment id", target)
	}
	return comment, nil
}

// clarificationCommentBody renders a bounded, machine-visible question list
// without making ordinary prose an executable command.
func clarificationCommentBody(run store.Run, packetVersion int, questions []store.PendingQuestion) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "<!-- factory-clarification: %s -->\n", run.ID)
	builder.WriteString("## Factory clarification requested\n\n")
	fmt.Fprintf(&builder, "This run is waiting for human input for specification packet version `%d`.\n\n", packetVersion)
	builder.WriteString("Questions:\n")
	for _, question := range questions {
		fmt.Fprintf(&builder, "- `%s`: %s\n", safeStatusCommentValue(question.ID), safeStatusCommentValue(question.Prompt))
	}
	builder.WriteString("\nTo answer, post a comment beginning with `/factory answer <question-id> <answer>`.\n")
	return builder.String()
}

// notifyClarification raises a concise cmux notification after the questions
// and waiting state have been durably projected.
func (s *Service) notifyClarification(ctx context.Context, registration config.RepositoryRegistration, run store.Run) error {
	body := fmt.Sprintf("%s is waiting for answers to %d clarification question(s)", run.ID, len(run.PendingQuestions))
	return s.notifyWorkspace(ctx, registration, "factory clarification requested", body)
}

// ensureClarificationPublication retries the two external attention effects
// from durable run markers, so a GitHub or cmux outage cannot strand a waiting
// run with no recoverable publication path.
func (s *Service) ensureClarificationPublication(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run) (store.Run, error) {
	if run.Status != store.StatusWaitingForHuman || len(run.PendingQuestions) == 0 {
		return run, nil
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return run, fmt.Errorf("decode specification packet for clarification publication: %w", err)
	}
	if run.ClarificationCommentID == "" {
		comment, postErr := s.postClarification(ctx, registration, run, packet.Version, run.PendingQuestions)
		if postErr != nil {
			return run, postErr
		}
		run.ClarificationCommentID = comment.ID
		run.UpdatedAt = s.deps.Now().UTC()
		if err := saveRunWithRetry(ctx, runStore, run); err != nil {
			return run, fmt.Errorf("persist clarification comment identity: %w", err)
		}
	}
	if !run.ClarificationNotificationSent {
		if err := s.notifyClarification(ctx, registration, run); err != nil {
			return run, err
		}
		run.ClarificationNotificationSent = true
		run.UpdatedAt = s.deps.Now().UTC()
		if err := saveRunWithRetry(ctx, runStore, run); err != nil {
			return run, fmt.Errorf("persist clarification notification state: %w", err)
		}
	}
	return run, nil
}

// pendingQuestion returns one run question by its stable identifier.
func pendingQuestion(questions []store.PendingQuestion, questionID string) (store.PendingQuestion, bool) {
	for _, question := range questions {
		if question.ID == questionID {
			return question, true
		}
	}
	return store.PendingQuestion{}, false
}
