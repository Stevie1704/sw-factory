package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	commandlanguage "github.com/Stevie1704/sw-factory/internal/command"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// CommandOutcome describes what happened to one recognized comment.
type CommandOutcome string

const (
	// CommandIgnored means the comment was ordinary discussion, not a command.
	CommandIgnored CommandOutcome = "ignored"
	// CommandAccepted means the coordinator applied the command once.
	CommandAccepted CommandOutcome = "accepted"
	// CommandRejected means the command was recognized but denied by policy.
	CommandRejected CommandOutcome = "rejected"
	// CommandReplayed means the comment ID was already behind the persisted
	// watermark and no command effect was repeated.
	CommandReplayed CommandOutcome = "replayed"
)

// PolicyRejectionCode identifies a typed command policy refusal.
type PolicyRejectionCode string

const (
	// PolicyRejectionUnauthorized means the GitHub author is not configured.
	PolicyRejectionUnauthorized PolicyRejectionCode = "unauthorized_author"
	// PolicyRejectionMalformed means the structured command grammar was invalid.
	PolicyRejectionMalformed PolicyRejectionCode = "malformed_command"
	// PolicyRejectionNoRun means no claimed run can receive the command.
	PolicyRejectionNoRun PolicyRejectionCode = "no_run"
	// PolicyRejectionWrongTarget means the comment targets another issue or PR.
	PolicyRejectionWrongTarget PolicyRejectionCode = "wrong_target"
	// PolicyRejectionRetryState means retry is not legal for the current state.
	PolicyRejectionRetryState PolicyRejectionCode = "retry_state"
	// PolicyRejectionHarnessOverride means repository configuration disallows the
	// requested harness override.
	PolicyRejectionHarnessOverride PolicyRejectionCode = "harness_override"
	// PolicyRejectionHarnessUnavailable means the current role has no adapter
	// for the requested harness yet.
	PolicyRejectionHarnessUnavailable PolicyRejectionCode = "harness_unavailable"
	// PolicyRejectionModelOverride means the requested model is outside the
	// role's repository-declared options.
	PolicyRejectionModelOverride PolicyRejectionCode = "model_override"
	// PolicyRejectionModelUnavailable means the role declares no model at all.
	PolicyRejectionModelUnavailable PolicyRejectionCode = "model_unavailable"
	// PolicyRejectionReasoningEffortOverride means the requested reasoning
	// effort is outside the role's repository-declared options.
	PolicyRejectionReasoningEffortOverride PolicyRejectionCode = "reasoning_effort_override"
	// PolicyRejectionCancelState means cancellation is not legal for the run.
	PolicyRejectionCancelState PolicyRejectionCode = "cancel_state"
	// PolicyRejectionAnswerState means the run is not waiting for clarification.
	PolicyRejectionAnswerState PolicyRejectionCode = "answer_state"
	// PolicyRejectionAnswerQuestion means an answer referenced no pending question.
	PolicyRejectionAnswerQuestion PolicyRejectionCode = "answer_question"
	// PolicyRejectionRefreshState means refresh is not legal for the run.
	PolicyRejectionRefreshState PolicyRejectionCode = "refresh_state"
)

// PolicyRejection is returned after a recognized command is visibly recorded
// as denied. Its stable code, rather than its message, controls policy logic.
type PolicyRejection struct {
	// Code is stable machine-readable policy vocabulary.
	Code PolicyRejectionCode
	// Problem is the operator-facing explanation rendered in the status comment.
	Problem string
}

// Error returns a human-readable typed policy rejection.
func (e *PolicyRejection) Error() string {
	if e == nil {
		return "policy rejection"
	}
	return fmt.Sprintf("policy rejection (%s): %s", e.Code, e.Problem)
}

// CommandRequest supplies one GitHub issue or pull-request comment to the
// coordinator. The target number is the issue number or PR number whose
// shared GitHub comments endpoint returned the comment.
type CommandRequest struct {
	// RunID optionally selects one active or latest run explicitly.
	RunID string
	// IssueNumber identifies the issue or pull request being commented on.
	IssueNumber int
	// Comment contains the immutable comment identity, author, and body.
	Comment github.Comment
}

// CommandPollRequest selects the run whose issue and pull-request comments
// should be scanned for structured commands.
type CommandPollRequest struct {
	// RunID optionally selects one active or latest run explicitly.
	RunID string
}

// CommandResult reports one command decision and the resulting persisted run.
type CommandResult struct {
	// Outcome distinguishes ordinary discussion, acceptance, rejection, and replay.
	Outcome CommandOutcome
	// Command is the parsed command when parsing succeeded.
	Command commandlanguage.Request
	// Rejection is populated for a typed policy refusal.
	Rejection *PolicyRejection
	// Run is the run projection after the command decision.
	Run store.Run
}

// HandleCommand recognizes and processes one GitHub comment exactly once.
// Ordinary discussion is ignored. Recognized policy rejections are persisted
// and reflected in the existing status comment before the typed error returns.
func (s *Service) HandleCommand(ctx context.Context, request CommandRequest) (CommandResult, error) {
	parsed, parseErr := commandlanguage.Parse(request.Comment.Body)
	if !parsed.Recognized {
		return CommandResult{Outcome: CommandIgnored}, nil
	}
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	registration, runStore, run, err := s.openCommandRunStore(ctx)
	if err != nil {
		return CommandResult{}, err
	}
	defer func() { _ = runStore.Close() }()
	return s.handleRecognizedCommand(ctx, registration, runStore, run, request, parsed, parseErr)
}

// handleRecognizedCommand applies one parsed command using an already-open run
// store. Polling uses this seam to keep one store open for its full batch.
func (s *Service) handleRecognizedCommand(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run *store.Run, request CommandRequest, parsed commandlanguage.ParseResult, parseErr error) (CommandResult, error) {
	if request.IssueNumber <= 0 {
		return CommandResult{}, errors.New("command issue number must be positive")
	}
	if strings.TrimSpace(request.Comment.ID) == "" {
		return CommandResult{}, errors.New("command comment id is required")
	}
	if run == nil {
		return CommandResult{Outcome: CommandRejected}, &PolicyRejection{Code: PolicyRejectionNoRun, Problem: "no claimed run is available for this command"}
	}
	if request.RunID != "" && request.RunID != run.ID {
		return CommandResult{Outcome: CommandRejected, Run: *run}, &PolicyRejection{Code: PolicyRejectionWrongTarget, Problem: fmt.Sprintf("run %q is not the selected run", request.RunID)}
	}
	if request.IssueNumber != run.IssueNumber && request.IssueNumber != run.PullRequestNumber {
		return CommandResult{Outcome: CommandRejected, Run: *run}, &PolicyRejection{Code: PolicyRejectionWrongTarget, Problem: fmt.Sprintf("comment target #%d does not belong to run %q", request.IssueNumber, run.ID)}
	}
	if (run.Status == store.StatusComplete || run.Status == store.StatusCancelled) && !run.LifecycleNotificationSent {
		updated, notificationErr := s.ensureTerminalNotification(ctx, registration, runStore, *run)
		if notificationErr != nil {
			return CommandResult{Outcome: CommandReplayed, Command: parsed.Command, Run: updated}, notificationErr
		}
		*run = updated
	}
	if commentAlreadyProcessed(run.ProcessedCommentID, request.Comment.ID) {
		return CommandResult{Outcome: CommandReplayed, Command: parsed.Command, Run: *run}, nil
	}
	if !authorizedCommentAuthor(registration.AuthorizedUsers, request.Comment.Author) {
		rejection := &PolicyRejection{Code: PolicyRejectionUnauthorized, Problem: fmt.Sprintf("GitHub author %q is not an authorized maintainer", request.Comment.Author)}
		return s.persistCommandRejection(ctx, registration, runStore, *run, request.Comment, parsed.Command, rejection)
	}
	if parseErr != nil {
		rejection := &PolicyRejection{Code: PolicyRejectionMalformed, Problem: parseErr.Error()}
		return s.persistCommandRejection(ctx, registration, runStore, *run, request.Comment, parsed.Command, rejection)
	}
	if published, publicationErr := s.ensureClarificationPublication(ctx, registration, runStore, *run); publicationErr != nil {
		return CommandResult{Outcome: CommandRejected, Command: parsed.Command, Run: published}, publicationErr
	} else {
		*run = published
	}

	switch parsed.Command.Kind {
	case commandlanguage.Status:
		updated, persistErr := s.persistCommandProjection(ctx, registration, runStore, *run, request.Comment, parsed.Command, string(parsed.Command.Kind), "command accepted")
		return CommandResult{Outcome: CommandAccepted, Command: parsed.Command, Run: updated}, persistErr
	case commandlanguage.Refresh:
		return s.handleRefreshCommand(ctx, registration, runStore, *run, request.Comment, parsed.Command)
	case commandlanguage.Answer:
		return s.handleAnswerCommand(ctx, registration, runStore, *run, request.Comment, parsed.Command)
	case commandlanguage.Cancel:
		return s.handleCancelCommand(ctx, registration, runStore, *run, request.Comment, parsed.Command)
	case commandlanguage.ConfigureHarness:
		return s.handleHarnessConfiguration(ctx, registration, runStore, *run, request.Comment, parsed.Command)
	case commandlanguage.Retry:
		return s.handleRetryCommand(ctx, registration, runStore, *run, request.Comment, parsed.Command)
	default:
		rejection := &PolicyRejection{Code: PolicyRejectionMalformed, Problem: "the parsed command is not registered by the coordinator"}
		return s.persistCommandRejection(ctx, registration, runStore, *run, request.Comment, parsed.Command, rejection)
	}
}

// runResultInvalidator is the persistence seam used when a new specification
// packet makes prior invocations and checkpoint results ineligible.
type runResultInvalidator interface {
	InvalidateRunResults(context.Context, string) error
}

// atomicPacketTransitionStore is the persistence seam that atomically applies
// a state transition with result invalidation, ensuring packet persistence,
// command watermark updates, and result invalidation commit or roll back together.
type atomicPacketTransitionStore interface {
	SaveRunAndInvalidateResults(context.Context, int64, store.Run) error
}

// handleAnswerCommand applies one authorized answer, versions the packet, and
// resumes the implementation role with that answer mounted in its packet.
func (s *Service) handleAnswerCommand(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, comment github.Comment, parsed commandlanguage.Request) (CommandResult, error) {
	if run.Status != store.StatusWaitingForHuman && !(run.Status == store.StatusActive && len(run.PendingQuestions) > 0) {
		rejection := &PolicyRejection{Code: PolicyRejectionAnswerState, Problem: fmt.Sprintf("answer is only allowed while the run waits for human input, not status %q", run.Status)}
		return s.persistCommandRejection(ctx, registration, runStore, run, comment, parsed, rejection)
	}
	pending, found := pendingQuestion(run.PendingQuestions, parsed.QuestionID)
	if !found {
		rejection := &PolicyRejection{Code: PolicyRejectionAnswerQuestion, Problem: fmt.Sprintf("question %q is not pending for run %q", parsed.QuestionID, run.ID)}
		return s.persistCommandRejection(ctx, registration, runStore, run, comment, parsed, rejection)
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return CommandResult{}, fmt.Errorf("decode specification packet for answer: %w", err)
	}
	packet.Clarifications = append(packet.Clarifications, Clarification{QuestionID: pending.ID, Question: pending.Prompt, Answer: parsed.Answer})
	packet.Version++
	encodedPacket, err := json.Marshal(packet)
	if err != nil {
		return CommandResult{}, fmt.Errorf("encode answered specification packet: %w", err)
	}
	next := commandProjection(run, comment, parsed, string(parsed.Kind), "command accepted; clarification answered")
	next.Status = store.StatusActive
	next.Stage = packetResumeStageForPacket(run, packet)
	next.SpecificationPacket = string(encodedPacket)
	resetTestProjectionForPacketChange(&next, packet)
	next.PendingQuestions = removePendingQuestion(run.PendingQuestions, parsed.QuestionID)
	next.ClarificationCommentID = ""
	next.ClarificationNotificationSent = false
	next.UpdatedAt = s.deps.Now().UTC()
	// Clear ProcessedCommentID temporarily to defer watermark until after resumption
	processedCommentID := next.ProcessedCommentID
	next.ProcessedCommentID = run.ProcessedCommentID
	if err := s.stopRunWorkerIfActive(ctx, runStore, run); err != nil {
		return CommandResult{}, err
	}
	issue, err := s.deps.GitHub.Issue(ctx, commandRepository(registration), run.IssueNumber)
	if err != nil {
		return CommandResult{}, fmt.Errorf("read issue for answer: %w", err)
	}
	updated, err := s.applyPacketChangeTransition(ctx, runStore, registration, issue, run, next)
	if err != nil {
		return CommandResult{}, err
	}
	updated, err = s.resumeAfterPacketChange(ctx, registration, runStore, updated)
	if err != nil {
		return CommandResult{}, fmt.Errorf("resume implementation after answer: %w", err)
	}
	// Claim the comment watermark only after successful resumption to keep
	// packet-change resumption retryable when startAgentWithStore fails.
	updated, err = s.persistPacketChangeWatermark(ctx, runStore, updated, processedCommentID)
	if err != nil {
		return CommandResult{}, fmt.Errorf("persist answer command watermark: %w", err)
	}
	return CommandResult{Outcome: CommandAccepted, Command: parsed, Run: updated}, nil
}

// handleRefreshCommand re-reads the issue, creates a new packet version, and
// resumes implementation against the refreshed specification.
func (s *Service) handleRefreshCommand(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, comment github.Comment, parsed commandlanguage.Request) (CommandResult, error) {
	if store.IsTerminalStatus(run.Status) {
		rejection := &PolicyRejection{Code: PolicyRejectionRefreshState, Problem: fmt.Sprintf("refresh is not allowed after run status %q", run.Status)}
		return s.persistCommandRejection(ctx, registration, runStore, run, comment, parsed, rejection)
	}
	if run.CheckRepairPendingAttempt != 0 {
		rejection := &PolicyRejection{Code: PolicyRejectionRefreshState, Problem: fmt.Sprintf("refresh is blocked while check-repair attempt %d awaits reconciliation", run.CheckRepairPendingAttempt)}
		return s.persistCommandRejection(ctx, registration, runStore, run, comment, parsed, rejection)
	}
	oldPacket, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return CommandResult{}, fmt.Errorf("decode specification packet for refresh: %w", err)
	}
	issue, err := s.deps.GitHub.Issue(ctx, commandRepository(registration), run.IssueNumber)
	if err != nil {
		return CommandResult{}, fmt.Errorf("read issue for refresh: %w", err)
	}
	if issue.Number == 0 {
		issue.Number = run.IssueNumber
	}
	if issue.Number != run.IssueNumber || issue.IsPullRequest {
		return CommandResult{}, fmt.Errorf("refresh returned an invalid issue identity for #%d", run.IssueNumber)
	}
	if !strings.EqualFold(strings.TrimSpace(issue.State), "open") {
		rejection := &PolicyRejection{Code: PolicyRejectionRefreshState, Problem: fmt.Sprintf("cannot refresh a %s issue", defaultString(issue.State, "non-open"))}
		return s.persistCommandRejection(ctx, registration, runStore, run, comment, parsed, rejection)
	}
	refreshedPacket := oldPacket
	refreshedPacket.Version++
	refreshedPacket.Issue = cloneIssue(issue)
	encodedPacket, err := json.Marshal(refreshedPacket)
	if err != nil {
		return CommandResult{}, fmt.Errorf("encode refreshed specification packet: %w", err)
	}
	if err := s.stopRunWorkerIfActive(ctx, runStore, run); err != nil {
		return CommandResult{}, err
	}
	next := commandProjection(run, comment, parsed, string(parsed.Kind), "command accepted; specification refreshed")
	next.Status = store.StatusActive
	next.Stage = packetResumeStageForPacket(run, refreshedPacket)
	next.SpecificationPacket = string(encodedPacket)
	resetTestProjectionForPacketChange(&next, refreshedPacket)
	next.PendingQuestions = nil
	next.ClarificationCommentID = ""
	next.ClarificationNotificationSent = false
	next.UpdatedAt = s.deps.Now().UTC()
	// Clear ProcessedCommentID temporarily to defer watermark until after resumption
	processedCommentID := next.ProcessedCommentID
	next.ProcessedCommentID = run.ProcessedCommentID
	updated, err := s.applyPacketChangeTransition(ctx, runStore, registration, issue, run, next)
	if err != nil {
		return CommandResult{}, err
	}
	updated, err = s.resumeAfterPacketChange(ctx, registration, runStore, updated)
	if err != nil {
		return CommandResult{}, fmt.Errorf("resume implementation after refresh: %w", err)
	}
	// Claim the comment watermark only after successful resumption to keep
	// packet-change resumption retryable when startAgentWithStore fails.
	updated, err = s.persistPacketChangeWatermark(ctx, runStore, updated, processedCommentID)
	if err != nil {
		return CommandResult{}, fmt.Errorf("persist refresh command watermark: %w", err)
	}
	return CommandResult{Outcome: CommandAccepted, Command: parsed, Run: updated}, nil
}

// resumeAfterPacketChange launches the implementation role when the store has
// the visible-invocation seam; reduced command-test stores still receive the
// durable packet/state projection and can resume through StartAgent later.
func (s *Service) resumeAfterPacketChange(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run) (store.Run, error) {
	if _, ok := runStore.(InvocationStore); !ok {
		return run, nil
	}
	request := normalizeAgentRequest(agentRequestForRun(run))
	if err := validateAgentRequest(request); err != nil {
		return run, err
	}
	if err := report.ValidatePermittedPaths(request.PermittedPaths); err != nil {
		return run, err
	}
	if _, err := s.startAgentWithStore(ctx, registration, runStore, &run, request); err != nil {
		return run, err
	}
	if current, err := runStore.CurrentRun(ctx); err == nil && current != nil {
		return *current, nil
	}
	return run, nil
}

// packetResumeStageForPacket preserves test ownership when a configured test
// stage receives a new specification packet, including refreshes initiated
// after implementation has already started.
func packetResumeStageForPacket(run store.Run, packet SpecificationPacket) store.Stage {
	if !testStageConfigured(packet.RepositoryConfig) {
		return store.StageTest
	}
	if testStageShouldRun(packet) && (run.Stage == store.StageTest || !run.TestStageSkipped) {
		return store.StageTest
	}
	return store.StageImplementation
}

// resetTestProjectionForPacketChange removes downstream test evidence that was
// produced for an older specification packet before a new role starts.
func resetTestProjectionForPacketChange(run *store.Run, packet SpecificationPacket) {
	run.TestHandoff = nil
	run.ImplementationHandoff = nil
	run.SpecificationReview = nil
	run.ProtectedTestPaths = nil
	run.TestCheckpointSHA = ""
	run.TestExemption = nil
	run.TestStageSkipped = false
	if !testStageConfigured(packet.RepositoryConfig) {
		run.Stage = store.StageTest
		run.Status = store.StatusWaitingForHuman
		run.LifecycleReason = "frozen repository packet lacks the mandatory test-stage role policy"
		return
	}
	if exemption, ok := testStageHumanExemption(packet); ok {
		run.TestStageSkipped = true
		run.Stage = store.StageImplementation
		run.TestExemption = exemption
		run.LifecycleReason = "test stage skipped by human exemption"
		return
	}
	run.LifecycleReason = "specification packet changed; test stage restarted"
}

// agentRequestForRun selects the role that owns the current packet boundary.
func agentRequestForRun(run store.Run) AgentRequest {
	if run.Stage == store.StageTest {
		return AgentRequest{RunID: run.ID, Role: "test", Stage: store.StageTest}
	}
	if run.Stage == store.StageDraftPR {
		return AgentRequest{RunID: run.ID, Role: "spec_review", Stage: store.StageReview}
	}
	return AgentRequest{RunID: run.ID, Role: "implementation", Stage: store.StageImplementation}
}

// stopRunWorkerIfActive stops only a currently active invocation so an answer
// or refresh does not create a spurious worker-stop side effect after a pause.
func (s *Service) stopRunWorkerIfActive(ctx context.Context, runStore RunStore, run store.Run) error {
	activeStore, ok := runStore.(ActiveInvocationStore)
	if !ok {
		return nil
	}
	active, err := activeStore.ActiveInvocation(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("look up active invocation before packet refresh: %w", err)
	}
	if active == nil {
		return nil
	}
	return s.stopRunWorker(ctx, run.ID)
}

// applyPacketChangeTransition atomically applies the state transition and
// invalidates prior run results, ensuring packet persistence and result
// invalidation commit or roll back together. The command watermark (ProcessedCommentID)
// is intentionally not set here; it is deferred until after successful resumption
// to keep packet-change resumption retryable when startAgentWithStore fails.
func (s *Service) applyPacketChangeTransition(ctx context.Context, runStore RunStore, registration config.RepositoryRegistration, issue github.Issue, previous, next store.Run) (store.Run, error) {
	repository := commandRepository(registration)
	next.UpdatedAt = s.deps.Now().UTC()
	if next.Revision <= previous.Revision {
		next.Revision = previous.Revision + 1
	}
	if _, journaled := runStore.(PendingEffectStore); journaled {
		return s.applyStateTransition(ctx, runStore, stateTransition{
			Repository:        repository,
			Issue:             issue,
			Previous:          previous,
			Next:              next,
			InvalidateResults: true,
		})
	}

	// Apply GitHub effects first (labels and status comment update)
	oldLabels := append([]string(nil), issue.Labels...)
	newLabels := replaceFactoryState(oldLabels, factoryLabelForStatus(next.Status))
	if err := s.deps.GitHub.ReplaceIssueLabels(ctx, repository, next.IssueNumber, newLabels); err != nil {
		return next, fmt.Errorf("set issue #%d state: %w", next.IssueNumber, err)
	}
	if err := s.deps.GitHub.EditIssueComment(ctx, repository, next.StatusCommentID, statusCommentBody(next)); err != nil {
		_ = s.deps.GitHub.ReplaceIssueLabels(ctx, repository, next.IssueNumber, oldLabels)
		return next, fmt.Errorf("edit status comment: %w", err)
	}

	// Atomically persist state and invalidate results, or use fallback
	if atomicStore, ok := runStore.(atomicPacketTransitionStore); ok {
		if err := atomicStore.SaveRunAndInvalidateResults(ctx, previous.Revision, next); err != nil {
			return next, fmt.Errorf("persist packet transition and invalidate results: %w", err)
		}
	} else {
		// Fallback for stores that don't support atomic operation
		if err := saveCommandRun(ctx, runStore, previous.Revision, next); err != nil {
			return next, fmt.Errorf("persist packet transition: %w", err)
		}
		if err := invalidateRunResults(ctx, runStore, next.ID); err != nil {
			return next, err
		}
	}

	if recorder, ok := runStore.(evaluationRecorder); ok {
		if err := recordEvaluationTransition(ctx, recorder, previous, next, next.UpdatedAt); err != nil {
			return next, fmt.Errorf("record evaluation state transition: %w", err)
		}
	}

	return next, nil
}

// persistPacketChangeWatermark claims the command watermark after successful
// packet change resumption, ensuring retryability when startAgentWithStore fails.
func (s *Service) persistPacketChangeWatermark(ctx context.Context, runStore RunStore, run store.Run, commentID string) (store.Run, error) {
	previousRevision := run.Revision
	run.ProcessedCommentID = commentID
	run.ProcessedCommentRevision = run.Revision + 1
	run.Revision = run.ProcessedCommentRevision
	run.UpdatedAt = s.deps.Now().UTC()
	if err := saveCommandRun(ctx, runStore, previousRevision, run); err != nil {
		return run, fmt.Errorf("persist command watermark after resumption: %w", err)
	}
	return run, nil
}

// invalidateRunResults makes the packet revision boundary visible to the
// operational store before a new invocation is launched.
func invalidateRunResults(ctx context.Context, runStore RunStore, runID string) error {
	invalidator, ok := runStore.(runResultInvalidator)
	if !ok {
		return errors.New("operational store does not support specification result invalidation")
	}
	if err := invalidator.InvalidateRunResults(ctx, runID); err != nil {
		return fmt.Errorf("invalidate superseded specification results: %w", err)
	}
	return nil
}

// removePendingQuestion returns all unresolved questions except the answered
// identifier, preserving the status-comment order.
func removePendingQuestion(questions []store.PendingQuestion, questionID string) []store.PendingQuestion {
	remaining := make([]store.PendingQuestion, 0, len(questions))
	for _, question := range questions {
		if question.ID != questionID {
			remaining = append(remaining, question)
		}
	}
	return remaining
}

// handleCancelCommand applies an authorized, idempotent cancellation request
// while retaining every run artifact needed for later inspection or retry.
func (s *Service) handleCancelCommand(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, comment github.Comment, parsed commandlanguage.Request) (CommandResult, error) {
	if run.Status == store.StatusCancelled {
		next := commandProjection(run, comment, parsed, string(parsed.Kind), "command accepted; run was already cancelled")
		updated, err := s.persistCommandProjectionWithRun(ctx, registration, runStore, next, run)
		return CommandResult{Outcome: CommandAccepted, Command: parsed, Run: updated}, err
	}
	if store.IsTerminalStatus(run.Status) {
		rejection := &PolicyRejection{Code: PolicyRejectionCancelState, Problem: fmt.Sprintf("run status %q cannot be cancelled", run.Status)}
		return s.persistCommandRejection(ctx, registration, runStore, run, comment, parsed, rejection)
	}
	repository := commandRepository(registration)
	issue, err := s.deps.GitHub.Issue(ctx, repository, run.IssueNumber)
	if err != nil {
		return CommandResult{}, fmt.Errorf("read issue for cancellation: %w", err)
	}
	next := commandProjection(run, comment, parsed, string(parsed.Kind), "command accepted; run cancelled")
	next.Status = store.StatusCancelled
	next.LifecycleReason = "authorized cancel command"
	next.MergeCommitSHA = ""
	next.LifecycleNotificationSent = false
	next.UpdatedAt = s.deps.Now().UTC()
	updated, err := s.transitionTerminal(ctx, registration, runStore, run, next, issue)
	return CommandResult{Outcome: CommandAccepted, Command: parsed, Run: updated}, err
}

// PollCommands lists comments for the run's issue and draft pull request and
// sends each structured comment through the same single-comment handler.
func (s *Service) PollCommands(ctx context.Context, request CommandPollRequest) ([]CommandResult, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	registration, runStore, run, err := s.openCommandRunStore(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = runStore.Close() }()
	if run == nil {
		return nil, &PolicyRejection{Code: PolicyRejectionNoRun, Problem: "no claimed run is available for command polling"}
	}
	if request.RunID != "" && request.RunID != run.ID {
		return nil, &PolicyRejection{Code: PolicyRejectionWrongTarget, Problem: fmt.Sprintf("run %q is not the selected run", request.RunID)}
	}
	lifecycle, lifecycleErr := s.observeLifecycle(ctx, registration, runStore, run)
	if lifecycleErr != nil {
		return nil, lifecycleErr
	}
	if lifecycle.Outcome != LifecycleUnchanged {
		return nil, nil
	}
	published, publicationErr := s.ensureClarificationPublication(ctx, registration, runStore, *run)
	if publicationErr != nil {
		return nil, publicationErr
	}
	*run = published
	if s.deps.Comments == nil {
		return nil, errors.New("GitHub comment reader is required for command polling")
	}
	targets := []int{run.IssueNumber}
	if run.PullRequestNumber > 0 && run.PullRequestNumber != run.IssueNumber {
		targets = append(targets, run.PullRequestNumber)
	}
	comments := make([]polledComment, 0)
	repository := commandRepository(registration)
	for _, target := range targets {
		listed, listErr := s.deps.Comments.IssueComments(ctx, repository, target)
		if listErr != nil {
			return nil, listErr
		}
		for _, comment := range listed {
			comments = append(comments, polledComment{target: target, comment: comment})
		}
	}
	sort.SliceStable(comments, func(left, right int) bool {
		return compareCommentIDs(comments[left].comment.ID, comments[right].comment.ID) < 0
	})
	results := make([]CommandResult, 0)
	currentRun := *run
	for _, polled := range comments {
		parsed, parseErr := commandlanguage.Parse(polled.comment.Body)
		if !parsed.Recognized {
			continue
		}
		result, handleErr := s.handleRecognizedCommand(ctx, registration, runStore, &currentRun, CommandRequest{RunID: currentRun.ID, IssueNumber: polled.target, Comment: polled.comment}, parsed, parseErr)
		if result.Run.ID != "" {
			currentRun = result.Run
		}
		if handleErr != nil {
			var rejection *PolicyRejection
			if !errors.As(handleErr, &rejection) {
				return results, handleErr
			}
			if parseErr != nil && result.Rejection == nil {
				result.Rejection = rejection
			}
		}
		results = append(results, result)
	}
	return results, nil
}

// polledComment retains the issue or pull-request number alongside a comment
// because GitHub's comment object does not repeat its parent target.
type polledComment struct {
	target  int
	comment github.Comment
}

// commandRepository maps one registered GitHub identity for all command
// operations, keeping polling and command effects on the same repository.
func commandRepository(registration config.RepositoryRegistration) github.Repository {
	return github.Repository{Owner: registration.GitHub.Owner, Name: registration.GitHub.Repository}
}

// commandRevisionStore is the optional atomic persistence seam implemented by
// the SQLite store. Lightweight test stores may fall back to SaveRun.
type commandRevisionStore interface {
	SaveRunIfRevision(context.Context, int64, store.Run) error
}

// handleRetryCommand reopens a failed run at its existing stage and applies
// the normal label/comment transition without creating a new status comment.
func (s *Service) handleRetryCommand(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, comment github.Comment, parsed commandlanguage.Request) (CommandResult, error) {
	if run.Status != store.StatusFailed && run.Status != store.StatusCancelled {
		rejection := &PolicyRejection{Code: PolicyRejectionRetryState, Problem: fmt.Sprintf("retry is only allowed for a failed or cancelled run, not status %q", run.Status)}
		return s.persistCommandRejection(ctx, registration, runStore, run, comment, parsed, rejection)
	}
	if run.StatusCommentID == "" {
		updated, err := s.recoverCommandStatusComment(ctx, registration, run)
		if err != nil {
			return CommandResult{}, err
		}
		run = updated
	}
	repository := commandRepository(registration)
	issue, err := s.deps.GitHub.Issue(ctx, repository, run.IssueNumber)
	if err != nil {
		return CommandResult{}, err
	}
	if run.Status == store.StatusCancelled {
		open, openErr := s.retryTargetIsOpen(ctx, registration, run, issue)
		if openErr != nil {
			return CommandResult{}, openErr
		}
		if !open {
			rejection := &PolicyRejection{Code: PolicyRejectionRetryState, Problem: "a cancelled run can be retried only after its issue or pull request is reopened"}
			return s.persistCommandRejection(ctx, registration, runStore, run, comment, parsed, rejection)
		}
	}
	next := commandProjection(run, comment, parsed, string(parsed.Kind), "command accepted; retrying run")
	next.Status = store.StatusActive
	next.MergeCommitSHA = ""
	next.LifecycleReason = ""
	next.LifecycleNotificationSent = false
	updated, err := s.applyStateTransition(ctx, runStore, stateTransition{
		Repository:           repository,
		Issue:                issue,
		Previous:             run,
		Next:                 next,
		PersistBeforeEffects: true,
	})
	return CommandResult{Outcome: CommandAccepted, Command: parsed, Run: updated}, err
}

// handleHarnessConfiguration validates an authorized harness override against
// the frozen repository configuration before recording it for a later invocation.
func (s *Service) handleHarnessConfiguration(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, comment github.Comment, parsed commandlanguage.Request) (CommandResult, error) {
	if store.IsTerminalStatus(run.Status) {
		rejection := &PolicyRejection{Code: PolicyRejectionHarnessOverride, Problem: fmt.Sprintf("harness configuration is not available after run status %q", run.Status)}
		return s.persistCommandRejection(ctx, registration, runStore, run, comment, parsed, rejection)
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return CommandResult{}, err
	}
	wanted := config.Harness(parsed.Harness)
	if wanted != config.HarnessCodex && wanted != config.HarnessClaude {
		rejection := &PolicyRejection{Code: PolicyRejectionHarnessUnavailable, Problem: fmt.Sprintf("harness %q is not available for the current implementation role", wanted)}
		return s.persistCommandRejection(ctx, registration, runStore, run, comment, parsed, rejection)
	}
	defaultHarness := packet.RepositoryConfig.RoleHarnessDefaults["implementation"]
	if wanted != defaultHarness && !hasOverride(packet.RepositoryConfig.AllowedOverrides, config.OverrideHarness) {
		rejection := &PolicyRejection{Code: PolicyRejectionHarnessOverride, Problem: fmt.Sprintf("repository configuration does not allow harness override from %q to %q", defaultHarness, wanted)}
		return s.persistCommandRejection(ctx, registration, runStore, run, comment, parsed, rejection)
	}
	next := commandProjection(run, comment, parsed, string(parsed.Kind), "command accepted; harness configured")
	next.HarnessOverride = string(wanted)
	updated, err := s.persistCommandProjectionWithRun(ctx, registration, runStore, next, run)
	return CommandResult{Outcome: CommandAccepted, Command: parsed, Run: updated}, err
}

// persistCommandRejection records a typed policy refusal in the existing
// status comment and watermark while leaving workflow state and labels intact.
func (s *Service) persistCommandRejection(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, comment github.Comment, parsed commandlanguage.Request, rejection *PolicyRejection) (CommandResult, error) {
	next := commandProjection(run, comment, parsed, commandResultName(parsed), rejection.Error())
	next.LastCommandOutcome = string(CommandRejected)
	updated, err := s.persistCommandProjectionWithRun(ctx, registration, runStore, next, run)
	result := CommandResult{Outcome: CommandRejected, Command: parsed, Rejection: rejection, Run: updated}
	if err != nil {
		return result, errors.Join(rejection, err)
	}
	return result, rejection
}

// persistCommandProjection stores a successful status/refresh result in the
// single editable comment and returns the updated run.
func (s *Service) persistCommandProjection(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, comment github.Comment, parsed commandlanguage.Request, name, message string) (store.Run, error) {
	next := commandProjection(run, comment, parsed, name, message)
	return s.persistCommandProjectionWithRun(ctx, registration, runStore, next, run)
}

// persistCommandProjectionWithRun durably claims the comment watermark before
// editing the status comment, so a restart cannot execute it twice.
func (s *Service) persistCommandProjectionWithRun(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, next, previous store.Run) (store.Run, error) {
	next.UpdatedAt = s.deps.Now().UTC()
	if next.StatusCommentID == "" {
		recovered, err := s.recoverCommandStatusComment(ctx, registration, next)
		if err != nil {
			return next, err
		}
		next.StatusCommentID = recovered.StatusCommentID
	}
	if _, journaled := runStore.(PendingEffectStore); journaled {
		return s.persistCommandProjectionWithEffect(ctx, runStore, commandRepository(registration), previous, next)
	}
	if err := saveCommandRun(ctx, runStore, previous.Revision, next); err != nil {
		return next, fmt.Errorf("persist command watermark: %w", err)
	}
	repository := commandRepository(registration)
	if err := s.deps.GitHub.EditIssueComment(ctx, repository, next.StatusCommentID, statusCommentBody(next)); err != nil {
		return next, fmt.Errorf("edit status comment for command: %w", err)
	}
	return next, nil
}

// saveCommandRun uses compare-and-set persistence when the store supports it,
// preventing a stale concurrent command from taking the same revision.
func saveCommandRun(ctx context.Context, runStore RunStore, expectedRevision int64, next store.Run) error {
	if revisionStore, ok := runStore.(commandRevisionStore); ok {
		return revisionStore.SaveRunIfRevision(ctx, expectedRevision, next)
	}
	return saveRunWithRetry(ctx, runStore, next)
}

// recoverCommandStatusComment finds and attaches the one editable status
// comment when an earlier process stopped before persisting its identity.
func (s *Service) recoverCommandStatusComment(ctx context.Context, registration config.RepositoryRegistration, run store.Run) (store.Run, error) {
	repository := commandRepository(registration)
	comment, err := s.deps.GitHub.FindStatusComment(ctx, repository, run.IssueNumber, statusCommentMarker(run.ID))
	if err != nil {
		return run, err
	}
	if strings.TrimSpace(comment.ID) == "" {
		return run, errors.New("active run has no recoverable status comment")
	}
	run.StatusCommentID = comment.ID
	return run, nil
}

// commandProjection creates the next revision and advances the immutable
// comment watermark without changing workflow stage or status.
func commandProjection(previous store.Run, comment github.Comment, parsed commandlanguage.Request, name, message string) store.Run {
	next := previous
	next.Revision = previous.Revision + 1
	next.ProcessedCommentID = comment.ID
	next.ProcessedCommentRevision = next.Revision
	next.LastCommandName = name
	next.LastCommandOutcome = string(CommandAccepted)
	next.LastCommandMessage = message
	if parsed.Kind == "" {
		next.LastCommandOutcome = string(CommandRejected)
	}
	next.UpdatedAt = previous.UpdatedAt
	return next
}

// commandResultName supplies a stable name for malformed command feedback.
func commandResultName(parsed commandlanguage.Request) string {
	if parsed.Kind == "" {
		return "malformed"
	}
	return string(parsed.Kind)
}

// openCommandRunStore opens a command-capable store and consumes any pending
// effect before command watermark checks. Legacy stores retain their historical
// command-only behavior because they do not expose a replay journal.
func (s *Service) openCommandRunStore(ctx context.Context) (config.RepositoryRegistration, RunStore, *store.Run, error) {
	registration, runStore, err := s.openRunStore(ctx)
	if err != nil {
		return config.RepositoryRegistration{}, nil, nil, err
	}
	var run *store.Run
	if latestStore, ok := runStore.(LatestRunStore); ok {
		run, err = latestStore.LatestRun(ctx)
	} else {
		run, err = runStore.CurrentRun(ctx)
	}
	if err != nil {
		_ = runStore.Close()
		return config.RepositoryRegistration{}, nil, nil, err
	}
	if _, journaled := runStore.(PendingEffectStore); journaled && run != nil {
		if err := s.ensureProgressionStartup(ctx, registration, runStore, run); err != nil {
			_ = runStore.Close()
			return config.RepositoryRegistration{}, nil, nil, err
		}
		// The process may have created a pending effect after its one-time
		// startup check. Commands must still drain that effect before checking
		// the processed-comment watermark.
		if err := s.drainPendingEffect(ctx, registration, runStore, run); err != nil {
			_ = runStore.Close()
			return config.RepositoryRegistration{}, nil, nil, err
		}
	}
	return registration, runStore, run, nil
}

// authorizedCommentAuthor compares GitHub logins case-insensitively because
// GitHub usernames are case-insensitive identifiers.
func authorizedCommentAuthor(authorized []string, author string) bool {
	for _, candidate := range authorized {
		if strings.EqualFold(strings.TrimSpace(candidate), strings.TrimSpace(author)) && strings.TrimSpace(author) != "" {
			return true
		}
	}
	return false
}

// commentAlreadyProcessed checks the persisted numeric GitHub watermark and
// falls back to identity equality for synthetic or nonnumeric test IDs.
func commentAlreadyProcessed(processed, current string) bool {
	if strings.TrimSpace(processed) == "" || strings.TrimSpace(current) == "" {
		return false
	}
	if processed == current {
		return true
	}
	processedID, processedErr := strconv.ParseUint(processed, 10, 64)
	currentID, currentErr := strconv.ParseUint(current, 10, 64)
	if processedErr == nil && currentErr == nil {
		return currentID <= processedID
	}
	return false
}

// compareCommentIDs sorts numeric GitHub IDs numerically and keeps other IDs
// deterministic without allowing a human-readable message to control flow.
func compareCommentIDs(left, right string) int {
	leftID, leftErr := strconv.ParseUint(left, 10, 64)
	rightID, rightErr := strconv.ParseUint(right, 10, 64)
	if leftErr == nil && rightErr == nil {
		switch {
		case leftID < rightID:
			return -1
		case leftID > rightID:
			return 1
		default:
			return 0
		}
	}
	return strings.Compare(left, right)
}
