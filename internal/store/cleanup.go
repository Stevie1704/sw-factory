package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrCleanupNotEligible identifies a run that changed state or is newer than
// the cleanup cutoff after a plan was created.
var ErrCleanupNotEligible = errors.New("run is not cleanup eligible")

// RunRemovalCandidate is one persisted run and the operational records the
// coordinator needs to plan safe external removal of its local resources. Both
// the seven-day cleanup and the whole-installation reset select from it, so it
// is named for neither.
type RunRemovalCandidate struct {
	// Run is the current persisted run projection.
	Run Run
	// Invocations contains every persisted invocation for the run.
	Invocations []Invocation
	// PendingEffect is the unresolved external mutation, when present.
	PendingEffect *PendingEffect
}

// CleanupDeletionResult reports operational rows deleted for one run. Local
// evaluation-summary rows are intentionally absent because cleanup never
// removes that separately retained projection.
type CleanupDeletionResult struct {
	// RunID identifies the deleted run.
	RunID string
	// Runs is one when the operational run row was deleted.
	Runs int
	// Invocations counts deleted harness invocation rows.
	Invocations int
	// GateResults counts deleted deterministic gate-result rows.
	GateResults int
	// PendingEffects counts deleted resolved-effect reservations.
	PendingEffects int
}

// CleanupTransaction reserves one terminal run for external artifact cleanup
// and then deletes its operational rows. The reservation holds the SQLite
// write transaction open while the Git and worker adapters remove resources,
// preventing the run from being reopened between planning and deletion.
type CleanupTransaction interface {
	// Delete removes the reserved run's operational rows and leaves separately
	// retained evaluation summaries untouched.
	Delete(context.Context) (CleanupDeletionResult, error)
	// Commit makes the operational deletion durable.
	Commit() error
	// Rollback releases the reservation and preserves operational rows.
	Rollback() error
}

// cleanupTransaction is the SQLite implementation of CleanupTransaction.
type cleanupTransaction struct {
	tx      *sql.Tx
	runID   string
	exists  bool
	deleted bool
}

// CleanupNotEligibleError carries the run identity that failed the final
// eligibility guard during deletion.
type CleanupNotEligibleError struct {
	// RunID identifies the run that was no longer eligible.
	RunID string
}

// Error describes why one cleanup deletion was refused.
func (e *CleanupNotEligibleError) Error() string {
	return fmt.Sprintf("run %q is not cleanup eligible", e.RunID)
}

// Unwrap lets callers classify cleanup races with ErrCleanupNotEligible.
func (*CleanupNotEligibleError) Unwrap() error { return ErrCleanupNotEligible }

// BeginCleanup reserves one terminal run after checking its current status and
// terminal timestamp. A no-op transaction is returned for an already absent
// run so repeated cleanup remains idempotent.
func (s *Store) BeginCleanup(ctx context.Context, runID string, before time.Time) (CleanupTransaction, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, errors.New("cleanup run id is required")
	}
	if strings.ContainsAny(runID, "\x00\r\n") {
		return nil, errors.New("cleanup run id contains a control character")
	}
	if before.IsZero() {
		return nil, errors.New("cleanup cutoff is required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin cleanup for run %q: %w", runID, err)
	}
	transaction := &cleanupTransaction{tx: tx, runID: runID}
	// This deliberately no-op update acquires SQLite's write reservation
	// before external cleanup begins. No coordinator state is changed.
	if _, err := tx.ExecContext(ctx, "UPDATE operational_runs SET id = id WHERE id = ?", runID); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("reserve cleanup for run %q: %w", runID, err)
	}
	var status Status
	var terminalAt, updatedAt string
	err = tx.QueryRowContext(ctx, `
		SELECT status, terminal_at, updated_at
		FROM operational_runs
		WHERE id = ?`, runID).Scan(&status, &terminalAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return transaction, nil
	}
	if err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("read cleanup run %q: %w", runID, err)
	}
	transaction.exists = true
	if !IsTerminalStatus(status) {
		_ = tx.Rollback()
		return nil, &CleanupNotEligibleError{RunID: runID}
	}
	eligibleAt, err := parseCleanupTimestamp(terminalAt, updatedAt)
	if err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("parse cleanup timestamp for run %q: %w", runID, err)
	}
	if eligibleAt.After(before.UTC()) {
		_ = tx.Rollback()
		return nil, &CleanupNotEligibleError{RunID: runID}
	}
	return transaction, nil
}

// ListRunRemovalCandidates returns terminal runs whose terminal time is at or
// before before. An empty runID lists every candidate; a non-empty runID
// narrows the result without changing the same eligibility rules.
func (s *Store) ListRunRemovalCandidates(ctx context.Context, before time.Time, runID string) ([]RunRemovalCandidate, error) {
	if before.IsZero() {
		return nil, errors.New("cleanup cutoff is required")
	}
	if strings.ContainsAny(runID, "\x00\r\n") {
		return nil, errors.New("cleanup run id contains a control character")
	}
	cutoff := before.UTC().Format(runTimestampLayout)
	runs, err := s.queryRuns(ctx, `
		SELECT `+runColumns+`
		FROM operational_runs
		WHERE status IN (?, ?, ?)
		  AND COALESCE(NULLIF(terminal_at, ''), updated_at) <= ?
		  AND (? = '' OR id = ?)
		ORDER BY COALESCE(NULLIF(terminal_at, ''), updated_at), id`,
		StatusComplete, StatusCancelled, StatusFailed, cutoff, runID, runID)
	if err != nil {
		return nil, fmt.Errorf("list cleanup candidates: %w", err)
	}
	return s.removalCandidates(ctx, runs)
}

// DeleteCleanupRun reserves and atomically removes one already-planned run and
// its operational children. It never deletes evaluation summaries, usage, or
// dispositions.
func (s *Store) DeleteCleanupRun(ctx context.Context, runID string, before time.Time) (CleanupDeletionResult, error) {
	transaction, err := s.BeginCleanup(ctx, runID, before)
	if err != nil {
		return CleanupDeletionResult{RunID: runID}, err
	}
	defer func() { _ = transaction.Rollback() }()
	result, err := transaction.Delete(ctx)
	if err != nil {
		return result, err
	}
	if err := transaction.Commit(); err != nil {
		return result, fmt.Errorf("commit cleanup for run %q: %w", runID, err)
	}
	return result, nil
}

// Delete removes the reserved run's operational children and parent row. It
// may be called once; absent runs return an idempotent zero-count result.
func (t *cleanupTransaction) Delete(ctx context.Context) (CleanupDeletionResult, error) {
	result := CleanupDeletionResult{RunID: t.runID}
	if !t.exists || t.deleted {
		return result, nil
	}
	var err error
	if result.PendingEffects, err = deleteCleanupRows(ctx, t.tx, "pending_effects", t.runID); err != nil {
		return result, fmt.Errorf("delete pending effects for run %q: %w", t.runID, err)
	}
	if result.GateResults, err = deleteCleanupRows(ctx, t.tx, "gate_results", t.runID); err != nil {
		return result, fmt.Errorf("delete gate results for run %q: %w", t.runID, err)
	}
	if result.Invocations, err = deleteCleanupRows(ctx, t.tx, "invocations", t.runID); err != nil {
		return result, fmt.Errorf("delete invocations for run %q: %w", t.runID, err)
	}
	if result.Runs, err = deleteCleanupRunRow(ctx, t.tx, t.runID); err != nil {
		return result, fmt.Errorf("delete operational run %q: %w", t.runID, err)
	}
	t.deleted = true
	return result, nil
}

// Commit makes the reserved deletion durable and releases the SQLite lock.
func (t *cleanupTransaction) Commit() error { return t.tx.Commit() }

// Rollback releases the reservation and preserves rows not yet committed.
func (t *cleanupTransaction) Rollback() error { return t.tx.Rollback() }

// Run returns the complete operational projection of one persisted run, or
// nil when no run has that identity.
func (s *Store) Run(ctx context.Context, runID string) (*Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+runColumns+`
		FROM operational_runs
		WHERE id = ?`, runID)
	return scanRun(row)
}

// Invocations returns every invocation of one run, oldest update first.
func (s *Store) Invocations(ctx context.Context, runID string) ([]Invocation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+invocationColumns+`
		FROM invocations
		WHERE run_id = ?
		ORDER BY updated_at, id`, runID)
	if err != nil {
		return nil, fmt.Errorf("list invocations for run %q: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()
	invocations := make([]Invocation, 0)
	for rows.Next() {
		invocation, err := scanInvocation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan invocation for run %q: %w", runID, err)
		}
		invocations = append(invocations, *invocation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read invocations for run %q: %w", runID, err)
	}
	return invocations, nil
}

// parseCleanupTimestamp selects the stable terminal timestamp and falls back
// to updated_at for databases created before terminal_at was introduced.
func parseCleanupTimestamp(terminalAt, updatedAt string) (time.Time, error) {
	value := terminalAt
	if value == "" {
		value = updatedAt
	}
	return time.Parse(time.RFC3339Nano, value)
}

// deleteCleanupRows deletes one run-scoped table and reports affected rows.
// Table names are fixed internal constants at every call site.
func deleteCleanupRows(ctx context.Context, tx *sql.Tx, table, runID string) (int, error) {
	result, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE run_id = ?", runID)
	if err != nil {
		return 0, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(changed), nil
}

// deleteCleanupRunRow deletes the parent row whose identity column is named
// id rather than run_id.
func deleteCleanupRunRow(ctx context.Context, tx *sql.Tx, runID string) (int, error) {
	result, err := tx.ExecContext(ctx, "DELETE FROM operational_runs WHERE id = ?", runID)
	if err != nil {
		return 0, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(changed), nil
}

// ListRuns returns every persisted run, newest update first.
func (s *Store) ListRuns(ctx context.Context) ([]Run, error) {
	runs, err := s.queryRuns(ctx, `
		SELECT `+runColumns+`
		FROM operational_runs
		ORDER BY updated_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	return runs, nil
}

// ListResetCandidates returns every persisted run with the operational records
// a complete local reset needs, regardless of status or retention age. It is
// separate from ListRunRemovalCandidates because reset owns the whole
// installation rather than the seven-day terminal-run retention window.
func (s *Store) ListResetCandidates(ctx context.Context) ([]RunRemovalCandidate, error) {
	runs, err := s.queryRuns(ctx, `
		SELECT `+runColumns+`
		FROM operational_runs
		ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list reset candidates: %w", err)
	}
	return s.removalCandidates(ctx, runs)
}

// queryRuns runs one SELECT of runColumns and decodes every row, so a list
// reads its runs in one query instead of one query per run.
func (s *Store) queryRuns(ctx context.Context, query string, args ...any) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	runs := make([]Run, 0)
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, *run)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return runs, nil
}

// removalCandidates adds the invocations and pending effect of each selected
// run. Both removal commands share it, so neither can plan from a narrower
// view of a run than the other.
func (s *Store) removalCandidates(ctx context.Context, runs []Run) ([]RunRemovalCandidate, error) {
	candidates := make([]RunRemovalCandidate, 0, len(runs))
	for _, run := range runs {
		invocations, err := s.Invocations(ctx, run.ID)
		if err != nil {
			return nil, err
		}
		pendingEffect, err := s.PendingEffect(ctx, run.ID)
		if err != nil {
			return nil, fmt.Errorf("read pending effect for removal candidate %q: %w", run.ID, err)
		}
		candidates = append(candidates, RunRemovalCandidate{Run: run, Invocations: invocations, PendingEffect: pendingEffect})
	}
	return candidates, nil
}
