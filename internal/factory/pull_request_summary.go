package factory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/prompt"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

const (
	// pullRequestSummaryStart marks the PR-writer section of the PR body. The
	// coordinator replaces only the text between this marker pair.
	pullRequestSummaryStart = "<!-- factory-pr-writer:start -->"
	// pullRequestSummaryEnd marks the end of the PR-writer section.
	pullRequestSummaryEnd = "<!-- factory-pr-writer:end -->"
	// maxPullRequestSummaryReasonBytes bounds the failure cause carried in the
	// saved summary.
	maxPullRequestSummaryReasonBytes = 240
	// pullRequestSummaryWarning is the content-free reason of the warning
	// event. The saved summary keeps the detailed cause.
	pullRequestSummaryWarning = "pr_writer produced no summary; hand-off continues with the coordinator section only"
)

// pullRequestWriterConfigured reports whether the frozen packet declares a
// harness and model for the optional PR writer.
func pullRequestWriterConfigured(run store.Run) bool {
	return reviewRoleConfigured(run, workflow.RolePullRequestWriter)
}

// pullRequestSummarySettled reports whether a written or failed summary
// exists for the current checkpoint. A settled summary is never generated
// again for the same checkpoint.
func pullRequestSummarySettled(run store.Run) bool {
	return run.PullRequestSummary != nil && run.PullRequestSummary.CheckpointSHA == run.CheckpointSHA
}

// pullRequestSummaryPending reports whether the readiness hand-off must wait
// for the PR writer at the current checkpoint.
func pullRequestSummaryPending(run store.Run) bool {
	return pullRequestWriterConfigured(run) && !pullRequestSummarySettled(run)
}

// pullRequestSummaryFailedAtCheckpoint reports whether the current checkpoint
// has a failed summary.
func pullRequestSummaryFailedAtCheckpoint(run store.Run) bool {
	return pullRequestSummarySettled(run) && run.PullRequestSummary.Status == store.PullRequestSummaryFailed
}

// pullRequestSummaryContextForRun assembles the read-only PR-writer input:
// the exact checkpoint, its gate results, and the findings of every review
// axis at that checkpoint. The diff artifact is added at materialisation.
func pullRequestSummaryContextForRun(ctx context.Context, run store.Run, runStore RunStore) (*prompt.ReviewContext, error) {
	contextValue := &prompt.ReviewContext{CheckpointSHA: run.CheckpointSHA}
	for _, role := range reviewAxisRoles {
		if review := checkpointReviewForRole(run, role); review != nil {
			contextValue.PriorFindings = append(contextValue.PriorFindings, review.Findings...)
		}
	}
	logs, err := checkpointGateLogs(ctx, run, runStore)
	if err != nil {
		return nil, err
	}
	contextValue.RelevantLogs = logs
	return contextValue, nil
}

// pullRequestSummaryFromReport converts one accepted PR-writer report into the
// saved summary. Only a completed report with a usable body becomes a written
// summary; every other result is a failed summary with a bounded reason.
func pullRequestSummaryFromReport(invocation store.Invocation, value report.Report, checkpoint string) store.PullRequestSummary {
	summary := store.PullRequestSummary{CheckpointSHA: checkpoint, InvocationID: invocation.ID, Status: store.PullRequestSummaryFailed}
	if value.Outcome != report.OutcomeCompleted || value.PullRequestSummary == nil {
		summary.Reason = fmt.Sprintf("pr_writer reported %s", value.Outcome)
		return summary
	}
	body := strings.TrimSpace(value.PullRequestSummary.Body)
	if containsPullRequestBodyMarker(body) {
		summary.Reason = "pr_writer body contains a coordinator marker"
		return summary
	}
	summary.Status = store.PullRequestSummaryWritten
	summary.Body = body
	return summary
}

// failedPullRequestSummary records that the PR writer produced no usable body
// for the current checkpoint.
func failedPullRequestSummary(run store.Run, invocationID, reason string) *store.PullRequestSummary {
	return &store.PullRequestSummary{
		CheckpointSHA: run.CheckpointSHA,
		InvocationID:  invocationID,
		Status:        store.PullRequestSummaryFailed,
		Reason:        boundedText(safeStatusCommentValue(reason), maxPullRequestSummaryReasonBytes),
	}
}

// containsPullRequestBodyMarker reports whether text would forge a
// coordinator-owned PR body marker. Such text could move the boundary of
// human-authored or generated sections in a later merge.
func containsPullRequestBodyMarker(text string) bool {
	for _, marker := range []string{pullRequestSummaryStart, pullRequestSummaryEnd, generatedPullRequestStart, generatedPullRequestEnd, generatedReviewStart, generatedReviewEnd} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// renderPullRequestSummarySection wraps a written summary in its markers and
// the advisory note that an agent wrote it.
func renderPullRequestSummarySection(summary store.PullRequestSummary) string {
	note := fmt.Sprintf("> [!NOTE]\n> An agent (`pr_writer`) wrote this summary for checkpoint `%s`. Merge Danger is advice, not a gate.", safeStatusCommentValue(summary.CheckpointSHA))
	return pullRequestSummaryStart + "\n" + note + "\n\n" + summary.Body + "\n" + pullRequestSummaryEnd
}

// mergePullRequestSummarySection replaces only the PR-writer section, or
// inserts it directly above the coordinator section when none exists yet. An
// empty section removes an existing one. Text outside both marker pairs stays
// unchanged.
func mergePullRequestSummarySection(existing, section string) string {
	// Pair the first end marker with the nearest start marker before it, so an
	// orphan marker that a person left behind never widens the replaced span
	// over human text.
	start, end := -1, strings.Index(existing, pullRequestSummaryEnd)
	if end >= 0 {
		start = strings.LastIndex(existing[:end], pullRequestSummaryStart)
	}
	if start >= 0 {
		end += len(pullRequestSummaryEnd)
		if section == "" {
			// Also drop the separator inserted with the section.
			return existing[:start] + strings.TrimPrefix(existing[end:], "\n\n")
		}
		return existing[:start] + section + existing[end:]
	}
	if section == "" {
		return existing
	}
	if generated := strings.Index(existing, generatedPullRequestStart); generated >= 0 {
		return existing[:generated] + section + "\n\n" + existing[generated:]
	}
	if strings.TrimSpace(existing) == "" {
		return section
	}
	return strings.TrimRight(existing, "\n") + "\n\n" + section
}

// startPullRequestWriter launches the PR writer for the current checkpoint.
// A launch failure never blocks the hand-off: it saves a failed summary so
// readiness continues with the coordinator section only.
func (s *Service) startPullRequestWriter(ctx context.Context, runID string) error {
	_, err := s.StartAgent(ctx, AgentRequest{RunID: runID, Role: workflow.RolePullRequestWriter, Stage: workflow.StagePullRequestSummary})
	if err == nil {
		return nil
	}
	return s.abandonPullRequestWriter(ctx, runID, "", "pr_writer launch failed: "+err.Error())
}

// acceptPullRequestWriterReport accepts the PR writer's report. An
// acceptance failure, such as an invalid report, ends the writer and saves a
// failed summary instead of pausing the run.
func (s *Service) acceptPullRequestWriterReport(ctx context.Context, runID, invocationID string) error {
	_, err := s.AcceptAgentReport(ctx, AgentReportRequest{RunID: runID, InvocationID: invocationID})
	if err == nil {
		return nil
	}
	return s.abandonPullRequestWriter(ctx, runID, invocationID, "pr_writer report was not accepted: "+err.Error())
}

// abandonPullRequestWriter ends one PR-writer invocation without a usable
// summary: it stops the invocation's worker, supersedes the invocation,
// releases it from the run, and saves a failed summary for the checkpoint.
// A summary that already exists for the checkpoint is kept.
func (s *Service) abandonPullRequestWriter(ctx context.Context, runID, invocationID, reason string) error {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	registration, runStore, run, err := s.openActiveRunStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = runStore.Close() }()
	if run == nil || run.ID != runID {
		return fmt.Errorf("active run is no longer %s", runID)
	}
	if invocationID != "" {
		if err := s.supersedePullRequestWriter(ctx, runStore, run.ID, invocationID); err != nil {
			return err
		}
	}
	next := *run
	releaseActiveInvocation(&next, invocationID)
	if !pullRequestSummarySettled(next) {
		next.PullRequestSummary = failedPullRequestSummary(next, invocationID, reason)
	}
	next.Revision = run.Revision + 1
	next.UpdatedAt = s.deps.Now().UTC()
	if err := s.persistAgentRunState(ctx, registration, runStore, *run, next); err != nil {
		return fmt.Errorf("persist failed pull-request summary: %w", err)
	}
	return nil
}

// supersedePullRequestWriter stops and supersedes a PR-writer invocation that
// is still active. A terminal invocation is left unchanged.
func (s *Service) supersedePullRequestWriter(ctx context.Context, runStore RunStore, runID, invocationID string) error {
	invocationStore, ok := runStore.(InvocationStore)
	if !ok {
		return errors.New("operational store does not support harness invocations")
	}
	invocation, err := invocationStore.Invocation(ctx, runID, invocationID)
	if err != nil {
		return fmt.Errorf("read pr_writer invocation: %w", err)
	}
	if invocation == nil || invocation.Status != store.InvocationStatusActive {
		return nil
	}
	if invocation.Role != workflow.RolePullRequestWriter {
		return fmt.Errorf("invocation %q is not a pr_writer invocation", invocationID)
	}
	if err := s.lifecycleModule().stopRunWorker(ctx, workerIDForInvocation(*invocation)); err != nil {
		return err
	}
	invocation.Status = store.InvocationStatusSuperseded
	invocation.UpdatedAt = s.deps.Now().UTC()
	if err := invocationStore.SaveInvocation(ctx, *invocation); err != nil {
		return fmt.Errorf("supersede pr_writer invocation: %w", err)
	}
	return nil
}

// activePullRequestWriter returns the live PR-writer invocation, if any.
func activePullRequestWriter(state progressionState) *store.Invocation {
	for _, invocation := range activeInvocationsFor(state) {
		if invocation.Role == workflow.RolePullRequestWriter {
			return invocation
		}
	}
	return nil
}

// pullRequestWriterStep selects the PR-writer transition that must precede the
// readiness hand-off: accept a ready report, end an expired invocation, wait
// for a live one, or start a new one.
func pullRequestWriterStep(state progressionState) (progressionAction, *progressionResult) {
	run := *state.Run
	writer := activePullRequestWriter(state)
	if writer == nil {
		return progressionAction{kind: progressionActionStartPullRequestWriter, name: "start pr_writer", runID: run.ID}, nil
	}
	if state.ReadyInvocationIDs[writer.ID] {
		return progressionAction{kind: progressionActionAcceptPullRequestSummary, name: "accept pr_writer summary", runID: run.ID, invocationID: writer.ID}, nil
	}
	if _, expired := agentInvocationExpired(*writer, state.Now, state.AgentTimeout); expired {
		return progressionAction{kind: progressionActionAbandonPullRequestWriter, name: "end expired pr_writer", runID: run.ID, invocationID: writer.ID}, nil
	}
	return progressionAction{}, &progressionResult{Outcome: progressionInvocationActive, Reason: fmt.Sprintf("invocations %s are executing", writer.ID)}
}

// emitPullRequestSummaryWarning publishes a warning when a progression step
// left the current checkpoint with a failed summary.
func (s *Service) emitPullRequestSummaryWarning(sink EventSink, before, after store.Run) {
	if pullRequestSummaryFailedAtCheckpoint(before) || !pullRequestSummaryFailedAtCheckpoint(after) {
		return
	}
	s.emitCoordinatorEvent(sink, CoordinatorEvent{
		Kind:   EventPullRequestSummaryWarning,
		RunID:  after.ID,
		Reason: pullRequestSummaryWarning,
	})
}
