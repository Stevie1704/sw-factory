package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// ReviewRoundStatusPending means a manifest exists but no unit is running.
	ReviewRoundStatusPending = "pending"
	// ReviewRoundStatusActive means the round has launched at least one unit.
	ReviewRoundStatusActive = "active"
	// ReviewRoundStatusAwaitingAuthorization means the normal fan-out ceiling
	// was exceeded and a maintainer must authorize the exact manifest.
	ReviewRoundStatusAwaitingAuthorization = "awaiting_authorization"
	// ReviewRoundStatusComplete means both configured review axes are terminal.
	ReviewRoundStatusComplete = "complete"
	// ReviewRoundStatusError means the round is terminal but requires a human.
	ReviewRoundStatusError = "error"

	// ReviewUnitOutcomeCompleted means the unit supplied a complete review.
	ReviewUnitOutcomeCompleted = "completed"
	// ReviewUnitOutcomeNeedsClarification means the unit needs human input.
	ReviewUnitOutcomeNeedsClarification = "needs_clarification"
	// ReviewUnitOutcomeCannotProceed means the unit could not complete.
	ReviewUnitOutcomeCannotProceed = "cannot_proceed"
	// MaxAuthorizedReviewUnits is the factory installation ceiling for one
	// exact review manifest.
	MaxAuthorizedReviewUnits = 8
	// MaxReviewUnitFindings bounds the findings one review unit may retain, so
	// no single unit can consume the whole axis budget.
	MaxReviewUnitFindings = 16
)

// ReviewRound is the durable identity of one exact-checkpoint review round.
// Its manifest and artifact identity remain fixed for every unit and axis.
type ReviewRound struct {
	ID                 string
	RunID              string
	BaseCheckpointSHA  string
	CheckpointSHA      string
	DiffPath           string
	DiffBytes          int64
	DiffSHA256         string
	ManifestSHA256     string
	SchemaVersion      int
	PolicyVersion      string
	MaxUnitBytes       int
	MaxUnits           int
	ContextLines       int
	Concurrency        int
	AuthorizedMaxUnits int
	Status             string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// ReviewUnitSegment identifies one byte interval in the canonical round diff.
type ReviewUnitSegment struct {
	StartByte int `json:"start_byte"`
	EndByte   int `json:"end_byte"`
}

// ReviewUnitRange identifies one primary or bounded context source range.
type ReviewUnitRange struct {
	Path      string `json:"path"`
	Hunk      int    `json:"hunk"`
	Side      string `json:"side"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// ReviewUnit is one persisted workload assignment shared by both review axes.
type ReviewUnit struct {
	RoundID       string
	UnitID        string
	Ordinal       int
	WorkloadBytes int
	DiffSHA256    string
	Segments      []ReviewUnitSegment
	PrimaryRanges []ReviewUnitRange
	ContextRanges []ReviewUnitRange
	// PrimaryNonTextFiles lists the renames, mode changes, and binary summaries
	// this unit owns. Each is assigned to exactly one unit in the manifest.
	PrimaryNonTextFiles []string
	ChangedLines        int
}

// ReviewUnitResult is one accepted axis-specific result for one unit. It is
// idempotent by (round, role, unit) and retains the invocation identity used.
type ReviewUnitResult struct {
	RoundID       string
	Role          string
	UnitID        string
	InvocationID  string
	CheckpointSHA string
	Outcome       string
	Summary       string
	Questions     []ReviewQuestion
	Evidence      []ReviewEvidence
	Findings      []ReviewFinding
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ReviewRoundStore is the normalized persistence seam for partitioned reviews.
type ReviewRoundStore interface {
	SaveReviewRound(context.Context, ReviewRound) error
	ReviewRound(context.Context, string, string) (*ReviewRound, error)
	UpdateReviewRoundStatus(context.Context, string, string) error
	SaveReviewUnits(context.Context, string, []ReviewUnit) error
	ReviewUnits(context.Context, string) ([]ReviewUnit, error)
	SaveReviewUnitResult(context.Context, ReviewUnitResult) error
	ReviewUnitResult(context.Context, string, string, string) (*ReviewUnitResult, error)
	ReviewUnitResults(context.Context, string, string) ([]ReviewUnitResult, error)
	DeleteReviewRounds(context.Context, string) error
}

// ReviewManifestStore atomically persists the immutable round identity and its
// ordered units before any reviewer invocation can be activated.
type ReviewManifestStore interface {
	SaveReviewManifest(context.Context, ReviewRound, []ReviewUnit) error
}

// ReviewAuthorizationStore records explicit maintainer approval for a
// persisted manifest whose unit count exceeds the normal repository budget.
type ReviewAuthorizationStore interface {
	AuthorizeReviewRound(context.Context, string, int) error
}

// UpdateReviewRoundStatus changes only the coordinator-owned lifecycle status
// of an already immutable round identity.
func (s *Store) UpdateReviewRoundStatus(ctx context.Context, roundID, status string) error {
	if !safeQuestionIdentifier(roundID) {
		return errors.New("review round id is unsafe")
	}
	switch status {
	case ReviewRoundStatusPending, ReviewRoundStatusActive, ReviewRoundStatusAwaitingAuthorization, ReviewRoundStatusComplete, ReviewRoundStatusError:
	default:
		return fmt.Errorf("unsupported review round status %q", status)
	}
	var current string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM review_rounds WHERE id = ?`, roundID).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("review round %q was not found", roundID)
		}
		return fmt.Errorf("read review round status: %w", err)
	}
	if current == status {
		return nil
	}
	if current == ReviewRoundStatusComplete || current == ReviewRoundStatusError {
		return fmt.Errorf("review round %q is terminal with status %q", roundID, current)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE review_rounds SET status = ?, updated_at = ? WHERE id = ? AND status = ?`, status, time.Now().UTC().Format(runTimestampLayout), roundID, current)
	if err != nil {
		return fmt.Errorf("update review round status: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect review round status update: %w", err)
	}
	if changed != 1 {
		return fmt.Errorf("review round %q status changed during update", roundID)
	}
	return nil
}

// AuthorizeReviewRound approves additional fan-out for one immutable manifest.
// The approval only raises the persisted unit ceiling and never changes the
// manifest or its workload assignments.
func (s *Store) AuthorizeReviewRound(ctx context.Context, roundID string, maxUnits int) error {
	if !safeQuestionIdentifier(roundID) {
		return errors.New("review round id is unsafe")
	}
	if maxUnits <= 0 || maxUnits > MaxAuthorizedReviewUnits {
		return fmt.Errorf("authorized review units must be between one and %d", MaxAuthorizedReviewUnits)
	}
	var normal, authorized int
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT max_units, authorized_max_units, status FROM review_rounds WHERE id = ?`, roundID).Scan(&normal, &authorized, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("review round %q was not found", roundID)
		}
		return fmt.Errorf("read review round authorization: %w", err)
	}
	if maxUnits < normal {
		return fmt.Errorf("review authorization %d cannot lower normal fan-out %d", maxUnits, normal)
	}
	if status == ReviewRoundStatusComplete || status == ReviewRoundStatusError {
		return fmt.Errorf("review round %q is terminal with status %q", roundID, status)
	}
	if status != ReviewRoundStatusAwaitingAuthorization && authorized >= maxUnits {
		return nil
	}
	if status != ReviewRoundStatusAwaitingAuthorization {
		return fmt.Errorf("review round %q is not awaiting authorization", roundID)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE review_rounds SET authorized_max_units = ?, status = ?, updated_at = ? WHERE id = ?`, maxUnits, ReviewRoundStatusPending, time.Now().UTC().Format(runTimestampLayout), roundID); err != nil {
		return fmt.Errorf("authorize review round: %w", err)
	}
	return nil
}

// SaveReviewRound persists one immutable round identity. Re-saving the exact
// identity is idempotent; a conflicting round is a recovery discrepancy.
func (s *Store) SaveReviewRound(ctx context.Context, round ReviewRound) error {
	if err := validateReviewRound(round); err != nil {
		return err
	}
	if round.CreatedAt.IsZero() {
		round.CreatedAt = time.Now().UTC()
	}
	if round.UpdatedAt.IsZero() {
		round.UpdatedAt = round.CreatedAt
	}
	existing, err := s.ReviewRound(ctx, round.RunID, round.CheckpointSHA)
	if err != nil {
		return err
	}
	if existing != nil {
		if !sameReviewRoundIdentity(*existing, round) {
			return fmt.Errorf("review round for run %q and checkpoint %q conflicts with persisted identity", round.RunID, round.CheckpointSHA)
		}
		return nil
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO review_rounds (
			id, run_id, base_checkpoint_sha, checkpoint_sha, diff_path, diff_bytes,
			diff_sha256, manifest_sha256, schema_version, policy_version,
			max_unit_bytes, max_units, context_lines, concurrency, authorized_max_units, status,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		round.ID, round.RunID, round.BaseCheckpointSHA, round.CheckpointSHA,
		round.DiffPath, round.DiffBytes, round.DiffSHA256, round.ManifestSHA256,
		round.SchemaVersion, round.PolicyVersion, round.MaxUnitBytes, round.MaxUnits,
		round.ContextLines, round.Concurrency, round.AuthorizedMaxUnits, round.Status,
		round.CreatedAt.UTC().Format(runTimestampLayout), round.UpdatedAt.UTC().Format(runTimestampLayout))
	if err != nil {
		return fmt.Errorf("save review round: %w", err)
	}
	return nil
}

// SaveReviewManifest atomically persists a round and its complete immutable
// unit manifest. Repeating the exact manifest is idempotent; a changed or
// partial manifest is a recovery discrepancy.
func (s *Store) SaveReviewManifest(ctx context.Context, round ReviewRound, units []ReviewUnit) error {
	if err := validateReviewRound(round); err != nil {
		return err
	}
	if len(units) == 0 {
		return errors.New("review unit manifest must contain at least one unit")
	}
	units = reviewUnitsWithRoundID(round.ID, units)
	if round.CreatedAt.IsZero() {
		round.CreatedAt = time.Now().UTC()
	}
	if round.UpdatedAt.IsZero() {
		round.UpdatedAt = round.CreatedAt
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin review manifest: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	existing, err := scanReviewRound(tx.QueryRowContext(ctx, `SELECT id, run_id, base_checkpoint_sha,
		checkpoint_sha, diff_path, diff_bytes, diff_sha256, manifest_sha256,
		schema_version, policy_version, max_unit_bytes, max_units, context_lines,
		concurrency, authorized_max_units, status, created_at, updated_at FROM review_rounds
		WHERE run_id = ? AND checkpoint_sha = ? LIMIT 1`, round.RunID, round.CheckpointSHA))
	if err != nil {
		return fmt.Errorf("read existing review manifest: %w", err)
	}
	if existing != nil && !sameReviewRoundIdentity(*existing, round) {
		return fmt.Errorf("review round for run %q and checkpoint %q conflicts with persisted identity", round.RunID, round.CheckpointSHA)
	}
	if existing == nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_rounds (
			id, run_id, base_checkpoint_sha, checkpoint_sha, diff_path, diff_bytes,
			diff_sha256, manifest_sha256, schema_version, policy_version,
			max_unit_bytes, max_units, context_lines, concurrency, authorized_max_units, status,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			round.ID, round.RunID, round.BaseCheckpointSHA, round.CheckpointSHA,
			round.DiffPath, round.DiffBytes, round.DiffSHA256, round.ManifestSHA256,
			round.SchemaVersion, round.PolicyVersion, round.MaxUnitBytes, round.MaxUnits,
			round.ContextLines, round.Concurrency, round.AuthorizedMaxUnits, round.Status,
			round.CreatedAt.UTC().Format(runTimestampLayout), round.UpdatedAt.UTC().Format(runTimestampLayout)); err != nil {
			return fmt.Errorf("save review round in manifest: %w", err)
		}
	}
	if existing != nil {
		stored, err := reviewUnitsTx(ctx, tx, round.ID)
		if err != nil {
			return err
		}
		if !sameReviewUnits(stored, units) {
			return fmt.Errorf("review unit manifest for round %q conflicts with persisted units", round.ID)
		}
	} else if err := saveReviewUnitsTx(ctx, tx, round.ID, units); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit review manifest: %w", err)
	}
	return s.refreshEvaluationReviewMetricsIfPresent(ctx, round.RunID)
}

// ReviewRound loads the one round bound to a run and exact checkpoint.
func (s *Store) ReviewRound(ctx context.Context, runID, checkpointSHA string) (*ReviewRound, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, run_id, base_checkpoint_sha,
		checkpoint_sha, diff_path, diff_bytes, diff_sha256, manifest_sha256,
		schema_version, policy_version, max_unit_bytes, max_units, context_lines,
		concurrency, authorized_max_units, status, created_at, updated_at
		FROM review_rounds WHERE run_id = ? AND checkpoint_sha = ?
		ORDER BY created_at DESC, id DESC LIMIT 1`, runID, checkpointSHA)
	return scanReviewRound(row)
}

// SaveReviewUnits persists an immutable ordered manifest for a round.
func (s *Store) SaveReviewUnits(ctx context.Context, roundID string, units []ReviewUnit) error {
	if !safeQuestionIdentifier(roundID) {
		return errors.New("review unit round id is unsafe")
	}
	if len(units) == 0 {
		return errors.New("review unit manifest must contain at least one unit")
	}
	units = reviewUnitsWithRoundID(roundID, units)
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM review_rounds WHERE id = ?`, roundID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("review round %q was not found", roundID)
		}
		return fmt.Errorf("read review round for units: %w", err)
	}
	existing, err := s.ReviewUnits(ctx, roundID)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		if sameReviewUnits(existing, units) {
			return nil
		}
		return fmt.Errorf("review unit manifest for round %q conflicts with persisted units", roundID)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin review unit manifest: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := saveReviewUnitsTx(ctx, tx, roundID, units); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit review unit manifest: %w", err)
	}
	return nil
}

// saveReviewUnitsTx writes one complete ordered unit set inside an existing
// transaction and verifies that no unit row is silently missing.
func saveReviewUnitsTx(ctx context.Context, tx *sql.Tx, roundID string, units []ReviewUnit) error {
	var diffBytes int64
	var maxUnitBytes int
	if err := tx.QueryRowContext(ctx, `SELECT diff_bytes, max_unit_bytes FROM review_rounds WHERE id = ?`, roundID).Scan(&diffBytes, &maxUnitBytes); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("review round %q was not found", roundID)
		}
		return fmt.Errorf("read review round diff size: %w", err)
	}
	for index, unit := range units {
		unit.RoundID = roundID
		if err := validateReviewUnit(roundID, unit, index); err != nil {
			return err
		}
		if unit.WorkloadBytes > maxUnitBytes {
			return fmt.Errorf("review unit %q workload %d exceeds round limit %d", unit.UnitID, unit.WorkloadBytes, maxUnitBytes)
		}
		for segmentIndex, segment := range unit.Segments {
			if int64(segment.EndByte) > diffBytes {
				return fmt.Errorf("review unit %q segment %d exceeds round diff size", unit.UnitID, segmentIndex)
			}
		}
		segments, err := json.Marshal(unit.Segments)
		if err != nil {
			return fmt.Errorf("encode review unit %q segments: %w", unit.UnitID, err)
		}
		primary, err := json.Marshal(unit.PrimaryRanges)
		if err != nil {
			return fmt.Errorf("encode review unit %q primary ranges: %w", unit.UnitID, err)
		}
		contextRanges, err := json.Marshal(unit.ContextRanges)
		if err != nil {
			return fmt.Errorf("encode review unit %q context ranges: %w", unit.UnitID, err)
		}
		files, err := json.Marshal(unit.PrimaryNonTextFiles)
		if err != nil {
			return fmt.Errorf("encode review unit %q primary files: %w", unit.UnitID, err)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO review_units (
				round_id, unit_id, ordinal, workload_bytes, diff_sha256,
				segments, primary_ranges, context_ranges, primary_non_text_files, changed_lines
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(round_id, unit_id) DO UPDATE SET
				ordinal = excluded.ordinal,
				workload_bytes = excluded.workload_bytes,
				diff_sha256 = excluded.diff_sha256,
				segments = excluded.segments,
				primary_ranges = excluded.primary_ranges,
				context_ranges = excluded.context_ranges,
				primary_non_text_files = excluded.primary_non_text_files,
				changed_lines = excluded.changed_lines`,
			roundID, unit.UnitID, unit.Ordinal, unit.WorkloadBytes, unit.DiffSHA256,
			string(segments), string(primary), string(contextRanges), string(files), unit.ChangedLines)
		if err != nil {
			return fmt.Errorf("save review unit %q: %w", unit.UnitID, err)
		}
	}
	return verifyStoredUnitOrdinals(ctx, tx, roundID, len(units))
}

// reviewUnitsTx reads a round's units through the caller-owned transaction.
func reviewUnitsTx(ctx context.Context, tx *sql.Tx, roundID string) ([]ReviewUnit, error) {
	rows, err := tx.QueryContext(ctx, `SELECT round_id, unit_id, ordinal,
		workload_bytes, diff_sha256, segments, primary_ranges, context_ranges,
		primary_non_text_files, changed_lines FROM review_units WHERE round_id = ?
		ORDER BY ordinal`, roundID)
	if err != nil {
		return nil, fmt.Errorf("read review units in manifest: %w", err)
	}
	defer func() { _ = rows.Close() }()
	units := make([]ReviewUnit, 0)
	for rows.Next() {
		unit, scanErr := scanReviewUnit(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		units = append(units, *unit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read review units in manifest: %w", err)
	}
	return units, nil
}

// ReviewUnits loads the ordered immutable units for one round.
func (s *Store) ReviewUnits(ctx context.Context, roundID string) ([]ReviewUnit, error) {
	if !safeQuestionIdentifier(roundID) {
		return nil, errors.New("review unit round id is unsafe")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT round_id, unit_id, ordinal,
		workload_bytes, diff_sha256, segments, primary_ranges, context_ranges,
		primary_non_text_files, changed_lines FROM review_units WHERE round_id = ?
		ORDER BY ordinal`, roundID)
	if err != nil {
		return nil, fmt.Errorf("read review units: %w", err)
	}
	defer func() { _ = rows.Close() }()
	units := make([]ReviewUnit, 0)
	for rows.Next() {
		unit, scanErr := scanReviewUnit(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		units = append(units, *unit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read review units: %w", err)
	}
	return units, nil
}

// SaveReviewUnitResult persists one accepted result idempotently. A repeated
// identical result is a no-op; a conflicting result fails closed.
func (s *Store) SaveReviewUnitResult(ctx context.Context, result ReviewUnitResult) error {
	if err := validateReviewUnitResult(result); err != nil {
		return err
	}
	var expectedCheckpoint string
	if err := s.db.QueryRowContext(ctx, `SELECT r.checkpoint_sha FROM review_rounds r
		JOIN review_units u ON u.round_id = r.id AND u.unit_id = ?
		WHERE r.id = ?`, result.UnitID, result.RoundID).Scan(&expectedCheckpoint); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("review unit %s/%s is not in a persisted round", result.RoundID, result.UnitID)
		}
		return fmt.Errorf("read review unit checkpoint: %w", err)
	}
	if expectedCheckpoint != result.CheckpointSHA {
		return fmt.Errorf("review unit result checkpoint %q does not match round checkpoint %q", result.CheckpointSHA, expectedCheckpoint)
	}
	existing, err := s.ReviewUnitResult(ctx, result.RoundID, result.Role, result.UnitID)
	if err != nil {
		return err
	}
	if existing != nil {
		if !sameReviewUnitResult(*existing, result) {
			return fmt.Errorf("review unit result %s/%s/%s conflicts with persisted result", result.RoundID, result.Role, result.UnitID)
		}
		return s.refreshEvaluationReviewMetricsForRound(ctx, result.RoundID)
	}
	if result.CreatedAt.IsZero() {
		result.CreatedAt = time.Now().UTC()
	}
	if result.UpdatedAt.IsZero() {
		result.UpdatedAt = result.CreatedAt
	}
	questions, err := json.Marshal(result.Questions)
	if err != nil {
		return fmt.Errorf("encode review unit questions: %w", err)
	}
	evidence, err := json.Marshal(result.Evidence)
	if err != nil {
		return fmt.Errorf("encode review unit evidence: %w", err)
	}
	findings, err := json.Marshal(result.Findings)
	if err != nil {
		return fmt.Errorf("encode review unit findings: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO review_unit_results (
		round_id, role, unit_id, invocation_id, checkpoint_sha, outcome, summary,
		questions, evidence, findings, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(round_id, role, unit_id) DO NOTHING`, result.RoundID, result.Role,
		result.UnitID, result.InvocationID, result.CheckpointSHA, result.Outcome,
		result.Summary, string(questions), string(evidence), string(findings),
		result.CreatedAt.UTC().Format(runTimestampLayout), result.UpdatedAt.UTC().Format(runTimestampLayout))
	if err != nil {
		return fmt.Errorf("save review unit result: %w", err)
	}
	existing, err = s.ReviewUnitResult(ctx, result.RoundID, result.Role, result.UnitID)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("review unit result %s/%s/%s was not persisted", result.RoundID, result.Role, result.UnitID)
	}
	if !sameReviewUnitResult(*existing, result) {
		return fmt.Errorf("review unit result %s/%s/%s conflicts with persisted result", result.RoundID, result.Role, result.UnitID)
	}
	return s.refreshEvaluationReviewMetricsForRound(ctx, result.RoundID)
}

// ReviewUnitResult loads one result by its normalized identity.
func (s *Store) ReviewUnitResult(ctx context.Context, roundID, role, unitID string) (*ReviewUnitResult, error) {
	row := s.db.QueryRowContext(ctx, `SELECT round_id, role, unit_id,
		invocation_id, checkpoint_sha, outcome, summary, questions, evidence,
		findings, created_at, updated_at FROM review_unit_results
		WHERE round_id = ? AND role = ? AND unit_id = ?`, roundID, role, unitID)
	return scanReviewUnitResult(row)
}

// ReviewUnitResults loads all results for one axis in deterministic unit order.
func (s *Store) ReviewUnitResults(ctx context.Context, roundID, role string) ([]ReviewUnitResult, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.round_id, r.role, r.unit_id,
		r.invocation_id, r.checkpoint_sha, r.outcome, r.summary, r.questions,
		r.evidence, r.findings, r.created_at, r.updated_at
		FROM review_unit_results r JOIN review_units u ON u.round_id = r.round_id
		AND u.unit_id = r.unit_id WHERE r.round_id = ? AND r.role = ?
		ORDER BY u.ordinal`, roundID, role)
	if err != nil {
		return nil, fmt.Errorf("read review unit results: %w", err)
	}
	defer func() { _ = rows.Close() }()
	results := make([]ReviewUnitResult, 0)
	for rows.Next() {
		result, scanErr := scanReviewUnitResult(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		results = append(results, *result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read review unit results: %w", err)
	}
	return results, nil
}

// refreshEvaluationReviewMetricsForRound refreshes telemetry for the run that
// owns a normalized review round when an evaluation summary exists.
func (s *Store) refreshEvaluationReviewMetricsForRound(ctx context.Context, roundID string) error {
	var runID string
	if err := s.db.QueryRowContext(ctx, `SELECT run_id FROM review_rounds WHERE id = ?`, roundID).Scan(&runID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("read review round run for evaluation: %w", err)
	}
	return s.refreshEvaluationReviewMetricsIfPresent(ctx, runID)
}

// DeleteReviewRounds removes round projections when a run's checkpoint or
// specification packet is invalidated. No review result survives that boundary.
func (s *Store) DeleteReviewRounds(ctx context.Context, runID string) error {
	if !safeQuestionIdentifier(runID) {
		return errors.New("review round run id is unsafe")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin review round deletion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM review_unit_results WHERE round_id IN (SELECT id FROM review_rounds WHERE run_id = ?)`, runID); err != nil {
		return fmt.Errorf("delete review unit results: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM review_units WHERE round_id IN (SELECT id FROM review_rounds WHERE run_id = ?)`, runID); err != nil {
		return fmt.Errorf("delete review units: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM review_rounds WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("delete review rounds: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit review round deletion: %w", err)
	}
	return nil
}

// validateReviewRound enforces immutable round identity, digest, and lifecycle
// bounds before a row can be persisted.
func validateReviewRound(round ReviewRound) error {
	for field, value := range map[string]string{"id": round.ID, "run id": round.RunID, "policy version": round.PolicyVersion, "status": round.Status} {
		if !safeQuestionIdentifier(value) {
			return fmt.Errorf("review round %s is unsafe", field)
		}
	}
	if !validGateCheckpointSHA(round.BaseCheckpointSHA) || !validGateCheckpointSHA(round.CheckpointSHA) || len(round.DiffSHA256) != 64 || !isLowerHex(round.DiffSHA256) || len(round.ManifestSHA256) != 64 || !isLowerHex(round.ManifestSHA256) {
		return errors.New("review round checkpoint and digest identities are invalid")
	}
	if round.DiffBytes < 0 || round.SchemaVersion <= 0 || round.MaxUnitBytes <= 0 || round.MaxUnits <= 0 || round.MaxUnits > MaxAuthorizedReviewUnits || round.ContextLines < 0 || round.Concurrency <= 0 || round.AuthorizedMaxUnits < 0 || round.AuthorizedMaxUnits > MaxAuthorizedReviewUnits {
		return errors.New("review round bounds are invalid")
	}
	if round.AuthorizedMaxUnits > 0 && round.AuthorizedMaxUnits < round.MaxUnits {
		return errors.New("review round authorization cannot be below normal fan-out")
	}
	if round.DiffPath == "" {
		return errors.New("review round diff path is required")
	}
	switch round.Status {
	case ReviewRoundStatusPending, ReviewRoundStatusActive, ReviewRoundStatusAwaitingAuthorization, ReviewRoundStatusComplete, ReviewRoundStatusError:
	default:
		return fmt.Errorf("unsupported review round status %q", round.Status)
	}
	return nil
}

// validateReviewUnit enforces one ordered unit's identity, ranges, and exact
// workload accounting.
func validateReviewUnit(roundID string, unit ReviewUnit, index int) error {
	if unit.RoundID != "" && unit.RoundID != roundID {
		return fmt.Errorf("review unit %q belongs to another round", unit.UnitID)
	}
	changedLines := 0
	for _, value := range unit.PrimaryRanges {
		changedLines += value.EndLine - value.StartLine + 1
	}
	if !safeQuestionIdentifier(unit.UnitID) || unit.Ordinal != index+1 || unit.WorkloadBytes < 0 || unit.ChangedLines != changedLines || !isLowerHex(unit.DiffSHA256) || len(unit.DiffSHA256) != 64 {
		return fmt.Errorf("review unit %q has invalid identity or workload", unit.UnitID)
	}
	segmentBytes := 0
	previousEnd := -1
	for _, segment := range unit.Segments {
		if segment.StartByte < 0 || segment.EndByte < segment.StartByte {
			return fmt.Errorf("review unit %q has invalid byte segments", unit.UnitID)
		}
		if previousEnd > segment.StartByte {
			return fmt.Errorf("review unit %q has overlapping byte segments", unit.UnitID)
		}
		segmentBytes += segment.EndByte - segment.StartByte
		previousEnd = segment.EndByte
	}
	if segmentBytes != unit.WorkloadBytes {
		return fmt.Errorf("review unit %q workload is %d, segments total %d", unit.UnitID, unit.WorkloadBytes, segmentBytes)
	}
	for _, value := range append(append([]ReviewUnitRange(nil), unit.PrimaryRanges...), unit.ContextRanges...) {
		if value.Path == "" || value.Hunk <= 0 || value.StartLine <= 0 || value.EndLine < value.StartLine {
			return fmt.Errorf("review unit %q has an invalid source range", unit.UnitID)
		}
		switch value.Side {
		case "old", "new", "both":
		default:
			return fmt.Errorf("review unit %q has an invalid source range", unit.UnitID)
		}
	}
	return nil
}

// validateReviewUnitResult enforces the bounded outcome payload and finding
// ownership contract for one axis/unit result.
func validateReviewUnitResult(result ReviewUnitResult) error {
	for field, value := range map[string]string{"round id": result.RoundID, "role": result.Role, "unit id": result.UnitID, "invocation id": result.InvocationID} {
		if !safeQuestionIdentifier(value) {
			return fmt.Errorf("review unit result %s is unsafe", field)
		}
	}
	if result.Role != "spec_review" && result.Role != "standards_review" {
		return fmt.Errorf("review unit result role %q is not a review axis", result.Role)
	}
	if !validGateCheckpointSHA(result.CheckpointSHA) {
		return errors.New("review unit result checkpoint SHA is invalid")
	}
	switch result.Outcome {
	case ReviewUnitOutcomeCompleted, ReviewUnitOutcomeNeedsClarification, ReviewUnitOutcomeCannotProceed:
	default:
		return fmt.Errorf("unsupported review unit result outcome %q", result.Outcome)
	}
	if len(result.Questions) > MaxReviewQuestions || len(result.Evidence) > MaxReviewEvidence || len(result.Findings) > MaxReviewUnitFindings {
		return errors.New("review unit result exceeds its bounded field limit")
	}
	switch result.Outcome {
	case ReviewUnitOutcomeCompleted:
		if len(result.Questions) != 0 || len(result.Evidence) != 0 {
			return errors.New("completed review unit result must not retain questions or evidence")
		}
	case ReviewUnitOutcomeNeedsClarification:
		if len(result.Questions) == 0 || len(result.Evidence) != 0 {
			return errors.New("review unit clarification must retain questions only")
		}
	case ReviewUnitOutcomeCannotProceed:
		if len(result.Evidence) == 0 || len(result.Questions) != 0 {
			return errors.New("review unit cannot-proceed result must retain evidence only")
		}
	}
	if len(result.Summary) > MaxReviewSummaryBytes || strings.ContainsAny(result.Summary, "\x00\r\n") {
		return errors.New("review unit result summary is invalid")
	}
	seenQuestions := make(map[string]struct{}, len(result.Questions))
	for _, question := range result.Questions {
		if !safeQuestionIdentifier(question.ID) || strings.TrimSpace(question.Prompt) == "" || strings.ContainsAny(question.Prompt, "\x00\r\n") {
			return errors.New("review unit result question is invalid")
		}
		if _, exists := seenQuestions[question.ID]; exists {
			return fmt.Errorf("review unit result question %q is duplicated", question.ID)
		}
		seenQuestions[question.ID] = struct{}{}
	}
	for _, evidence := range result.Evidence {
		if strings.TrimSpace(evidence.Kind) == "" || strings.TrimSpace(evidence.Detail) == "" || strings.ContainsAny(evidence.Kind+evidence.Detail, "\x00\r\n") {
			return errors.New("review unit result evidence is invalid")
		}
	}
	for index, finding := range result.Findings {
		if err := validateReviewFinding("review unit result", index, finding); err != nil {
			return err
		}
		if finding.UnitID != result.UnitID {
			return fmt.Errorf("review unit result finding %d belongs to unit %q, want %q", index, finding.UnitID, result.UnitID)
		}
	}
	return nil
}

// sameReviewRoundIdentity compares immutable manifest fields, excluding
// coordinator-owned lifecycle status, authorization, and timestamps.
func sameReviewRoundIdentity(left, right ReviewRound) bool {
	left.CreatedAt = time.Time{}
	left.UpdatedAt = time.Time{}
	right.CreatedAt = time.Time{}
	right.UpdatedAt = time.Time{}
	return left.ID == right.ID && left.RunID == right.RunID && left.BaseCheckpointSHA == right.BaseCheckpointSHA && left.CheckpointSHA == right.CheckpointSHA && left.DiffPath == right.DiffPath && left.DiffBytes == right.DiffBytes && left.DiffSHA256 == right.DiffSHA256 && left.ManifestSHA256 == right.ManifestSHA256 && left.SchemaVersion == right.SchemaVersion && left.PolicyVersion == right.PolicyVersion && left.MaxUnitBytes == right.MaxUnitBytes && left.MaxUnits == right.MaxUnits && left.ContextLines == right.ContextLines && left.Concurrency == right.Concurrency
}

// sameReviewUnits compares normalized ordered unit manifests byte-for-byte.
func sameReviewUnits(left, right []ReviewUnit) bool {
	if len(left) != len(right) {
		return false
	}
	leftData, leftErr := json.Marshal(left)
	rightData, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftData) == string(rightData)
}

// reviewUnitsWithRoundID normalizes caller-provided units to their persisted
// parent round without mutating the caller's slice.
func reviewUnitsWithRoundID(roundID string, units []ReviewUnit) []ReviewUnit {
	normalized := make([]ReviewUnit, len(units))
	copy(normalized, units)
	for index := range normalized {
		normalized[index].RoundID = roundID
	}
	return normalized
}

// sameReviewUnitResult compares immutable result content while ignoring store
// timestamps assigned at first persistence.
func sameReviewUnitResult(left, right ReviewUnitResult) bool {
	left.CreatedAt = time.Time{}
	left.UpdatedAt = time.Time{}
	right.CreatedAt = time.Time{}
	right.UpdatedAt = time.Time{}
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) == string(rightJSON)
}

// verifyStoredUnitOrdinals confirms that one transaction contains exactly the
// unit count supplied by its manifest.
func verifyStoredUnitOrdinals(ctx context.Context, tx *sql.Tx, roundID string, count int) error {
	var actual int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_units WHERE round_id = ?`, roundID).Scan(&actual); err != nil {
		return fmt.Errorf("count stored review units: %w", err)
	}
	if actual != count {
		return fmt.Errorf("review unit manifest for round %q has %d units, want %d", roundID, actual, count)
	}
	return nil
}

// scanReviewRound decodes one SQLite round row and its RFC3339 timestamps.
func scanReviewRound(row interface{ Scan(...any) error }) (*ReviewRound, error) {
	var round ReviewRound
	var createdAt, updatedAt string
	if err := row.Scan(&round.ID, &round.RunID, &round.BaseCheckpointSHA, &round.CheckpointSHA, &round.DiffPath, &round.DiffBytes, &round.DiffSHA256, &round.ManifestSHA256, &round.SchemaVersion, &round.PolicyVersion, &round.MaxUnitBytes, &round.MaxUnits, &round.ContextLines, &round.Concurrency, &round.AuthorizedMaxUnits, &round.Status, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read review round: %w", err)
	}
	var err error
	round.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse review round created_at: %w", err)
	}
	round.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return nil, fmt.Errorf("parse review round updated_at: %w", err)
	}
	return &round, nil
}

// scanReviewUnit decodes one normalized unit row and its JSON range columns.
func scanReviewUnit(row interface{ Scan(...any) error }) (*ReviewUnit, error) {
	var unit ReviewUnit
	var segmentsJSON, primaryJSON, contextJSON, filesJSON string
	if err := row.Scan(&unit.RoundID, &unit.UnitID, &unit.Ordinal, &unit.WorkloadBytes, &unit.DiffSHA256, &segmentsJSON, &primaryJSON, &contextJSON, &filesJSON, &unit.ChangedLines); err != nil {
		return nil, fmt.Errorf("read review unit: %w", err)
	}
	for _, value := range []struct {
		value string
		dest  any
	}{{segmentsJSON, &unit.Segments}, {primaryJSON, &unit.PrimaryRanges}, {contextJSON, &unit.ContextRanges}, {filesJSON, &unit.PrimaryNonTextFiles}} {
		if err := json.Unmarshal([]byte(value.value), value.dest); err != nil {
			return nil, fmt.Errorf("decode review unit manifest: %w", err)
		}
	}
	return &unit, nil
}

// scanReviewUnitResult decodes one normalized axis result and its bounded JSON
// payload columns.
func scanReviewUnitResult(row interface{ Scan(...any) error }) (*ReviewUnitResult, error) {
	var result ReviewUnitResult
	var questionsJSON, evidenceJSON, findingsJSON, createdAt, updatedAt string
	if err := row.Scan(&result.RoundID, &result.Role, &result.UnitID, &result.InvocationID, &result.CheckpointSHA, &result.Outcome, &result.Summary, &questionsJSON, &evidenceJSON, &findingsJSON, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read review unit result: %w", err)
	}
	for _, value := range []struct {
		value string
		dest  any
	}{{questionsJSON, &result.Questions}, {evidenceJSON, &result.Evidence}, {findingsJSON, &result.Findings}} {
		if err := json.Unmarshal([]byte(value.value), value.dest); err != nil {
			return nil, fmt.Errorf("decode review unit result: %w", err)
		}
	}
	var err error
	result.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse review unit result created_at: %w", err)
	}
	result.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return nil, fmt.Errorf("parse review unit result updated_at: %w", err)
	}
	return &result, nil
}
