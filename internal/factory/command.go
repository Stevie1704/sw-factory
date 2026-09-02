package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	commandlanguage "github.com/Stevie1704/sw-factory/internal/command"
	"github.com/Stevie1704/sw-factory/internal/config"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
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
	// PolicyRejectionRevisionState means a specification amendment is not legal
	// for the current ready pull-request state.
	PolicyRejectionRevisionState PolicyRejectionCode = "revision_state"
	// PolicyRejectionRoleUnavailable means the requested role is not factory-declared.
	PolicyRejectionRoleUnavailable PolicyRejectionCode = "role_unavailable"
	// PolicyRejectionStageUnavailable means the requested stage is not factory-declared.
	PolicyRejectionStageUnavailable PolicyRejectionCode = "stage_unavailable"
	// PolicyRejectionRoleStageMismatch means a role/stage pair is not declared.
	PolicyRejectionRoleStageMismatch PolicyRejectionCode = "role_stage_mismatch"
	// PolicyRejectionRouteInvalid means the frozen issue route marker is
	// unterminated or names a route the factory does not declare.
	PolicyRejectionRouteInvalid PolicyRejectionCode = "route_invalid"
	// PolicyRejectionRouteDuplicate means the frozen issue declares more than
	// one factory route marker.
	PolicyRejectionRouteDuplicate PolicyRejectionCode = "route_duplicate"
	// PolicyRejectionRouteUnavailable means repository policy declares no
	// harness or model for a role the selected route runs.
	PolicyRejectionRouteUnavailable PolicyRejectionCode = "route_unavailable"
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
	if githubIDAlreadyProcessed(run.ProcessedCommentID, request.Comment.ID) {
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
	case commandlanguage.Revision:
		return s.handleRevisionCommand(ctx, registration, runStore, *run, request.Comment, parsed.Command)
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

// atomicAllPacketTransitionStore is the stronger persistence seam used by an
// authorized specification amendment. It invalidates the superseded packet's
// baseline as well as its downstream results in the same compare-and-set.
type atomicAllPacketTransitionStore interface {
	SaveRunAndInvalidateAllResults(context.Context, int64, store.Run) error
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

// handleRevisionCommand applies an authorized specification amendment to a
// ready pull request. It preserves the existing branch, worktree, checkpoint,
// pull-request identity, and invocation artifacts while invalidating every
// result from the superseded packet and restarting at the current checkpoint.
func (s *Service) handleRevisionCommand(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run, comment github.Comment, parsed commandlanguage.Request) (CommandResult, error) {
	readyStatus := run.Status == store.StatusActive || run.Status == store.StatusWaitingForHuman
	if run.Stage != store.StageReady || !readyStatus {
		rejection := &PolicyRejection{Code: PolicyRejectionRevisionState, Problem: fmt.Sprintf("revision is only allowed for a ready run, not stage %q/status %q", run.Stage, run.Status)}
		return s.persistCommandRejection(ctx, registration, runStore, run, comment, parsed, rejection)
	}
	if run.PullRequestNumber <= 0 {
		rejection := &PolicyRejection{Code: PolicyRejectionRevisionState, Problem: "revision requires a tracked pull request"}
		return s.persistCommandRejection(ctx, registration, runStore, run, comment, parsed, rejection)
	}
	if len(run.ActiveInvocationIDs) != 0 {
		rejection := &PolicyRejection{Code: PolicyRejectionRevisionState, Problem: "revision requires a ready run with no active invocation"}
		return s.persistCommandRejection(ctx, registration, runStore, run, comment, parsed, rejection)
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return CommandResult{}, fmt.Errorf("decode specification packet for revision: %w", err)
	}
	repository := commandRepository(registration)
	issue, err := s.deps.GitHub.Issue(ctx, repository, run.IssueNumber)
	if err != nil {
		return CommandResult{}, fmt.Errorf("read issue for revision: %w", err)
	}
	if issue.Number == 0 {
		issue.Number = run.IssueNumber
	}
	if issue.Number != run.IssueNumber || issue.IsPullRequest {
		return CommandResult{}, fmt.Errorf("revision returned an invalid issue identity for #%d", run.IssueNumber)
	}
	if !strings.EqualFold(strings.TrimSpace(issue.State), "open") {
		rejection := &PolicyRejection{Code: PolicyRejectionRevisionState, Problem: fmt.Sprintf("cannot revise a %s issue", defaultString(issue.State, "non-open"))}
		return s.persistCommandRejection(ctx, registration, runStore, run, comment, parsed, rejection)
	}
	client := s.pullRequestClient()
	if client == nil {
		rejection := &PolicyRejection{Code: PolicyRejectionRevisionState, Problem: "revision requires pull-request operations"}
		return s.persistCommandRejection(ctx, registration, runStore, run, comment, parsed, rejection)
	}
	existing, err := client.FindPullRequest(ctx, repository, run.Branch, packet.RepositoryConfig.TargetBranch)
	if err != nil {
		return CommandResult{}, fmt.Errorf("find pull request for revision: %w", err)
	}
	if existing.Number == 0 || existing.Number != run.PullRequestNumber {
		return CommandResult{}, fmt.Errorf("tracked pull request #%d was not found for revision", run.PullRequestNumber)
	}
	if existing.Merged || !strings.EqualFold(strings.TrimSpace(existing.State), "open") {
		rejection := &PolicyRejection{Code: PolicyRejectionRevisionState, Problem: fmt.Sprintf("pull request #%d is not open", existing.Number)}
		return s.persistCommandRejection(ctx, registration, runStore, run, comment, parsed, rejection)
	}
	worktreeManager := s.worktreeManager()
	if worktreeManager == nil {
		return CommandResult{}, errors.New("revision requires a GitWorkspace for exact repository guidance capture")
	}
	packet.RepositoryGuidance, packet.RepositoryGuidancePaths, err = captureRepositoryGuidance(ctx, worktreeManager, gitadapter.Workspace{
		BaseSHA:  run.CheckpointSHA,
		Worktree: run.Worktree,
	})
	if err != nil {
		return CommandResult{}, fmt.Errorf("capture repository guidance for revision: %w", err)
	}
	if _, err := s.setPullRequestDraft(ctx, repository, existing, true); err != nil {
		return CommandResult{}, err
	}

	packet.Version++
	packet.Issue = cloneIssue(issue)
	encodedPacket, err := json.Marshal(packet)
	if err != nil {
		return CommandResult{}, fmt.Errorf("encode specification amendment: %w", err)
	}
	next := commandProjection(run, comment, parsed, string(parsed.Kind), "command accepted; specification amendment created")
	next.Status = store.StatusActive
	next.SpecificationPacket = string(encodedPacket)
	// The current ready checkpoint is the clean pre-edit boundary for the new
	// amendment. Baseline gates are rerun there before a new role starts.
	next.BaseCheckpointSHA = run.CheckpointSHA
	next.Stage = store.StageClaim
	resetRevisionProjection(&next, packet)
	next.PendingQuestions = nil
	next.ClarificationCommentID = ""
	next.ClarificationNotificationSent = false
	next.ReadyNotificationSent = false
	next.LifecycleNotificationSent = false
	next.LifecycleReason = fmt.Sprintf("authorized specification amendment v%d; implementation restarted at checkpoint", packet.Version)
	next.UpdatedAt = s.deps.Now().UTC()
	// The command watermark commits with the amendment and complete result
	// invalidation. If rebaseline or role launch fails afterward, normal
	// progression resumes the durable claim-stage restart without replaying the
	// external command.
	if err := s.stopRunWorkerIfActive(ctx, runStore, run); err != nil {
		return CommandResult{}, err
	}
	updated, err := s.applyPacketChangeTransitionWithInvalidation(ctx, runStore, registration, issue, run, next, true)
	if err != nil {
		return CommandResult{}, err
	}
	updated, err = s.resumeAfterRevision(ctx, registration, runStore, updated)
	if err != nil {
		return CommandResult{Outcome: CommandAccepted, Command: parsed, Run: updated}, fmt.Errorf("resume amended workflow after revision: %w", err)
	}
	return CommandResult{Outcome: CommandAccepted, Command: parsed, Run: updated}, nil
}

// resetRevisionProjection removes every review, repair, and test-stage
// projection tied to the superseded packet. The caller establishes the new
// amendment baseline at the existing clean checkpoint before resumption.
func resetRevisionProjection(run *store.Run, packet SpecificationPacket) {
	if run == nil {
		return
	}
	run.TestHandoff = nil
	run.TestInvocationID = ""
	run.TestRevisionAttempts = 0
	run.TestRevisionBudget = testRevisionBudgetForPacket(packet)
	run.TestRevisionHistory = nil
	run.TestObjection = nil
	run.TestRevisionBaseChangedPaths = nil
	run.ReviewRepairAttempts = 0
	run.ReviewRepairBudget = reviewRepairBudgetForPacket(packet)
	run.ReviewRepairPendingAttempt = 0
	run.ReviewRepairHistory = nil
	run.ReviewRepairPacket = nil
	run.CheckRepairAttempts = 0
	run.CheckRepairBudget = packet.RepositoryConfig.RetryLimits.CheckRepair
	run.CheckRepairPendingAttempt = 0
	run.RoleHandoff = nil
	run.SpecificationReview = nil
	run.StandardsReview = nil
	run.TestExemption = nil
	run.ProtectedTestPaths = nil
	run.TestCheckpointSHA = ""
	run.TestStageSkipped = false
	clearActiveInvocations(run)
}

// resumeAfterRevision re-establishes the new amendment baseline at the current
// clean checkpoint, then launches the role selected by the frozen route and
// test policy. Implementation resumes its native session when possible.
func (s *Service) resumeAfterRevision(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run) (store.Run, error) {
	if _, ok := runStore.(InvocationStore); !ok {
		return run, nil
	}
	if err := s.ensureInvocationAttached(ctx, runStore, run); err != nil {
		return run, err
	}
	packet, err := decodeSpecificationPacket(run.SpecificationPacket)
	if err != nil {
		return run, fmt.Errorf("decode specification packet for revision baseline: %w", err)
	}
	baseline, err := s.runBaselineProjection(ctx, registration, runStore, run, packet)
	if err != nil {
		return baseline.Run, fmt.Errorf("re-establish baseline after revision: %w", err)
	}
	updated := baseline.Run
	definition, ok := workflow.DefaultRegistry().RoleForRunStage(updated.Stage)
	if !ok {
		return updated, fmt.Errorf("no role is declared for revision stage %q", updated.Stage)
	}
	request := AgentRequest{RunID: updated.ID, Role: definition.Name, Stage: definition.Stage}
	if definition.Name == workflow.RoleImplementation {
		request.resumeImplementation = true
	}
	if _, err := s.startAgentWithStore(ctx, registration, runStore, &updated, request); err != nil {
		if cleanupErr := s.supersedeFailedRevisionInvocation(ctx, runStore, updated.ID, definition.Name); cleanupErr != nil {
			return updated, errors.Join(err, cleanupErr)
		}
		return updated, err
	}
	if current, err := runStore.CurrentRun(ctx); err == nil && current != nil {
		return *current, nil
	}
	return updated, nil
}

// supersedeFailedRevisionInvocation makes a failed post-amendment launch
// retryable by normal progression. A pending effect is left untouched for
// startup reconciliation; otherwise the failed invocation is superseded so a
// fresh role launch cannot be mistaken for a duplicate.
func (s *Service) supersedeFailedRevisionInvocation(ctx context.Context, runStore RunStore, runID, role string) error {
	if journal, ok := runStore.(PendingEffectStore); ok {
		pending, err := journal.PendingEffect(ctx, runID)
		if err != nil {
			return fmt.Errorf("inspect pending revision launch: %w", err)
		}
		if pending != nil {
			return nil
		}
	}
	if invalidator, ok := runStore.(runResultInvalidator); ok {
		return invalidator.InvalidateRunResults(ctx, runID)
	}
	roleStore, ok := runStore.(LatestInvocationByRoleStore)
	if !ok {
		return nil
	}
	invocationStore, ok := runStore.(InvocationStore)
	if !ok {
		return nil
	}
	invocation, err := roleStore.LatestInvocationByRole(ctx, runID, role)
	if err != nil {
		return fmt.Errorf("look up failed revision invocation: %w", err)
	}
	if invocation == nil || invocation.Status == store.InvocationStatusSuperseded {
		return nil
	}
	invocation.Status = store.InvocationStatusSuperseded
	invocation.UpdatedAt = s.deps.Now().UTC()
	return invocationStore.SaveInvocation(ctx, *invocation)
}

// resumeAfterPacketChange launches the implementation role when the store has
// the visible-invocation seam; reduced command-test stores still receive the
// durable packet/state projection and can resume through StartAgent later.
func (s *Service) resumeAfterPacketChange(ctx context.Context, registration config.RepositoryRegistration, runStore RunStore, run store.Run) (store.Run, error) {
	if _, ok := runStore.(InvocationStore); !ok {
		return run, nil
	}
	// The coordinator's progression pass owns the baseline, so a run that has
	// not passed it yet must not launch a role here.
	if awaitingBaseline(run) {
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

// awaitingBaseline reports whether a run has not yet passed its frozen
// baseline suite. Only the claim stage runs that suite, so a packet change
// must leave such a run there: a post-baseline stage it jumped to could never
// satisfy the baseline gate and would wedge the run permanently.
func awaitingBaseline(run store.Run) bool {
	return run.Stage == store.StageClaim
}

// packetResumeStageForPacket preserves test ownership when a configured test
// stage receives a new specification packet, including refreshes initiated
// after implementation has already started.
func packetResumeStageForPacket(run store.Run, packet SpecificationPacket) store.Stage {
	if awaitingBaseline(run) {
		return store.StageClaim
	}
	if entry := postBaselineStage(packet); entry != store.StageTest {
		return entry
	}
	if !testRolePolicySatisfied(packet) {
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
	run.TestInvocationID = ""
	run.TestRevisionAttempts = 0
	run.TestRevisionBudget = testRevisionBudgetForPacket(packet)
	run.TestRevisionHistory = nil
	run.ReviewRepairAttempts = 0
	run.ReviewRepairBudget = reviewRepairBudgetForPacket(packet)
	run.ReviewRepairPendingAttempt = 0
	run.ReviewRepairHistory = nil
	run.ReviewRepairPacket = nil
	run.TestObjection = nil
	run.TestRevisionBaseChangedPaths = nil
	run.RoleHandoff = nil
	run.SpecificationReview = nil
	run.StandardsReview = nil
	run.ProtectedTestPaths = nil
	run.TestCheckpointSHA = ""
	run.TestExemption = nil
	run.TestStageSkipped = false
	// The healthy baseline result selects the entry stage, not the packet
	// change itself. A packet the baseline could never leave still parks
	// visibly, because advanceAfterBaseline rejects it after every pass.
	if awaitingBaseline(*run) {
		if !testRolePolicySatisfied(packet) {
			run.Status = store.StatusWaitingForHuman
			run.LifecycleReason = "frozen repository packet lacks the mandatory test-stage role policy"
			return
		}
		run.Status = store.StatusActive
		run.LifecycleReason = "specification packet changed; baseline restarted"
		return
	}
	if entry := postBaselineStage(packet); entry != store.StageTest {
		run.Stage = entry
		run.Status = store.StatusActive
		run.LifecycleReason = "specification packet changed; " + postBaselineReason(entry)
		return
	}
	if !testRolePolicySatisfied(packet) {
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
	if definition, exists := workflow.DefaultRegistry().RoleForRunStage(run.Stage); exists {
		return AgentRequest{RunID: run.ID, Role: definition.Name, Stage: definition.Stage}
	}
	return AgentRequest{RunID: run.ID}
}

// stopRunWorkerIfActive stops only a currently active invocation so an answer
// or refresh does not create a spurious worker-stop side effect after a pause.
func (s *Service) stopRunWorkerIfActive(ctx context.Context, runStore RunStore, run store.Run) error {
	activeValues, supported, err := activeInvocationsForRun(ctx, runStore, run.ID)
	if err != nil {
		return fmt.Errorf("look up active invocation before packet refresh: %w", err)
	}
	if !supported || len(activeValues) == 0 {
		return nil
	}
	return s.stopActiveRunWorkers(ctx, runStore, run)
}

// applyPacketChangeTransition atomically applies the state transition and
// invalidates prior run results, ensuring packet persistence and result
// invalidation commit or roll back together. The command watermark (ProcessedCommentID)
// is intentionally not set here; it is deferred until after successful resumption
// to keep packet-change resumption retryable when startAgentWithStore fails.
func (s *Service) applyPacketChangeTransition(ctx context.Context, runStore RunStore, registration config.RepositoryRegistration, issue github.Issue, previous, next store.Run) (store.Run, error) {
	return s.applyPacketChangeTransitionWithInvalidation(ctx, runStore, registration, issue, previous, next, false)
}

// applyPacketChangeTransitionWithInvalidation applies a packet transition and
// chooses whether ordinary refresh invalidation or complete amendment
// invalidation is required.
func (s *Service) applyPacketChangeTransitionWithInvalidation(ctx context.Context, runStore RunStore, registration config.RepositoryRegistration, issue github.Issue, previous, next store.Run, invalidateAll bool) (store.Run, error) {
	repository := commandRepository(registration)
	next.UpdatedAt = s.deps.Now().UTC()
	if next.Revision <= previous.Revision {
		next.Revision = previous.Revision + 1
	}
	if _, journaled := runStore.(PendingEffectStore); journaled {
		return s.applyStateTransition(ctx, runStore, stateTransition{
			Repository:           repository,
			Issue:                issue,
			Previous:             previous,
			Next:                 next,
			InvalidateResults:    true,
			InvalidateAllResults: invalidateAll,
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
		var persistErr error
		if invalidateAll {
			if allStore, supported := runStore.(atomicAllPacketTransitionStore); supported {
				persistErr = allStore.SaveRunAndInvalidateAllResults(ctx, previous.Revision, next)
			} else {
				persistErr = atomicStore.SaveRunAndInvalidateResults(ctx, previous.Revision, next)
				if persistErr == nil {
					persistErr = invalidateAllRunResults(ctx, runStore, next.ID)
				}
			}
		} else {
			persistErr = atomicStore.SaveRunAndInvalidateResults(ctx, previous.Revision, next)
		}
		if persistErr != nil {
			return next, fmt.Errorf("persist packet transition and invalidate results: %w", persistErr)
		}
	} else {
		// Fallback for stores that don't support atomic operation
		if err := saveCommandRun(ctx, runStore, previous.Revision, next); err != nil {
			return next, fmt.Errorf("persist packet transition: %w", err)
		}
		var invalidationErr error
		if invalidateAll {
			invalidationErr = invalidateAllRunResults(ctx, runStore, next.ID)
		} else {
			invalidationErr = invalidateRunResults(ctx, runStore, next.ID)
		}
		if invalidationErr != nil {
			return next, invalidationErr
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

// invalidateAllRunResults removes every gate result for a superseded packet
// version while retaining the content-free evaluation history and invocation
// artifacts needed for restart diagnosis.
func invalidateAllRunResults(ctx context.Context, runStore RunStore, runID string) error {
	invalidator, ok := runStore.(interface {
		InvalidateAllRunResults(context.Context, string) error
	})
	if !ok {
		return errors.New("operational store does not support complete specification result invalidation")
	}
	if err := invalidator.InvalidateAllRunResults(ctx, runID); err != nil {
		return fmt.Errorf("invalidate all superseded specification results: %w", err)
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
	if reviewReadinessEligible(*run) {
		updated, readinessErr := s.finalizeReviewReadiness(ctx, registration, runStore, *run)
		*run = updated
		if readinessErr != nil {
			return nil, readinessErr
		}
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
			return nil, &pollingTransportError{err: fmt.Errorf("list comments for #%d: %w", target, listErr)}
		}
		for _, comment := range listed {
			comments = append(comments, polledComment{target: target, comment: comment})
		}
	}
	sort.SliceStable(comments, func(left, right int) bool {
		return compareGitHubIDs(comments[left].comment.ID, comments[right].comment.ID) < 0
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
	var launch *store.Invocation
	launchRetry := false
	if run.Status == store.StatusWaitingForHuman {
		var launchErr error
		launch, launchRetry, launchErr = failedLaunchForRetry(ctx, runStore, run)
		if launchErr != nil {
			return CommandResult{}, launchErr
		}
	}
	if run.Status != store.StatusFailed && run.Status != store.StatusCancelled && !launchRetry {
		rejection := &PolicyRejection{Code: PolicyRejectionRetryState, Problem: fmt.Sprintf("retry is only allowed for a failed or cancelled run, not status %q", run.Status)}
		return s.persistCommandRejection(ctx, registration, runStore, run, comment, parsed, rejection)
	}
	if launchRetry {
		launch.Status = store.InvocationStatusSuperseded
		launch.UpdatedAt = s.deps.Now().UTC()
		invocationStore, ok := runStore.(InvocationStore)
		if !ok {
			return CommandResult{}, errors.New("operational store does not support failed-launch retry")
		}
		if err := invocationStore.SaveInvocation(ctx, *launch); err != nil {
			return CommandResult{}, fmt.Errorf("supersede failed launch before retry: %w", err)
		}
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
	retryMessage := "command accepted; retrying run"
	if launchRetry {
		retryMessage = fmt.Sprintf("command accepted; retrying void %s/%s launch", launch.Role, launch.Stage)
	}
	next := commandProjection(run, comment, parsed, string(parsed.Kind), retryMessage)
	if launchRetry {
		// The active projection is authoritative for the retry precondition; a
		// stale denormalized ID must not follow the fresh invocation.
		next.ActiveInvocationIDs = nil
	}
	next.Status = store.StatusActive
	next.MergeCommitSHA = ""
	next.LifecycleReason = ""
	next.LifecycleNotificationSent = false
	next.TerminalAt = time.Time{}
	updated, err := s.applyStateTransition(ctx, runStore, stateTransition{
		Repository:           repository,
		Issue:                issue,
		Previous:             run,
		Next:                 next,
		PersistBeforeEffects: true,
	})
	return CommandResult{Outcome: CommandAccepted, Command: parsed, Run: updated}, err
}

// failedLaunchForRetry returns the one invocation a bounded waiting-state
// retry may void. A retry is admitted only when the run has no active
// invocation and the latest invocation has neither a native session nor a
// structured report, so accepted work and recoverable sessions remain guarded.
func failedLaunchForRetry(ctx context.Context, runStore RunStore, run store.Run) (*store.Invocation, bool, error) {
	if run.Status != store.StatusWaitingForHuman {
		return nil, false, nil
	}
	active, supported, err := activeInvocationsForRun(ctx, runStore, run.ID)
	if err != nil {
		return nil, false, fmt.Errorf("look up active invocations for failed-launch retry: %w", err)
	}
	if !supported || len(active) != 0 {
		return nil, false, nil
	}
	latestStore, ok := runStore.(LatestInvocationStore)
	if !ok {
		return nil, false, nil
	}
	latest, err := latestStore.LatestInvocation(ctx, run.ID)
	if err != nil {
		return nil, false, fmt.Errorf("look up failed launch for retry: %w", err)
	}
	if latest == nil || !isRetryableFailedLaunch(run, *latest) {
		return nil, false, nil
	}
	if _, ok := runStore.(InvocationStore); !ok {
		return nil, false, nil
	}
	return latest, true, nil
}

// isRetryableFailedLaunch applies the void rule at the command seam. The
// invocation must be a failed launch for the run's current role boundary; a
// report or native session is evidence that the history must remain protected.
func isRetryableFailedLaunch(run store.Run, invocation store.Invocation) bool {
	if invocation.RunID != run.ID || !failedLaunchHasNoEvidence(invocation) {
		return false
	}
	if invocation.Status != store.InvocationStatusCannotProceed && invocation.Status != store.InvocationStatusSuperseded {
		return false
	}
	if invocation.Status == store.InvocationStatusSuperseded && !supersededFailedLaunchReason(run.LifecycleReason) {
		return false
	}
	definition, declared := workflow.DefaultRegistry().Role(invocation.Role)
	if !declared || definition.Stage != invocation.Stage {
		return false
	}
	if run.Stage == store.StageDraftPR {
		return invocation.Stage == store.StageReview && definition.Kind == workflow.RoleKindReview
	}
	return invocation.Stage == definition.Stage && workflow.DefaultRegistry().CanStartFrom(invocation.Role, run.Stage)
}

// supersededFailedLaunchReason identifies a coordinator-voided launch in the
// lifecycle projection without adding another persisted run status.
func supersededFailedLaunchReason(reason string) bool {
	return strings.Contains(reason, failedLaunchVoidMarker)
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
	defaultHarness := packet.RepositoryConfig.RoleHarnessDefaults[workflow.RoleImplementation]
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

// githubIDAlreadyProcessed checks a persisted numeric GitHub watermark, such
// as a processed comment or an applied review, and falls back to identity
// equality for synthetic or nonnumeric test IDs.
func githubIDAlreadyProcessed(processed, current string) bool {
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

// compareGitHubIDs orders numeric GitHub identities numerically and keeps
// other identities deterministic without allowing a human-readable message to
// control flow.
func compareGitHubIDs(left, right string) int {
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
