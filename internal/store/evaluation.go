package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// EvaluationOutcome identifies the content-free terminal or active state of a
// local evaluation summary.
type EvaluationOutcome string

const (
	// EvaluationOutcomeActive identifies a summary for a run still in progress.
	EvaluationOutcomeActive EvaluationOutcome = "active"
	// EvaluationOutcomeWaitingForHuman identifies a run paused for a human.
	EvaluationOutcomeWaitingForHuman EvaluationOutcome = "waiting_for_human"
	// EvaluationOutcomeWaitingForHarness identifies a run paused for a harness.
	EvaluationOutcomeWaitingForHarness EvaluationOutcome = "waiting_for_harness"
	// EvaluationOutcomeFailed identifies a run that failed.
	EvaluationOutcomeFailed EvaluationOutcome = "failed"
	// EvaluationOutcomeCancelled identifies a run cancelled by lifecycle policy.
	EvaluationOutcomeCancelled EvaluationOutcome = "cancelled"
	// EvaluationOutcomeComplete identifies a successfully completed run.
	EvaluationOutcomeComplete EvaluationOutcome = "complete"
)

// EvaluationAttempt identifies a retry or revision category retained for
// tuning workflow policy.
type EvaluationAttempt string

const (
	// EvaluationAttemptCheckRepair counts a deterministic-check repair attempt.
	EvaluationAttemptCheckRepair EvaluationAttempt = "check_repair"
	// EvaluationAttemptTestRevision counts a test-stage revision attempt.
	EvaluationAttemptTestRevision EvaluationAttempt = "test_revision"
	// EvaluationAttemptReviewRevision counts a review-stage revision attempt.
	EvaluationAttemptReviewRevision EvaluationAttempt = "review_revision"
)

// EvaluationExemption identifies a stable human or technical exemption class.
type EvaluationExemption string

const (
	// EvaluationExemptionHuman identifies a human-approved exemption.
	EvaluationExemptionHuman EvaluationExemption = "human"
	// EvaluationExemptionTechnical identifies an infrastructure or technical exemption.
	EvaluationExemptionTechnical EvaluationExemption = "technical"
	// EvaluationExemptionBaseline identifies an accepted pre-existing baseline condition.
	EvaluationExemptionBaseline EvaluationExemption = "baseline"
)

// EvaluationEscalationCategory identifies a stable escalation class without
// retaining the escalated finding or question.
type EvaluationEscalationCategory string

const (
	// EvaluationEscalationTestDispute identifies a disputed test result.
	EvaluationEscalationTestDispute EvaluationEscalationCategory = "test_dispute"
	// EvaluationEscalationReviewFinding identifies an escalated review finding.
	EvaluationEscalationReviewFinding EvaluationEscalationCategory = "review_finding"
	// EvaluationEscalationClarification identifies a request for clarification.
	EvaluationEscalationClarification EvaluationEscalationCategory = "clarification"
	// EvaluationEscalationRuntime identifies a harness or runtime escalation.
	EvaluationEscalationRuntime EvaluationEscalationCategory = "runtime"
	// EvaluationEscalationBudget identifies a budget escalation.
	EvaluationEscalationBudget EvaluationEscalationCategory = "budget"
	// EvaluationEscalationBlocked identifies an inability to proceed without retaining its evidence.
	EvaluationEscalationBlocked EvaluationEscalationCategory = "blocked"
)

// EvaluationBlockerCategory identifies a repeated blocker class without
// retaining blocker text.
type EvaluationBlockerCategory string

const (
	// EvaluationBlockerSetup identifies dependency setup blockers.
	EvaluationBlockerSetup EvaluationBlockerCategory = "setup"
	// EvaluationBlockerGate identifies deterministic gate blockers.
	EvaluationBlockerGate EvaluationBlockerCategory = "gate"
	// EvaluationBlockerTest identifies test-policy blockers.
	EvaluationBlockerTest EvaluationBlockerCategory = "test"
	// EvaluationBlockerReview identifies review blockers.
	EvaluationBlockerReview EvaluationBlockerCategory = "review"
	// EvaluationBlockerHarness identifies harness blockers.
	EvaluationBlockerHarness EvaluationBlockerCategory = "harness"
	// EvaluationBlockerBudget identifies budget blockers.
	EvaluationBlockerBudget EvaluationBlockerCategory = "budget"
	// EvaluationBlockerUnknown identifies a blocker whose stable class is unavailable.
	EvaluationBlockerUnknown EvaluationBlockerCategory = "unknown"
)

// EvaluationDisposition is the human classification attached to an escalated
// test dispute or review finding.
type EvaluationDisposition string

const (
	// EvaluationDispositionUpheld means the human agreed with the escalation.
	EvaluationDispositionUpheld EvaluationDisposition = "upheld"
	// EvaluationDispositionAdvisory means the human retained it as advisory.
	EvaluationDispositionAdvisory EvaluationDisposition = "advisory"
	// EvaluationDispositionOverturned means the human rejected the escalation.
	EvaluationDispositionOverturned EvaluationDisposition = "overturned"
)

const (
	// EvaluationUsageNotReported explains that no harness measurement was supplied.
	EvaluationUsageNotReported = "not_reported"
	// EvaluationUsageMixedCurrency explains that cost values could not be combined safely.
	EvaluationUsageMixedCurrency = "mixed_currency"
	// EvaluationUsagePartialCost explains that only some available usage reports included cost.
	EvaluationUsagePartialCost = "partial_cost"
)

// EvaluationInvocationVersion records version identifiers observed for one
// invocation. It contains no prompt, transcript, source, or command content.
type EvaluationInvocationVersion struct {
	InvocationID        string `json:"invocation_id"`
	Harness             string `json:"harness"`
	Model               string `json:"model"`
	PromptVersion       string `json:"prompt_version"`
	ReportSchemaVersion int    `json:"report_schema_version"`
	WorkerVersion       string `json:"worker_version"`
}

// EvaluationStageDuration records time spent in one coordinator stage.
type EvaluationStageDuration struct {
	Stage    Stage         `json:"stage"`
	Duration time.Duration `json:"duration"`
}

// EvaluationBlocker records how often a stable blocker category repeated.
type EvaluationBlocker struct {
	Category EvaluationBlockerCategory `json:"category"`
	Count    int                       `json:"count"`
}

// EvaluationUsage contains only reliable harness-reported measurements. Zero
// values are not estimates; Available and CostReported identify what was
// actually supplied by the harness.
type EvaluationUsage struct {
	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
	InputTokens       int64  `json:"input_tokens,omitempty"`
	OutputTokens      int64  `json:"output_tokens,omitempty"`
	TotalTokens       int64  `json:"total_tokens,omitempty"`
	CostReported      bool   `json:"cost_reported,omitempty"`
	CostMicros        int64  `json:"cost_micros,omitempty"`
	Currency          string `json:"currency,omitempty"`
}

// EvaluationSummary is the content-free local evidence retained for one run.
// It is deliberately separate from Run, Invocation, gate output, and run
// artifacts, and it never contains issue text, prompts, transcripts, diffs,
// source contents, command output, logs, or credentials.
type EvaluationSummary struct {
	RunID              string                        `json:"run_id"`
	Outcome            EvaluationOutcome             `json:"outcome"`
	StartedAt          time.Time                     `json:"started_at"`
	CompletedAt        time.Time                     `json:"completed_at,omitempty"`
	TotalWallTime      time.Duration                 `json:"total_wall_time"`
	StageDurations     []EvaluationStageDuration     `json:"stage_durations,omitempty"`
	InvocationVersions []EvaluationInvocationVersion `json:"invocation_versions,omitempty"`
	InvocationCount    int                           `json:"invocation_count"`
	// GateCount counts gate executions, including repeated evaluations of one
	// exact checkpoint whose durable result projection is idempotently upserted.
	GateCount            int                            `json:"gate_count"`
	CheckRepairCount     int                            `json:"check_repair_count"`
	TestRevisionCount    int                            `json:"test_revision_count"`
	ReviewRevisionCount  int                            `json:"review_revision_count"`
	BudgetExhausted      bool                           `json:"budget_exhausted"`
	Exemptions           []EvaluationExemption          `json:"exemptions,omitempty"`
	EscalationCategories []EvaluationEscalationCategory `json:"escalation_categories,omitempty"`
	RepeatedBlockers     []EvaluationBlocker            `json:"repeated_blockers,omitempty"`
	Usage                EvaluationUsage                `json:"usage"`
	CreatedAt            time.Time                      `json:"created_at"`
	UpdatedAt            time.Time                      `json:"updated_at"`
}

// EvaluationDispositionRequest attaches a human disposition to an opaque
// escalation identity. The identity is hashed before persistence so accidental
// finding text cannot be retained.
type EvaluationDispositionRequest struct {
	RunID       string
	EventID     string
	Category    EvaluationEscalationCategory
	Disposition EvaluationDisposition
	DecidedAt   time.Time
}

// EvaluationDispositionRecord is the persisted, content-free human
// classification.
// EventIDHash is a one-way identity, never the original finding or question.
type EvaluationDispositionRecord struct {
	RunID       string                       `json:"run_id"`
	EventIDHash string                       `json:"event_id_hash"`
	Category    EvaluationEscalationCategory `json:"category"`
	Disposition EvaluationDisposition        `json:"disposition"`
	DecidedAt   time.Time                    `json:"decided_at"`
}

// EvaluationAggregate contains local aggregate counts, rates, durations, and
// only the usage measurements that were actually available.
type EvaluationAggregate struct {
	RunCount              int                                      `json:"run_count"`
	OutcomeCounts         map[EvaluationOutcome]int                `json:"outcome_counts"`
	OutcomeRates          map[EvaluationOutcome]float64            `json:"outcome_rates"`
	SuccessRate           float64                                  `json:"success_rate"`
	BudgetExhaustionRate  float64                                  `json:"budget_exhaustion_rate"`
	TotalWallTime         time.Duration                            `json:"total_wall_time"`
	AverageTotalWallTime  time.Duration                            `json:"average_total_wall_time"`
	StageDurations        map[Stage]time.Duration                  `json:"stage_durations"`
	InvocationCount       int                                      `json:"invocation_count"`
	GateCount             int                                      `json:"gate_count"`
	CheckRepairCount      int                                      `json:"check_repair_count"`
	TestRevisionCount     int                                      `json:"test_revision_count"`
	ReviewRevisionCount   int                                      `json:"review_revision_count"`
	EscalationCounts      map[EvaluationEscalationCategory]int     `json:"escalation_counts"`
	EscalationRates       map[EvaluationEscalationCategory]float64 `json:"escalation_rates"`
	DispositionCounts     map[EvaluationDisposition]int            `json:"disposition_counts"`
	UsageAvailableRuns    int                                      `json:"usage_available_runs"`
	UsageCostReportedRuns int                                      `json:"usage_cost_reported_runs"`
	Usage                 EvaluationUsage                          `json:"usage"`
}

// EvaluationDeletionResult reports the visible effect of deliberate summary
// deletion without exposing any work content.
type EvaluationDeletionResult struct {
	Summaries    int `json:"summaries"`
	Dispositions int `json:"dispositions"`
	UsageRows    int `json:"usage_rows"`
}

// EvaluationSummary returns one persisted local evaluation summary, or nil
// when the run has not produced one.
func (s *Store) EvaluationSummary(ctx context.Context, runID string) (*EvaluationSummary, error) {
	if err := validateEvaluationRunID(runID); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx, evaluationSummarySelect+" WHERE run_id = ?", runID)
	return scanEvaluationSummary(row)
}

// ListEvaluationSummaries returns content-free summaries in completion order.
// An empty run ID lists every retained summary; a non-empty run ID selects one.
func (s *Store) ListEvaluationSummaries(ctx context.Context, runID string) ([]EvaluationSummary, error) {
	if strings.TrimSpace(runID) != "" {
		one, err := s.EvaluationSummary(ctx, runID)
		if err != nil {
			return nil, err
		}
		if one == nil {
			return []EvaluationSummary{}, nil
		}
		return []EvaluationSummary{*one}, nil
	}
	rows, err := s.db.QueryContext(ctx, evaluationSummarySelect+" ORDER BY completed_at DESC, run_id")
	if err != nil {
		return nil, fmt.Errorf("list evaluation summaries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]EvaluationSummary, 0)
	for rows.Next() {
		value, err := scanEvaluationSummaryRows(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read evaluation summaries: %w", err)
	}
	return result, nil
}

// SaveEvaluationSummary validates and upserts one content-free local summary.
// It never accepts or stores issue, specification, prompt, transcript, diff,
// source, command, log, or credential fields.
func (s *Store) SaveEvaluationSummary(ctx context.Context, summary EvaluationSummary) error {
	normalized, values, err := normalizeEvaluationSummary(summary)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO evaluation_summaries (
			run_id, outcome, started_at, completed_at, total_wall_nanos,
			stage_durations, invocation_versions, invocation_count, gate_count,
			check_repair_count, test_revision_count, review_revision_count,
			budget_exhausted, exemptions, escalation_categories, repeated_blockers,
			usage_available, usage_unavailable_reason, usage_input_tokens,
			usage_output_tokens, usage_total_tokens, usage_cost_reported,
			usage_cost_micros, usage_currency, active_stage,
			active_stage_started_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET
			outcome = excluded.outcome,
			started_at = excluded.started_at,
			completed_at = excluded.completed_at,
			total_wall_nanos = excluded.total_wall_nanos,
			stage_durations = excluded.stage_durations,
			invocation_versions = excluded.invocation_versions,
			invocation_count = excluded.invocation_count,
			gate_count = excluded.gate_count,
			check_repair_count = excluded.check_repair_count,
			test_revision_count = excluded.test_revision_count,
			review_revision_count = excluded.review_revision_count,
			budget_exhausted = excluded.budget_exhausted,
			exemptions = excluded.exemptions,
			escalation_categories = excluded.escalation_categories,
			repeated_blockers = excluded.repeated_blockers,
			usage_available = excluded.usage_available,
			usage_unavailable_reason = excluded.usage_unavailable_reason,
			usage_input_tokens = excluded.usage_input_tokens,
			usage_output_tokens = excluded.usage_output_tokens,
			usage_total_tokens = excluded.usage_total_tokens,
			usage_cost_reported = excluded.usage_cost_reported,
			usage_cost_micros = excluded.usage_cost_micros,
			usage_currency = excluded.usage_currency,
			updated_at = excluded.updated_at`,
		values...)
	if err != nil {
		return fmt.Errorf("save evaluation summary %q: %w", normalized.RunID, err)
	}
	return nil
}

// EnsureEvaluationSummary creates the active summary for a run exactly once.
// The worker image digest is retained as a version identifier, never as work
// content; all other summary fields begin explicitly empty or unavailable.
func (s *Store) EnsureEvaluationSummary(ctx context.Context, run Run) error {
	if err := validateEvaluationRunID(run.ID); err != nil {
		return err
	}
	startedAt := run.CreatedAt.UTC()
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	stage := run.Stage
	if stage == "" {
		stage = StageClaim
	}
	if err := validateEvaluationStage(stage); err != nil {
		return err
	}
	if run.ImageDigest != "" && !safeEvaluationValue(run.ImageDigest) {
		return errors.New("evaluation worker version contains unsafe metadata")
	}
	versions := []EvaluationInvocationVersion{}
	if run.ImageDigest != "" {
		versions = append(versions, EvaluationInvocationVersion{WorkerVersion: run.ImageDigest})
	}
	stageStarted := startedAt.Format(runTimestampLayout)
	versionJSON, err := json.Marshal(versions)
	if err != nil {
		return fmt.Errorf("encode initial evaluation versions: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO evaluation_summaries (
			run_id, outcome, started_at, completed_at, total_wall_nanos,
			stage_durations, invocation_versions, invocation_count, gate_count,
			check_repair_count, test_revision_count, review_revision_count,
			budget_exhausted, exemptions, escalation_categories, repeated_blockers,
			usage_available, usage_unavailable_reason, usage_input_tokens,
			usage_output_tokens, usage_total_tokens, usage_cost_reported,
			usage_cost_micros, usage_currency, active_stage,
			active_stage_started_at, created_at, updated_at
		) VALUES (?, ?, ?, '', 0, '[]', ?, 0, 0, 0, 0, 0, 0, '[]', '[]', '[]', 0, ?, 0, 0, 0, 0, 0, '', ?, ?, ?, ?)
		ON CONFLICT(run_id) DO NOTHING`,
		run.ID,
		EvaluationOutcomeActive,
		startedAt.Format(runTimestampLayout),
		string(versionJSON),
		EvaluationUsageNotReported,
		stage,
		stageStarted,
		startedAt.Format(runTimestampLayout),
		startedAt.Format(runTimestampLayout),
	)
	if err != nil {
		return fmt.Errorf("ensure evaluation summary %q: %w", run.ID, err)
	}
	return nil
}

const evaluationSummarySelect = `SELECT run_id, outcome, started_at, completed_at,
	total_wall_nanos, stage_durations, invocation_versions, invocation_count,
	gate_count, check_repair_count, test_revision_count, review_revision_count,
	budget_exhausted, exemptions, escalation_categories, repeated_blockers,
	usage_available, usage_unavailable_reason, usage_input_tokens,
	usage_output_tokens, usage_total_tokens, usage_cost_reported,
	usage_cost_micros, usage_currency, created_at, updated_at
	FROM evaluation_summaries`

// scanEvaluationSummary decodes one summary row returned by SQLite.
func scanEvaluationSummary(row *sql.Row) (*EvaluationSummary, error) {
	return scanEvaluationSummaryValues(row.Scan)
}

// scanEvaluationSummaryRows decodes one summary row returned by a row iterator.
func scanEvaluationSummaryRows(rows *sql.Rows) (*EvaluationSummary, error) {
	return scanEvaluationSummaryValues(rows.Scan)
}

// scanEvaluationSummaryValues translates the persisted content-free columns
// into the public summary model.
func scanEvaluationSummaryValues(scan func(...any) error) (*EvaluationSummary, error) {
	var summary EvaluationSummary
	var outcome string
	var completedAt, startedAt, createdAt, updatedAt string
	var totalWallNanos int64
	var stageDurationsJSON, invocationVersionsJSON, exemptionsJSON string
	var escalationJSON, blockersJSON string
	var usageAvailable, usageCostReported int
	var usageReason, usageCurrency string
	var usageInput, usageOutput, usageTotal, usageCost int64
	if err := scan(
		&summary.RunID, &outcome, &startedAt, &completedAt, &totalWallNanos,
		&stageDurationsJSON, &invocationVersionsJSON, &summary.InvocationCount,
		&summary.GateCount, &summary.CheckRepairCount, &summary.TestRevisionCount,
		&summary.ReviewRevisionCount, &summary.BudgetExhausted, &exemptionsJSON,
		&escalationJSON, &blockersJSON, &usageAvailable, &usageReason,
		&usageInput, &usageOutput, &usageTotal, &usageCostReported, &usageCost,
		&usageCurrency, &createdAt, &updatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read evaluation summary row: %w", err)
	}
	summary.Outcome = EvaluationOutcome(outcome)
	summary.TotalWallTime = time.Duration(totalWallNanos)
	summary.Usage = EvaluationUsage{
		Available:         usageAvailable != 0,
		UnavailableReason: usageReason,
		InputTokens:       usageInput,
		OutputTokens:      usageOutput,
		TotalTokens:       usageTotal,
		CostReported:      usageCostReported != 0,
		CostMicros:        usageCost,
		Currency:          usageCurrency,
	}
	if err := decodeEvaluationJSON("stage durations", stageDurationsJSON, &summary.StageDurations); err != nil {
		return nil, err
	}
	if err := decodeEvaluationJSON("invocation versions", invocationVersionsJSON, &summary.InvocationVersions); err != nil {
		return nil, err
	}
	if err := decodeEvaluationJSON("exemptions", exemptionsJSON, &summary.Exemptions); err != nil {
		return nil, err
	}
	if err := decodeEvaluationJSON("escalation categories", escalationJSON, &summary.EscalationCategories); err != nil {
		return nil, err
	}
	if err := decodeEvaluationJSON("repeated blockers", blockersJSON, &summary.RepeatedBlockers); err != nil {
		return nil, err
	}
	var err error
	if summary.StartedAt, err = parseEvaluationTime("started_at", startedAt); err != nil {
		return nil, err
	}
	if completedAt != "" {
		if summary.CompletedAt, err = parseEvaluationTime("completed_at", completedAt); err != nil {
			return nil, err
		}
	}
	if summary.CreatedAt, err = parseEvaluationTime("created_at", createdAt); err != nil {
		return nil, err
	}
	if summary.UpdatedAt, err = parseEvaluationTime("updated_at", updatedAt); err != nil {
		return nil, err
	}
	return &summary, nil
}

// normalizeEvaluationSummary validates a public summary and returns its SQL
// values in the exact order used by SaveEvaluationSummary.
func normalizeEvaluationSummary(summary EvaluationSummary) (EvaluationSummary, []any, error) {
	if !summary.Usage.Available && summary.Usage.UnavailableReason == "" {
		summary.Usage.UnavailableReason = EvaluationUsageNotReported
	}
	if err := validateEvaluationSummary(summary); err != nil {
		return EvaluationSummary{}, nil, err
	}
	now := time.Now().UTC()
	if summary.StartedAt.IsZero() {
		summary.StartedAt = now
	}
	if summary.CreatedAt.IsZero() {
		summary.CreatedAt = summary.StartedAt
	}
	if summary.UpdatedAt.IsZero() {
		summary.UpdatedAt = now
	}
	if summary.TotalWallTime == 0 && !summary.CompletedAt.IsZero() {
		summary.TotalWallTime = summary.CompletedAt.Sub(summary.StartedAt)
	}
	stageDurations, err := json.Marshal(nonNilStageDurations(summary.StageDurations))
	if err != nil {
		return EvaluationSummary{}, nil, fmt.Errorf("encode evaluation stage durations: %w", err)
	}
	versions, err := json.Marshal(nonNilInvocationVersions(summary.InvocationVersions))
	if err != nil {
		return EvaluationSummary{}, nil, fmt.Errorf("encode evaluation invocation versions: %w", err)
	}
	exemptions, err := json.Marshal(nonNilExemptions(summary.Exemptions))
	if err != nil {
		return EvaluationSummary{}, nil, fmt.Errorf("encode evaluation exemptions: %w", err)
	}
	escalations, err := json.Marshal(nonNilEscalations(summary.EscalationCategories))
	if err != nil {
		return EvaluationSummary{}, nil, fmt.Errorf("encode evaluation escalations: %w", err)
	}
	blockers, err := json.Marshal(nonNilBlockers(summary.RepeatedBlockers))
	if err != nil {
		return EvaluationSummary{}, nil, fmt.Errorf("encode evaluation blockers: %w", err)
	}
	completedAt := ""
	if !summary.CompletedAt.IsZero() {
		completedAt = summary.CompletedAt.UTC().Format(runTimestampLayout)
	}
	return summary, []any{
		summary.RunID,
		summary.Outcome,
		summary.StartedAt.UTC().Format(runTimestampLayout),
		completedAt,
		int64(summary.TotalWallTime),
		string(stageDurations),
		string(versions),
		summary.InvocationCount,
		summary.GateCount,
		summary.CheckRepairCount,
		summary.TestRevisionCount,
		summary.ReviewRevisionCount,
		summary.BudgetExhausted,
		string(exemptions),
		string(escalations),
		string(blockers),
		summary.Usage.Available,
		summary.Usage.UnavailableReason,
		summary.Usage.InputTokens,
		summary.Usage.OutputTokens,
		summary.Usage.TotalTokens,
		summary.Usage.CostReported,
		summary.Usage.CostMicros,
		summary.Usage.Currency,
		"",
		"",
		summary.CreatedAt.UTC().Format(runTimestampLayout),
		summary.UpdatedAt.UTC().Format(runTimestampLayout),
	}, nil
}

// validateEvaluationSummary validates the privacy-safe vocabulary and numeric
// bounds before any summary value reaches SQLite.
func validateEvaluationSummary(summary EvaluationSummary) error {
	if err := validateEvaluationRunID(summary.RunID); err != nil {
		return err
	}
	if err := validateEvaluationOutcome(summary.Outcome); err != nil {
		return err
	}
	if summary.TotalWallTime < 0 {
		return errors.New("evaluation total wall time must not be negative")
	}
	if !summary.CompletedAt.IsZero() && !summary.StartedAt.IsZero() && summary.CompletedAt.Before(summary.StartedAt) {
		return errors.New("evaluation completion time must not precede start time")
	}
	for _, stage := range summary.StageDurations {
		if err := validateEvaluationStage(stage.Stage); err != nil {
			return err
		}
		if stage.Duration < 0 {
			return fmt.Errorf("evaluation stage %q duration must not be negative", stage.Stage)
		}
	}
	for _, version := range summary.InvocationVersions {
		for field, value := range map[string]string{
			"invocation id":  version.InvocationID,
			"harness":        version.Harness,
			"model":          version.Model,
			"prompt version": version.PromptVersion,
			"worker version": version.WorkerVersion,
		} {
			if value != "" && !safeEvaluationValue(value) {
				return fmt.Errorf("evaluation %s contains unsafe metadata", field)
			}
		}
		if version.ReportSchemaVersion < 0 {
			return errors.New("evaluation report schema version must not be negative")
		}
	}
	for field, value := range map[string]int{
		"invocation count":      summary.InvocationCount,
		"gate count":            summary.GateCount,
		"check repair count":    summary.CheckRepairCount,
		"test revision count":   summary.TestRevisionCount,
		"review revision count": summary.ReviewRevisionCount,
	} {
		if value < 0 {
			return fmt.Errorf("evaluation %s must not be negative", field)
		}
	}
	for _, exemption := range summary.Exemptions {
		if !validEvaluationExemption(exemption) {
			return fmt.Errorf("unsupported evaluation exemption %q", exemption)
		}
	}
	for _, category := range summary.EscalationCategories {
		if !validEvaluationEscalation(category) {
			return fmt.Errorf("unsupported evaluation escalation category %q", category)
		}
	}
	for _, blocker := range summary.RepeatedBlockers {
		if !validEvaluationBlocker(blocker.Category) || blocker.Count < 1 {
			return fmt.Errorf("invalid evaluation blocker %q", blocker.Category)
		}
	}
	if err := validateEvaluationUsage(summary.Usage); err != nil {
		return err
	}
	return nil
}

// validateEvaluationUsage rejects guessed, negative, or malformed usage data.
func validateEvaluationUsage(usage EvaluationUsage) error {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.TotalTokens < 0 || usage.CostMicros < 0 {
		return errors.New("evaluation usage values must not be negative")
	}
	if !usage.Available {
		if usage.InputTokens != 0 || usage.OutputTokens != 0 || usage.TotalTokens != 0 || usage.CostMicros != 0 || usage.CostReported || usage.Currency != "" {
			return errors.New("unavailable evaluation usage must not contain measurements")
		}
		if usage.UnavailableReason == "" {
			usage.UnavailableReason = EvaluationUsageNotReported
		}
		if usage.UnavailableReason != EvaluationUsageNotReported && usage.UnavailableReason != EvaluationUsageMixedCurrency {
			return fmt.Errorf("unsupported evaluation usage unavailable reason %q", usage.UnavailableReason)
		}
		return nil
	}
	if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.TotalTokens == 0 && !usage.CostReported {
		return errors.New("available evaluation usage must contain a reported measurement")
	}
	if usage.CostReported {
		if usage.UnavailableReason != "" {
			return errors.New("reported evaluation cost must not have an unavailable reason")
		}
		if !validEvaluationCurrency(usage.Currency) {
			return errors.New("reported evaluation cost requires a three-letter uppercase currency")
		}
	} else {
		if usage.CostMicros != 0 || usage.Currency != "" {
			return errors.New("evaluation cost metadata requires cost_reported")
		}
		if usage.UnavailableReason != "" && usage.UnavailableReason != EvaluationUsageMixedCurrency && usage.UnavailableReason != EvaluationUsagePartialCost {
			return fmt.Errorf("unsupported evaluation usage unavailable reason %q", usage.UnavailableReason)
		}
	}
	return nil
}

// validEvaluationCurrency accepts only bounded uppercase currency metadata.
func validEvaluationCurrency(value string) bool {
	if len(value) != 3 || strings.ToUpper(value) != value {
		return false
	}
	for _, character := range value {
		if character < 'A' || character > 'Z' {
			return false
		}
	}
	return true
}

// validateEvaluationRunID validates an opaque run identifier without allowing
// content-bearing or path-like values into the projection.
func validateEvaluationRunID(value string) error {
	if !safeEvaluationValue(value) {
		return errors.New("evaluation run id is required and must be safe metadata")
	}
	return nil
}

// safeEvaluationValue accepts bounded, single-line metadata identifiers only.
func safeEvaluationValue(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 512 || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') {
			continue
		}
		switch character {
		case '-', '_', '.', ':', '/', '+', '@', '=':
		default:
			return false
		}
	}
	return true
}

// validateEvaluationOutcome validates the persisted outcome vocabulary.
func validateEvaluationOutcome(value EvaluationOutcome) error {
	switch value {
	case EvaluationOutcomeActive, EvaluationOutcomeWaitingForHuman, EvaluationOutcomeWaitingForHarness,
		EvaluationOutcomeFailed, EvaluationOutcomeCancelled, EvaluationOutcomeComplete:
		return nil
	default:
		return fmt.Errorf("unsupported evaluation outcome %q", value)
	}
}

// validateEvaluationStage validates the fixed coordinator stage vocabulary.
func validateEvaluationStage(value Stage) error {
	switch value {
	case StageClaim, StagePreflight, StageTest, StageImplementation, StageCheck, StageDraftPR, StageReview, StageReady:
		return nil
	default:
		return fmt.Errorf("unsupported evaluation stage %q", value)
	}
}

// validEvaluationExemption reports whether an exemption category is known.
func validEvaluationExemption(value EvaluationExemption) bool {
	return value == EvaluationExemptionHuman || value == EvaluationExemptionTechnical || value == EvaluationExemptionBaseline
}

// validEvaluationEscalation reports whether an escalation category is known.
func validEvaluationEscalation(value EvaluationEscalationCategory) bool {
	switch value {
	case EvaluationEscalationTestDispute, EvaluationEscalationReviewFinding, EvaluationEscalationClarification, EvaluationEscalationRuntime, EvaluationEscalationBudget, EvaluationEscalationBlocked:
		return true
	default:
		return false
	}
}

// validEvaluationBlocker reports whether a blocker category is known.
func validEvaluationBlocker(value EvaluationBlockerCategory) bool {
	switch value {
	case EvaluationBlockerSetup, EvaluationBlockerGate, EvaluationBlockerTest, EvaluationBlockerReview, EvaluationBlockerHarness, EvaluationBlockerBudget, EvaluationBlockerUnknown:
		return true
	default:
		return false
	}
}

// parseEvaluationTime parses the fixed-width timestamps used by the store.
func parseEvaluationTime(field, value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse evaluation %s: %w", field, err)
	}
	return parsed, nil
}

// decodeEvaluationJSON decodes a JSON projection column and rejects malformed
// persisted data instead of silently dropping evaluation evidence.
func decodeEvaluationJSON(field, value string, target any) error {
	if strings.TrimSpace(value) == "" {
		value = "[]"
	}
	if err := json.Unmarshal([]byte(value), target); err != nil {
		return fmt.Errorf("decode evaluation %s: %w", field, err)
	}
	return nil
}

// nonNilStageDurations keeps empty JSON arrays stable across persistence.
func nonNilStageDurations(value []EvaluationStageDuration) []EvaluationStageDuration {
	if value == nil {
		return []EvaluationStageDuration{}
	}
	return value
}

// nonNilInvocationVersions keeps empty JSON arrays stable across persistence.
func nonNilInvocationVersions(value []EvaluationInvocationVersion) []EvaluationInvocationVersion {
	if value == nil {
		return []EvaluationInvocationVersion{}
	}
	return value
}

// nonNilExemptions keeps empty JSON arrays stable across persistence.
func nonNilExemptions(value []EvaluationExemption) []EvaluationExemption {
	if value == nil {
		return []EvaluationExemption{}
	}
	return value
}

// nonNilEscalations keeps empty JSON arrays stable across persistence.
func nonNilEscalations(value []EvaluationEscalationCategory) []EvaluationEscalationCategory {
	if value == nil {
		return []EvaluationEscalationCategory{}
	}
	return value
}

// nonNilBlockers keeps empty JSON arrays stable across persistence.
func nonNilBlockers(value []EvaluationBlocker) []EvaluationBlocker {
	if value == nil {
		return []EvaluationBlocker{}
	}
	return value
}

// appendUniqueEvaluationString appends a category only when it is new.
func appendUniqueEvaluationString[T comparable](values []T, value T) []T {
	for _, current := range values {
		if current == value {
			return values
		}
	}
	return append(values, value)
}

// sortEvaluationStageDurations orders durations by the fixed workflow order.
func sortEvaluationStageDurations(values []EvaluationStageDuration) {
	order := map[Stage]int{StageClaim: 0, StagePreflight: 1, StageTest: 2, StageImplementation: 3, StageCheck: 4, StageDraftPR: 5, StageReview: 6, StageReady: 7}
	sort.Slice(values, func(i, j int) bool {
		if order[values[i].Stage] == order[values[j].Stage] {
			return values[i].Stage < values[j].Stage
		}
		return order[values[i].Stage] < order[values[j].Stage]
	})
}

// addEvaluationStageDuration adds elapsed time to one stage entry.
func addEvaluationStageDuration(summary *EvaluationSummary, stage Stage, duration time.Duration) {
	if duration <= 0 {
		return
	}
	for index := range summary.StageDurations {
		if summary.StageDurations[index].Stage == stage {
			summary.StageDurations[index].Duration += duration
			return
		}
	}
	summary.StageDurations = append(summary.StageDurations, EvaluationStageDuration{Stage: stage, Duration: duration})
	sortEvaluationStageDurations(summary.StageDurations)
}

// metadataHash produces the one-way identity used for human dispositions.
func metadataHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

// RecordEvaluationStageTransition closes the previous stage interval and
// starts the next one. It is idempotent for same-stage transitions and leaves
// all stage names content-free.
func (s *Store) RecordEvaluationStageTransition(ctx context.Context, runID string, from, to Stage, at time.Time) error {
	if err := validateEvaluationRunID(runID); err != nil {
		return err
	}
	if err := validateEvaluationStage(from); err != nil {
		return err
	}
	if err := validateEvaluationStage(to); err != nil {
		return err
	}
	if from == to {
		return nil
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	summary, err := s.EvaluationSummary(ctx, runID)
	if err != nil {
		return err
	}
	if summary == nil {
		return fmt.Errorf("evaluation summary %q does not exist", runID)
	}
	var activeStage, activeStartedAt string
	if err := s.db.QueryRowContext(ctx, `SELECT active_stage, active_stage_started_at FROM evaluation_summaries WHERE run_id = ?`, runID).Scan(&activeStage, &activeStartedAt); err != nil {
		return fmt.Errorf("read active evaluation stage %q: %w", runID, err)
	}
	if activeStage == "" {
		activeStage = string(from)
		activeStartedAt = summary.StartedAt.UTC().Format(runTimestampLayout)
	}
	if activeStartedAt != "" {
		startedAt, parseErr := time.Parse(time.RFC3339Nano, activeStartedAt)
		if parseErr != nil {
			return fmt.Errorf("parse active evaluation stage for %q: %w", runID, parseErr)
		}
		if at.After(startedAt) {
			addEvaluationStageDuration(summary, Stage(activeStage), at.Sub(startedAt))
		}
	}
	summary.UpdatedAt = at.UTC()
	stageJSON, err := json.Marshal(nonNilStageDurations(summary.StageDurations))
	if err != nil {
		return fmt.Errorf("encode stage durations for %q: %w", runID, err)
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE evaluation_summaries
		SET stage_durations = ?, active_stage = ?, active_stage_started_at = ?, updated_at = ?
		WHERE run_id = ?`, string(stageJSON), to, at.UTC().Format(runTimestampLayout), at.UTC().Format(runTimestampLayout), runID)
	if err != nil {
		return fmt.Errorf("record evaluation stage transition %q: %w", runID, err)
	}
	return nil
}

// RecordEvaluationInvocation records one invocation's safe version metadata
// and refreshes the invocation count from the operational invocation table.
func (s *Store) RecordEvaluationInvocation(ctx context.Context, runID string, invocation Invocation, workerVersion string, reportSchemaVersion int) error {
	if err := validateEvaluationRunID(runID); err != nil {
		return err
	}
	if invocation.ID == "" || invocation.RunID != runID {
		return errors.New("evaluation invocation identity does not match the run")
	}
	if reportSchemaVersion < 0 {
		return errors.New("evaluation report schema version must not be negative")
	}
	for field, value := range map[string]string{
		"harness":        invocation.Harness,
		"model":          invocation.Model,
		"prompt version": invocation.PromptVersion,
		"worker version": workerVersion,
	} {
		if value != "" && !safeEvaluationValue(value) {
			return fmt.Errorf("evaluation %s contains unsafe metadata", field)
		}
	}
	version := EvaluationInvocationVersion{
		InvocationID:        invocation.ID,
		Harness:             invocation.Harness,
		Model:               invocation.Model,
		PromptVersion:       invocation.PromptVersion,
		ReportSchemaVersion: reportSchemaVersion,
		WorkerVersion:       workerVersion,
	}
	if err := s.RefreshEvaluationCounts(ctx, runID); err != nil {
		return err
	}
	// Re-read summary after RefreshEvaluationCounts to get current gate_count and invocation_count
	summary, err := s.EvaluationSummary(ctx, runID)
	if err != nil {
		return err
	}
	if summary == nil {
		return fmt.Errorf("evaluation summary %q does not exist", runID)
	}
	// Apply the invocation version changes to the refreshed summary
	found := false
	for index := range summary.InvocationVersions {
		if summary.InvocationVersions[index].InvocationID == invocation.ID {
			summary.InvocationVersions[index] = version
			found = true
			break
		}
	}
	filteredVersions := make([]EvaluationInvocationVersion, 0, len(summary.InvocationVersions))
	for _, existing := range summary.InvocationVersions {
		if existing.InvocationID != "" {
			filteredVersions = append(filteredVersions, existing)
		}
	}
	summary.InvocationVersions = filteredVersions
	if !found {
		summary.InvocationVersions = append(summary.InvocationVersions, version)
	}
	summary.InvocationCount = len(summary.InvocationVersions)
	summary.UpdatedAt = time.Now().UTC()
	return s.updateEvaluationSummaryProjection(ctx, *summary)
}

// RefreshEvaluationCounts derives invocation and gate counts from the existing
// content-limited operational projections, never from work content.
func (s *Store) RefreshEvaluationCounts(ctx context.Context, runID string) error {
	if err := validateEvaluationRunID(runID); err != nil {
		return err
	}
	summary, err := s.EvaluationSummary(ctx, runID)
	if err != nil {
		return err
	}
	if summary == nil {
		return fmt.Errorf("evaluation summary %q does not exist", runID)
	}
	var invocationCount, uniqueGateCount, gateCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM invocations WHERE run_id = ?`, runID).Scan(&invocationCount); err != nil {
		return fmt.Errorf("count evaluation invocations for %q: %w", runID, err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM gate_results WHERE run_id = ?`, runID).Scan(&uniqueGateCount); err != nil {
		return fmt.Errorf("count evaluation gates for %q: %w", runID, err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT gate_count FROM evaluation_summaries WHERE run_id = ?`, runID).Scan(&gateCount); err != nil {
		return fmt.Errorf("read evaluation gate count for %q: %w", runID, err)
	}
	if uniqueGateCount > gateCount {
		gateCount = uniqueGateCount
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE evaluation_summaries
		SET invocation_count = ?, gate_count = ?, updated_at = ?
		WHERE run_id = ?`, invocationCount, gateCount, time.Now().UTC().Format(runTimestampLayout), runID)
	if err != nil {
		return fmt.Errorf("refresh evaluation counts for %q: %w", runID, err)
	}
	return nil
}

// RecordEvaluationGateRun increments the content-free count for one complete
// gate-suite execution, including a retry against an unchanged checkpoint.
func (s *Store) RecordEvaluationGateRun(ctx context.Context, runID string, count int) error {
	if err := validateEvaluationRunID(runID); err != nil {
		return err
	}
	if count <= 0 {
		return errors.New("evaluation gate run count must be positive")
	}
	if summary, err := s.EvaluationSummary(ctx, runID); err != nil {
		return err
	} else if summary == nil {
		return fmt.Errorf("evaluation summary %q does not exist", runID)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE evaluation_summaries
		SET gate_count = gate_count + ?, updated_at = ?
		WHERE run_id = ?`, count, time.Now().UTC().Format(runTimestampLayout), runID)
	if err != nil {
		return fmt.Errorf("record evaluation gate run for %q: %w", runID, err)
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr == nil && changed != 1 {
		return fmt.Errorf("evaluation summary %q does not exist", runID)
	}
	return nil
}

// RecordEvaluationAttempt increments one policy-relevant repair or revision
// counter. The category is a fixed vocabulary and no attempt detail is stored.
func (s *Store) RecordEvaluationAttempt(ctx context.Context, runID string, attempt EvaluationAttempt) error {
	if err := validateEvaluationRunID(runID); err != nil {
		return err
	}
	column := ""
	switch attempt {
	case EvaluationAttemptCheckRepair:
		column = "check_repair_count"
	case EvaluationAttemptTestRevision:
		column = "test_revision_count"
	case EvaluationAttemptReviewRevision:
		column = "review_revision_count"
	default:
		return fmt.Errorf("unsupported evaluation attempt %q", attempt)
	}
	if _, err := s.EvaluationSummary(ctx, runID); err != nil {
		return err
	}
	query := fmt.Sprintf("UPDATE evaluation_summaries SET %s = %s + 1, updated_at = ? WHERE run_id = ?", column, column)
	result, err := s.db.ExecContext(ctx, query, time.Now().UTC().Format(runTimestampLayout), runID)
	if err != nil {
		return fmt.Errorf("record evaluation attempt %q for %q: %w", attempt, runID, err)
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr == nil && changed != 1 {
		return fmt.Errorf("evaluation summary %q does not exist", runID)
	}
	return nil
}

// SetEvaluationOutcome updates the current non-terminal outcome without
// finalizing the summary. It keeps waiting-for-human and waiting-for-harness
// visible in local reporting while the run remains active.
func (s *Store) SetEvaluationOutcome(ctx context.Context, runID string, outcome EvaluationOutcome) error {
	if err := validateEvaluationRunID(runID); err != nil {
		return err
	}
	if err := validateEvaluationOutcome(outcome); err != nil {
		return err
	}
	if outcome != EvaluationOutcomeActive && outcome != EvaluationOutcomeWaitingForHuman && outcome != EvaluationOutcomeWaitingForHarness {
		return errors.New("SetEvaluationOutcome accepts only non-terminal outcomes")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE evaluation_summaries SET outcome = ?, updated_at = ? WHERE run_id = ?`, outcome, time.Now().UTC().Format(runTimestampLayout), runID)
	if err != nil {
		return fmt.Errorf("set evaluation outcome for %q: %w", runID, err)
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr == nil && changed != 1 {
		return fmt.Errorf("evaluation summary %q does not exist", runID)
	}
	return nil
}

// RecordEvaluationBudgetExhaustion records the fact of budget exhaustion
// without retaining a budget value, prompt, command, or other work content.
func (s *Store) RecordEvaluationBudgetExhaustion(ctx context.Context, runID string) error {
	if err := validateEvaluationRunID(runID); err != nil {
		return err
	}
	return s.setEvaluationBudgetExhausted(ctx, runID)
}

// setEvaluationBudgetExhausted applies the idempotent budget flag shared by
// direct signals and terminal finalization.
func (s *Store) setEvaluationBudgetExhausted(ctx context.Context, runID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE evaluation_summaries SET budget_exhausted = 1, updated_at = ? WHERE run_id = ?`, time.Now().UTC().Format(runTimestampLayout), runID)
	if err != nil {
		return fmt.Errorf("record evaluation budget exhaustion for %q: %w", runID, err)
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr == nil && changed != 1 {
		return fmt.Errorf("evaluation summary %q does not exist", runID)
	}
	return nil
}

// RecordEvaluationExemption records a stable exemption category once.
func (s *Store) RecordEvaluationExemption(ctx context.Context, runID string, exemption EvaluationExemption) error {
	if err := validateEvaluationRunID(runID); err != nil {
		return err
	}
	if !validEvaluationExemption(exemption) {
		return fmt.Errorf("unsupported evaluation exemption %q", exemption)
	}
	if err := s.RefreshEvaluationCounts(ctx, runID); err != nil {
		return err
	}
	// Re-read summary after RefreshEvaluationCounts to get current gate_count and invocation_count
	summary, err := s.EvaluationSummary(ctx, runID)
	if err != nil {
		return err
	}
	if summary == nil {
		return fmt.Errorf("evaluation summary %q does not exist", runID)
	}
	summary.Exemptions = appendUniqueEvaluationString(summary.Exemptions, exemption)
	summary.UpdatedAt = time.Now().UTC()
	return s.updateEvaluationSummaryProjection(ctx, *summary)
}

// RecordEvaluationEscalation records a stable escalation category once. The
// escalated text remains outside the evaluation projection.
func (s *Store) RecordEvaluationEscalation(ctx context.Context, runID string, category EvaluationEscalationCategory) error {
	if err := validateEvaluationRunID(runID); err != nil {
		return err
	}
	if !validEvaluationEscalation(category) {
		return fmt.Errorf("unsupported evaluation escalation category %q", category)
	}
	if err := s.RefreshEvaluationCounts(ctx, runID); err != nil {
		return err
	}
	// Re-read summary after RefreshEvaluationCounts to get current gate_count and invocation_count
	summary, err := s.EvaluationSummary(ctx, runID)
	if err != nil {
		return err
	}
	if summary == nil {
		return fmt.Errorf("evaluation summary %q does not exist", runID)
	}
	summary.EscalationCategories = appendUniqueEvaluationString(summary.EscalationCategories, category)
	summary.UpdatedAt = time.Now().UTC()
	return s.updateEvaluationSummaryProjection(ctx, *summary)
}

// RecordEvaluationBlocker increments a stable repeated-blocker category.
func (s *Store) RecordEvaluationBlocker(ctx context.Context, runID string, category EvaluationBlockerCategory) error {
	if err := validateEvaluationRunID(runID); err != nil {
		return err
	}
	if !validEvaluationBlocker(category) {
		return fmt.Errorf("unsupported evaluation blocker category %q", category)
	}
	if err := s.RefreshEvaluationCounts(ctx, runID); err != nil {
		return err
	}
	// Re-read summary after RefreshEvaluationCounts to get current gate_count and invocation_count
	summary, err := s.EvaluationSummary(ctx, runID)
	if err != nil {
		return err
	}
	if summary == nil {
		return fmt.Errorf("evaluation summary %q does not exist", runID)
	}
	found := false
	for index := range summary.RepeatedBlockers {
		if summary.RepeatedBlockers[index].Category == category {
			summary.RepeatedBlockers[index].Count++
			found = true
			break
		}
	}
	if !found {
		summary.RepeatedBlockers = append(summary.RepeatedBlockers, EvaluationBlocker{Category: category, Count: 1})
	}
	summary.UpdatedAt = time.Now().UTC()
	return s.updateEvaluationSummaryProjection(ctx, *summary)
}

// RecordEvaluationUsage stores one idempotent harness measurement and safely
// recomputes its summary. Missing measurements remain explicitly unavailable.
func (s *Store) RecordEvaluationUsage(ctx context.Context, runID, invocationID string, usage EvaluationUsage) error {
	if err := validateEvaluationRunID(runID); err != nil {
		return err
	}
	if !safeEvaluationValue(invocationID) {
		return errors.New("evaluation usage invocation id is required and must be safe metadata")
	}
	if err := validateEvaluationUsage(usage); err != nil {
		return err
	}
	if !usage.Available {
		return nil
	}
	if summary, err := s.EvaluationSummary(ctx, runID); err != nil {
		return err
	} else if summary == nil {
		return fmt.Errorf("evaluation summary %q does not exist", runID)
	}
	now := time.Now().UTC().Format(runTimestampLayout)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO evaluation_usage (
			run_id, invocation_id, input_tokens, output_tokens, total_tokens,
			cost_reported, cost_micros, currency, recorded_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, invocation_id) DO NOTHING`, runID, invocationID,
		usage.InputTokens, usage.OutputTokens, usage.TotalTokens, usage.CostReported,
		usage.CostMicros, usage.Currency, now)
	if err != nil {
		return fmt.Errorf("record evaluation usage for %q: %w", runID, err)
	}
	return s.refreshEvaluationUsage(ctx, runID)
}

// refreshEvaluationUsage sums only persisted harness measurements and marks
// mixed currencies unavailable instead of guessing a conversion.
func (s *Store) refreshEvaluationUsage(ctx context.Context, runID string) error {
	var rows, costRows, input, output, total, cost int64
	var currencies int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_micros), 0),
		COUNT(CASE WHEN cost_reported = 1 THEN 1 END),
		COUNT(DISTINCT CASE WHEN cost_reported = 1 THEN currency END)
		FROM evaluation_usage WHERE run_id = ?`, runID).Scan(&rows, &input, &output, &total, &cost, &costRows, &currencies); err != nil {
		return fmt.Errorf("aggregate evaluation usage for %q: %w", runID, err)
	}
	if rows == 0 {
		return nil
	}
	var currency string
	if currencies == 1 {
		if err := s.db.QueryRowContext(ctx, `SELECT currency FROM evaluation_usage WHERE run_id = ? AND cost_reported = 1 ORDER BY invocation_id LIMIT 1`, runID).Scan(&currency); err != nil {
			return fmt.Errorf("read evaluation usage currency for %q: %w", runID, err)
		}
	}
	usage := EvaluationUsage{Available: true, InputTokens: input, OutputTokens: output, TotalTokens: total}
	if currencies == 1 {
		if costRows == rows {
			usage.CostReported = true
			usage.CostMicros = cost
			usage.Currency = currency
		} else {
			usage.UnavailableReason = EvaluationUsagePartialCost
		}
	} else if currencies == 0 && costRows < rows {
		usage.UnavailableReason = EvaluationUsagePartialCost
	} else if currencies > 1 {
		if input == 0 && output == 0 && total == 0 {
			usage = EvaluationUsage{UnavailableReason: EvaluationUsageMixedCurrency}
		} else {
			usage.UnavailableReason = EvaluationUsageMixedCurrency
		}
	}
	if err := validateEvaluationUsage(usage); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE evaluation_summaries SET usage_available = ?, usage_unavailable_reason = ?,
		usage_input_tokens = ?, usage_output_tokens = ?, usage_total_tokens = ?,
		usage_cost_reported = ?, usage_cost_micros = ?, usage_currency = ?, updated_at = ?
		WHERE run_id = ?`, usage.Available, usage.UnavailableReason, usage.InputTokens,
		usage.OutputTokens, usage.TotalTokens, usage.CostReported, usage.CostMicros,
		usage.Currency, time.Now().UTC().Format(runTimestampLayout), runID)
	if err != nil {
		return fmt.Errorf("save aggregated evaluation usage for %q: %w", runID, err)
	}
	return nil
}

// FinalizeEvaluation closes the active stage and records the run outcome and
// wall time. It is safe to call repeatedly with the same terminal values.
func (s *Store) FinalizeEvaluation(ctx context.Context, runID string, outcome EvaluationOutcome, completedAt time.Time) error {
	if err := validateEvaluationRunID(runID); err != nil {
		return err
	}
	if err := validateEvaluationOutcome(outcome); err != nil {
		return err
	}
	if outcome == EvaluationOutcomeActive || outcome == EvaluationOutcomeWaitingForHuman || outcome == EvaluationOutcomeWaitingForHarness {
		return errors.New("evaluation finalization requires a terminal outcome")
	}
	if completedAt.IsZero() {
		completedAt = time.Now().UTC()
	}
	summary, err := s.EvaluationSummary(ctx, runID)
	if err != nil {
		return err
	}
	if summary == nil {
		return fmt.Errorf("evaluation summary %q does not exist", runID)
	}
	if err := s.RefreshEvaluationCounts(ctx, runID); err != nil {
		return err
	}
	if summary, err = s.EvaluationSummary(ctx, runID); err != nil {
		return err
	}
	if summary == nil {
		return fmt.Errorf("evaluation summary %q does not exist", runID)
	}
	var activeStage, activeStartedAt string
	if err := s.db.QueryRowContext(ctx, `SELECT active_stage, active_stage_started_at FROM evaluation_summaries WHERE run_id = ?`, runID).Scan(&activeStage, &activeStartedAt); err != nil {
		return fmt.Errorf("read active evaluation stage for finalization %q: %w", runID, err)
	}
	if activeStage != "" && activeStartedAt != "" {
		startedAt, parseErr := time.Parse(time.RFC3339Nano, activeStartedAt)
		if parseErr != nil {
			return fmt.Errorf("parse active evaluation stage for finalization %q: %w", runID, parseErr)
		}
		if completedAt.After(startedAt) {
			addEvaluationStageDuration(summary, Stage(activeStage), completedAt.Sub(startedAt))
		}
	}
	if completedAt.Before(summary.StartedAt) {
		return errors.New("evaluation completion time must not precede start time")
	}
	summary.Outcome = outcome
	summary.CompletedAt = completedAt.UTC()
	summary.TotalWallTime = completedAt.Sub(summary.StartedAt)
	summary.UpdatedAt = completedAt.UTC()
	stageJSON, err := json.Marshal(nonNilStageDurations(summary.StageDurations))
	if err != nil {
		return fmt.Errorf("encode final evaluation stage durations: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE evaluation_summaries
		SET outcome = ?, completed_at = ?, total_wall_nanos = ?, stage_durations = ?,
			active_stage = '', active_stage_started_at = '', updated_at = ?
		WHERE run_id = ?`, summary.Outcome, summary.CompletedAt.Format(runTimestampLayout),
		int64(summary.TotalWallTime), string(stageJSON), summary.UpdatedAt.Format(runTimestampLayout), runID)
	if err != nil {
		return fmt.Errorf("finalize evaluation summary %q: %w", runID, err)
	}
	return nil
}

// ReopenEvaluation marks a previously terminal summary active for an explicit
// retry while retaining its content-free historical counters.
func (s *Store) ReopenEvaluation(ctx context.Context, runID string, stage Stage, at time.Time) error {
	if err := validateEvaluationRunID(runID); err != nil {
		return err
	}
	if err := validateEvaluationStage(stage); err != nil {
		return err
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE evaluation_summaries
		SET outcome = ?, completed_at = '', active_stage = ?, active_stage_started_at = ?, updated_at = ?
		WHERE run_id = ?`, EvaluationOutcomeActive, stage, at.UTC().Format(runTimestampLayout), at.UTC().Format(runTimestampLayout), runID)
	if err != nil {
		return fmt.Errorf("reopen evaluation summary %q: %w", runID, err)
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr == nil && changed != 1 {
		return fmt.Errorf("evaluation summary %q does not exist", runID)
	}
	return nil
}

// updateEvaluationSummaryProjection updates the public summary columns while
// preserving the coordinator's internal active-stage cursor.
func (s *Store) updateEvaluationSummaryProjection(ctx context.Context, summary EvaluationSummary) error {
	normalized, values, err := normalizeEvaluationSummary(summary)
	if err != nil {
		return err
	}
	// The first 24 values are the public summary values through usage currency;
	// the final two are internal active-stage placeholders and the final two are
	// timestamps. This update intentionally leaves active_stage untouched.
	_, err = s.db.ExecContext(ctx, `
		UPDATE evaluation_summaries SET
			outcome = ?, started_at = ?, completed_at = ?, total_wall_nanos = ?,
			stage_durations = ?, invocation_versions = ?, invocation_count = ?,
			gate_count = ?, check_repair_count = ?, test_revision_count = ?,
			review_revision_count = ?, budget_exhausted = ?, exemptions = ?,
			escalation_categories = ?, repeated_blockers = ?, usage_available = ?,
			usage_unavailable_reason = ?, usage_input_tokens = ?, usage_output_tokens = ?,
			usage_total_tokens = ?, usage_cost_reported = ?, usage_cost_micros = ?,
			usage_currency = ?, updated_at = ?
		WHERE run_id = ?`, values[1], values[2], values[3], values[4], values[5], values[6],
		values[7], values[8], values[9], values[10], values[11], values[12], values[13],
		values[14], values[15], values[16], values[17], values[18], values[19], values[20],
		values[21], values[22], values[23], normalized.UpdatedAt.UTC().Format(runTimestampLayout), normalized.RunID)
	if err != nil {
		return fmt.Errorf("update evaluation summary %q: %w", normalized.RunID, err)
	}
	return nil
}

// SaveEvaluationDisposition hashes an opaque event identity and records a
// human disposition only for an already-recorded test dispute or review
// finding escalation.
func (s *Store) SaveEvaluationDisposition(ctx context.Context, request EvaluationDispositionRequest) (EvaluationDispositionRecord, error) {
	if err := validateEvaluationRunID(request.RunID); err != nil {
		return EvaluationDispositionRecord{}, err
	}
	if strings.TrimSpace(request.EventID) == "" || len(request.EventID) > 4096 || strings.ContainsAny(request.EventID, "\x00\r\n") {
		return EvaluationDispositionRecord{}, errors.New("evaluation disposition event identity is required and bounded")
	}
	if request.Category != EvaluationEscalationTestDispute && request.Category != EvaluationEscalationReviewFinding {
		return EvaluationDispositionRecord{}, errors.New("evaluation disposition category must be a test dispute or review finding")
	}
	switch request.Disposition {
	case EvaluationDispositionUpheld, EvaluationDispositionAdvisory, EvaluationDispositionOverturned:
	default:
		return EvaluationDispositionRecord{}, fmt.Errorf("unsupported evaluation disposition %q", request.Disposition)
	}
	summary, err := s.EvaluationSummary(ctx, request.RunID)
	if err != nil {
		return EvaluationDispositionRecord{}, err
	}
	if summary == nil {
		return EvaluationDispositionRecord{}, fmt.Errorf("evaluation summary %q does not exist", request.RunID)
	}
	escalated := false
	for _, category := range summary.EscalationCategories {
		if category == request.Category {
			escalated = true
			break
		}
	}
	if !escalated {
		return EvaluationDispositionRecord{}, fmt.Errorf("evaluation run %q has no %q escalation", request.RunID, request.Category)
	}
	decidedAt := request.DecidedAt.UTC()
	if decidedAt.IsZero() {
		decidedAt = time.Now().UTC()
	}
	record := EvaluationDispositionRecord{
		RunID:       request.RunID,
		EventIDHash: metadataHash(request.EventID),
		Category:    request.Category,
		Disposition: request.Disposition,
		DecidedAt:   decidedAt,
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO evaluation_dispositions (run_id, event_id_hash, category, disposition, decided_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(run_id, event_id_hash) DO UPDATE SET
			category = excluded.category, disposition = excluded.disposition, decided_at = excluded.decided_at`,
		record.RunID, record.EventIDHash, record.Category, record.Disposition, record.DecidedAt.Format(runTimestampLayout))
	if err != nil {
		return EvaluationDispositionRecord{}, fmt.Errorf("save evaluation disposition for %q: %w", request.RunID, err)
	}
	return record, nil
}

// RecordEvaluationDisposition is an explicit alias for callers that model a
// human disposition as an event rather than a saved record.
func (s *Store) RecordEvaluationDisposition(ctx context.Context, request EvaluationDispositionRequest) (EvaluationDispositionRecord, error) {
	return s.SaveEvaluationDisposition(ctx, request)
}

// ListEvaluationDispositions returns only hashed identities and fixed
// categories for one run.
func (s *Store) ListEvaluationDispositions(ctx context.Context, runID string) ([]EvaluationDispositionRecord, error) {
	if err := validateEvaluationRunID(runID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT run_id, event_id_hash, category, disposition, decided_at
		FROM evaluation_dispositions WHERE run_id = ? ORDER BY decided_at, event_id_hash`, runID)
	if err != nil {
		return nil, fmt.Errorf("list evaluation dispositions for %q: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]EvaluationDispositionRecord, 0)
	for rows.Next() {
		var record EvaluationDispositionRecord
		var decidedAt string
		if err := rows.Scan(&record.RunID, &record.EventIDHash, &record.Category, &record.Disposition, &decidedAt); err != nil {
			return nil, fmt.Errorf("scan evaluation disposition: %w", err)
		}
		var parseErr error
		if record.DecidedAt, parseErr = parseEvaluationTime("disposition decided_at", decidedAt); parseErr != nil {
			return nil, parseErr
		}
		result = append(result, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read evaluation dispositions for %q: %w", runID, err)
	}
	return result, nil
}

// FinalizeEvaluationWithBudget records a terminal outcome and its explicit
// budget-exhaustion signal. It is a convenience for coordinator paths that
// know the budget disposition at finalization time.
func (s *Store) FinalizeEvaluationWithBudget(ctx context.Context, runID string, outcome EvaluationOutcome, completedAt time.Time, budgetExhausted bool) error {
	if err := s.FinalizeEvaluation(ctx, runID, outcome, completedAt); err != nil {
		return err
	}
	if !budgetExhausted {
		return nil
	}
	return s.setEvaluationBudgetExhausted(ctx, runID)
}

// AggregateEvaluation computes local counts, rates, durations, dispositions,
// and available usage without contacting GitHub or any other network service.
func (s *Store) AggregateEvaluation(ctx context.Context) (EvaluationAggregate, error) {
	summaries, err := s.ListEvaluationSummaries(ctx, "")
	if err != nil {
		return EvaluationAggregate{}, err
	}
	aggregate := EvaluationAggregate{
		OutcomeCounts:     map[EvaluationOutcome]int{},
		OutcomeRates:      map[EvaluationOutcome]float64{},
		StageDurations:    map[Stage]time.Duration{},
		EscalationCounts:  map[EvaluationEscalationCategory]int{},
		EscalationRates:   map[EvaluationEscalationCategory]float64{},
		DispositionCounts: map[EvaluationDisposition]int{},
		Usage:             EvaluationUsage{UnavailableReason: EvaluationUsageNotReported},
	}
	aggregate.RunCount = len(summaries)
	var usageCurrency string
	var usageCurrencies = map[string]struct{}{}
	var usageCostIncomplete bool
	usageUnavailableReason := EvaluationUsageNotReported
	for _, summary := range summaries {
		aggregate.OutcomeCounts[summary.Outcome]++
		aggregate.TotalWallTime += summary.TotalWallTime
		aggregate.InvocationCount += summary.InvocationCount
		aggregate.GateCount += summary.GateCount
		aggregate.CheckRepairCount += summary.CheckRepairCount
		aggregate.TestRevisionCount += summary.TestRevisionCount
		aggregate.ReviewRevisionCount += summary.ReviewRevisionCount
		if summary.BudgetExhausted {
			aggregate.BudgetExhaustionRate++
		}
		if summary.Usage.Available {
			aggregate.UsageAvailableRuns++
			aggregate.Usage.InputTokens += summary.Usage.InputTokens
			aggregate.Usage.OutputTokens += summary.Usage.OutputTokens
			aggregate.Usage.TotalTokens += summary.Usage.TotalTokens
			if summary.Usage.CostReported {
				aggregate.UsageCostReportedRuns++
				usageCurrencies[summary.Usage.Currency] = struct{}{}
				usageCurrency = summary.Usage.Currency
				aggregate.Usage.CostMicros += summary.Usage.CostMicros
			} else {
				usageCostIncomplete = true
			}
		} else if summary.Usage.UnavailableReason != "" && summary.Usage.UnavailableReason != EvaluationUsageNotReported {
			usageUnavailableReason = summary.Usage.UnavailableReason
		}
		for _, duration := range summary.StageDurations {
			aggregate.StageDurations[duration.Stage] += duration.Duration
		}
		for _, category := range summary.EscalationCategories {
			aggregate.EscalationCounts[category]++
		}
	}
	if aggregate.RunCount > 0 {
		aggregate.AverageTotalWallTime = aggregate.TotalWallTime / time.Duration(aggregate.RunCount)
		aggregate.SuccessRate = float64(aggregate.OutcomeCounts[EvaluationOutcomeComplete]) / float64(aggregate.RunCount)
		aggregate.BudgetExhaustionRate /= float64(aggregate.RunCount)
		for outcome, count := range aggregate.OutcomeCounts {
			aggregate.OutcomeRates[outcome] = float64(count) / float64(aggregate.RunCount)
		}
		for category, count := range aggregate.EscalationCounts {
			aggregate.EscalationRates[category] = float64(count) / float64(aggregate.RunCount)
		}
	}
	if len(usageCurrencies) == 1 && !usageCostIncomplete {
		aggregate.Usage.Available = aggregate.UsageAvailableRuns > 0
		aggregate.Usage.UnavailableReason = ""
		aggregate.Usage.CostReported = true
		aggregate.Usage.Currency = usageCurrency
	} else if len(usageCurrencies) == 1 && usageCostIncomplete {
		aggregate.Usage.Available = aggregate.UsageAvailableRuns > 0
		aggregate.Usage.UnavailableReason = EvaluationUsagePartialCost
		aggregate.Usage.CostReported = false
		aggregate.Usage.CostMicros = 0
		aggregate.Usage.Currency = ""
	} else if len(usageCurrencies) > 1 {
		if aggregate.Usage.InputTokens == 0 && aggregate.Usage.OutputTokens == 0 && aggregate.Usage.TotalTokens == 0 {
			aggregate.Usage = EvaluationUsage{UnavailableReason: EvaluationUsageMixedCurrency}
		} else {
			aggregate.Usage.Available = aggregate.UsageAvailableRuns > 0
			aggregate.Usage.UnavailableReason = EvaluationUsageMixedCurrency
			aggregate.Usage.CostReported = false
			aggregate.Usage.CostMicros = 0
		}
	} else if aggregate.UsageAvailableRuns > 0 {
		aggregate.Usage.Available = true
		aggregate.Usage.UnavailableReason = EvaluationUsagePartialCost
	} else {
		aggregate.Usage.UnavailableReason = usageUnavailableReason
	}
	if rows, queryErr := s.db.QueryContext(ctx, `SELECT disposition, COUNT(*) FROM evaluation_dispositions GROUP BY disposition`); queryErr != nil {
		return EvaluationAggregate{}, fmt.Errorf("aggregate evaluation dispositions: %w", queryErr)
	} else {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var disposition EvaluationDisposition
			var count int
			if scanErr := rows.Scan(&disposition, &count); scanErr != nil {
				return EvaluationAggregate{}, fmt.Errorf("scan evaluation disposition aggregate: %w", scanErr)
			}
			aggregate.DispositionCounts[disposition] = count
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			return EvaluationAggregate{}, fmt.Errorf("read evaluation disposition aggregate: %w", rowsErr)
		}
	}
	return aggregate, nil
}

// DeleteEvaluationSummariesBefore deliberately removes only terminal
// evaluation projections completed before before. It never removes runs,
// invocation artifacts, logs, worktrees, or any other operational data.
func (s *Store) DeleteEvaluationSummariesBefore(ctx context.Context, before time.Time) (EvaluationDeletionResult, error) {
	if before.IsZero() {
		return EvaluationDeletionResult{}, errors.New("evaluation deletion cutoff is required")
	}
	cutoff := before.UTC().Format(runTimestampLayout)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EvaluationDeletionResult{}, fmt.Errorf("begin evaluation deletion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT run_id FROM evaluation_summaries WHERE completed_at <> '' AND completed_at < ?`, cutoff)
	if err != nil {
		return EvaluationDeletionResult{}, fmt.Errorf("select evaluation summaries for deletion: %w", err)
	}
	ids := make([]string, 0)
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			_ = rows.Close()
			return EvaluationDeletionResult{}, fmt.Errorf("scan evaluation deletion id: %w", err)
		}
		ids = append(ids, runID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return EvaluationDeletionResult{}, fmt.Errorf("read evaluation deletion ids: %w", err)
	}
	if err := rows.Close(); err != nil {
		return EvaluationDeletionResult{}, fmt.Errorf("close evaluation deletion ids: %w", err)
	}
	result := EvaluationDeletionResult{}
	for _, runID := range ids {
		usageResult, err := tx.ExecContext(ctx, `DELETE FROM evaluation_usage WHERE run_id = ?`, runID)
		if err != nil {
			return EvaluationDeletionResult{}, fmt.Errorf("delete evaluation usage for %q: %w", runID, err)
		}
		if count, countErr := usageResult.RowsAffected(); countErr == nil {
			result.UsageRows += int(count)
		}
		dispositionResult, err := tx.ExecContext(ctx, `DELETE FROM evaluation_dispositions WHERE run_id = ?`, runID)
		if err != nil {
			return EvaluationDeletionResult{}, fmt.Errorf("delete evaluation dispositions for %q: %w", runID, err)
		}
		if count, countErr := dispositionResult.RowsAffected(); countErr == nil {
			result.Dispositions += int(count)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM evaluation_summaries WHERE run_id = ?`, runID); err != nil {
			return EvaluationDeletionResult{}, fmt.Errorf("delete evaluation summary for %q: %w", runID, err)
		}
		result.Summaries++
	}
	if err := tx.Commit(); err != nil {
		return EvaluationDeletionResult{}, fmt.Errorf("commit evaluation deletion: %w", err)
	}
	return result, nil
}
