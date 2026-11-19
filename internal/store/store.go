package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

// CurrentSchemaVersion is the supported operational-store schema version.
const CurrentSchemaVersion = 53

const (
	// MaxReviewFindings bounds the findings one review axis may retain.
	MaxReviewFindings = 64
	// MaxReviewQuestions bounds the clarification questions one axis may retain.
	MaxReviewQuestions = 32
	// MaxReviewEvidence bounds the evidence entries one axis may retain.
	MaxReviewEvidence = 32
	// MaxReviewSummaryBytes bounds one axis summary.
	MaxReviewSummaryBytes = 4000
)

// maxPendingQuestions bounds the flattened operator question surface. Review
// axes retain up to 32 questions independently, so the shared surface allows
// both concurrent axes to remain visible without rejecting the second result.
const maxPendingQuestions = 64

// ErrRevisionConflict reports that another coordinator revision was persisted
// after a command read the run and before it attempted its compare-and-set.
var ErrRevisionConflict = errors.New("run revision conflict")

// ErrRevisionNotAdvanced reports that a compare-and-set update did not move
// the run to a strictly newer revision.
var ErrRevisionNotAdvanced = errors.New("run revision did not advance")

// ErrPendingEffectConflict reports that a run already has a different
// external effect reserved in its durable journal.
var ErrPendingEffectConflict = errors.New("pending effect conflict")

// runTimestampLayout keeps serialized run timestamps fixed-width so SQLite
// text ordering matches chronological ordering for sub-second timestamps.
const runTimestampLayout = "2006-01-02T15:04:05.000000000Z07:00"

// Stage is an open factory-stage identity. The registry owns the supported
// graph; the operational store persists any validated declaration without
// requiring a closed Go enum or a stage-specific schema migration.
type Stage string

const (
	StageClaim          Stage = "claim"
	StagePreflight      Stage = "preflight"
	StageTest           Stage = "test"
	StageImplementation Stage = "implementation"
	// StageArchitecture is the optional document-producing architecture stage.
	StageArchitecture Stage = "architecture"
	StageCheck        Stage = "check"
	StageDraftPR      Stage = "draft_pr"
	StageReview       Stage = "review"
	StageReady        Stage = "ready"
)

type Status string

const (
	StatusActive            Status = "active"
	StatusWaitingForHuman   Status = "waiting_for_human"
	StatusWaitingForHarness Status = "waiting_for_harness"
	StatusFailed            Status = "failed"
	StatusCancelled         Status = "cancelled"
	StatusComplete          Status = "complete"
)

// IsTerminalStatus reports whether a run has no further coordinator
// progression state to reconcile.
func IsTerminalStatus(status Status) bool {
	return status == StatusComplete || status == StatusCancelled || status == StatusFailed
}

// SupervisorHeartbeat is the host-local liveness projection for the polling
// coordinator. Its expiry is deliberately persisted so another process can
// distinguish a live supervisor from a coordinator that stopped renewing its
// heartbeat.
type SupervisorHeartbeat struct {
	// Coordinator identifies the host or coordinator identity that owns the
	// heartbeat.
	Coordinator string
	// PID identifies the process that last renewed the heartbeat.
	PID int
	// StartedAt is when the current supervisor process began renewing.
	StartedAt time.Time
	// RenewedAt is when the heartbeat was last written.
	RenewedAt time.Time
	// ExpiresAt is the first instant at which the heartbeat is no longer live.
	ExpiresAt time.Time
}

// Live reports whether the heartbeat is valid and unexpired at now.
func (heartbeat SupervisorHeartbeat) Live(now time.Time) bool {
	return heartbeat.Coordinator != "" && heartbeat.PID > 0 && !heartbeat.RenewedAt.IsZero() && now.Before(heartbeat.ExpiresAt)
}

// PendingEffectKind identifies the external mutation reserved before a
// coordinator process crosses its restart boundary.
type PendingEffectKind string

const (
	// PendingEffectKindStateTransition covers the paired issue-label and status
	// comment projection for one run revision.
	PendingEffectKindStateTransition PendingEffectKind = "state_transition"
	// PendingEffectKindLabelTransition identifies a standalone issue-label
	// projection when a caller does not use a complete state transition.
	PendingEffectKindLabelTransition PendingEffectKind = "label_transition"
	// PendingEffectKindStatusComment identifies a standalone status-comment
	// projection when a caller does not use a complete state transition.
	PendingEffectKindStatusComment PendingEffectKind = "status_comment"
	// PendingEffectKindCommitStatus identifies one exact-SHA Commit Status.
	PendingEffectKindCommitStatus PendingEffectKind = "commit_status"
	// PendingEffectKindPush identifies one run-branch push.
	PendingEffectKindPush PendingEffectKind = "push"
	// PendingEffectKindPullRequest identifies one draft pull-request mutation.
	PendingEffectKindPullRequest PendingEffectKind = "pull_request"
	// PendingEffectKindCheckpoint identifies one checkpoint commit.
	PendingEffectKindCheckpoint PendingEffectKind = "checkpoint"
	// PendingEffectKindWorkerLaunch identifies one worker creation or reuse.
	PendingEffectKindWorkerLaunch PendingEffectKind = "worker_launch"
	// PendingEffectKindHarnessResume identifies one native-session resume.
	PendingEffectKindHarnessResume PendingEffectKind = "harness_resume"
	// PendingEffectKindResultAcceptance identifies one accepted report
	// projection.
	PendingEffectKindResultAcceptance PendingEffectKind = "result_acceptance"
	// PendingEffectKindClarificationComment identifies one clarification
	// question comment and its persisted comment identity.
	PendingEffectKindClarificationComment PendingEffectKind = "clarification_comment"
	// PendingEffectKindLaunchRollback identifies the worker stop of a failed
	// launch and the terminal invocation status saved only after that stop.
	PendingEffectKindLaunchRollback PendingEffectKind = "launch_rollback"
)

// PendingEffect is the bounded intent record for one external mutation. A run
// may have at most one row, so a restart can inspect and finish the exact
// operation that was in flight without replaying an unrelated effect.
type PendingEffect struct {
	// RunID identifies the run owning the effect.
	RunID string
	// ID is the deterministic idempotency key for the effect.
	ID string
	// Kind identifies the adapter operation represented by Payload.
	Kind PendingEffectKind
	// Payload is bounded JSON containing the complete replay intent.
	Payload string
	// CreatedAt is the first reservation time.
	CreatedAt time.Time
	// UpdatedAt is the latest reservation time.
	UpdatedAt time.Time
}

// InvocationStatus describes the lifecycle of one harness invocation.
type InvocationStatus string

const (
	// InvocationStatusActive means the harness may still produce a report.
	InvocationStatusActive InvocationStatus = "active"
	// InvocationStatusCompleted means the coordinator accepted a completed report.
	InvocationStatusCompleted InvocationStatus = "completed"
	// InvocationStatusWaitingForHuman means the report requested clarification.
	InvocationStatusWaitingForHuman InvocationStatus = "waiting_for_human"
	// InvocationStatusCannotProceed means the report contained blocking evidence.
	InvocationStatusCannotProceed InvocationStatus = "cannot_proceed"
	// InvocationStatusSuperseded means a later specification packet invalidated
	// the invocation, or the coordinator voided a launch with no native session
	// or structured report, before its result could be used for progression.
	InvocationStatusSuperseded InvocationStatus = "superseded"
)

// GatePhase identifies why a deterministic gate result was recorded.
type GatePhase string

const (
	// GatePhaseBaseline identifies the pre-edit health evaluation.
	GatePhaseBaseline GatePhase = "baseline"
	// GatePhaseCheckpoint identifies an implementation checkpoint evaluation.
	GatePhaseCheckpoint GatePhase = "checkpoint"
)

// GateOutcome identifies the coordinator-owned semantic result of one gate.
type GateOutcome string

const (
	// GateOutcomePassed records a successful declared command.
	GateOutcomePassed GateOutcome = "passed"
	// GateOutcomeFailed records a declared command failure or timeout.
	GateOutcomeFailed GateOutcome = "failed"
	// GateOutcomeError records a worker/runtime execution error.
	GateOutcomeError GateOutcome = "error"
	// GateOutcomeSkipped records a dependency that was not evaluated.
	GateOutcomeSkipped GateOutcome = "skipped"
	// GateOutcomeSetupFailed records a gate blocked by setup failure.
	GateOutcomeSetupFailed GateOutcome = "setup_failed"
)

type UnknownSchemaVersionError struct {
	Version   int
	Supported int
}

func (e *UnknownSchemaVersionError) Error() string {
	return fmt.Sprintf("operational store schema version %d is newer than supported version %d", e.Version, e.Supported)
}

type UnversionedDatabaseError struct {
	Path string
	Err  error
}

func (e *UnversionedDatabaseError) Error() string {
	return fmt.Sprintf("operational store %q has no readable schema version: %v", e.Path, e.Err)
}

func (e *UnversionedDatabaseError) Unwrap() error { return e.Err }

// PendingQuestion is one clarification request awaiting an authorized answer.
// It is deliberately small so the operational store retains workflow state
// rather than a transcript.
type PendingQuestion struct {
	// ID is the stable identifier referenced by an answer command.
	ID string
	// Prompt is the bounded human-readable clarification request.
	Prompt string
}

// TestAcceptanceCoverage maps one frozen criterion to test-stage evidence in
// the durable handoff projection.
type TestAcceptanceCoverage struct {
	// Criterion identifies the acceptance criterion covered by a test.
	Criterion string `json:"criterion"`
	// Evidence identifies the observable test evidence for the criterion.
	Evidence string `json:"evidence"`
}

// TestFailureEvidence is the content-limited red-test observation retained in
// the run handoff.
type TestFailureEvidence struct {
	// Kind identifies the observation category, such as exit_code.
	Kind string `json:"kind"`
	// Detail identifies the bounded observable detail.
	Detail string `json:"detail"`
}

// TestObjection is the durable implementation claim that a protected test is
// incorrect. InvocationID is coordinator-owned and records which original
// test session must receive the objection on resume.
type TestObjection struct {
	// Test identifies the disputed test by path, name, or both.
	Test string `json:"test"`
	// Claim states the behavioral assertion the test makes.
	Claim string `json:"claim"`
	// Evidence identifies observable implementation evidence supporting the
	// objection.
	Evidence string `json:"evidence"`
	// InvocationID identifies the original test invocation to resume.
	InvocationID string `json:"invocation_id,omitempty"`
}

// TestRevisionOutcome is the bounded status of one objection cycle.
type TestRevisionOutcome string

const (
	// TestRevisionPending means the test role has not answered the objection.
	TestRevisionPending TestRevisionOutcome = "pending"
	// TestRevisionAccepted means the test role accepted and revised the test.
	TestRevisionAccepted TestRevisionOutcome = "accepted"
	// TestRevisionRejected means the test role rejected the objection.
	TestRevisionRejected TestRevisionOutcome = "rejected"
	// TestRevisionVerificationFailed means the revised test did not reproduce
	// the expected red failure under coordinator verification.
	TestRevisionVerificationFailed TestRevisionOutcome = "verification_failed"
	// TestRevisionEscalated means the bounded revision budget was exhausted.
	TestRevisionEscalated TestRevisionOutcome = "escalated"
)

// TestRevision records one bounded objection cycle and its latest outcome.
type TestRevision struct {
	// Attempt is the one-based cycle number.
	Attempt int `json:"attempt"`
	// Outcome is pending, accepted, rejected, verification_failed, or escalated.
	Outcome TestRevisionOutcome `json:"outcome"`
	// DisputeKey groups revision cycles that address the same test claim while
	// allowing a later, distinct objection to receive its own two-cycle budget.
	DisputeKey string `json:"dispute_key,omitempty"`
}

// TestHandoff is the durable test-stage transfer packet for implementation.
type TestHandoff struct {
	// AcceptanceCoverage maps acceptance criteria to test evidence.
	AcceptanceCoverage []TestAcceptanceCoverage `json:"acceptance_coverage"`
	// ChangedFiles lists test-role files changed by the test invocation.
	ChangedFiles []string `json:"changed_files"`
	// FocusedTestCommand is the command independently rerun for red verification.
	FocusedTestCommand string `json:"focused_test_command"`
	// ExpectedFailureReason is the bounded failure identifier verified in rerun output.
	ExpectedFailureReason string `json:"expected_failure_reason"`
	// ObservedFailureEvidence records bounded evidence supplied by the test role.
	ObservedFailureEvidence []TestFailureEvidence `json:"observed_failure_evidence"`
	// InfrastructureChanges lists changed files that support tests.
	InfrastructureChanges []string `json:"infrastructure_changes,omitempty"`
	// UncoveredCriteria records criteria not covered by this handoff.
	UncoveredCriteria []string `json:"uncovered_criteria,omitempty"`
}

// TestExemption records a human or technical reason the normal test handoff
// was skipped or provisionally accepted.
type TestExemption struct {
	// Kind is human or technical.
	Kind string `json:"kind"`
	// Justification is the bounded operator-visible reason.
	Justification string `json:"justification"`
}

// HandoffAcceptance maps one implementation criterion to observable evidence
// retained for the independent reviewer.
type HandoffAcceptance struct {
	// Criterion identifies the frozen requirement covered by the handoff.
	Criterion string `json:"criterion"`
	// Evidence identifies the command or artifact supporting the criterion.
	Evidence string `json:"evidence"`
}

// RoleHandoff is the bounded structured result transferred from any
// implementation-oriented role to the next supervised stage. Its production
// file field also accepts document artifacts produced by non-code roles.
type RoleHandoff struct {
	// ChangeSummary describes the accepted role change.
	ChangeSummary string `json:"change_summary"`
	// AcceptanceMapping connects frozen criteria to observable evidence.
	AcceptanceMapping []HandoffAcceptance `json:"acceptance_mapping"`
	// ProductionFilesChanged lists the repository paths reported as changed.
	ProductionFilesChanged []string `json:"production_files_changed"`
	// FocusedCommands lists commands the role ran.
	FocusedCommands []string `json:"focused_commands"`
	// KnownLimitations carries bounded visible limitations.
	KnownLimitations []string `json:"known_limitations,omitempty"`
	// TestObjections contains structured disputes about protected test-stage
	// behavior.
	TestObjections []TestObjection `json:"test_objections,omitempty"`
}

// ReviewFinding is the durable finding projection shared by status and PR
// renderers without retaining the reviewer transcript.
type ReviewFinding struct {
	// Location identifies the repository path and optional line or symbol.
	Location string `json:"location"`
	// Claim states the reviewer's observation.
	Claim string `json:"claim"`
	// Evidence identifies observable support for the claim.
	Evidence string `json:"evidence"`
	// Severity is blocker or advisory as proposed by the reviewer.
	Severity string `json:"severity"`
	// Category identifies the bounded policy category.
	Category string `json:"category"`
	// SuggestedResolution makes the finding actionable.
	SuggestedResolution string `json:"suggested_resolution"`
	// SuggestedOwner identifies the suggested repair owner.
	SuggestedOwner string `json:"suggested_owner"`
	// UnitID identifies the exact review unit that owns this finding.
	UnitID string `json:"unit_id,omitempty"`
}

// SourceEvent returns the explicit maintainer event of a human repair packet.
func (p ReviewRepairPacket) SourceEvent() (ReviewRepairSourceEvent, string) {
	return ReviewRepairSourceEvent(strings.TrimSpace(string(p.SourceEventKind))), strings.TrimSpace(p.SourceEventID)
}

// ReviewResult is one review role's latest exact-checkpoint result. A result
// is stale as soon as the run checkpoint changes.
type ReviewResult struct {
	// CheckpointSHA identifies the exact commit reviewed.
	CheckpointSHA string `json:"checkpoint_sha"`
	// Outcome identifies whether the reviewer completed its assignment.
	Outcome string `json:"outcome,omitempty"`
	// Summary is the reviewer's bounded observable reason or completion summary.
	Summary string `json:"summary,omitempty"`
	// Questions retains clarification requests for this review axis.
	Questions []ReviewQuestion `json:"questions,omitempty"`
	// Evidence retains bounded evidence for a review that cannot proceed.
	Evidence []ReviewEvidence `json:"evidence,omitempty"`
	// Findings contains every accepted finding established before the review
	// result was reported, including findings from incomplete outcomes.
	Findings []ReviewFinding `json:"findings"`
}

// ReviewQuestion is one review-axis clarification request retained for operator
// disposition and restart recovery.
type ReviewQuestion struct {
	// ID is the stable identifier for the question.
	ID string `json:"id"`
	// Prompt is the bounded question shown to the operator.
	Prompt string `json:"prompt"`
}

// ReviewEvidence is one bounded observation retained when a review cannot
// proceed.
type ReviewEvidence struct {
	// Kind identifies the evidence category.
	Kind string `json:"kind"`
	// Detail describes the observable evidence.
	Detail string `json:"detail"`
}

// ReviewRepairOutcome is the bounded lifecycle of one review-repair round.
type ReviewRepairOutcome string

const (
	// ReviewRepairPending means a repair round is reserved but not complete.
	ReviewRepairPending ReviewRepairOutcome = "pending"
	// ReviewRepairStarted means implementation repair was launched.
	ReviewRepairStarted ReviewRepairOutcome = "started"
	// ReviewRepairRepeated means a materially identical blocker returned.
	ReviewRepairRepeated ReviewRepairOutcome = "repeated"
	// ReviewRepairExhausted means the configured review-repair budget was used.
	ReviewRepairExhausted ReviewRepairOutcome = "exhausted"
	// ReviewRepairWaitingForHuman means the repair could not safely continue.
	ReviewRepairWaitingForHuman ReviewRepairOutcome = "waiting_for_human"
)

// ReviewRepairFinding associates one blocking finding with its reviewer so a
// single implementation packet preserves the independent review provenance.
type ReviewRepairFinding struct {
	// ReviewerRole identifies the isolated reviewer that reported the finding.
	ReviewerRole string `json:"reviewer_role"`
	// Finding is the validated content-bearing repair instruction.
	Finding ReviewFinding `json:"finding"`
}

// ReviewRepairSource identifies who produced the blocking findings in one
// repair packet. An empty value is the factory reviewers, which keeps packets
// written before human-review repair readable.
type ReviewRepairSource string

const (
	// ReviewRepairSourceFactory means the independent factory reviewers
	// produced the findings and the bounded repair budget applies.
	ReviewRepairSourceFactory ReviewRepairSource = "factory"
	// ReviewRepairSourceHuman means an authorized maintainer requested the
	// changes through a completed GitHub review.
	ReviewRepairSourceHuman ReviewRepairSource = "human"
)

// ReviewRepairSourceEvent identifies the maintainer source that produced one
// human repair packet.
type ReviewRepairSourceEvent string

const (
	// ReviewRepairEventPullRequestReview means a submitted CHANGES_REQUESTED
	// pull-request review.
	ReviewRepairEventPullRequestReview ReviewRepairSourceEvent = "pull_request_review"
	// ReviewRepairEventSupervisionCommand means an authorized supervision
	// comment addressed to the coordinator.
	ReviewRepairEventSupervisionCommand ReviewRepairSourceEvent = "supervision_command"
)

// ReviewRepairPacket is the coordinator-owned combined blocking-review packet
// mounted in the implementation repair invocation.
type ReviewRepairPacket struct {
	// Version identifies this packet shape.
	Version int `json:"version"`
	// Source identifies who requested the repair: factory or human.
	Source ReviewRepairSource `json:"source,omitempty"`
	// SourceEventKind identifies which maintainer source produced a human
	// packet. It is empty for a factory packet.
	SourceEventKind ReviewRepairSourceEvent `json:"source_event_kind,omitempty"`
	// SourceEventID is the identity of that source event, and is what makes
	// a human packet replayable. It is empty for a factory packet.
	SourceEventID string `json:"source_event_id,omitempty"`
	// RunID identifies the owning factory run.
	RunID string `json:"run_id"`
	// CheckpointSHA identifies the exact checkpoint that produced the findings.
	CheckpointSHA string `json:"checkpoint_sha"`
	// Attempt is the one-based repair round number.
	Attempt int `json:"attempt"`
	// Budget is the frozen maximum number of review-repair rounds.
	Budget int `json:"budget"`
	// Findings contains every blocking finding from the completed review round;
	// implementation routes test-owned findings through the objection protocol.
	Findings []ReviewRepairFinding `json:"findings"`
	// ResolvedTestOwnerFindingKeys records opaque identities for test-owned
	// findings that the independent test role accepted and revised. These
	// findings no longer require the implementation to submit the same
	// objection after the revised test checkpoint is created.
	ResolvedTestOwnerFindingKeys []string `json:"resolved_test_owner_finding_keys,omitempty"`
}

// ReviewRepairAttempt records one content-free review-repair lifecycle entry.
type ReviewRepairAttempt struct {
	// Attempt is the one-based round number.
	Attempt int `json:"attempt"`
	// Outcome identifies the bounded lifecycle result.
	Outcome ReviewRepairOutcome `json:"outcome"`
	// BlockerKeys are stable hashes used to recognize repeated blockers.
	BlockerKeys []string `json:"blocker_keys,omitempty"`
}

// SpecificationReview is the specification review's exact-checkpoint result.
type SpecificationReview = ReviewResult

// StandardsReview is the documented-standards review's exact-checkpoint
// result. It reuses the same finding contract while remaining a separate
// durable projection.
type StandardsReview = ReviewResult

// PullRequestSummaryStatus identifies how the PR writer ended for one
// checkpoint.
type PullRequestSummaryStatus string

const (
	// PullRequestSummaryWritten means the PR writer returned a valid body.
	PullRequestSummaryWritten PullRequestSummaryStatus = "written"
	// PullRequestSummaryFailed means the PR writer produced no usable body.
	// The hand-off continues with the coordinator section only.
	PullRequestSummaryFailed PullRequestSummaryStatus = "failed"
)

// PullRequestSummary is the saved PR-writer result for one checkpoint. The
// coordinator saves it before the pull-request update, so a readiness retry
// at the same checkpoint reuses it instead of starting a new invocation.
type PullRequestSummary struct {
	// CheckpointSHA is the checkpoint the summary describes.
	CheckpointSHA string `json:"checkpoint_sha"`
	// InvocationID identifies the PR-writer invocation, when one started.
	InvocationID string `json:"invocation_id,omitempty"`
	// Status records whether a usable body exists.
	Status PullRequestSummaryStatus `json:"status"`
	// Body is the markdown summary. It is empty for a failed summary.
	Body string `json:"body,omitempty"`
	// Reason is a bounded single-line cause of a failed summary.
	Reason string `json:"reason,omitempty"`
}

// ProtectedTestPath records the content identity implementation must preserve.
type ProtectedTestPath struct {
	// Path is the repository-relative protected test path.
	Path string `json:"path"`
	// SHA256 is the content hash recorded at the test checkpoint.
	SHA256 string `json:"sha256"`
}

// TrackerRepository is the stable identity of the Azure Repos repository
// that a run was assigned to by its factory-repo tag, frozen at claim.
type TrackerRepository struct {
	// Organization is the Azure DevOps organization name.
	Organization string `json:"organization"`
	// ProjectID and Project identify the project of the repository.
	ProjectID string `json:"project_id"`
	Project   string `json:"project"`
	// RepositoryID is the repository GUID the assignment tag names, and
	// Repository is its name at claim time.
	RepositoryID string `json:"repository_id"`
	Repository   string `json:"repository"`
}

// validate requires every identity field as a single line.
func (r TrackerRepository) validate() error {
	for _, value := range []string{r.Organization, r.ProjectID, r.Project, r.RepositoryID, r.Repository} {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("run tracker repository must name organization, project, and repository identities as single lines")
		}
	}
	return nil
}

// Run is the persisted operational identity and current state of one run.
type Run struct {
	ID             string
	RepositoryPath string
	IssueNumber    int
	Stage          Stage
	Status         Status
	Branch         string
	Worktree       string
	CheckpointSHA  string
	// BaseCheckpointSHA is the immutable pre-edit checkpoint for the current
	// specification version. An authorized revision promotes its clean current
	// checkpoint to establish a new amendment baseline.
	BaseCheckpointSHA string
	// AcceptedImplementationCheckpointSHA records the implementation checkpoint
	// accepted before a refresh restart. It remains valid while packet changes
	// invalidate the checkpoint's gate-result rows and lifecycle reason.
	AcceptedImplementationCheckpointSHA string
	// TestCheckpointSHA identifies the separate protected test checkpoint.
	TestCheckpointSHA string
	// TestHandoff is the accepted structured test-stage transfer packet.
	TestHandoff *TestHandoff
	// TestInvocationID identifies the latest test invocation whose native
	// session should receive a future implementation objection.
	TestInvocationID string
	// TestRevisionAttempts is the number of objection cycles started.
	TestRevisionAttempts int
	// TestRevisionBudget is the frozen maximum number of objection cycles.
	TestRevisionBudget int
	// TestRevisionHistory records bounded objection outcomes for status and
	// recovery projection.
	TestRevisionHistory []TestRevision
	// TestObjection is the current implementation objection awaiting a test
	// role response. It is cleared after an accepted revision.
	TestObjection *TestObjection
	// TestRevisionBaseChangedPaths records implementation changes already in
	// the worktree when the current objection was submitted, including their
	// content identity so a resumed test role cannot alter them silently.
	TestRevisionBaseChangedPaths []ProtectedTestPath
	// RoleHandoff is the accepted role transfer packet.
	RoleHandoff *RoleHandoff
	// SpecificationReview is the latest exact-checkpoint review result.
	SpecificationReview *SpecificationReview
	// StandardsReview is the latest exact-checkpoint standards result.
	StandardsReview *StandardsReview
	// PullRequestSummary is the latest saved PR-writer result.
	PullRequestSummary *PullRequestSummary
	// ReviewRepairAttempts is the number of review-repair rounds started.
	ReviewRepairAttempts int
	// ReviewRepairBudget is the frozen maximum number of review-repair rounds.
	ReviewRepairBudget int
	// ReviewRepairPendingAttempt is the next reserved round awaiting launch
	// reconciliation.
	ReviewRepairPendingAttempt int
	// ReviewRepairHistory records bounded review-repair outcomes and opaque
	// blocker identities without replacing the finding packet itself.
	ReviewRepairHistory []ReviewRepairAttempt
	// ReviewRepairPacket is the combined blocking-review context for the current
	// implementation repair, when one is active.
	ReviewRepairPacket *ReviewRepairPacket
	// TestExemption records a pre-authorized or technical test-stage exemption.
	TestExemption *TestExemption
	// ProtectedTestPaths records every test-stage path and its checkpoint hash.
	ProtectedTestPaths []ProtectedTestPath
	// TestStageSkipped records an authorized skip before implementation.
	TestStageSkipped bool
	// ActiveInvocationIDs identifies every concurrently executing harness
	// invocation.
	ActiveInvocationIDs []string
	ImageDigest         string
	Coordinator         string
	StatusCommentID     string
	// PullRequestNumber is the persisted idempotency identity of the draft PR.
	PullRequestNumber int
	// PullRequestURL is the operator-facing URL of the draft PR.
	PullRequestURL string
	// MergeCommitSHA is the immutable commit recorded when the tracked PR merges.
	MergeCommitSHA string
	// LifecycleReason explains an automatic or authorized terminal transition.
	LifecycleReason string
	// TerminalAt records when the run first entered its current terminal state.
	// It remains stable while terminal GitHub lifecycle projections are
	// retried, and is cleared when a terminal run is explicitly reopened.
	TerminalAt time.Time
	// Revision is the monotonic coordinator revision used by command replay
	// prevention and persisted supervision updates.
	Revision int64
	// ProcessedCommentID is the processed comment watermark for
	// this run. Claim initializes it to the latest pre-claim issue comment so
	// historical commands cannot execute against the new run.
	// Separate tracker/code-host namespaces use a versioned per-stream payload;
	// combined adapters use raw IDs in their shared comment namespace.
	ProcessedCommentID string
	// ProcessedCommentRevision records the run revision at which the watermark
	// was persisted, making the watermark tuple restart-safe and auditable.
	ProcessedCommentRevision int64
	// ProcessedReviewID is the highest applied GitHub pull-request review
	// watermark for this run. Repeated polling and a coordinator restart must
	// never apply the same completed human review twice.
	ProcessedReviewID string
	// ProcessedReviewRevision records the run revision at which the review
	// watermark was persisted, making the tuple restart-safe and auditable.
	ProcessedReviewRevision int64
	// LastCommandName is the last recognized structured command kind.
	LastCommandName string
	// LastCommandOutcome is accepted, rejected, or replayed for the last command.
	LastCommandOutcome string
	// LastCommandMessage is the operator-facing result detail rendered in the
	// single editable status comment.
	LastCommandMessage string
	// HarnessOverride is the authorized harness choice for a later invocation.
	HarnessOverride string
	// CheckRepairAttempts is the number of deterministic check-repair sessions
	// successfully launched for this run.
	CheckRepairAttempts int
	// CheckRepairBudget is the frozen maximum number of check-repair sessions
	// allowed for this run.
	CheckRepairBudget int
	// CheckRepairPendingAttempt is a durable reservation between worker setup
	// and native resume. It lets reconciliation distinguish an interrupted
	// launch from a successfully consumed attempt.
	CheckRepairPendingAttempt int
	SpecificationPacket       string
	// PendingQuestions contains clarification requests awaiting authorized
	// answers for the current specification packet.
	PendingQuestions []PendingQuestion
	// ClarificationCommentID identifies the question comment already published
	// for the current pending clarification set.
	ClarificationCommentID string
	// TrackerRepository is the repository identity validated at claim for an
	// Azure DevOps extension-route run (ADR 0030). Once set, the store
	// refuses any change, so a tag edit cannot redirect active work. Nil
	// for every other run.
	TrackerRepository *TrackerRepository
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Invocation is the persisted recoverable identity of one harness session.
type Invocation struct {
	// ID is the immutable invocation identifier used by the report protocol.
	ID string
	// RunID binds the invocation to one factory run.
	RunID string
	// Harness identifies the selected role harness.
	Harness string
	// Role identifies the workflow role owning the invocation.
	Role string
	// Stage identifies the workflow stage owning the invocation.
	Stage Stage
	// Model is the validated model policy selection.
	Model string
	// ReasoningEffort is the validated reasoning policy selection.
	ReasoningEffort string
	// CredentialStoreID identifies the factory-managed credential volume used
	// by this invocation, without retaining the host auth path or its contents.
	CredentialStoreID string
	// NativeSessionID is the harness-native continuation identity when known.
	NativeSessionID string
	// InvocationDirectory is the host directory containing the frozen packet.
	InvocationDirectory string
	// ResultDirectory is the host directory containing report.json.
	ResultDirectory string
	// PermittedPaths contains repository-relative prefixes authorized for the
	// completed handoff and is persisted for coordinator restart validation.
	PermittedPaths []string
	// PromptVersion identifies the versioned core role prompt.
	PromptVersion string
	// PromptCraftSourcePath identifies the frozen repository craft source used by
	// this invocation, when the repository selected one.
	PromptCraftSourcePath string
	// PromptCraftSHA256 identifies the exact frozen repository craft bytes used by
	// this invocation, when the repository selected one.
	PromptCraftSHA256 string
	// Status is the invocation lifecycle state.
	Status InvocationStatus
	// LaunchVoided records that the coordinator or an explicit operator retry
	// proved this launch produced no native session or structured report and
	// rolled it back before retry.
	LaunchVoided bool
	// RecoveryResumeCount records the number of coordinator-owned native resume
	// attempts completed for this invocation. Reconciliation permits one.
	RecoveryResumeCount int
	// ManualResumeCount is the durable generation reserved before each explicit
	// operator-requested native resume. It prevents replay after response loss.
	ManualResumeCount int
	// ReviewRoundID identifies the exact review round for a partitioned review
	// invocation. It is empty for non-review invocations.
	ReviewRoundID string
	// ReviewUnitID identifies the manifest unit assigned to this invocation. It
	// is empty for non-review invocations.
	ReviewUnitID string
	// CreatedAt is the immutable invocation creation time.
	CreatedAt time.Time
	// UpdatedAt is the latest coordinator update time.
	UpdatedAt time.Time
}

// GateResult is the content-limited durable projection of one deterministic
// gate outcome. Its primary identity includes the exact checkpoint and phase,
// so a later code change cannot reuse an earlier result.
type GateResult struct {
	// RunID identifies the factory run.
	RunID string
	// CheckpointSHA identifies the exact evaluated commit.
	CheckpointSHA string
	// Phase distinguishes baseline from checkpoint evaluation.
	Phase GatePhase
	// Ordinal preserves repository declaration order.
	Ordinal int
	// GateName identifies the repository-declared gate.
	GateName string
	// Outcome is the coordinator-owned semantic result.
	Outcome GateOutcome
	// Status is the published exact-SHA status state.
	Status string
	// Blocking records the repository policy for the gate.
	Blocking bool
	// SkipReason explains a dependency or setup skip.
	SkipReason string
	// SetupFingerprint identifies the configured dependency inputs used by setup.
	SetupFingerprint string
	// CreatedAt is the first persistence time for this result identity.
	CreatedAt time.Time
	// UpdatedAt is the latest persistence time for this result identity.
	UpdatedAt time.Time
}

// GateResultTransaction atomically persists gate results and the corresponding
// content-free evaluation gate-run count.
type GateResultTransaction interface {
	SaveGateResults(context.Context, []GateResult) error
	RecordEvaluationGateRun(context.Context, string, int) error
	Commit() error
	Rollback() error
}

type Store struct {
	db            *sql.DB
	path          string
	schemaVersion int
}

// Open opens an existing current operational store, or creates a new one.
// It never migrates an existing store: an older schema is refused with a
// *MigrationRequiredError, because factory upgrade migrate-store is the only
// migration entry point. It requires a private parent directory and rejects
// empty databases, newer schema versions, and databases with missing or
// unreadable schema metadata.
func Open(ctx context.Context, path string) (*Store, error) {
	opened, _, err := openMigrating(ctx, path, false, ReleaseIdentity{})
	return opened, err
}

// MigrationRequiredError reports an existing store with an older schema
// that only the explicit migration entry point may migrate.
type MigrationRequiredError struct {
	Version   int
	Supported int
}

// Error names the supported migration entry point.
func (e *MigrationRequiredError) Error() string {
	return fmt.Sprintf("operational store schema version %d is older than supported version %d; migrate it with factory upgrade migrate-store", e.Version, e.Supported)
}

// openMigrating opens or creates a store and reports its schema migration.
// Existing pre-baseline schemas are refused even by the migration entry point.
// A fresh store creates its complete baseline and metadata atomically.
func openMigrating(ctx context.Context, path string, migrateExisting bool, release ReleaseIdentity) (*Store, Migration, error) {
	if path == "" {
		return nil, Migration{}, errors.New("operational store path is required")
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, Migration{}, fmt.Errorf("resolve operational store path: %w", err)
	}
	if err := refuseDuringRestore(absolutePath); err != nil {
		return nil, Migration{}, err
	}
	storeDirectory := filepath.Dir(absolutePath)
	if err := os.MkdirAll(storeDirectory, 0o700); err != nil {
		return nil, Migration{}, fmt.Errorf("create operational store directory: %w", err)
	}
	if info, err := os.Stat(storeDirectory); err != nil {
		return nil, Migration{}, fmt.Errorf("inspect operational store directory: %w", err)
	} else if info.Mode().Perm()&0o077 != 0 {
		return nil, Migration{}, fmt.Errorf("operational store directory %q is not private; expected no group or other permissions", storeDirectory)
	}
	existed, empty, err := databaseState(absolutePath)
	if err != nil {
		return nil, Migration{}, err
	}
	if existed && empty {
		return nil, Migration{}, &UnversionedDatabaseError{Path: absolutePath, Err: errors.New("database file is empty")}
	}
	database, err := openConfiguredDatabase(ctx, absolutePath)
	if err != nil {
		return nil, Migration{}, err
	}
	closeOnError := true
	defer func() {
		if closeOnError && database != nil {
			_ = database.Close()
		}
	}()
	version := 0
	if existed {
		version, err = readSchemaVersion(ctx, database, absolutePath)
		if err != nil {
			return nil, Migration{}, err
		}
	}
	if version > CurrentSchemaVersion {
		return nil, Migration{}, &UnknownSchemaVersionError{Version: version, Supported: CurrentSchemaVersion}
	}
	if existed && (version < MinimumUpgradeSchemaVersion || (version < CurrentSchemaVersion && !migrateExisting)) {
		return nil, Migration{}, &MigrationRequiredError{Version: version, Supported: CurrentSchemaVersion}
	}
	migration := Migration{FromVersion: version, ToVersion: version}
	if version < CurrentSchemaVersion {
		if existed {
			if err := database.Close(); err != nil {
				return nil, Migration{}, fmt.Errorf("close store before migration: %w", err)
			}
			backup, err := backupDatabase(ctx, absolutePath, release)
			if err != nil {
				return nil, Migration{}, err
			}
			migration.Backup = backup
			migration.Provenance = backup + backupProvenanceSuffix
			database, err = openConfiguredDatabase(ctx, absolutePath)
			if err != nil {
				return nil, Migration{}, fmt.Errorf("reopen operational store for migration: %w", err)
			}
		}
		if err := migrate(ctx, database, version); err != nil {
			return nil, Migration{}, err
		}
		version = CurrentSchemaVersion
		migration.ToVersion = version
	}
	closeOnError = false
	return &Store{db: database, path: absolutePath, schemaVersion: version}, migration, nil
}

// OpenReadOnly opens an existing, current operational store without creating
// directories, changing permissions, initializing metadata, migrating schema,
// or creating a backup. It is the only store entry point used by diagnosis.
func OpenReadOnly(ctx context.Context, path string) (*Store, error) {
	absolutePath, err := checkExistingStoreFile(path)
	if err != nil {
		return nil, err
	}
	return openReadOnlyFile(ctx, absolutePath)
}

// openRestoredReadOnly opens the replacement that a restore put at the live
// path, as OpenReadOnly does, while that restore's journal exists. Only the
// restore that owns the journal calls it, through RestoredStoreCheck.
func openRestoredReadOnly(ctx context.Context, path string) (*Store, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve operational store path: %w", err)
	}
	if err := checkStoreFile(absolutePath); err != nil {
		return nil, err
	}
	return openReadOnlyFile(ctx, absolutePath)
}

// openReadOnlyFile opens the checked store file at absolutePath read-only
// and refuses any schema other than the current one.
func openReadOnlyFile(ctx context.Context, absolutePath string) (*Store, error) {
	database, err := openReadOnlyDatabase(ctx, absolutePath)
	if err != nil {
		return nil, err
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = database.Close()
		}
	}()
	version, err := readSchemaVersion(ctx, database, absolutePath)
	if err != nil {
		return nil, err
	}
	if version > CurrentSchemaVersion {
		return nil, &UnknownSchemaVersionError{Version: version, Supported: CurrentSchemaVersion}
	}
	if version < CurrentSchemaVersion {
		return nil, &MigrationRequiredError{Version: version, Supported: CurrentSchemaVersion}
	}
	closeOnError = false
	return &Store{db: database, path: absolutePath, schemaVersion: version}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Path() string { return s.path }

func (s *Store) SchemaVersion() int { return s.schemaVersion }

// CurrentRun returns the newest non-terminal run, if one is active.
func (s *Store) CurrentRun(ctx context.Context) (*Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+runColumns+`
		FROM operational_runs
		WHERE status NOT IN (?, ?, ?)
		ORDER BY updated_at DESC
		LIMIT 1`, StatusComplete, StatusCancelled, StatusFailed)
	return scanRun(row)
}

// LatestRun returns the most recently updated claimed run, including terminal
// runs so status can show the last known branch and worktree.
func (s *Store) LatestRun(ctx context.Context) (*Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+runColumns+`
		FROM operational_runs
		ORDER BY updated_at DESC
		LIMIT 1`)
	return scanRun(row)
}

// ReadSupervisorHeartbeat returns the latest persisted coordinator heartbeat,
// or nil when no supervisor has written one for this operational store.
func (s *Store) ReadSupervisorHeartbeat(ctx context.Context) (*SupervisorHeartbeat, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT coordinator, pid, started_at, renewed_at, expires_at
		FROM supervisor_heartbeat
		WHERE singleton = 1`)
	var heartbeat SupervisorHeartbeat
	var startedAt, renewedAt, expiresAt string
	if err := row.Scan(&heartbeat.Coordinator, &heartbeat.PID, &startedAt, &renewedAt, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read supervisor heartbeat: %w", err)
	}
	var err error
	if heartbeat.StartedAt, err = time.Parse(time.RFC3339Nano, startedAt); err != nil {
		return nil, fmt.Errorf("parse supervisor heartbeat started_at: %w", err)
	}
	if heartbeat.RenewedAt, err = time.Parse(time.RFC3339Nano, renewedAt); err != nil {
		return nil, fmt.Errorf("parse supervisor heartbeat renewed_at: %w", err)
	}
	if heartbeat.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresAt); err != nil {
		return nil, fmt.Errorf("parse supervisor heartbeat expires_at: %w", err)
	}
	if err := validateSupervisorHeartbeat(heartbeat); err != nil {
		return nil, fmt.Errorf("validate supervisor heartbeat: %w", err)
	}
	return &heartbeat, nil
}

// SaveSupervisorHeartbeat replaces the singleton coordinator heartbeat with a
// validated, timestamped liveness projection.
func (s *Store) SaveSupervisorHeartbeat(ctx context.Context, heartbeat SupervisorHeartbeat) error {
	if err := validateSupervisorHeartbeat(heartbeat); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO supervisor_heartbeat (singleton, coordinator, pid, started_at, renewed_at, expires_at)
		VALUES (1, ?, ?, ?, ?, ?)
		ON CONFLICT(singleton) DO UPDATE SET
			coordinator = excluded.coordinator,
			pid = excluded.pid,
			started_at = excluded.started_at,
			renewed_at = excluded.renewed_at,
			expires_at = excluded.expires_at`,
		heartbeat.Coordinator,
		heartbeat.PID,
		heartbeat.StartedAt.UTC().Format(runTimestampLayout),
		heartbeat.RenewedAt.UTC().Format(runTimestampLayout),
		heartbeat.ExpiresAt.UTC().Format(runTimestampLayout)); err != nil {
		return fmt.Errorf("save supervisor heartbeat: %w", err)
	}
	return nil
}

// runColumns is the canonical column order of every operational run read;
// scanRun decodes exactly this order.
const runColumns = `id, repository_path, issue_number, stage, status, branch, worktree,
	checkpoint_sha, base_checkpoint_sha, accepted_implementation_checkpoint_sha, test_checkpoint_sha,
	test_handoff, test_invocation_id, test_revision_attempts,
	test_revision_budget, test_revision_history, test_objection,
	test_revision_base_changed_paths,
	implementation_handoff, specification_review, standards_review,
	pull_request_summary,
	review_repair_attempts, review_repair_budget,
	review_repair_pending_attempt, review_repair_history, review_repair_packet,
	test_exemption, protected_test_paths, test_stage_skipped,
	active_invocation_ids,
	image_digest, coordinator, status_comment_id,
	pull_request_number, pull_request_url, merge_commit_sha,
	lifecycle_reason,
	revision, processed_comment_id, processed_comment_revision,
	processed_review_id, processed_review_revision,
	last_command_name, last_command_outcome, last_command_message,
	harness_override, check_repair_attempts, check_repair_budget,
	check_repair_pending_attempt,
	specification_packet, pending_questions, clarification_comment_id,
	tracker_repository,
	terminal_at, created_at, updated_at`

// scanRun decodes one operational run row in runColumns order and its
// RFC3339 timestamps. A single-row read with no row returns nil.
func scanRun(row interface{ Scan(...any) error }) (*Run, error) {
	var run Run
	var baseCheckpointSHA, acceptedImplementationCheckpointSHA, testCheckpointSHA string
	var testHandoffJSON, roleHandoffJSON, specificationReviewJSON, standardsReviewJSON string
	var pullRequestSummaryJSON string
	var reviewRepairHistoryJSON, reviewRepairPacketJSON string
	var testRevisionHistoryJSON, testObjectionJSON, testRevisionBaseChangedPathsJSON string
	var testExemptionJSON, protectedTestPathsJSON string
	var activeInvocationIDsJSON string
	var pendingQuestionsJSON, clarificationCommentID, trackerRepositoryJSON, terminalAt, createdAt, updatedAt string
	var err error
	if err := row.Scan(
		&run.ID,
		&run.RepositoryPath,
		&run.IssueNumber,
		&run.Stage,
		&run.Status,
		&run.Branch,
		&run.Worktree,
		&run.CheckpointSHA,
		&baseCheckpointSHA,
		&acceptedImplementationCheckpointSHA,
		&testCheckpointSHA,
		&testHandoffJSON,
		&run.TestInvocationID,
		&run.TestRevisionAttempts,
		&run.TestRevisionBudget,
		&testRevisionHistoryJSON,
		&testObjectionJSON,
		&testRevisionBaseChangedPathsJSON,
		&roleHandoffJSON,
		&specificationReviewJSON,
		&standardsReviewJSON,
		&pullRequestSummaryJSON,
		&run.ReviewRepairAttempts,
		&run.ReviewRepairBudget,
		&run.ReviewRepairPendingAttempt,
		&reviewRepairHistoryJSON,
		&reviewRepairPacketJSON,
		&testExemptionJSON,
		&protectedTestPathsJSON,
		&run.TestStageSkipped,
		&activeInvocationIDsJSON,
		&run.ImageDigest,
		&run.Coordinator,
		&run.StatusCommentID,
		&run.PullRequestNumber,
		&run.PullRequestURL,
		&run.MergeCommitSHA,
		&run.LifecycleReason,
		&run.Revision,
		&run.ProcessedCommentID,
		&run.ProcessedCommentRevision,
		&run.ProcessedReviewID,
		&run.ProcessedReviewRevision,
		&run.LastCommandName,
		&run.LastCommandOutcome,
		&run.LastCommandMessage,
		&run.HarnessOverride,
		&run.CheckRepairAttempts,
		&run.CheckRepairBudget,
		&run.CheckRepairPendingAttempt,
		&run.SpecificationPacket,
		&pendingQuestionsJSON,
		&clarificationCommentID,
		&trackerRepositoryJSON,
		&terminalAt,
		&createdAt,
		&updatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read run row: %w", err)
	}
	if pendingQuestionsJSON != "" {
		if err := json.Unmarshal([]byte(pendingQuestionsJSON), &run.PendingQuestions); err != nil {
			return nil, fmt.Errorf("decode pending clarification questions: %w", err)
		}
	}
	run.BaseCheckpointSHA = baseCheckpointSHA
	if run.BaseCheckpointSHA == "" {
		run.BaseCheckpointSHA = run.CheckpointSHA
	}
	run.AcceptedImplementationCheckpointSHA = acceptedImplementationCheckpointSHA
	run.TestCheckpointSHA = testCheckpointSHA
	if testHandoffJSON != "" {
		run.TestHandoff = &TestHandoff{}
		if err := json.Unmarshal([]byte(testHandoffJSON), run.TestHandoff); err != nil {
			return nil, fmt.Errorf("decode test handoff: %w", err)
		}
	}
	if testRevisionHistoryJSON != "" {
		if err := json.Unmarshal([]byte(testRevisionHistoryJSON), &run.TestRevisionHistory); err != nil {
			return nil, fmt.Errorf("decode test revision history: %w", err)
		}
	}
	if testObjectionJSON != "" {
		run.TestObjection = &TestObjection{}
		if err := json.Unmarshal([]byte(testObjectionJSON), run.TestObjection); err != nil {
			return nil, fmt.Errorf("decode test objection: %w", err)
		}
	}
	if testRevisionBaseChangedPathsJSON != "" {
		if err := json.Unmarshal([]byte(testRevisionBaseChangedPathsJSON), &run.TestRevisionBaseChangedPaths); err != nil {
			return nil, fmt.Errorf("decode test revision base changed paths: %w", err)
		}
	}
	if roleHandoffJSON != "" {
		run.RoleHandoff = &RoleHandoff{}
		if err := json.Unmarshal([]byte(roleHandoffJSON), run.RoleHandoff); err != nil {
			return nil, fmt.Errorf("decode role handoff: %w", err)
		}
	}
	if specificationReviewJSON != "" {
		run.SpecificationReview = &SpecificationReview{}
		if err := json.Unmarshal([]byte(specificationReviewJSON), run.SpecificationReview); err != nil {
			return nil, fmt.Errorf("decode specification review: %w", err)
		}
	}
	if standardsReviewJSON != "" {
		run.StandardsReview = &StandardsReview{}
		if err := json.Unmarshal([]byte(standardsReviewJSON), run.StandardsReview); err != nil {
			return nil, fmt.Errorf("decode standards review: %w", err)
		}
	}
	if pullRequestSummaryJSON != "" {
		run.PullRequestSummary = &PullRequestSummary{}
		if err := json.Unmarshal([]byte(pullRequestSummaryJSON), run.PullRequestSummary); err != nil {
			return nil, fmt.Errorf("decode pull-request summary: %w", err)
		}
	}
	if reviewRepairHistoryJSON != "" {
		if err := json.Unmarshal([]byte(reviewRepairHistoryJSON), &run.ReviewRepairHistory); err != nil {
			return nil, fmt.Errorf("decode review repair history: %w", err)
		}
	}
	if reviewRepairPacketJSON != "" {
		run.ReviewRepairPacket = &ReviewRepairPacket{}
		if err := json.Unmarshal([]byte(reviewRepairPacketJSON), run.ReviewRepairPacket); err != nil {
			return nil, fmt.Errorf("decode review repair packet: %w", err)
		}
	}
	if activeInvocationIDsJSON != "" {
		if err := json.Unmarshal([]byte(activeInvocationIDsJSON), &run.ActiveInvocationIDs); err != nil {
			return nil, fmt.Errorf("decode active invocation ids: %w", err)
		}
	}
	if testExemptionJSON != "" {
		run.TestExemption = &TestExemption{}
		if err := json.Unmarshal([]byte(testExemptionJSON), run.TestExemption); err != nil {
			return nil, fmt.Errorf("decode test exemption: %w", err)
		}
	}
	if protectedTestPathsJSON != "" {
		if err := json.Unmarshal([]byte(protectedTestPathsJSON), &run.ProtectedTestPaths); err != nil {
			return nil, fmt.Errorf("decode protected test paths: %w", err)
		}
	}
	run.ClarificationCommentID = clarificationCommentID
	if trackerRepositoryJSON != "" {
		run.TrackerRepository = &TrackerRepository{}
		if err := json.Unmarshal([]byte(trackerRepositoryJSON), run.TrackerRepository); err != nil {
			return nil, fmt.Errorf("decode tracker repository: %w", err)
		}
	}
	if terminalAt != "" {
		run.TerminalAt, err = time.Parse(time.RFC3339Nano, terminalAt)
		if err != nil {
			return nil, fmt.Errorf("parse run terminal_at: %w", err)
		}
	}
	run.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse run created_at: %w", err)
	}
	run.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return nil, fmt.Errorf("parse run updated_at: %w", err)
	}
	return &run, nil
}

// SaveRun validates and upserts one operational run record.
func (s *Store) SaveRun(ctx context.Context, run Run) error {
	run, err := normalizeRun(run)
	if err != nil {
		return err
	}
	values, err := runValues(run)
	if err != nil {
		return fmt.Errorf("encode run projections: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, saveRunStatement, values...); err != nil {
		return fmt.Errorf("save run: %w", err)
	}
	return nil
}

// ValidateRun applies the operational-store run rules without persisting the
// projection. Coordinator effect paths use it before reserving an external
// mutation so an unpersistable run cannot cross that effect boundary.
func ValidateRun(run Run) error {
	_, err := normalizeRun(run)
	return err
}

// PendingEffect returns the one durable external-effect reservation for a run,
// or nil when the run has no mutation awaiting reconciliation.
func (s *Store) PendingEffect(ctx context.Context, runID string) (*PendingEffect, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, errors.New("pending effect run id is required")
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT run_id, effect_id, kind, payload, created_at, updated_at
		FROM pending_effects
		WHERE run_id = ?`, runID)
	return scanPendingEffect(row)
}

// SavePendingEffect reserves one external mutation. The effect identity is the
// reservation; re-saving the same run/effect identity refreshes the replay
// payload so a retried attempt can carry a newer external snapshot. A different
// effect is rejected until the existing reservation is completed or abandoned.
func (s *Store) SavePendingEffect(ctx context.Context, effect PendingEffect) error {
	effect, err := normalizePendingEffect(effect)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO pending_effects (run_id, effect_id, kind, payload, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET
			payload = excluded.payload,
			updated_at = excluded.updated_at
		WHERE pending_effects.effect_id = excluded.effect_id
		  AND pending_effects.kind = excluded.kind`,
		effect.RunID, effect.ID, effect.Kind, effect.Payload,
		effect.CreatedAt.UTC().Format(runTimestampLayout),
		effect.UpdatedAt.UTC().Format(runTimestampLayout)); err != nil {
		return fmt.Errorf("save pending effect: %w", err)
	}
	current, err := s.PendingEffect(ctx, effect.RunID)
	if err != nil {
		return err
	}
	if current == nil || current.ID != effect.ID || current.Kind != effect.Kind {
		return fmt.Errorf("%w: run %q already has effect %q", ErrPendingEffectConflict, effect.RunID, pendingEffectIdentity(current))
	}
	return nil
}

// ClearPendingEffect completes a matching external-effect reservation. A
// missing row is already complete; a different row is left untouched so a
// caller cannot accidentally acknowledge another operation.
func (s *Store) ClearPendingEffect(ctx context.Context, runID, effectID string) error {
	if strings.TrimSpace(runID) == "" {
		return errors.New("pending effect run id is required")
	}
	if strings.TrimSpace(effectID) == "" {
		return errors.New("pending effect id is required")
	}
	result, err := s.db.ExecContext(ctx, "DELETE FROM pending_effects WHERE run_id = ? AND effect_id = ?", runID, effectID)
	if err != nil {
		return fmt.Errorf("clear pending effect: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect cleared pending effect: %w", err)
	}
	if changed == 1 {
		return nil
	}
	current, err := s.PendingEffect(ctx, runID)
	if err != nil {
		return err
	}
	if current != nil && current.ID != effectID {
		return fmt.Errorf("%w: cannot clear effect %q while effect %q is pending", ErrPendingEffectConflict, effectID, current.ID)
	}
	return nil
}

// AbandonPendingEffect removes a matching reservation after an operator has
// deliberately decided that the effect must not be replayed. The reason is
// intentionally supplied by the caller and is not retained in the store.
func (s *Store) AbandonPendingEffect(ctx context.Context, runID, effectID, _ string) error {
	return s.ClearPendingEffect(ctx, runID, effectID)
}

// scanPendingEffect decodes one pending-effect row and its canonical
// timestamps.
func scanPendingEffect(row *sql.Row) (*PendingEffect, error) {
	var effect PendingEffect
	var kind, createdAt, updatedAt string
	if err := row.Scan(&effect.RunID, &effect.ID, &kind, &effect.Payload, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read pending effect: %w", err)
	}
	effect.Kind = PendingEffectKind(kind)
	var err error
	effect.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse pending effect created_at: %w", err)
	}
	effect.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return nil, fmt.Errorf("parse pending effect updated_at: %w", err)
	}
	return &effect, nil
}

// normalizePendingEffect validates the bounded journal identity and fills
// absent timestamps before persistence.
func normalizePendingEffect(effect PendingEffect) (PendingEffect, error) {
	if strings.TrimSpace(effect.RunID) == "" {
		return PendingEffect{}, errors.New("pending effect run id is required")
	}
	if strings.TrimSpace(effect.ID) == "" || strings.ContainsAny(effect.ID, "\x00\r\n") {
		return PendingEffect{}, errors.New("pending effect id is required and must be a single line")
	}
	if strings.TrimSpace(string(effect.Kind)) == "" || strings.ContainsAny(string(effect.Kind), "\x00\r\n") {
		return PendingEffect{}, errors.New("pending effect kind is required and must be a single line")
	}
	if strings.TrimSpace(effect.Payload) == "" || len(effect.Payload) > 1<<20 || !json.Valid([]byte(effect.Payload)) {
		return PendingEffect{}, errors.New("pending effect payload must be valid bounded JSON")
	}
	if effect.CreatedAt.IsZero() {
		effect.CreatedAt = time.Now().UTC()
	}
	if effect.UpdatedAt.IsZero() {
		effect.UpdatedAt = effect.CreatedAt
	}
	return effect, nil
}

// pendingEffectIdentity describes a conflict even when its row is missing.
func pendingEffectIdentity(effect *PendingEffect) string {
	if effect == nil {
		return "<missing>"
	}
	return effect.ID
}

// SaveRunIfRevision persists one command result only when the run still has
// expectedRevision. It prevents concurrent coordinators from executing one
// comment against the same stale run snapshot.
func (s *Store) SaveRunIfRevision(ctx context.Context, expectedRevision int64, run Run) error {
	run, err := normalizeRun(run)
	if err != nil {
		return err
	}
	if run.Revision <= expectedRevision {
		return fmt.Errorf("%w: got %d, expected greater than %d", ErrRevisionNotAdvanced, run.Revision, expectedRevision)
	}
	values, err := runValues(run)
	if err != nil {
		return fmt.Errorf("encode run projections: %w", err)
	}
	result, err := s.db.ExecContext(ctx, saveRunIfRevisionStatement, append(values[1:], run.ID, expectedRevision)...)
	if err != nil {
		return fmt.Errorf("save run at revision %d: %w", expectedRevision, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect run revision %d update: %w", expectedRevision, err)
	}
	if changed != 1 {
		return fmt.Errorf("%w: run %q no longer has revision %d", ErrRevisionConflict, run.ID, expectedRevision)
	}
	return nil
}

// normalizeRun validates a run and fills absent timestamps before persistence.
func normalizeRun(run Run) (Run, error) {
	if run.ID == "" {
		return Run{}, errors.New("run id is required")
	}
	if run.RepositoryPath == "" {
		return Run{}, errors.New("run repository path is required")
	}
	if run.Status == "" {
		return Run{}, errors.New("run status is required")
	}
	if run.BaseCheckpointSHA == "" {
		run.BaseCheckpointSHA = run.CheckpointSHA
	}
	if run.CheckpointSHA == "" && run.BaseCheckpointSHA != "" {
		run.CheckpointSHA = run.BaseCheckpointSHA
	}
	if err := validateTestProjection(run); err != nil {
		return Run{}, err
	}
	if run.CheckRepairAttempts < 0 {
		return Run{}, errors.New("run check-repair attempts must not be negative")
	}
	if run.CheckRepairBudget < 0 {
		return Run{}, errors.New("run check-repair budget must not be negative")
	}
	if run.CheckRepairAttempts > run.CheckRepairBudget {
		return Run{}, errors.New("run check-repair attempts must not exceed budget")
	}
	if run.CheckRepairPendingAttempt < 0 {
		return Run{}, errors.New("run pending check-repair attempt must not be negative")
	}
	if run.CheckRepairPendingAttempt > run.CheckRepairBudget {
		return Run{}, errors.New("run pending check-repair attempt must not exceed budget")
	}
	if run.CheckRepairPendingAttempt != 0 && run.CheckRepairPendingAttempt != run.CheckRepairAttempts+1 {
		return Run{}, errors.New("run pending check-repair attempt must be the next attempt")
	}
	if run.ReviewRepairAttempts < 0 {
		return Run{}, errors.New("run review-repair attempts must not be negative")
	}
	if run.ReviewRepairBudget < 0 {
		return Run{}, errors.New("run review-repair budget must not be negative")
	}
	if run.ReviewRepairAttempts > run.ReviewRepairBudget {
		return Run{}, errors.New("run review-repair attempts must not exceed budget")
	}
	if run.ReviewRepairPendingAttempt < 0 {
		return Run{}, errors.New("run pending review-repair attempt must not be negative")
	}
	if run.ReviewRepairPendingAttempt > run.ReviewRepairBudget {
		return Run{}, errors.New("run pending review-repair attempt must not exceed budget")
	}
	if run.ReviewRepairPendingAttempt != 0 && run.ReviewRepairPendingAttempt != run.ReviewRepairAttempts+1 {
		return Run{}, errors.New("run pending review-repair attempt must be the next attempt")
	}
	if run.TestRevisionAttempts < 0 {
		return Run{}, errors.New("run test-revision attempts must not be negative")
	}
	if run.TestRevisionBudget < 0 {
		return Run{}, errors.New("run test-revision budget must not be negative")
	}
	if run.TestRevisionAttempts > run.TestRevisionBudget {
		return Run{}, errors.New("run test-revision attempts must not exceed budget")
	}
	if err := validateReviewRepairProjection(run); err != nil {
		return Run{}, err
	}
	if strings.ContainsAny(run.ClarificationCommentID, "\x00\r\n") {
		return Run{}, errors.New("run clarification comment id contains a control character")
	}
	if run.TrackerRepository != nil {
		if err := run.TrackerRepository.validate(); err != nil {
			return Run{}, err
		}
	}
	if err := validatePendingQuestions(run.PendingQuestions); err != nil {
		return Run{}, err
	}
	if run.CreatedAt.IsZero() {
		run.CreatedAt = time.Now().UTC()
	}
	if run.UpdatedAt.IsZero() {
		run.UpdatedAt = run.CreatedAt
	}
	if IsTerminalStatus(run.Status) {
		if run.TerminalAt.IsZero() {
			run.TerminalAt = run.UpdatedAt
		}
	} else {
		run.TerminalAt = time.Time{}
	}
	return run, nil
}

// validateSupervisorHeartbeat checks the bounded fields and expiry ordering of
// one coordinator liveness projection before it crosses the store boundary.
// Wall-clock time may step backwards, so a renewal is not required to follow
// the process start timestamp.
func validateSupervisorHeartbeat(heartbeat SupervisorHeartbeat) error {
	if strings.TrimSpace(heartbeat.Coordinator) == "" || strings.ContainsAny(heartbeat.Coordinator, "\x00\r\n") {
		return errors.New("supervisor heartbeat coordinator is required and must be single-line")
	}
	if heartbeat.PID <= 0 {
		return errors.New("supervisor heartbeat PID must be positive")
	}
	if heartbeat.StartedAt.IsZero() || heartbeat.RenewedAt.IsZero() || heartbeat.ExpiresAt.IsZero() {
		return errors.New("supervisor heartbeat timestamps are required")
	}
	// A clean stop records ExpiresAt equal to RenewedAt: the coordinator stays
	// observable and is no longer live.
	if heartbeat.ExpiresAt.Before(heartbeat.RenewedAt) {
		return errors.New("supervisor heartbeat expiry must not precede renewal")
	}
	return nil
}

// validateTestProjection checks the bounded durable test-stage state before it
// is serialized into SQLite.
func validateTestProjection(run Run) error {
	if len(run.ProtectedTestPaths) > 256 {
		return errors.New("run protected test paths exceed 256 entries")
	}
	seen := make(map[string]struct{}, len(run.ProtectedTestPaths))
	for index, value := range run.ProtectedTestPaths {
		if err := validateStoreRelativePath(value.Path); err != nil {
			return fmt.Errorf("run protected test path %d: %w", index, err)
		}
		if len(value.SHA256) != 64 || !isLowerHex(value.SHA256) {
			return fmt.Errorf("run protected test path %q has an invalid SHA256", value.Path)
		}
		if _, exists := seen[value.Path]; exists {
			return fmt.Errorf("run protected test path %q is duplicated", value.Path)
		}
		seen[value.Path] = struct{}{}
	}
	if run.TestExemption != nil {
		if run.TestExemption.Kind != "human" && run.TestExemption.Kind != "technical" {
			return fmt.Errorf("unsupported test exemption %q", run.TestExemption.Kind)
		}
		if strings.TrimSpace(run.TestExemption.Justification) == "" || strings.ContainsAny(run.TestExemption.Justification, "\x00\r\n") {
			return errors.New("test exemption justification is required and must be single-line")
		}
	}
	if run.TestHandoff != nil {
		if len(run.TestHandoff.AcceptanceCoverage) == 0 || len(run.TestHandoff.ChangedFiles) == 0 || strings.TrimSpace(run.TestHandoff.FocusedTestCommand) == "" || strings.TrimSpace(run.TestHandoff.ExpectedFailureReason) == "" || len(run.TestHandoff.ObservedFailureEvidence) == 0 {
			return errors.New("run test handoff is incomplete")
		}
		for _, value := range run.TestHandoff.ChangedFiles {
			if err := validateStoreRelativePath(value); err != nil {
				return fmt.Errorf("run test handoff changed file: %w", err)
			}
		}
		for _, value := range run.TestHandoff.InfrastructureChanges {
			if err := validateStoreRelativePath(value); err != nil {
				return fmt.Errorf("run test handoff infrastructure file: %w", err)
			}
		}
	}
	if run.TestInvocationID != "" && !safeQuestionIdentifier(run.TestInvocationID) {
		return errors.New("run test invocation id is unsafe")
	}
	seenAttempts := make(map[int]struct{}, len(run.TestRevisionHistory))
	disputeKey := ""
	for index, revision := range run.TestRevisionHistory {
		if revision.Attempt < 1 || revision.Attempt > run.TestRevisionBudget {
			return fmt.Errorf("run test revision history entry %d has an invalid attempt", index)
		}
		if _, exists := seenAttempts[revision.Attempt]; exists {
			return fmt.Errorf("run test revision history attempt %d is duplicated", revision.Attempt)
		}
		seenAttempts[revision.Attempt] = struct{}{}
		if revision.DisputeKey != "" {
			if len(revision.DisputeKey) != 64 || !isLowerHex(revision.DisputeKey) {
				return fmt.Errorf("run test revision history entry %d has an invalid dispute key", index)
			}
			if disputeKey == "" {
				disputeKey = revision.DisputeKey
			} else if disputeKey != revision.DisputeKey {
				return errors.New("run test revision history must contain one dispute key")
			}
		}
		switch revision.Outcome {
		case TestRevisionPending, TestRevisionAccepted, TestRevisionRejected, TestRevisionVerificationFailed, TestRevisionEscalated:
		default:
			return fmt.Errorf("unsupported test revision outcome %q", revision.Outcome)
		}
	}
	if len(run.TestRevisionBaseChangedPaths) > 256 {
		return errors.New("run test revision base changed paths exceed 256 entries")
	}
	seenBasePaths := make(map[string]struct{}, len(run.TestRevisionBaseChangedPaths))
	for index, path := range run.TestRevisionBaseChangedPaths {
		if err := validateStoreRelativePath(path.Path); err != nil {
			return fmt.Errorf("run test revision base changed path %d: %w", index, err)
		}
		if len(path.SHA256) != 64 || !isLowerHex(path.SHA256) {
			return fmt.Errorf("run test revision base changed path %q has an invalid SHA256", path.Path)
		}
		if _, exists := seenBasePaths[path.Path]; exists {
			return fmt.Errorf("run test revision base changed path %q is duplicated", path.Path)
		}
		seenBasePaths[path.Path] = struct{}{}
	}
	if run.TestObjection != nil {
		for field, value := range map[string]string{
			"test": run.TestObjection.Test, "claim": run.TestObjection.Claim, "evidence": run.TestObjection.Evidence,
		} {
			if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\x00\r\n") {
				return fmt.Errorf("test objection %s is required and must be single-line", field)
			}
		}
		if strings.TrimSpace(run.TestObjection.InvocationID) == "" {
			return errors.New("test objection invocation id is required")
		}
		if !safeQuestionIdentifier(run.TestObjection.InvocationID) {
			return errors.New("test objection invocation id is unsafe")
		}
	}
	roleHandoff := run.RoleHandoff
	if roleHandoff != nil {
		if strings.TrimSpace(roleHandoff.ChangeSummary) == "" || strings.ContainsAny(roleHandoff.ChangeSummary, "\x00\r\n") {
			return errors.New("role handoff change summary is required and must be single-line")
		}
		if len(roleHandoff.AcceptanceMapping) == 0 || len(roleHandoff.ProductionFilesChanged) == 0 || len(roleHandoff.FocusedCommands) == 0 {
			return errors.New("role handoff is incomplete")
		}
		for _, mapping := range roleHandoff.AcceptanceMapping {
			if strings.TrimSpace(mapping.Criterion) == "" || strings.TrimSpace(mapping.Evidence) == "" || strings.ContainsAny(mapping.Criterion+mapping.Evidence, "\x00\r\n") {
				return errors.New("role handoff acceptance mapping is incomplete")
			}
		}
		for _, path := range roleHandoff.ProductionFilesChanged {
			if err := validateStoreRelativePath(path); err != nil {
				return fmt.Errorf("role handoff changed path: %w", err)
			}
		}
		for index, objection := range roleHandoff.TestObjections {
			for field, value := range map[string]string{
				"test": objection.Test, "claim": objection.Claim, "evidence": objection.Evidence,
			} {
				if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\x00\r\n") {
					return fmt.Errorf("role handoff test objection %d %s is required and must be single-line", index, field)
				}
			}
		}
	}
	if err := validateReviewProjection("specification", run.SpecificationReview, run.CheckpointSHA); err != nil {
		return err
	}
	if err := validateReviewProjection("standards", run.StandardsReview, run.CheckpointSHA); err != nil {
		return err
	}
	if err := validatePullRequestSummary(run.PullRequestSummary); err != nil {
		return err
	}
	if len(run.ActiveInvocationIDs) > 32 {
		return errors.New("run active invocation ids exceed 32 entries")
	}
	seenActiveInvocations := make(map[string]struct{}, len(run.ActiveInvocationIDs))
	for index, invocationID := range run.ActiveInvocationIDs {
		if !safeQuestionIdentifier(invocationID) {
			return fmt.Errorf("run active invocation %d has an unsafe identifier", index)
		}
		if _, exists := seenActiveInvocations[invocationID]; exists {
			return fmt.Errorf("run active invocation %q is duplicated", invocationID)
		}
		seenActiveInvocations[invocationID] = struct{}{}
	}
	return nil
}

// validateReviewRepairProjection checks the bounded review-repair state before
// it is serialized into the operational store.
func validateReviewRepairProjection(run Run) error {
	seenAttempts := make(map[int]struct{}, len(run.ReviewRepairHistory))
	for index, revision := range run.ReviewRepairHistory {
		if revision.Attempt < 1 || revision.Attempt > run.ReviewRepairBudget {
			return fmt.Errorf("run review repair history entry %d has an invalid attempt", index)
		}
		if _, exists := seenAttempts[revision.Attempt]; exists {
			return fmt.Errorf("run review repair history attempt %d is duplicated", revision.Attempt)
		}
		seenAttempts[revision.Attempt] = struct{}{}
		switch revision.Outcome {
		case ReviewRepairPending, ReviewRepairStarted, ReviewRepairRepeated, ReviewRepairExhausted, ReviewRepairWaitingForHuman:
		default:
			return fmt.Errorf("unsupported review repair outcome %q", revision.Outcome)
		}
		if len(revision.BlockerKeys) > 256 {
			return fmt.Errorf("review repair history entry %d has too many blocker keys", index)
		}
		for _, key := range revision.BlockerKeys {
			if len(key) != 64 || !isLowerHex(key) {
				return fmt.Errorf("review repair history entry %d has an invalid blocker key", index)
			}
		}
	}
	if packet := run.ReviewRepairPacket; packet != nil {
		if packet.Version <= 0 || packet.RunID != run.ID || !validGateCheckpointSHA(packet.CheckpointSHA) {
			return errors.New("review repair packet identity is invalid")
		}
		if packet.Budget != run.ReviewRepairBudget {
			return errors.New("review repair packet budget is invalid")
		}
		switch packet.Source {
		case ReviewRepairSourceHuman:
			// A human-requested repair is not one of the bounded factory
			// rounds, so it carries no attempt number and never exhausts the
			// budget. Its maintainer event identity is what makes it replayable.
			kind, identity := packet.SourceEvent()
			if packet.Attempt != 0 || identity == "" {
				return errors.New("human review repair packet must identify its maintainer event and carry no attempt")
			}
			if kind != ReviewRepairEventPullRequestReview && kind != ReviewRepairEventSupervisionCommand {
				return fmt.Errorf("unsupported review repair source event %q", kind)
			}
		case ReviewRepairSourceFactory:
			if packet.Attempt < 1 || packet.Attempt > run.ReviewRepairBudget || strings.TrimSpace(packet.SourceEventID) != "" {
				return errors.New("factory review repair packet attempt is invalid")
			}
		default:
			return fmt.Errorf("unsupported review repair source %q", packet.Source)
		}
		if len(packet.Findings) == 0 || len(packet.Findings) > 256 {
			return errors.New("review repair packet findings must contain one to 256 entries")
		}
		for index, finding := range packet.Findings {
			if !safeQuestionIdentifier(finding.ReviewerRole) {
				return fmt.Errorf("review repair packet finding %d has an unsafe reviewer role", index)
			}
			if err := validateReviewFinding("review repair packet", index, finding.Finding); err != nil {
				return err
			}
		}
		if len(packet.ResolvedTestOwnerFindingKeys) > len(packet.Findings) {
			return errors.New("review repair packet has too many resolved test-owner findings")
		}
		seenResolved := make(map[string]struct{}, len(packet.ResolvedTestOwnerFindingKeys))
		for index, key := range packet.ResolvedTestOwnerFindingKeys {
			if len(key) != 64 || !isLowerHex(key) {
				return fmt.Errorf("review repair packet resolved test-owner key %d is invalid", index)
			}
			if _, exists := seenResolved[key]; exists {
				return fmt.Errorf("review repair packet resolved test-owner key %d is duplicated", index)
			}
			seenResolved[key] = struct{}{}
		}
	}
	return nil
}

// validatePullRequestSummary checks the saved PR-writer result. A summary may
// describe an earlier checkpoint; readiness compares it with the current one.
func validatePullRequestSummary(summary *PullRequestSummary) error {
	if summary == nil {
		return nil
	}
	if !validGateCheckpointSHA(summary.CheckpointSHA) {
		return errors.New("pull-request summary checkpoint SHA must contain exactly 40 or 64 lowercase hexadecimal characters")
	}
	if strings.ContainsAny(summary.Reason, "\x00\r\n") || strings.ContainsAny(summary.InvocationID, "\x00\r\n") {
		return errors.New("pull-request summary reason and invocation id must be single-line")
	}
	switch summary.Status {
	case PullRequestSummaryWritten:
		if strings.TrimSpace(summary.Body) == "" {
			return errors.New("written pull-request summary must retain a body")
		}
	case PullRequestSummaryFailed:
		if summary.Body != "" {
			return errors.New("failed pull-request summary must not retain a body")
		}
	default:
		return fmt.Errorf("unsupported pull-request summary status %q", summary.Status)
	}
	return nil
}

// validateReviewProjection checks one nullable exact-checkpoint review result
// before it is serialized into the operational store.
func validateReviewProjection(label string, review *ReviewResult, checkpoint string) error {
	if review == nil {
		return nil
	}
	if review.CheckpointSHA != checkpoint {
		return fmt.Errorf("%s review must match the run checkpoint SHA", label)
	}
	if !validGateCheckpointSHA(review.CheckpointSHA) {
		return fmt.Errorf("%s review checkpoint SHA must contain exactly 40 or 64 lowercase hexadecimal characters", label)
	}
	outcome := review.Outcome
	switch outcome {
	case "completed":
		if len(review.Questions) != 0 || len(review.Evidence) != 0 {
			return fmt.Errorf("%s completed review must not retain questions or evidence", label)
		}
	case "needs_clarification":
		if len(review.Questions) == 0 || len(review.Evidence) != 0 {
			return fmt.Errorf("%s needs_clarification review must retain questions only", label)
		}
	case "cannot_proceed":
		if len(review.Evidence) == 0 || len(review.Questions) != 0 {
			return fmt.Errorf("%s cannot_proceed review must retain evidence only", label)
		}
	default:
		return fmt.Errorf("unsupported %s review outcome %q", label, review.Outcome)
	}
	if len(review.Summary) > MaxReviewSummaryBytes || strings.ContainsAny(review.Summary, "\x00\r\n") {
		return fmt.Errorf("%s review summary is invalid", label)
	}
	if len(review.Questions) > MaxReviewQuestions {
		return fmt.Errorf("%s review questions exceed %d entries", label, MaxReviewQuestions)
	}
	seenQuestions := make(map[string]struct{}, len(review.Questions))
	for index, question := range review.Questions {
		if !safeQuestionIdentifier(question.ID) || strings.TrimSpace(question.Prompt) == "" || strings.ContainsAny(question.Prompt, "\x00\r\n") {
			return fmt.Errorf("%s review question %d is invalid", label, index)
		}
		if _, exists := seenQuestions[question.ID]; exists {
			return fmt.Errorf("%s review question %q is duplicated", label, question.ID)
		}
		seenQuestions[question.ID] = struct{}{}
	}
	if len(review.Evidence) > MaxReviewEvidence {
		return fmt.Errorf("%s review evidence exceeds %d entries", label, MaxReviewEvidence)
	}
	for index, evidence := range review.Evidence {
		if strings.TrimSpace(evidence.Kind) == "" || strings.TrimSpace(evidence.Detail) == "" || strings.ContainsAny(evidence.Kind+evidence.Detail, "\x00\r\n") {
			return fmt.Errorf("%s review evidence %d is invalid", label, index)
		}
	}
	if len(review.Findings) > MaxReviewFindings {
		return fmt.Errorf("%s review findings exceed %d entries", label, MaxReviewFindings)
	}
	for index, finding := range review.Findings {
		if err := validateReviewFinding(label, index, finding); err != nil {
			return err
		}
	}
	return nil
}

// validateReviewFinding applies the shared nested finding contract to both
// exact-checkpoint reviews and review-repair packets.
func validateReviewFinding(label string, index int, finding ReviewFinding) error {
	if finding.UnitID != "" && !safeQuestionIdentifier(finding.UnitID) {
		return fmt.Errorf("%s review finding %d unit_id is unsafe", label, index)
	}
	for field, value := range map[string]string{
		"location": finding.Location, "claim": finding.Claim, "evidence": finding.Evidence,
		"suggested_resolution": finding.SuggestedResolution, "suggested_owner": finding.SuggestedOwner,
	} {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("%s review finding %d %s is required and must be single-line", label, index, field)
		}
	}
	switch finding.Severity {
	case "blocker", "advisory":
	default:
		return fmt.Errorf("unsupported %s review severity %q", label, finding.Severity)
	}
	switch finding.Category {
	case "correctness", "security", "specification", "documented_standards", "taste", "scope":
	default:
		return fmt.Errorf("unsupported %s review category %q", label, finding.Category)
	}
	return nil
}

// validateStoreRelativePath validates one persisted repository-relative path.
func validateStoreRelativePath(value string) error {
	if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\x00\r\n\\") || filepath.IsAbs(value) {
		return errors.New("path must be a safe repository-relative path")
	}
	clean := filepath.ToSlash(filepath.Clean(value))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean == ".git" || strings.HasPrefix(clean, ".git/") {
		return errors.New("path must remain inside the repository checkout")
	}
	return nil
}

// isLowerHex reports whether value is a lowercase hexadecimal digest.
func isLowerHex(value string) bool {
	for _, character := range value {
		if (character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') {
			continue
		}
		return false
	}
	return true
}

// validatePendingQuestions checks the bounded workflow projection before it is
// serialized into the operational store.
func validatePendingQuestions(values []PendingQuestion) error {
	if len(values) > maxPendingQuestions {
		return fmt.Errorf("run pending questions exceed %d entries", maxPendingQuestions)
	}
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		if !safeQuestionIdentifier(value.ID) {
			return fmt.Errorf("run pending question %d has an unsafe identifier", index)
		}
		if _, exists := seen[value.ID]; exists {
			return fmt.Errorf("run pending question %q is duplicated", value.ID)
		}
		seen[value.ID] = struct{}{}
		if strings.TrimSpace(value.Prompt) == "" {
			return fmt.Errorf("run pending question %d has an empty prompt", index)
		}
		if strings.ContainsAny(value.Prompt, "\x00\r\n") {
			return fmt.Errorf("run pending question %q contains a control character", value.ID)
		}
	}
	return nil
}

// safeQuestionIdentifier reports whether a pending-question ID is safe to
// reference from the structured GitHub command surface.
func safeQuestionIdentifier(value string) bool {
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

// sliceJSON serializes a slice projection as a stable JSON array. A nil slice
// intentionally remains the empty array used by the operational store.
func sliceJSON[T any](values []T) (string, error) {
	if values == nil {
		return "[]", nil
	}
	data, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// nullableJSON serializes an optional projection while preserving the empty
// string representation used for an absent value in the operational store.
func nullableJSON[T any](value *T) (string, error) {
	if value == nil {
		return "", nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// runValues returns the ordered SQL values shared by normal and revision-safe
// run persistence, including errors from every JSON projection.
func runValues(run Run) ([]any, error) {
	testHandoff, err := nullableJSON(run.TestHandoff)
	if err != nil {
		return nil, fmt.Errorf("encode test handoff: %w", err)
	}
	testRevisionHistory, err := sliceJSON(run.TestRevisionHistory)
	if err != nil {
		return nil, fmt.Errorf("encode test revision history: %w", err)
	}
	testObjection, err := nullableJSON(run.TestObjection)
	if err != nil {
		return nil, fmt.Errorf("encode test objection: %w", err)
	}
	testRevisionBaseChangedPaths, err := sliceJSON(run.TestRevisionBaseChangedPaths)
	if err != nil {
		return nil, fmt.Errorf("encode test revision base changed paths: %w", err)
	}
	roleHandoff, err := nullableJSON(run.RoleHandoff)
	if err != nil {
		return nil, fmt.Errorf("encode role handoff: %w", err)
	}
	specificationReview, err := nullableJSON(run.SpecificationReview)
	if err != nil {
		return nil, fmt.Errorf("encode specification review: %w", err)
	}
	standardsReview, err := nullableJSON(run.StandardsReview)
	if err != nil {
		return nil, fmt.Errorf("encode standards review: %w", err)
	}
	pullRequestSummary, err := nullableJSON(run.PullRequestSummary)
	if err != nil {
		return nil, fmt.Errorf("encode pull-request summary: %w", err)
	}
	reviewRepairHistory, err := sliceJSON(run.ReviewRepairHistory)
	if err != nil {
		return nil, fmt.Errorf("encode review repair history: %w", err)
	}
	reviewRepairPacket, err := nullableJSON(run.ReviewRepairPacket)
	if err != nil {
		return nil, fmt.Errorf("encode review repair packet: %w", err)
	}
	activeInvocationIDs, err := sliceJSON(run.ActiveInvocationIDs)
	if err != nil {
		return nil, fmt.Errorf("encode active invocation ids: %w", err)
	}
	testExemption, err := nullableJSON(run.TestExemption)
	if err != nil {
		return nil, fmt.Errorf("encode test exemption: %w", err)
	}
	protectedTestPaths, err := sliceJSON(run.ProtectedTestPaths)
	if err != nil {
		return nil, fmt.Errorf("encode protected test paths: %w", err)
	}
	pendingQuestions, err := sliceJSON(run.PendingQuestions)
	if err != nil {
		return nil, fmt.Errorf("encode pending questions: %w", err)
	}
	trackerRepository, err := nullableJSON(run.TrackerRepository)
	if err != nil {
		return nil, fmt.Errorf("encode tracker repository: %w", err)
	}
	return []any{
		run.ID,
		run.RepositoryPath,
		run.IssueNumber,
		run.Stage,
		run.Status,
		run.Branch,
		run.Worktree,
		run.CheckpointSHA,
		run.BaseCheckpointSHA,
		run.AcceptedImplementationCheckpointSHA,
		run.TestCheckpointSHA,
		testHandoff,
		run.TestInvocationID,
		run.TestRevisionAttempts,
		run.TestRevisionBudget,
		testRevisionHistory,
		testObjection,
		testRevisionBaseChangedPaths,
		roleHandoff,
		specificationReview,
		standardsReview,
		pullRequestSummary,
		run.ReviewRepairAttempts,
		run.ReviewRepairBudget,
		run.ReviewRepairPendingAttempt,
		reviewRepairHistory,
		reviewRepairPacket,
		testExemption,
		protectedTestPaths,
		run.TestStageSkipped,
		activeInvocationIDs,
		run.ImageDigest,
		run.Coordinator,
		run.StatusCommentID,
		run.PullRequestNumber,
		run.PullRequestURL,
		run.MergeCommitSHA,
		run.LifecycleReason,
		run.Revision,
		run.ProcessedCommentID,
		run.ProcessedCommentRevision,
		run.ProcessedReviewID,
		run.ProcessedReviewRevision,
		run.LastCommandName,
		run.LastCommandOutcome,
		run.LastCommandMessage,
		run.HarnessOverride,
		run.CheckRepairAttempts,
		run.CheckRepairBudget,
		run.CheckRepairPendingAttempt,
		run.SpecificationPacket,
		pendingQuestions,
		run.ClarificationCommentID,
		trackerRepository,
		terminalAtValue(run.TerminalAt),
		run.CreatedAt.UTC().Format(runTimestampLayout),
		run.UpdatedAt.UTC().Format(runTimestampLayout),
	}, nil
}

// terminalAtValue serializes an absent terminal timestamp as the empty
// projection used by migrated and active runs.
func terminalAtValue(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(runTimestampLayout)
}

const saveRunStatement = `
		INSERT INTO operational_runs (
			id, repository_path, issue_number, stage, status, branch, worktree,
			checkpoint_sha, base_checkpoint_sha, accepted_implementation_checkpoint_sha, test_checkpoint_sha,
			test_handoff, test_invocation_id, test_revision_attempts,
			test_revision_budget, test_revision_history, test_objection,
			test_revision_base_changed_paths, implementation_handoff,
			specification_review, standards_review, pull_request_summary,
			review_repair_attempts, review_repair_budget,
			review_repair_pending_attempt, review_repair_history, review_repair_packet,
			test_exemption, protected_test_paths, test_stage_skipped,
			active_invocation_ids,
			image_digest, coordinator, status_comment_id,
			pull_request_number, pull_request_url, merge_commit_sha, lifecycle_reason,
			revision, processed_comment_id,
			processed_comment_revision, processed_review_id, processed_review_revision,
			last_command_name, last_command_outcome,
			last_command_message, harness_override, check_repair_attempts,
			check_repair_budget, check_repair_pending_attempt, specification_packet,
			pending_questions, clarification_comment_id, tracker_repository,
			terminal_at, created_at, updated_at
		) VALUES (
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?
		)
		ON CONFLICT(id) DO UPDATE SET
			repository_path = excluded.repository_path,
			issue_number = excluded.issue_number,
			stage = excluded.stage,
			status = excluded.status,
			branch = excluded.branch,
			worktree = excluded.worktree,
			checkpoint_sha = excluded.checkpoint_sha,
			base_checkpoint_sha = excluded.base_checkpoint_sha,
			accepted_implementation_checkpoint_sha = excluded.accepted_implementation_checkpoint_sha,
			test_checkpoint_sha = excluded.test_checkpoint_sha,
			test_handoff = excluded.test_handoff,
			test_invocation_id = excluded.test_invocation_id,
			test_revision_attempts = excluded.test_revision_attempts,
			test_revision_budget = excluded.test_revision_budget,
			test_revision_history = excluded.test_revision_history,
			test_objection = excluded.test_objection,
			test_revision_base_changed_paths = excluded.test_revision_base_changed_paths,
			implementation_handoff = excluded.implementation_handoff,
			specification_review = excluded.specification_review,
			standards_review = excluded.standards_review,
			pull_request_summary = excluded.pull_request_summary,
			review_repair_attempts = excluded.review_repair_attempts,
			review_repair_budget = excluded.review_repair_budget,
			review_repair_pending_attempt = excluded.review_repair_pending_attempt,
			review_repair_history = excluded.review_repair_history,
			review_repair_packet = excluded.review_repair_packet,
			test_exemption = excluded.test_exemption,
			protected_test_paths = excluded.protected_test_paths,
			test_stage_skipped = excluded.test_stage_skipped,
			active_invocation_ids = excluded.active_invocation_ids,
			image_digest = excluded.image_digest,
			coordinator = excluded.coordinator,
			status_comment_id = excluded.status_comment_id,
			pull_request_number = excluded.pull_request_number,
			pull_request_url = excluded.pull_request_url,
			merge_commit_sha = excluded.merge_commit_sha,
			lifecycle_reason = excluded.lifecycle_reason,
			revision = excluded.revision,
			processed_comment_id = excluded.processed_comment_id,
			processed_comment_revision = excluded.processed_comment_revision,
			processed_review_id = excluded.processed_review_id,
			processed_review_revision = excluded.processed_review_revision,
			last_command_name = excluded.last_command_name,
			last_command_outcome = excluded.last_command_outcome,
			last_command_message = excluded.last_command_message,
			harness_override = excluded.harness_override,
			check_repair_attempts = excluded.check_repair_attempts,
			check_repair_budget = excluded.check_repair_budget,
			check_repair_pending_attempt = excluded.check_repair_pending_attempt,
			specification_packet = excluded.specification_packet,
			pending_questions = excluded.pending_questions,
			clarification_comment_id = excluded.clarification_comment_id,
			tracker_repository = excluded.tracker_repository,
			terminal_at = excluded.terminal_at,
			updated_at = excluded.updated_at`

const saveRunIfRevisionStatement = `
		UPDATE operational_runs SET
			repository_path = ?, issue_number = ?, stage = ?, status = ?, branch = ?, worktree = ?,
			checkpoint_sha = ?, base_checkpoint_sha = ?, accepted_implementation_checkpoint_sha = ?, test_checkpoint_sha = ?,
			test_handoff = ?, test_invocation_id = ?, test_revision_attempts = ?,
			test_revision_budget = ?, test_revision_history = ?, test_objection = ?,
			test_revision_base_changed_paths = ?, implementation_handoff = ?,
			specification_review = ?, standards_review = ?, pull_request_summary = ?,
			review_repair_attempts = ?, review_repair_budget = ?,
			review_repair_pending_attempt = ?, review_repair_history = ?, review_repair_packet = ?,
			test_exemption = ?, protected_test_paths = ?, test_stage_skipped = ?,
			active_invocation_ids = ?,
			image_digest = ?, coordinator = ?, status_comment_id = ?,
			pull_request_number = ?, pull_request_url = ?, merge_commit_sha = ?, lifecycle_reason = ?,
			revision = ?, processed_comment_id = ?,
			processed_comment_revision = ?, processed_review_id = ?, processed_review_revision = ?,
			last_command_name = ?, last_command_outcome = ?,
			last_command_message = ?, harness_override = ?, check_repair_attempts = ?,
			check_repair_budget = ?, check_repair_pending_attempt = ?, specification_packet = ?,
			pending_questions = ?, clarification_comment_id = ?, tracker_repository = ?,
			terminal_at = ?, created_at = ?, updated_at = ?
		WHERE id = ? AND revision = ?`

// invocationColumns is the canonical order shared by every invocation write,
// read, and scanner. Changing the durable projection requires one edit here.
const invocationColumns = `id, run_id, harness, role, stage, model, reasoning_effort, credential_store_id,
	native_session_id, invocation_directory, result_directory, permitted_paths,
	prompt_version, prompt_craft_source_path, prompt_craft_sha256, status,
	launch_voided, recovery_resume_count, manual_resume_count, review_round_id,
	review_unit_id, created_at, updated_at`

// SaveInvocation validates and upserts one recoverable harness invocation.
func (s *Store) SaveInvocation(ctx context.Context, invocation Invocation) error {
	if invocation.ID == "" {
		return errors.New("invocation id is required")
	}
	if invocation.RunID == "" {
		return errors.New("invocation run id is required")
	}
	if invocation.Harness == "" {
		return errors.New("invocation harness is required")
	}
	if invocation.Role == "" {
		return errors.New("invocation role is required")
	}
	if invocation.Stage == "" {
		return errors.New("invocation stage is required")
	}
	if invocation.Status == "" {
		return errors.New("invocation status is required")
	}
	if invocation.RecoveryResumeCount < 0 {
		return errors.New("invocation recovery resume count must not be negative")
	}
	if invocation.ManualResumeCount < 0 {
		return errors.New("invocation manual resume count must not be negative")
	}
	if (invocation.ReviewRoundID == "") != (invocation.ReviewUnitID == "") {
		return errors.New("invocation review round and unit ids must be supplied together")
	}
	if invocation.ReviewRoundID != "" && (!safeQuestionIdentifier(invocation.ReviewRoundID) || !safeQuestionIdentifier(invocation.ReviewUnitID)) {
		return errors.New("invocation review round and unit ids are unsafe")
	}
	if strings.ContainsAny(invocation.CredentialStoreID, "\x00\r\n") {
		return errors.New("invocation credential store id contains control characters")
	}
	if err := validatePromptCraftIdentity(invocation.PromptCraftSourcePath, invocation.PromptCraftSHA256); err != nil {
		return err
	}
	if invocation.CreatedAt.IsZero() {
		invocation.CreatedAt = time.Now().UTC()
	}
	if invocation.UpdatedAt.IsZero() {
		invocation.UpdatedAt = invocation.CreatedAt
	}
	permittedPathJSON, err := json.Marshal(invocation.PermittedPaths)
	if err != nil {
		return fmt.Errorf("encode invocation permitted paths: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
			INSERT INTO invocations (`+invocationColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			run_id = excluded.run_id,
			harness = excluded.harness,
			role = excluded.role,
			stage = excluded.stage,
			model = excluded.model,
			reasoning_effort = excluded.reasoning_effort,
			credential_store_id = excluded.credential_store_id,
			native_session_id = excluded.native_session_id,
			invocation_directory = excluded.invocation_directory,
			result_directory = excluded.result_directory,
				permitted_paths = excluded.permitted_paths,
				prompt_version = excluded.prompt_version,
				prompt_craft_source_path = excluded.prompt_craft_source_path,
				prompt_craft_sha256 = excluded.prompt_craft_sha256,
			status = excluded.status,
			launch_voided = excluded.launch_voided,
			recovery_resume_count = excluded.recovery_resume_count,
			manual_resume_count = excluded.manual_resume_count,
			review_round_id = excluded.review_round_id,
			review_unit_id = excluded.review_unit_id,
				updated_at = excluded.updated_at`,
		invocation.ID,
		invocation.RunID,
		invocation.Harness,
		invocation.Role,
		invocation.Stage,
		invocation.Model,
		invocation.ReasoningEffort,
		invocation.CredentialStoreID,
		invocation.NativeSessionID,
		invocation.InvocationDirectory,
		invocation.ResultDirectory,
		string(permittedPathJSON),
		invocation.PromptVersion,
		invocation.PromptCraftSourcePath,
		invocation.PromptCraftSHA256,
		invocation.Status,
		invocation.LaunchVoided,
		invocation.RecoveryResumeCount,
		invocation.ManualResumeCount,
		invocation.ReviewRoundID,
		invocation.ReviewUnitID,
		invocation.CreatedAt.UTC().Format(runTimestampLayout),
		invocation.UpdatedAt.UTC().Format(runTimestampLayout),
	)
	if err != nil {
		if isActiveInvocationConflict(err) {
			return fmt.Errorf("save invocation: active invocation already exists for run %q: %w", invocation.RunID, err)
		}
		return fmt.Errorf("save invocation: %w", err)
	}
	return nil
}

// validatePromptCraftIdentity validates the optional source and content digest
// persisted beside an invocation's embedded prompt version.
func validatePromptCraftIdentity(sourcePath, digest string) error {
	if sourcePath == "" && digest == "" {
		return nil
	}
	if sourcePath == "" || digest == "" {
		return errors.New("invocation prompt craft source path and SHA-256 must be supplied together")
	}
	if strings.ContainsAny(sourcePath, "\x00\r\n\\") || filepath.IsAbs(sourcePath) {
		return errors.New("invocation prompt craft source path must be repository-relative")
	}
	if !strings.EqualFold(filepath.Ext(sourcePath), ".md") {
		return errors.New("invocation prompt craft source path must name a Markdown file")
	}
	for _, segment := range strings.Split(filepath.ToSlash(sourcePath), "/") {
		if segment == ".." {
			return errors.New("invocation prompt craft source path must not contain parent traversal segments")
		}
	}
	clean := filepath.ToSlash(filepath.Clean(sourcePath))
	if clean == "." || clean == ".git" || strings.HasPrefix(clean, ".git/") {
		return errors.New("invocation prompt craft source path must remain inside the repository checkout")
	}
	if len(digest) != 64 {
		return errors.New("invocation prompt craft SHA-256 must contain 64 lowercase hexadecimal characters")
	}
	for _, character := range digest {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return errors.New("invocation prompt craft SHA-256 must contain 64 lowercase hexadecimal characters")
		}
	}
	return nil
}

// InvalidateRunResults supersedes invocations and removes downstream checkpoint
// results produced before a new specification packet was accepted. Baseline
// results remain valid because the packet change only revises issue intent and
// clarifications; the frozen repository configuration and checkpoint remain.
// The evaluation summary remains intact because it is content-free history.
func (s *Store) InvalidateRunResults(ctx context.Context, runID string) error {
	return s.invalidateRunResults(ctx, runID, false)
}

// InvalidateAllRunResults supersedes every prior invocation and removes every
// gate result for an authorized specification amendment. Unlike an ordinary
// refresh, an amendment changes the authoritative issue packet, so even the
// prior baseline must be re-established.
func (s *Store) InvalidateAllRunResults(ctx context.Context, runID string) error {
	return s.invalidateRunResults(ctx, runID, true)
}

// invalidateRunResults applies one packet-boundary invalidation policy inside a
// transaction while retaining the separate content-free evaluation summary.
func (s *Store) invalidateRunResults(ctx context.Context, runID string, includeBaseline bool) error {
	if strings.TrimSpace(runID) == "" {
		return errors.New("run id is required for result invalidation")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin run-result invalidation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format(runTimestampLayout)
	if _, err := tx.ExecContext(ctx, `UPDATE invocations
		SET status = ?, updated_at = ?
		WHERE run_id = ? AND status <> ?`, InvocationStatusSuperseded, now, runID, InvocationStatusSuperseded); err != nil {
		return fmt.Errorf("supersede invocations for run %q: %w", runID, err)
	}
	deleteStatement := "DELETE FROM gate_results WHERE run_id = ? AND phase <> ?"
	deleteArguments := []any{runID, GatePhaseBaseline}
	if includeBaseline {
		deleteStatement = "DELETE FROM gate_results WHERE run_id = ?"
		deleteArguments = []any{runID}
	}
	if _, err := tx.ExecContext(ctx, deleteStatement, deleteArguments...); err != nil {
		return fmt.Errorf("invalidate gate results for run %q: %w", runID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM review_unit_results WHERE round_id IN (SELECT id FROM review_rounds WHERE run_id = ?)`, runID); err != nil {
		return fmt.Errorf("invalidate review unit results for run %q: %w", runID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM review_units WHERE round_id IN (SELECT id FROM review_rounds WHERE run_id = ?)`, runID); err != nil {
		return fmt.Errorf("invalidate review units for run %q: %w", runID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM review_rounds WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("invalidate review rounds for run %q: %w", runID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit run-result invalidation: %w", err)
	}
	return nil
}

// SaveRunAndInvalidateResults atomically persists a run state transition and
// invalidates prior invocations and checkpoint results when a specification
// packet revision boundary is crossed. This operation ensures packet updates,
// command watermark advances, and result invalidation commit or roll back
// together, preventing inconsistent persistence when resumption is attempted.
func (s *Store) SaveRunAndInvalidateResults(ctx context.Context, expectedRevision int64, run Run) error {
	return s.saveRunAndInvalidateResults(ctx, expectedRevision, run, false)
}

// SaveRunAndInvalidateAllResults atomically persists an authorized
// specification amendment and removes all results from the superseded packet,
// including its baseline gate result.
func (s *Store) SaveRunAndInvalidateAllResults(ctx context.Context, expectedRevision int64, run Run) error {
	return s.saveRunAndInvalidateResults(ctx, expectedRevision, run, true)
}

// saveRunAndInvalidateResults is the compare-and-set implementation shared by
// ordinary packet refreshes and complete specification amendments.
func (s *Store) saveRunAndInvalidateResults(ctx context.Context, expectedRevision int64, run Run, includeBaseline bool) error {
	run, err := normalizeRun(run)
	if err != nil {
		return err
	}
	if run.Revision <= expectedRevision {
		return fmt.Errorf("%w: got %d, expected greater than %d", ErrRevisionNotAdvanced, run.Revision, expectedRevision)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin atomic run update and result invalidation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Update the run state with revision check
	values, err := runValues(run)
	if err != nil {
		return fmt.Errorf("encode run projections: %w", err)
	}
	result, err := tx.ExecContext(ctx, saveRunIfRevisionStatement, append(values[1:], run.ID, expectedRevision)...)
	if err != nil {
		return fmt.Errorf("save run at revision %d: %w", expectedRevision, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect run revision %d update: %w", expectedRevision, err)
	}
	if changed != 1 {
		return fmt.Errorf("%w: run %q no longer has revision %d", ErrRevisionConflict, run.ID, expectedRevision)
	}

	// Invalidate prior results in the same transaction
	now := time.Now().UTC().Format(runTimestampLayout)
	if _, err := tx.ExecContext(ctx, `UPDATE invocations
		SET status = ?, updated_at = ?
		WHERE run_id = ? AND status <> ?`, InvocationStatusSuperseded, now, run.ID, InvocationStatusSuperseded); err != nil {
		return fmt.Errorf("supersede invocations for run %q: %w", run.ID, err)
	}
	deleteStatement := "DELETE FROM gate_results WHERE run_id = ? AND phase <> ?"
	deleteArguments := []any{run.ID, GatePhaseBaseline}
	if includeBaseline {
		deleteStatement = "DELETE FROM gate_results WHERE run_id = ?"
		deleteArguments = []any{run.ID}
	}
	if _, err := tx.ExecContext(ctx, deleteStatement, deleteArguments...); err != nil {
		return fmt.Errorf("invalidate gate results for run %q: %w", run.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM review_unit_results WHERE round_id IN (SELECT id FROM review_rounds WHERE run_id = ?)`, run.ID); err != nil {
		return fmt.Errorf("invalidate review unit results for run %q: %w", run.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM review_units WHERE round_id IN (SELECT id FROM review_rounds WHERE run_id = ?)`, run.ID); err != nil {
		return fmt.Errorf("invalidate review units for run %q: %w", run.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM review_rounds WHERE run_id = ?`, run.ID); err != nil {
		return fmt.Errorf("invalidate review rounds for run %q: %w", run.ID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit atomic run update and result invalidation: %w", err)
	}
	return nil
}

// Invocation loads one persisted invocation only when both identifiers match.
func (s *Store) Invocation(ctx context.Context, runID, invocationID string) (*Invocation, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+invocationColumns+`
		FROM invocations
		WHERE run_id = ? AND id = ?`, runID, invocationID)
	return scanInvocation(row)
}

// LatestInvocation returns the most recently updated invocation for one run,
// including terminal history needed to resume the implementation session that
// produced a failed checkpoint.
func (s *Store) LatestInvocation(ctx context.Context, runID string) (*Invocation, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, errors.New("invocation run id is required")
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+invocationColumns+`
		FROM invocations
		WHERE run_id = ?
		ORDER BY updated_at DESC, id DESC
		LIMIT 1`, runID)
	return scanInvocation(row)
}

// LatestInvocationByRole returns the newest invocation for one run and role,
// allowing review repair to resume implementation without mistaking a later
// isolated reviewer for the implementation session.
func (s *Store) LatestInvocationByRole(ctx context.Context, runID, role string) (*Invocation, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, errors.New("invocation run id is required")
	}
	if strings.TrimSpace(role) == "" || strings.ContainsAny(role, "\x00\r\n") {
		return nil, errors.New("invocation role is required and must be a single line")
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+invocationColumns+`
		FROM invocations
		WHERE run_id = ? AND role = ?
		ORDER BY updated_at DESC, id DESC
		LIMIT 1`, runID, role)
	return scanInvocation(row)
}

// HasInvocation reports whether any invocation, including terminal history,
// has been persisted for one run.
func (s *Store) HasInvocation(ctx context.Context, runID string) (bool, error) {
	if strings.TrimSpace(runID) == "" {
		return false, errors.New("invocation run id is required")
	}
	var found bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM invocations WHERE run_id = ?)`, runID).Scan(&found); err != nil {
		return false, fmt.Errorf("check invocation history: %w", err)
	}
	return found, nil
}

// ActiveInvocation returns the newest active harness invocation for one run.
// The coordinator uses it to avoid launching duplicate harness sessions after
// a process restart or a repeated operator command.
func (s *Store) ActiveInvocation(ctx context.Context, runID string) (*Invocation, error) {
	if runID == "" {
		return nil, errors.New("invocation run id is required")
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+invocationColumns+`
		FROM invocations
		WHERE run_id = ? AND status = ?
		ORDER BY updated_at DESC
		LIMIT 1`, runID, InvocationStatusActive)
	return scanInvocation(row)
}

// ActiveInvocations returns every currently executing invocation for one run
// in deterministic update order. Review roles may run together; ordinary
// roles remain constrained by the coordinator before they reach this query.
func (s *Store) ActiveInvocations(ctx context.Context, runID string) ([]Invocation, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, errors.New("invocation run id is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+invocationColumns+`
		FROM invocations
		WHERE run_id = ? AND status = ?
		ORDER BY updated_at, id`, runID, InvocationStatusActive)
	if err != nil {
		return nil, fmt.Errorf("list active invocations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]Invocation, 0)
	for rows.Next() {
		invocation, scanErr := scanInvocation(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, *invocation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read active invocations: %w", err)
	}
	return result, nil
}

// scanInvocation decodes one invocation row and its RFC3339 timestamps.
func scanInvocation(row interface{ Scan(...any) error }) (*Invocation, error) {
	var invocation Invocation
	var permittedPathJSON, createdAt, updatedAt string
	if err := row.Scan(
		&invocation.ID,
		&invocation.RunID,
		&invocation.Harness,
		&invocation.Role,
		&invocation.Stage,
		&invocation.Model,
		&invocation.ReasoningEffort,
		&invocation.CredentialStoreID,
		&invocation.NativeSessionID,
		&invocation.InvocationDirectory,
		&invocation.ResultDirectory,
		&permittedPathJSON,
		&invocation.PromptVersion,
		&invocation.PromptCraftSourcePath,
		&invocation.PromptCraftSHA256,
		&invocation.Status,
		&invocation.LaunchVoided,
		&invocation.RecoveryResumeCount,
		&invocation.ManualResumeCount,
		&invocation.ReviewRoundID,
		&invocation.ReviewUnitID,
		&createdAt,
		&updatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read invocation row: %w", err)
	}
	if permittedPathJSON != "" {
		if err := json.Unmarshal([]byte(permittedPathJSON), &invocation.PermittedPaths); err != nil {
			return nil, fmt.Errorf("decode invocation permitted paths: %w", err)
		}
	}
	var err error
	invocation.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse invocation created_at: %w", err)
	}
	invocation.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return nil, fmt.Errorf("parse invocation updated_at: %w", err)
	}
	return &invocation, nil
}

// validateGateResults validates the complete batch before a transaction is
// opened, keeping invalid input from creating any partial durable state.
func validateGateResults(results []GateResult) error {
	if len(results) == 0 {
		return errors.New("at least one gate result is required")
	}
	for _, result := range results {
		if err := validateGateResult(result); err != nil {
			return err
		}
	}
	return nil
}

// saveGateResults writes one validated batch through the supplied transaction.
func saveGateResults(ctx context.Context, tx *sql.Tx, results []GateResult) error {
	for _, result := range results {
		createdAt := result.CreatedAt
		if createdAt.IsZero() {
			createdAt = time.Now().UTC()
		}
		updatedAt := result.UpdatedAt
		if updatedAt.IsZero() {
			updatedAt = createdAt
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO gate_results (
				run_id, checkpoint_sha, phase, ordinal, gate_name, outcome, status,
				blocking, skip_reason, setup_fingerprint, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(run_id, checkpoint_sha, phase, gate_name) DO UPDATE SET
				ordinal = excluded.ordinal,
				outcome = excluded.outcome,
				status = excluded.status,
				blocking = excluded.blocking,
				skip_reason = excluded.skip_reason,
				setup_fingerprint = excluded.setup_fingerprint,
				updated_at = excluded.updated_at`,
			result.RunID,
			result.CheckpointSHA,
			result.Phase,
			result.Ordinal,
			result.GateName,
			result.Outcome,
			result.Status,
			result.Blocking,
			result.SkipReason,
			result.SetupFingerprint,
			createdAt.UTC().Format(runTimestampLayout),
			updatedAt.UTC().Format(runTimestampLayout),
		); err != nil {
			return fmt.Errorf("save gate result %q: %w", result.GateName, err)
		}
	}
	return nil
}

// SaveGateResults atomically upserts the ordered deterministic results from
// one suite while preserving their exact run, phase, and checkpoint identity.
func (s *Store) SaveGateResults(ctx context.Context, results []GateResult) error {
	if err := validateGateResults(results); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin gate-result save: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := saveGateResults(ctx, tx, results); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit gate-result save: %w", err)
	}
	return nil
}

// BeginGateResultTransaction starts the transaction used to persist gate
// results together with their evaluation execution count.
func (s *Store) BeginGateResultTransaction(ctx context.Context) (GateResultTransaction, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin gate-result save: %w", err)
	}
	return &gateResultTransaction{tx: tx}, nil
}

// gateResultTransaction adapts a SQLite transaction to the durable gate
// persistence seam used by the coordinator.
type gateResultTransaction struct {
	tx *sql.Tx
}

// SaveGateResults persists validated gate results within the open transaction.
func (t *gateResultTransaction) SaveGateResults(ctx context.Context, results []GateResult) error {
	if err := validateGateResults(results); err != nil {
		return err
	}
	return saveGateResults(ctx, t.tx, results)
}

// RecordEvaluationGateRun persists the execution count within the open
// transaction so it commits or rolls back with the gate results.
func (t *gateResultTransaction) RecordEvaluationGateRun(ctx context.Context, runID string, count int) error {
	return recordEvaluationGateRun(ctx, t.tx, runID, count)
}

// Commit commits all writes made through the transaction.
func (t *gateResultTransaction) Commit() error {
	return t.tx.Commit()
}

// Rollback discards all writes made through the transaction.
func (t *gateResultTransaction) Rollback() error {
	return t.tx.Rollback()
}

// GateResults returns results only for the requested exact checkpoint and
// phase, ordered as declared in the frozen repository configuration.
func (s *Store) GateResults(ctx context.Context, runID string, phase GatePhase, checkpointSHA string) ([]GateResult, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, errors.New("gate result run id is required")
	}
	if err := validateGatePhase(phase); err != nil {
		return nil, err
	}
	if strings.TrimSpace(checkpointSHA) == "" {
		return nil, errors.New("gate result checkpoint SHA is required")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT run_id, checkpoint_sha, phase, ordinal, gate_name, outcome, status,
		       blocking, skip_reason, setup_fingerprint, created_at, updated_at
		FROM gate_results
		WHERE run_id = ? AND phase = ? AND checkpoint_sha = ?
		ORDER BY ordinal, gate_name`, runID, phase, checkpointSHA)
	if err != nil {
		return nil, fmt.Errorf("read gate results: %w", err)
	}
	return scanGateResultRows(rows)
}

// RunGateResults returns every retained gate result of one run, ordered by
// phase, checkpoint, and repository declaration order. A run keeps results for
// several checkpoints until a packet change invalidates them.
func (s *Store) RunGateResults(ctx context.Context, runID string) ([]GateResult, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, errors.New("gate result run id is required")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT run_id, checkpoint_sha, phase, ordinal, gate_name, outcome, status,
		       blocking, skip_reason, setup_fingerprint, created_at, updated_at
		FROM gate_results
		WHERE run_id = ?
		ORDER BY phase, checkpoint_sha, ordinal, gate_name`, runID)
	if err != nil {
		return nil, fmt.Errorf("read gate results: %w", err)
	}
	return scanGateResultRows(rows)
}

// scanGateResultRows decodes gate result rows and closes them.
func scanGateResultRows(rows *sql.Rows) ([]GateResult, error) {
	defer func() { _ = rows.Close() }()
	results := []GateResult{}
	for rows.Next() {
		var result GateResult
		var createdAt, updatedAt string
		if err := rows.Scan(
			&result.RunID,
			&result.CheckpointSHA,
			&result.Phase,
			&result.Ordinal,
			&result.GateName,
			&result.Outcome,
			&result.Status,
			&result.Blocking,
			&result.SkipReason,
			&result.SetupFingerprint,
			&createdAt,
			&updatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan gate result: %w", err)
		}
		var parseErr error
		result.CreatedAt, parseErr = time.Parse(time.RFC3339Nano, createdAt)
		if parseErr != nil {
			return nil, fmt.Errorf("parse gate result created_at: %w", parseErr)
		}
		result.UpdatedAt, parseErr = time.Parse(time.RFC3339Nano, updatedAt)
		if parseErr != nil {
			return nil, fmt.Errorf("parse gate result updated_at: %w", parseErr)
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read gate results: %w", err)
	}
	return results, nil
}

// validateGateResult validates the durable result identity and vocabulary.
func validateGateResult(result GateResult) error {
	if strings.TrimSpace(result.RunID) == "" {
		return errors.New("gate result run id is required")
	}
	if !validGateCheckpointSHA(result.CheckpointSHA) {
		return errors.New("gate result checkpoint SHA must contain exactly 40 or 64 lowercase hexadecimal characters")
	}
	if err := validateGatePhase(result.Phase); err != nil {
		return err
	}
	if result.Ordinal < 0 {
		return errors.New("gate result ordinal must not be negative")
	}
	if strings.TrimSpace(result.GateName) == "" {
		return errors.New("gate result gate name is required")
	}
	switch result.Outcome {
	case GateOutcomePassed, GateOutcomeFailed, GateOutcomeError, GateOutcomeSkipped, GateOutcomeSetupFailed:
	default:
		return fmt.Errorf("unsupported gate result outcome %q", result.Outcome)
	}
	if strings.TrimSpace(result.Status) == "" {
		return errors.New("gate result status is required")
	}
	return nil
}

// validGateCheckpointSHA validates the exact commit identity retained by a
// durable gate result without importing the host-side GitHub adapter.
func validGateCheckpointSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') {
			continue
		}
		return false
	}
	return true
}

// validateGatePhase validates the durable baseline/checkpoint phase vocabulary.
func validateGatePhase(phase GatePhase) error {
	if phase != GatePhaseBaseline && phase != GatePhaseCheckpoint {
		return fmt.Errorf("unsupported gate result phase %q", phase)
	}
	return nil
}

// databaseState reports whether the path identifies an existing database file and whether that file is empty.
func databaseState(path string) (bool, bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("inspect operational store: %w", err)
	}
	if info.IsDir() {
		return false, false, fmt.Errorf("operational store path %q is a directory", path)
	}
	return true, info.Size() == 0, nil
}

// openConfiguredDatabase opens the SQLite database at path, restricts its file permissions,
// limits it to a single connection, and applies the store configuration. It closes the
// database and returns an error if setup fails.
func openConfiguredDatabase(ctx context.Context, path string) (*sql.DB, error) {
	database, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open operational store: %w", err)
	}
	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("open operational store: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("protect operational store: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	if err := configure(ctx, database); err != nil {
		_ = database.Close()
		return nil, err
	}
	return database, nil
}

// openReadOnlyDatabase opens a SQLite URI in read-only mode and applies only
// connection settings, never database-changing pragmas. The busy timeout lets
// a reader wait for a coordinator commit instead of failing with SQLITE_BUSY.
func openReadOnlyDatabase(ctx context.Context, path string) (*sql.DB, error) {
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open operational store read-only: %w", err)
	}
	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("open operational store read-only: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	return database, nil
}

// reserveWriteLock takes SQLite's write lock inside tx before the transaction
// reads. With journal_mode=DELETE a transaction that reads first holds a
// shared lock and fails at once with SQLITE_BUSY when it later upgrades past
// another connection's write, such as a UI command append; a reserved lock
// instead makes the other connection wait for its busy timeout. The no-op
// update names one row of table by id and changes nothing.
func reserveWriteLock(ctx context.Context, tx *sql.Tx, table, id string) error {
	_, err := tx.ExecContext(ctx, "UPDATE "+table+" SET id = id WHERE id = ?", id)
	return err
}

// configure applies the SQLite settings required by the operational store.
func configure(ctx context.Context, database *sql.DB) error {
	for _, statement := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = DELETE",
	} {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure SQLite: %w", err)
		}
	}
	return nil
}

// readSchemaVersion reads the schema version recorded in the database metadata.
// It returns an UnversionedDatabaseError if the metadata cannot be read.
func readSchemaVersion(ctx context.Context, database Querier, path string) (int, error) {
	var version int
	if err := database.QueryRowContext(ctx, "SELECT version FROM schema_metadata WHERE singleton = 1").Scan(&version); err != nil {
		return 0, &UnversionedDatabaseError{Path: path, Err: err}
	}
	return version, nil
}

// storeBaselineSchema creates the supported operational store schema directly.
//
//go:embed store_baseline.sql
var storeBaselineSchema string

// migrate creates the baseline in one transaction. Future releases add their
// supported upgrade steps here; pre-baseline stores are refused by the opener.
func migrate(ctx context.Context, database *sql.DB, from int) error {
	if from != 0 {
		return fmt.Errorf("no migration registered from schema version %d", from)
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin store migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, storeBaselineSchema); err != nil {
		return fmt.Errorf("create store baseline schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO schema_metadata(singleton, version) VALUES (1, ?)", CurrentSchemaVersion); err != nil {
		return fmt.Errorf("record store baseline schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit store migration: %w", err)
	}
	return nil
}

// isActiveInvocationConflict recognizes the migration-created unique index
// without depending on a driver-specific SQLite error type.
func isActiveInvocationConflict(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "one_active_invocation_per_review_unit") ||
		strings.Contains(message, "unique constraint failed: invocations.run_id, invocations.role")
}
