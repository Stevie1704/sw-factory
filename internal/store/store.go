package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

// CurrentSchemaVersion is the supported operational-store schema version.
const CurrentSchemaVersion = 35

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

// InvocationStatus describes the lifecycle of one visible harness invocation.
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

// ImplementationHandoff is the legacy name retained for callers and persisted
// rows created before role-owned handoffs were introduced.
type ImplementationHandoff = RoleHandoff

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
}

// SourceEvent returns the maintainer surface and event identity behind a human
// repair packet. A packet written before supervision commands existed recorded
// only its GitHub review identity, so that shape reads as a pull-request review
// event and remains replayable without a migration.
func (p ReviewRepairPacket) SourceEvent() (ReviewRepairSourceEvent, string) {
	kind := ReviewRepairSourceEvent(strings.TrimSpace(string(p.SourceEventKind)))
	if identity := strings.TrimSpace(p.SourceEventID); kind != "" && identity != "" {
		return kind, identity
	}
	if identity := strings.TrimSpace(p.ReviewID); identity != "" {
		return ReviewRepairEventPullRequestReview, identity
	}
	return "", ""
}

// ReviewResult is one review role's latest exact-checkpoint result. A result
// is stale as soon as the run checkpoint changes.
type ReviewResult struct {
	// CheckpointSHA identifies the exact commit reviewed.
	CheckpointSHA string `json:"checkpoint_sha"`
	// Outcome identifies whether the reviewer completed its assignment. An empty
	// value is treated as completed for rows written before this field existed.
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

// ReviewRepairSourceEvent identifies the maintainer surface that produced one
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
	// Source identifies who requested the repair. It is empty for packets
	// written before human-requested repair existed, which means factory.
	Source ReviewRepairSource `json:"source,omitempty"`
	// ReviewID is the GitHub review identity of a human repair packet written
	// before supervision commands could produce one. It is empty for a factory
	// packet and for a command-sourced packet; SourceEvent reads it so a packet
	// persisted by an earlier coordinator stays replayable.
	ReviewID string `json:"review_id,omitempty"`
	// SourceEventKind identifies which maintainer surface produced a human
	// packet. It is empty for a factory packet.
	SourceEventKind ReviewRepairSourceEvent `json:"source_event_kind,omitempty"`
	// SourceEventID is the identity of that surface's event, and is what makes
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

// ProtectedTestPath records the content identity implementation must preserve.
type ProtectedTestPath struct {
	// Path is the repository-relative protected test path.
	Path string `json:"path"`
	// SHA256 is the content hash recorded at the test checkpoint.
	SHA256 string `json:"sha256"`
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
	// It remains stable while terminal notifications or status projections are
	// retried, and is cleared when a terminal run is explicitly reopened.
	TerminalAt time.Time
	// LifecycleNotificationSent records successful cmux notification delivery.
	LifecycleNotificationSent bool
	// ReadyNotificationSent records successful PR-readiness notification
	// delivery for the current ready transition.
	ReadyNotificationSent bool
	// Revision is the monotonic coordinator revision used by command replay
	// prevention and persisted supervision updates.
	Revision int64
	// ProcessedCommentID is the highest processed GitHub comment watermark for
	// this run. Claim initializes it to the latest pre-claim issue comment so
	// historical commands cannot execute against the new run.
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
	// ClarificationNotificationSent records successful cmux notification delivery
	// for the current pending clarification set.
	ClarificationNotificationSent bool
	CreatedAt                     time.Time
	UpdatedAt                     time.Time
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
	// WorkspaceID is the opaque terminal workspace handle.
	WorkspaceID string
	// StatusSurfaceID is the opaque supervision status surface handle.
	StatusSurfaceID string
	// RoleSurfaceID is the opaque surface handle owned by this invocation's role.
	RoleSurfaceID string
	// ImplementationSurfaceID is the legacy opaque implementation surface handle.
	// New code should use RoleSurfaceID; it remains populated for operational-store
	// compatibility with runs created before role-owned surfaces were introduced.
	ImplementationSurfaceID string
	// ChecksSurfaceID is the opaque deterministic-check surface handle.
	ChecksSurfaceID string
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
	// AttachRequired blocks workflow progression until an operator has attached
	// the manually resumed native session through the factory attach command.
	AttachRequired bool
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

// Open opens the operational store at path, creating or migrating its database as needed.
// Open opens or creates an operational store at path, applying supported schema migrations as needed.
// It requires a private parent directory and rejects empty databases, unsupported schema versions,
// and databases with missing or unreadable schema metadata. Existing databases are backed up before migration.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("operational store path is required")
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve operational store path: %w", err)
	}
	storeDirectory := filepath.Dir(absolutePath)
	if err := os.MkdirAll(storeDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create operational store directory: %w", err)
	}
	if info, err := os.Stat(storeDirectory); err != nil {
		return nil, fmt.Errorf("inspect operational store directory: %w", err)
	} else if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("operational store directory %q is not private; expected no group or other permissions", storeDirectory)
	}
	existed, empty, err := databaseState(absolutePath)
	if err != nil {
		return nil, err
	}
	if existed && empty {
		return nil, &UnversionedDatabaseError{Path: absolutePath, Err: errors.New("database file is empty")}
	}
	database, err := openConfiguredDatabase(ctx, absolutePath)
	if err != nil {
		return nil, err
	}
	closeOnError := true
	defer func() {
		if closeOnError && database != nil {
			_ = database.Close()
		}
	}()
	if !existed {
		if err := initializeMetadata(ctx, database); err != nil {
			return nil, err
		}
	}
	version, err := readSchemaVersion(ctx, database, absolutePath)
	if err != nil {
		return nil, err
	}
	if version > CurrentSchemaVersion {
		return nil, &UnknownSchemaVersionError{Version: version, Supported: CurrentSchemaVersion}
	}
	if version < CurrentSchemaVersion {
		if existed {
			if err := database.Close(); err != nil {
				return nil, fmt.Errorf("close store before migration: %w", err)
			}
			if _, err := backupDatabase(absolutePath); err != nil {
				return nil, err
			}
			database, err = openConfiguredDatabase(ctx, absolutePath)
			if err != nil {
				return nil, fmt.Errorf("reopen operational store for migration: %w", err)
			}
		}
		if err := migrate(ctx, database, version); err != nil {
			return nil, err
		}
		version = CurrentSchemaVersion
	}
	closeOnError = false
	return &Store{db: database, path: absolutePath, schemaVersion: version}, nil
}

// OpenReadOnly opens an existing, current operational store without creating
// directories, changing permissions, initializing metadata, migrating schema,
// or creating a backup. It is the only store entry point used by diagnosis.
func OpenReadOnly(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("operational store path is required")
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve operational store path: %w", err)
	}
	storeDirectory := filepath.Dir(absolutePath)
	if info, err := os.Stat(storeDirectory); err != nil {
		return nil, fmt.Errorf("inspect operational store directory: %w", err)
	} else if !info.IsDir() {
		return nil, errors.New("operational store parent is not a directory")
	} else if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("operational store directory is not private")
	}
	info, err := os.Lstat(absolutePath)
	if err != nil {
		return nil, fmt.Errorf("inspect operational store: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("operational store is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o400 == 0 {
		return nil, errors.New("operational store file is not private and owner-readable")
	}
	if info.Size() == 0 {
		return nil, &UnversionedDatabaseError{Path: absolutePath, Err: errors.New("database file is empty")}
	}
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
		return nil, errors.New("operational store schema requires migration")
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
		SELECT id, repository_path, issue_number, stage, status, branch, worktree,
		       checkpoint_sha, base_checkpoint_sha, accepted_implementation_checkpoint_sha, test_checkpoint_sha,
		       test_handoff, test_invocation_id, test_revision_attempts,
		       test_revision_budget, test_revision_history, test_objection,
		       test_revision_base_changed_paths,
		       implementation_handoff, specification_review, standards_review,
		       review_repair_attempts, review_repair_budget,
		       review_repair_pending_attempt, review_repair_history, review_repair_packet,
		       test_exemption, protected_test_paths, test_stage_skipped,
			active_invocation_ids,
		       image_digest, coordinator, status_comment_id,
		       pull_request_number, pull_request_url, merge_commit_sha,
		       lifecycle_reason, lifecycle_notification_sent, ready_notification_sent,
		       revision, processed_comment_id, processed_comment_revision,
		       processed_review_id, processed_review_revision,
		       last_command_name, last_command_outcome, last_command_message,
		       harness_override, check_repair_attempts, check_repair_budget,
		       check_repair_pending_attempt,
		       specification_packet, pending_questions, clarification_comment_id,
		       clarification_notification_sent, terminal_at, created_at, updated_at
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
		SELECT id, repository_path, issue_number, stage, status, branch, worktree,
		       checkpoint_sha, base_checkpoint_sha, accepted_implementation_checkpoint_sha, test_checkpoint_sha,
		       test_handoff, test_invocation_id, test_revision_attempts,
		       test_revision_budget, test_revision_history, test_objection,
		       test_revision_base_changed_paths,
		       implementation_handoff, specification_review, standards_review,
		       review_repair_attempts, review_repair_budget,
		       review_repair_pending_attempt, review_repair_history, review_repair_packet,
		       test_exemption, protected_test_paths, test_stage_skipped,
		       active_invocation_ids,
		       image_digest, coordinator, status_comment_id,
		       pull_request_number, pull_request_url, merge_commit_sha,
		       lifecycle_reason, lifecycle_notification_sent, ready_notification_sent,
		       revision, processed_comment_id, processed_comment_revision,
		       processed_review_id, processed_review_revision,
		       last_command_name, last_command_outcome, last_command_message,
		       harness_override, check_repair_attempts, check_repair_budget,
		       check_repair_pending_attempt,
		       specification_packet, pending_questions, clarification_comment_id,
		       clarification_notification_sent, terminal_at, created_at, updated_at
		FROM operational_runs
		ORDER BY updated_at DESC
		LIMIT 1`)
	return scanRun(row)
}

// scanRun decodes one operational run row and its RFC3339 timestamps.
func scanRun(row *sql.Row) (*Run, error) {
	var run Run
	var baseCheckpointSHA, acceptedImplementationCheckpointSHA, testCheckpointSHA string
	var testHandoffJSON, roleHandoffJSON, specificationReviewJSON, standardsReviewJSON string
	var reviewRepairHistoryJSON, reviewRepairPacketJSON string
	var testRevisionHistoryJSON, testObjectionJSON, testRevisionBaseChangedPathsJSON string
	var testExemptionJSON, protectedTestPathsJSON string
	var activeInvocationIDsJSON string
	var pendingQuestionsJSON, clarificationCommentID, terminalAt, createdAt, updatedAt string
	var clarificationNotificationSent bool
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
		&run.LifecycleNotificationSent,
		&run.ReadyNotificationSent,
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
		&clarificationNotificationSent,
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
	run.ClarificationNotificationSent = clarificationNotificationSent
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

// pendingEffectIdentity keeps a conflict error useful even if a malformed
// legacy row is ever encountered.
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
		case "", ReviewRepairSourceFactory:
			if packet.Attempt < 1 || packet.Attempt > run.ReviewRepairBudget || packet.ReviewID != "" || strings.TrimSpace(packet.SourceEventID) != "" {
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
	if outcome == "" {
		// Rows written before incomplete review outcomes were represented have
		// only a checkpoint and findings, which is the completed shape.
		outcome = "completed"
	}
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
	if len(review.Summary) > 4000 || strings.ContainsAny(review.Summary, "\x00\r\n") {
		return fmt.Errorf("%s review summary is invalid", label)
	}
	if len(review.Questions) > 32 {
		return fmt.Errorf("%s review questions exceed 32 entries", label)
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
	if len(review.Evidence) > 32 {
		return fmt.Errorf("%s review evidence exceeds 32 entries", label)
	}
	for index, evidence := range review.Evidence {
		if strings.TrimSpace(evidence.Kind) == "" || strings.TrimSpace(evidence.Detail) == "" || strings.ContainsAny(evidence.Kind+evidence.Detail, "\x00\r\n") {
			return fmt.Errorf("%s review evidence %d is invalid", label, index)
		}
	}
	if len(review.Findings) > 64 {
		return fmt.Errorf("%s review findings exceed 64 entries", label)
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
		run.LifecycleNotificationSent,
		run.ReadyNotificationSent,
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
		run.ClarificationNotificationSent,
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
			specification_review, standards_review,
			review_repair_attempts, review_repair_budget,
			review_repair_pending_attempt, review_repair_history, review_repair_packet,
			test_exemption, protected_test_paths, test_stage_skipped,
			active_invocation_ids,
			image_digest, coordinator, status_comment_id,
			pull_request_number, pull_request_url, merge_commit_sha, lifecycle_reason,
			lifecycle_notification_sent, ready_notification_sent,
			revision, processed_comment_id,
			processed_comment_revision, processed_review_id, processed_review_revision,
			last_command_name, last_command_outcome,
			last_command_message, harness_override, check_repair_attempts,
			check_repair_budget, check_repair_pending_attempt, specification_packet,
			pending_questions, clarification_comment_id,
			clarification_notification_sent, terminal_at, created_at, updated_at
		) VALUES (
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?
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
			lifecycle_notification_sent = excluded.lifecycle_notification_sent,
			ready_notification_sent = excluded.ready_notification_sent,
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
			clarification_notification_sent = excluded.clarification_notification_sent,
			terminal_at = excluded.terminal_at,
			updated_at = excluded.updated_at`

const saveRunIfRevisionStatement = `
		UPDATE operational_runs SET
			repository_path = ?, issue_number = ?, stage = ?, status = ?, branch = ?, worktree = ?,
			checkpoint_sha = ?, base_checkpoint_sha = ?, accepted_implementation_checkpoint_sha = ?, test_checkpoint_sha = ?,
			test_handoff = ?, test_invocation_id = ?, test_revision_attempts = ?,
			test_revision_budget = ?, test_revision_history = ?, test_objection = ?,
			test_revision_base_changed_paths = ?, implementation_handoff = ?,
			specification_review = ?, standards_review = ?,
			review_repair_attempts = ?, review_repair_budget = ?,
			review_repair_pending_attempt = ?, review_repair_history = ?, review_repair_packet = ?,
			test_exemption = ?, protected_test_paths = ?, test_stage_skipped = ?,
			active_invocation_ids = ?,
			image_digest = ?, coordinator = ?, status_comment_id = ?,
			pull_request_number = ?, pull_request_url = ?, merge_commit_sha = ?, lifecycle_reason = ?,
			lifecycle_notification_sent = ?, ready_notification_sent = ?,
			revision = ?, processed_comment_id = ?,
			processed_comment_revision = ?, processed_review_id = ?, processed_review_revision = ?,
			last_command_name = ?, last_command_outcome = ?,
			last_command_message = ?, harness_override = ?, check_repair_attempts = ?,
			check_repair_budget = ?, check_repair_pending_attempt = ?, specification_packet = ?,
			pending_questions = ?, clarification_comment_id = ?,
			clarification_notification_sent = ?, terminal_at = ?, created_at = ?, updated_at = ?
		WHERE id = ? AND revision = ?`

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
	if invocation.RoleSurfaceID != "" && invocation.ImplementationSurfaceID != "" && invocation.RoleSurfaceID != invocation.ImplementationSurfaceID {
		return errors.New("invocation role surface id conflicts with legacy implementation surface id")
	}
	roleSurfaceID := invocation.RoleSurfaceID
	if roleSurfaceID == "" {
		roleSurfaceID = invocation.ImplementationSurfaceID
	}
	implementationSurfaceID := invocation.ImplementationSurfaceID
	if implementationSurfaceID == "" {
		implementationSurfaceID = roleSurfaceID
	}
	permittedPathJSON, err := json.Marshal(invocation.PermittedPaths)
	if err != nil {
		return fmt.Errorf("encode invocation permitted paths: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
			INSERT INTO invocations (
				id, run_id, harness, role, stage, model, reasoning_effort, credential_store_id,
				native_session_id, workspace_id, status_surface_id,
				role_surface_id, implementation_surface_id, checks_surface_id, invocation_directory,
				result_directory, permitted_paths, prompt_version, prompt_craft_source_path, prompt_craft_sha256, status, launch_voided, recovery_resume_count, attach_required, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			run_id = excluded.run_id,
			harness = excluded.harness,
			role = excluded.role,
			stage = excluded.stage,
			model = excluded.model,
			reasoning_effort = excluded.reasoning_effort,
			credential_store_id = excluded.credential_store_id,
			native_session_id = excluded.native_session_id,
				workspace_id = excluded.workspace_id,
				status_surface_id = excluded.status_surface_id,
				role_surface_id = excluded.role_surface_id,
				implementation_surface_id = excluded.implementation_surface_id,
			checks_surface_id = excluded.checks_surface_id,
			invocation_directory = excluded.invocation_directory,
			result_directory = excluded.result_directory,
				permitted_paths = excluded.permitted_paths,
				prompt_version = excluded.prompt_version,
				prompt_craft_source_path = excluded.prompt_craft_source_path,
				prompt_craft_sha256 = excluded.prompt_craft_sha256,
				status = excluded.status,
				launch_voided = excluded.launch_voided,
				recovery_resume_count = excluded.recovery_resume_count,
				attach_required = excluded.attach_required,
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
		invocation.WorkspaceID,
		invocation.StatusSurfaceID,
		roleSurfaceID,
		implementationSurfaceID,
		invocation.ChecksSurfaceID,
		invocation.InvocationDirectory,
		invocation.ResultDirectory,
		string(permittedPathJSON),
		invocation.PromptVersion,
		invocation.PromptCraftSourcePath,
		invocation.PromptCraftSHA256,
		invocation.Status,
		invocation.LaunchVoided,
		invocation.RecoveryResumeCount,
		invocation.AttachRequired,
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

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit atomic run update and result invalidation: %w", err)
	}
	return nil
}

// Invocation loads one persisted invocation only when both identifiers match.
func (s *Store) Invocation(ctx context.Context, runID, invocationID string) (*Invocation, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, run_id, harness, role, stage, model, reasoning_effort, credential_store_id,
		       native_session_id, workspace_id, status_surface_id,
		       role_surface_id, implementation_surface_id, checks_surface_id, invocation_directory,
		       result_directory, permitted_paths, prompt_version, prompt_craft_source_path, prompt_craft_sha256, status, launch_voided, recovery_resume_count, attach_required, created_at, updated_at
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
	row := s.db.QueryRowContext(ctx, `
		SELECT id, run_id, harness, role, stage, model, reasoning_effort, credential_store_id,
		       native_session_id, workspace_id, status_surface_id,
		       role_surface_id, implementation_surface_id, checks_surface_id, invocation_directory,
		       result_directory, permitted_paths, prompt_version, prompt_craft_source_path, prompt_craft_sha256, status, launch_voided, recovery_resume_count, attach_required, created_at, updated_at
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
	row := s.db.QueryRowContext(ctx, `
		SELECT id, run_id, harness, role, stage, model, reasoning_effort, credential_store_id,
		       native_session_id, workspace_id, status_surface_id,
		       role_surface_id, implementation_surface_id, checks_surface_id, invocation_directory,
		       result_directory, permitted_paths, prompt_version, prompt_craft_source_path, prompt_craft_sha256, status, launch_voided, recovery_resume_count, attach_required, created_at, updated_at
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

// ActiveInvocation returns the newest active visible invocation for one run.
// The coordinator uses it to avoid launching duplicate harness sessions after
// a process restart or a repeated operator command.
func (s *Store) ActiveInvocation(ctx context.Context, runID string) (*Invocation, error) {
	if runID == "" {
		return nil, errors.New("invocation run id is required")
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id, run_id, harness, role, stage, model, reasoning_effort, credential_store_id,
		       native_session_id, workspace_id, status_surface_id,
		       role_surface_id, implementation_surface_id, checks_surface_id, invocation_directory,
		       result_directory, permitted_paths, prompt_version, prompt_craft_source_path, prompt_craft_sha256, status, launch_voided, recovery_resume_count, attach_required, created_at, updated_at
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
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, harness, role, stage, model, reasoning_effort, credential_store_id,
		       native_session_id, workspace_id, status_surface_id,
		       role_surface_id, implementation_surface_id, checks_surface_id, invocation_directory,
		       result_directory, permitted_paths, prompt_version, prompt_craft_source_path, prompt_craft_sha256, status, launch_voided, recovery_resume_count, attach_required, created_at, updated_at
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
		&invocation.WorkspaceID,
		&invocation.StatusSurfaceID,
		&invocation.RoleSurfaceID,
		&invocation.ImplementationSurfaceID,
		&invocation.ChecksSurfaceID,
		&invocation.InvocationDirectory,
		&invocation.ResultDirectory,
		&permittedPathJSON,
		&invocation.PromptVersion,
		&invocation.PromptCraftSourcePath,
		&invocation.PromptCraftSHA256,
		&invocation.Status,
		&invocation.LaunchVoided,
		&invocation.RecoveryResumeCount,
		&invocation.AttachRequired,
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
	if invocation.RoleSurfaceID == "" {
		invocation.RoleSurfaceID = invocation.ImplementationSurfaceID
	}
	if invocation.ImplementationSurfaceID == "" {
		invocation.ImplementationSurfaceID = invocation.RoleSurfaceID
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

// ClaimLifecycleNotification atomically claims the right to send one terminal
// notification, returning true if the claim succeeded (caller should send),
// false if the claim already exists (notification already sent or being sent).
func (s *Store) ClaimLifecycleNotification(ctx context.Context, runID string, terminalStatus Status) (bool, error) {
	if runID == "" {
		return false, errors.New("run id is required for lifecycle notification claim")
	}
	if !IsTerminalStatus(terminalStatus) {
		return false, fmt.Errorf("cannot claim lifecycle notification for non-terminal status %q", terminalStatus)
	}
	now := time.Now().UTC().Format(runTimestampLayout)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO lifecycle_notifications (run_id, terminal_status, created_at)
		VALUES (?, ?, ?)`, runID, terminalStatus, now)
	if err != nil {
		if isLifecycleNotificationClaimConflict(err) {
			return false, nil
		}
		return false, fmt.Errorf("claim lifecycle notification for run %q status %q: %w", runID, terminalStatus, err)
	}
	return true, nil
}

// ReleaseLifecycleNotification removes a notification claim, allowing a future
// retry to attempt delivery again. This is called when notification delivery
// fails after the claim was successfully recorded.
func (s *Store) ReleaseLifecycleNotification(ctx context.Context, runID string, terminalStatus Status) error {
	if runID == "" {
		return errors.New("run id is required for lifecycle notification release")
	}
	if !IsTerminalStatus(terminalStatus) {
		return fmt.Errorf("cannot release lifecycle notification for non-terminal status %q", terminalStatus)
	}
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM lifecycle_notifications
		WHERE run_id = ? AND terminal_status = ?`, runID, terminalStatus)
	if err != nil {
		return fmt.Errorf("release lifecycle notification claim for run %q status %q: %w", runID, terminalStatus, err)
	}
	return nil
}

// isLifecycleNotificationClaimConflict recognizes the primary key violation
// without depending on a driver-specific SQLite error type.
func isLifecycleNotificationClaimConflict(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "primary key") || strings.Contains(message, "unique constraint failed: lifecycle_notifications")
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
// connection-pool settings, never database-changing pragmas.
func openReadOnlyDatabase(ctx context.Context, path string) (*sql.DB, error) {
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
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

// initializeMetadata creates the schema metadata table and initializes its version to zero.
func initializeMetadata(ctx context.Context, database *sql.DB) error {
	if _, err := database.ExecContext(ctx, `CREATE TABLE schema_metadata (
		singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
		version INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema metadata: %w", err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO schema_metadata(singleton, version) VALUES (1, 0)`); err != nil {
		return fmt.Errorf("initialize schema metadata: %w", err)
	}
	return nil
}

// readSchemaVersion reads the schema version recorded in the database metadata.
// It returns an UnversionedDatabaseError if the metadata cannot be read.
func readSchemaVersion(ctx context.Context, database *sql.DB, path string) (int, error) {
	var version int
	if err := database.QueryRowContext(ctx, "SELECT version FROM schema_metadata WHERE singleton = 1").Scan(&version); err != nil {
		return 0, &UnversionedDatabaseError{Path: path, Err: err}
	}
	return version, nil
}

// migrate applies supported schema migrations from the specified version through the current schema version in a single transaction. It records each completed migration and leaves the database unchanged if a migration fails.
func migrate(ctx context.Context, database *sql.DB, from int) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin store migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	version := from
	for version < CurrentSchemaVersion {
		switch version + 1 {
		case 1:
			if _, err := tx.ExecContext(ctx, `CREATE TABLE operational_runs (
				id TEXT PRIMARY KEY,
				repository_path TEXT NOT NULL,
				issue_number INTEGER NOT NULL,
				stage TEXT NOT NULL,
				status TEXT NOT NULL,
				branch TEXT NOT NULL DEFAULT '',
				worktree TEXT NOT NULL DEFAULT '',
				checkpoint_sha TEXT NOT NULL DEFAULT '',
				image_digest TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`); err != nil {
				return fmt.Errorf("apply store migration 1: %w", err)
			}
		case 2:
			for _, statement := range []string{
				"ALTER TABLE operational_runs ADD COLUMN coordinator TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN status_comment_id TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN specification_packet TEXT NOT NULL DEFAULT ''",
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("apply store migration 2: %w", err)
				}
			}
		case 3:
			if err := reconcileDuplicateRuns(ctx, tx); err != nil {
				return fmt.Errorf("reconcile runs before store migration 3: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX one_active_run_per_repository
				ON operational_runs (repository_path)
				WHERE status NOT IN ('complete', 'cancelled', 'failed')`); err != nil {
				return fmt.Errorf("apply store migration 3: %w", err)
			}
		case 4:
			if _, err := tx.ExecContext(ctx, `CREATE TABLE invocations (
					id TEXT PRIMARY KEY,
					run_id TEXT NOT NULL,
					harness TEXT NOT NULL,
					role TEXT NOT NULL,
					stage TEXT NOT NULL,
					model TEXT NOT NULL DEFAULT '',
					reasoning_effort TEXT NOT NULL DEFAULT '',
					native_session_id TEXT NOT NULL DEFAULT '',
					workspace_id TEXT NOT NULL DEFAULT '',
					status_surface_id TEXT NOT NULL DEFAULT '',
					implementation_surface_id TEXT NOT NULL DEFAULT '',
					checks_surface_id TEXT NOT NULL DEFAULT '',
					invocation_directory TEXT NOT NULL DEFAULT '',
					result_directory TEXT NOT NULL DEFAULT '',
					prompt_version TEXT NOT NULL DEFAULT '',
					status TEXT NOT NULL,
					created_at TEXT NOT NULL,
					updated_at TEXT NOT NULL
				)`); err != nil {
				return fmt.Errorf("apply store migration 4: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `CREATE INDEX invocations_by_run
					ON invocations (run_id, updated_at)`); err != nil {
				return fmt.Errorf("apply store migration 4 index: %w", err)
			}
		case 5:
			if _, err := tx.ExecContext(ctx, `ALTER TABLE invocations ADD COLUMN permitted_paths TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("apply store migration 5: %w", err)
			}
		case 6:
			if err := reconcileDuplicateInvocations(ctx, tx); err != nil {
				return fmt.Errorf("reconcile invocations before store migration 6: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX one_active_invocation_per_run
				ON invocations (run_id)
				WHERE status = 'active'`); err != nil {
				return fmt.Errorf("apply store migration 6: %w", err)
			}
		case 7:
			for _, statement := range []string{
				"ALTER TABLE operational_runs ADD COLUMN pull_request_number INTEGER NOT NULL DEFAULT 0",
				"ALTER TABLE operational_runs ADD COLUMN pull_request_url TEXT NOT NULL DEFAULT ''",
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("apply store migration 7: %w", err)
				}
			}
		case 8:
			for _, statement := range []string{
				"ALTER TABLE operational_runs ADD COLUMN revision INTEGER NOT NULL DEFAULT 0",
				"ALTER TABLE operational_runs ADD COLUMN processed_comment_id TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN processed_comment_revision INTEGER NOT NULL DEFAULT 0",
				"ALTER TABLE operational_runs ADD COLUMN last_command_name TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN last_command_outcome TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN last_command_message TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN harness_override TEXT NOT NULL DEFAULT ''",
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("apply store migration 8: %w", err)
				}
			}
		case 9:
			for _, statement := range []string{
				"ALTER TABLE operational_runs ADD COLUMN merge_commit_sha TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN lifecycle_reason TEXT NOT NULL DEFAULT ''",
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("apply store migration 9: %w", err)
				}
			}
		case 10:
			if _, err := tx.ExecContext(ctx, "ALTER TABLE operational_runs ADD COLUMN lifecycle_notification_sent INTEGER NOT NULL DEFAULT 0"); err != nil {
				return fmt.Errorf("apply store migration 10: %w", err)
			}
		case 11:
			if _, err := tx.ExecContext(ctx, `CREATE TABLE lifecycle_notifications (
					run_id TEXT NOT NULL,
					terminal_status TEXT NOT NULL,
					created_at TEXT NOT NULL,
					PRIMARY KEY (run_id, terminal_status)
				)`); err != nil {
				return fmt.Errorf("apply store migration 11: %w", err)
			}
		case 12:
			if _, err := tx.ExecContext(ctx, `CREATE TABLE gate_results (
					run_id TEXT NOT NULL,
					checkpoint_sha TEXT NOT NULL,
					phase TEXT NOT NULL,
					ordinal INTEGER NOT NULL,
					gate_name TEXT NOT NULL,
					outcome TEXT NOT NULL,
					status TEXT NOT NULL,
					blocking INTEGER NOT NULL,
					skip_reason TEXT NOT NULL DEFAULT '',
					setup_fingerprint TEXT NOT NULL DEFAULT '',
					created_at TEXT NOT NULL,
					updated_at TEXT NOT NULL,
					PRIMARY KEY (run_id, checkpoint_sha, phase, gate_name)
				)`); err != nil {
				return fmt.Errorf("apply store migration 12: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `CREATE INDEX gate_results_by_run
					ON gate_results (run_id, phase, checkpoint_sha, ordinal)`); err != nil {
				return fmt.Errorf("apply store migration 12 index: %w", err)
			}
		case 13:
			if _, err := tx.ExecContext(ctx, `CREATE TABLE evaluation_summaries (
				run_id TEXT PRIMARY KEY,
				outcome TEXT NOT NULL,
				started_at TEXT NOT NULL,
				completed_at TEXT NOT NULL DEFAULT '',
				total_wall_nanos INTEGER NOT NULL DEFAULT 0,
				stage_durations TEXT NOT NULL DEFAULT '[]',
				invocation_versions TEXT NOT NULL DEFAULT '[]',
				invocation_count INTEGER NOT NULL DEFAULT 0,
				gate_count INTEGER NOT NULL DEFAULT 0,
				check_repair_count INTEGER NOT NULL DEFAULT 0,
				test_revision_count INTEGER NOT NULL DEFAULT 0,
				review_revision_count INTEGER NOT NULL DEFAULT 0,
				budget_exhausted INTEGER NOT NULL DEFAULT 0,
				exemptions TEXT NOT NULL DEFAULT '[]',
				escalation_categories TEXT NOT NULL DEFAULT '[]',
				repeated_blockers TEXT NOT NULL DEFAULT '[]',
				usage_available INTEGER NOT NULL DEFAULT 0,
				usage_unavailable_reason TEXT NOT NULL DEFAULT 'not_reported',
				usage_input_tokens INTEGER NOT NULL DEFAULT 0,
				usage_output_tokens INTEGER NOT NULL DEFAULT 0,
				usage_total_tokens INTEGER NOT NULL DEFAULT 0,
				usage_cost_reported INTEGER NOT NULL DEFAULT 0,
				usage_cost_micros INTEGER NOT NULL DEFAULT 0,
				usage_currency TEXT NOT NULL DEFAULT '',
				active_stage TEXT NOT NULL DEFAULT '',
				active_stage_started_at TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`); err != nil {
				return fmt.Errorf("apply store migration 13 evaluation summaries: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `CREATE TABLE evaluation_usage (
				run_id TEXT NOT NULL,
				invocation_id TEXT NOT NULL,
				input_tokens INTEGER NOT NULL DEFAULT 0,
				output_tokens INTEGER NOT NULL DEFAULT 0,
				total_tokens INTEGER NOT NULL DEFAULT 0,
				cost_reported INTEGER NOT NULL DEFAULT 0,
				cost_micros INTEGER NOT NULL DEFAULT 0,
				currency TEXT NOT NULL DEFAULT '',
				recorded_at TEXT NOT NULL,
				PRIMARY KEY (run_id, invocation_id)
			)`); err != nil {
				return fmt.Errorf("apply store migration 13 evaluation usage: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `CREATE INDEX evaluation_usage_by_run
					ON evaluation_usage (run_id, recorded_at)`); err != nil {
				return fmt.Errorf("apply store migration 13 evaluation usage index: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `CREATE TABLE evaluation_dispositions (
				run_id TEXT NOT NULL,
				event_id_hash TEXT NOT NULL,
				category TEXT NOT NULL,
				disposition TEXT NOT NULL,
				decided_at TEXT NOT NULL,
				PRIMARY KEY (run_id, event_id_hash)
			)`); err != nil {
				return fmt.Errorf("apply store migration 13 evaluation dispositions: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `CREATE INDEX evaluation_dispositions_by_run
					ON evaluation_dispositions (run_id, decided_at)`); err != nil {
				return fmt.Errorf("apply store migration 13 evaluation dispositions index: %w", err)
			}
		case 14:
			for _, statement := range []string{
				"ALTER TABLE operational_runs ADD COLUMN check_repair_attempts INTEGER NOT NULL DEFAULT 0",
				"ALTER TABLE operational_runs ADD COLUMN check_repair_budget INTEGER NOT NULL DEFAULT 0",
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("apply store migration 14: %w", err)
				}
			}
		case 15:
			if _, err := tx.ExecContext(ctx, "ALTER TABLE invocations ADD COLUMN credential_store_id TEXT NOT NULL DEFAULT ''"); err != nil {
				return fmt.Errorf("apply store migration 15: %w", err)
			}
		case 16:
			if _, err := tx.ExecContext(ctx, "ALTER TABLE operational_runs ADD COLUMN check_repair_pending_attempt INTEGER NOT NULL DEFAULT 0"); err != nil {
				return fmt.Errorf("apply store migration 16: %w", err)
			}
		case 17:
			if _, err := tx.ExecContext(ctx, "ALTER TABLE operational_runs ADD COLUMN pending_questions TEXT NOT NULL DEFAULT '[]'"); err != nil {
				return fmt.Errorf("apply store migration 17: %w", err)
			}
		case 18:
			for _, statement := range []string{
				"ALTER TABLE operational_runs ADD COLUMN clarification_comment_id TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN clarification_notification_sent INTEGER NOT NULL DEFAULT 0",
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("apply store migration 18: %w", err)
				}
			}
		case 19:
			for _, statement := range []string{
				"ALTER TABLE operational_runs ADD COLUMN base_checkpoint_sha TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN test_checkpoint_sha TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN test_handoff TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN test_exemption TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN protected_test_paths TEXT NOT NULL DEFAULT '[]'",
				"ALTER TABLE operational_runs ADD COLUMN test_stage_skipped INTEGER NOT NULL DEFAULT 0",
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("apply store migration 19: %w", err)
				}
			}
		case 20:
			for _, statement := range []string{
				"ALTER TABLE operational_runs ADD COLUMN implementation_handoff TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN specification_review TEXT NOT NULL DEFAULT ''",
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("apply store migration 20: %w", err)
				}
			}
		case 21:
			if _, err := tx.ExecContext(ctx, `CREATE TABLE pending_effects (
				run_id TEXT PRIMARY KEY,
				effect_id TEXT NOT NULL,
				kind TEXT NOT NULL,
				payload TEXT NOT NULL,
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`); err != nil {
				return fmt.Errorf("apply store migration 21: %w", err)
			}
		case 22:
			if _, err := tx.ExecContext(ctx, "ALTER TABLE invocations ADD COLUMN recovery_resume_count INTEGER NOT NULL DEFAULT 0"); err != nil {
				return fmt.Errorf("apply store migration 22: %w", err)
			}
		case 23:
			if _, err := tx.ExecContext(ctx, "ALTER TABLE invocations ADD COLUMN attach_required INTEGER NOT NULL DEFAULT 0"); err != nil {
				return fmt.Errorf("apply store migration 23: %w", err)
			}
		case 24:
			if _, err := tx.ExecContext(ctx, "ALTER TABLE operational_runs ADD COLUMN terminal_at TEXT NOT NULL DEFAULT ''"); err != nil {
				return fmt.Errorf("apply store migration 24: %w", err)
			}
		case 25:
			for _, statement := range []string{
				"ALTER TABLE invocations ADD COLUMN role_surface_id TEXT NOT NULL DEFAULT ''",
				"UPDATE invocations SET role_surface_id = implementation_surface_id WHERE role_surface_id = '' AND implementation_surface_id <> ''",
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("apply store migration 25: %w", err)
				}
			}
		case 26:
			if _, err := tx.ExecContext(ctx, "ALTER TABLE operational_runs ADD COLUMN active_invocation_id TEXT NOT NULL DEFAULT ''"); err != nil {
				return fmt.Errorf("apply store migration 26: %w", err)
			}
		case 27:
			for _, statement := range []string{
				"ALTER TABLE operational_runs ADD COLUMN test_invocation_id TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN test_revision_attempts INTEGER NOT NULL DEFAULT 0",
				"ALTER TABLE operational_runs ADD COLUMN test_revision_budget INTEGER NOT NULL DEFAULT 0",
				"ALTER TABLE operational_runs ADD COLUMN test_revision_history TEXT NOT NULL DEFAULT '[]'",
				"ALTER TABLE operational_runs ADD COLUMN test_objection TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN test_revision_base_changed_paths TEXT NOT NULL DEFAULT '[]'",
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("apply store migration 27: %w", err)
				}
			}
		case 28:
			for _, statement := range []string{
				"ALTER TABLE operational_runs ADD COLUMN standards_review TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN active_invocation_ids TEXT NOT NULL DEFAULT '[]'",
				"DROP INDEX one_active_invocation_per_run",
				"CREATE UNIQUE INDEX one_active_invocation_per_role ON invocations (run_id, role) WHERE status = 'active'",
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("apply store migration 28: %w", err)
				}
			}
		case 29:
			for _, statement := range []string{
				"ALTER TABLE operational_runs ADD COLUMN review_repair_attempts INTEGER NOT NULL DEFAULT 0",
				"ALTER TABLE operational_runs ADD COLUMN review_repair_budget INTEGER NOT NULL DEFAULT 0",
				"ALTER TABLE operational_runs ADD COLUMN review_repair_pending_attempt INTEGER NOT NULL DEFAULT 0",
				"ALTER TABLE operational_runs ADD COLUMN review_repair_history TEXT NOT NULL DEFAULT '[]'",
				"ALTER TABLE operational_runs ADD COLUMN review_repair_packet TEXT NOT NULL DEFAULT ''",
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("apply store migration 29: %w", err)
				}
			}
		case 30:
			if _, err := tx.ExecContext(ctx, "ALTER TABLE operational_runs ADD COLUMN ready_notification_sent INTEGER NOT NULL DEFAULT 0"); err != nil {
				return fmt.Errorf("apply store migration 30: %w", err)
			}
		case 31:
			for _, statement := range []string{
				"ALTER TABLE operational_runs ADD COLUMN processed_review_id TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE operational_runs ADD COLUMN processed_review_revision INTEGER NOT NULL DEFAULT 0",
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("apply store migration 31: %w", err)
				}
			}
		case 32:
			for _, statement := range []string{
				"ALTER TABLE invocations ADD COLUMN prompt_craft_source_path TEXT NOT NULL DEFAULT ''",
				"ALTER TABLE invocations ADD COLUMN prompt_craft_sha256 TEXT NOT NULL DEFAULT ''",
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("apply store migration 32: %w", err)
				}
			}
		case 33:
			if _, err := tx.ExecContext(ctx, `UPDATE operational_runs
				SET active_invocation_ids = (
					SELECT json_group_array(value)
					FROM (
						SELECT operational_runs.active_invocation_id AS value, -1 AS position
						UNION ALL
						SELECT json_each.value, json_each.key
						FROM json_each(operational_runs.active_invocation_ids)
						WHERE json_each.value <> operational_runs.active_invocation_id
						ORDER BY position
					)
				)
				WHERE active_invocation_id <> ''`); err != nil {
				return fmt.Errorf("apply store migration 33 active invocation fold: %w", err)
			}
			if _, err := tx.ExecContext(ctx, "ALTER TABLE operational_runs DROP COLUMN active_invocation_id"); err != nil {
				return fmt.Errorf("apply store migration 33 active invocation removal: %w", err)
			}
		case 34:
			if _, err := tx.ExecContext(ctx, "ALTER TABLE invocations ADD COLUMN launch_voided INTEGER NOT NULL DEFAULT 0"); err != nil {
				return fmt.Errorf("apply store migration 34: %w", err)
			}
		case 35:
			if _, err := tx.ExecContext(ctx, "ALTER TABLE operational_runs ADD COLUMN accepted_implementation_checkpoint_sha TEXT NOT NULL DEFAULT ''"); err != nil {
				return fmt.Errorf("apply store migration 35: %w", err)
			}
		default:
			return fmt.Errorf("no migration registered for schema version %d", version+1)
		}
		version++
		if _, err := tx.ExecContext(ctx, "UPDATE schema_metadata SET version = ? WHERE singleton = 1", version); err != nil {
			return fmt.Errorf("record store schema version %d: %w", version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit store migration: %w", err)
	}
	return nil
}

// reconcileDuplicateRuns keeps the newest non-terminal run per repository
// while migrating legacy rows whose timestamp text may have variable-width
// fractional seconds. Go's RFC3339Nano parser provides chronological ordering
// at nanosecond precision before older duplicates are marked failed.
func reconcileDuplicateRuns(ctx context.Context, tx *sql.Tx) error {
	type runCandidate struct {
		id        string
		updatedAt time.Time
	}
	// First, normalize all legacy timestamps to the canonical fixed-width format
	// so that subsequent SQL ORDER BY operations on created_at/updated_at produce
	// correct chronological results via lexical string comparison.
	allRows, err := tx.QueryContext(ctx, `SELECT id, created_at, updated_at FROM operational_runs`)
	if err != nil {
		return fmt.Errorf("list runs for timestamp normalization: %w", err)
	}
	type timestampUpdate struct {
		id        string
		createdAt string
		updatedAt string
	}
	updates := make([]timestampUpdate, 0)
	for allRows.Next() {
		var id, createdAt, updatedAt string
		if err := allRows.Scan(&id, &createdAt, &updatedAt); err != nil {
			_ = allRows.Close()
			return fmt.Errorf("scan run for timestamp normalization: %w", err)
		}
		parsedCreatedAt, err := time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			_ = allRows.Close()
			return fmt.Errorf("parse run created_at for normalization: %w", err)
		}
		parsedUpdatedAt, err := time.Parse(time.RFC3339Nano, updatedAt)
		if err != nil {
			_ = allRows.Close()
			return fmt.Errorf("parse run updated_at for normalization: %w", err)
		}
		normalizedCreatedAt := parsedCreatedAt.Format(runTimestampLayout)
		normalizedUpdatedAt := parsedUpdatedAt.Format(runTimestampLayout)
		if createdAt != normalizedCreatedAt || updatedAt != normalizedUpdatedAt {
			updates = append(updates, timestampUpdate{
				id:        id,
				createdAt: normalizedCreatedAt,
				updatedAt: normalizedUpdatedAt,
			})
		}
	}
	if err := allRows.Err(); err != nil {
		_ = allRows.Close()
		return fmt.Errorf("read runs for timestamp normalization: %w", err)
	}
	if err := allRows.Close(); err != nil {
		return fmt.Errorf("close runs for timestamp normalization: %w", err)
	}
	for _, update := range updates {
		if _, err := tx.ExecContext(ctx, `UPDATE operational_runs SET created_at = ?, updated_at = ? WHERE id = ?`,
			update.createdAt, update.updatedAt, update.id); err != nil {
			return fmt.Errorf("normalize timestamps for run %q: %w", update.id, err)
		}
	}
	// Now reconcile duplicates using the normalized timestamps
	rows, err := tx.QueryContext(ctx, `
		SELECT id, repository_path, updated_at
		FROM operational_runs
		WHERE status NOT IN (?, ?, ?)`, StatusComplete, StatusCancelled, StatusFailed)
	if err != nil {
		return fmt.Errorf("list non-terminal runs: %w", err)
	}
	newestByRepository := make(map[string]runCandidate)
	duplicates := make([]string, 0)
	for rows.Next() {
		var id, repositoryPath, updatedAt string
		if err := rows.Scan(&id, &repositoryPath, &updatedAt); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan run for reconciliation: %w", err)
		}
		parsedUpdatedAt, err := time.Parse(time.RFC3339Nano, updatedAt)
		if err != nil {
			_ = rows.Close()
			return fmt.Errorf("parse run updated_at for reconciliation: %w", err)
		}
		candidate := runCandidate{id: id, updatedAt: parsedUpdatedAt}
		current, exists := newestByRepository[repositoryPath]
		if !exists || parsedUpdatedAt.After(current.updatedAt) || (parsedUpdatedAt.Equal(current.updatedAt) && id > current.id) {
			if exists {
				duplicates = append(duplicates, current.id)
			}
			newestByRepository[repositoryPath] = candidate
		} else {
			duplicates = append(duplicates, candidate.id)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read runs for reconciliation: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close runs for reconciliation: %w", err)
	}
	for _, id := range duplicates {
		if _, err := tx.ExecContext(ctx, "UPDATE operational_runs SET status = ? WHERE id = ?", StatusFailed, id); err != nil {
			return fmt.Errorf("mark duplicate run %q failed: %w", id, err)
		}
	}
	return nil
}

// reconcileDuplicateInvocations keeps the newest active invocation per run
// before the partial unique index is created for migrated stores.
func reconcileDuplicateInvocations(ctx context.Context, tx *sql.Tx) error {
	type invocationCandidate struct {
		id        string
		updatedAt time.Time
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, run_id, updated_at
		FROM invocations
		WHERE status = ?`, InvocationStatusActive)
	if err != nil {
		return fmt.Errorf("list active invocations: %w", err)
	}
	newestByRun := make(map[string]invocationCandidate)
	duplicates := make([]string, 0)
	for rows.Next() {
		var id, runID, updatedAt string
		if err := rows.Scan(&id, &runID, &updatedAt); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan invocation for reconciliation: %w", err)
		}
		parsedUpdatedAt, err := time.Parse(time.RFC3339Nano, updatedAt)
		if err != nil {
			_ = rows.Close()
			return fmt.Errorf("parse invocation updated_at for reconciliation: %w", err)
		}
		candidate := invocationCandidate{id: id, updatedAt: parsedUpdatedAt}
		current, exists := newestByRun[runID]
		if !exists || parsedUpdatedAt.After(current.updatedAt) || (parsedUpdatedAt.Equal(current.updatedAt) && id > current.id) {
			if exists {
				duplicates = append(duplicates, current.id)
			}
			newestByRun[runID] = candidate
		} else {
			duplicates = append(duplicates, candidate.id)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read invocations for reconciliation: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close invocations for reconciliation: %w", err)
	}
	for _, id := range duplicates {
		if _, err := tx.ExecContext(ctx, "UPDATE invocations SET status = ? WHERE id = ?", InvocationStatusCannotProceed, id); err != nil {
			return fmt.Errorf("mark duplicate invocation %q unavailable: %w", id, err)
		}
	}
	return nil
}

// isActiveInvocationConflict recognizes the migration-created unique index
// without depending on a driver-specific SQLite error type.
func isActiveInvocationConflict(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "one_active_invocation_per_run") ||
		strings.Contains(message, "one_active_invocation_per_role") ||
		strings.Contains(message, "unique constraint failed: invocations.run_id, invocations.role")
}

// backupDatabase creates a private 0600 backup of the database and returns its path.
// It removes an incomplete backup when copying fails.
func backupDatabase(path string) (string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		suffix := time.Now().UTC().Format("20060102T150405.000000000Z07:00")
		if attempt > 0 {
			suffix += "-" + strconv.Itoa(attempt)
		}
		backupPath := path + ".bak-" + suffix
		if _, err := os.Stat(backupPath); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect store backup path: %w", err)
		}
		input, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("open store for backup: %w", err)
		}
		output, err := os.OpenFile(backupPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			_ = input.Close()
			return "", fmt.Errorf("create store backup: %w", err)
		}
		_, copyErr := io.Copy(output, input)
		if copyErr == nil {
			copyErr = output.Sync()
		}
		closeOutputErr := output.Close()
		closeInputErr := input.Close()
		if copyErr != nil {
			_ = os.Remove(backupPath)
			return "", fmt.Errorf("copy store backup: %w", copyErr)
		}
		if closeOutputErr != nil {
			return "", fmt.Errorf("close store backup: %w", closeOutputErr)
		}
		if closeInputErr != nil {
			return "", fmt.Errorf("close source store after backup: %w", closeInputErr)
		}
		return backupPath, nil
	}
	return "", errors.New("could not choose a unique store backup path")
}
