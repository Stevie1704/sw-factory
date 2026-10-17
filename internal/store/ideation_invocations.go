package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SessionInvocationStatus is the lifecycle state of one session-role
// invocation.
type SessionInvocationStatus string

const (
	// SessionInvocationActive means the role process is in flight for its
	// pinned session revision.
	SessionInvocationActive SessionInvocationStatus = "active"
	// SessionInvocationAccepted means the coordinator accepted the role's
	// report. At most one invocation per trigger reaches this state.
	SessionInvocationAccepted SessionInvocationStatus = "accepted"
	// SessionInvocationRejected means the coordinator refused the role's
	// report, for example because it was invalid.
	SessionInvocationRejected SessionInvocationStatus = "rejected"
	// SessionInvocationFailed means the role process failed with a typed cause.
	SessionInvocationFailed SessionInvocationStatus = "failed"
	// SessionInvocationCancelled means the coordinator cancelled the process,
	// for example because the session moved on.
	SessionInvocationCancelled SessionInvocationStatus = "cancelled"
)

// maxSessionInvocationTextBytes bounds every single-line invocation field.
const maxSessionInvocationTextBytes = 4096

// ErrSessionInvocationNotFound reports that no session invocation has the ID.
var ErrSessionInvocationNotFound = errors.New("session invocation not found")

// ErrSessionInvocationActive reports that the session already has an active
// role invocation.
var ErrSessionInvocationActive = errors.New("session already has an active role invocation")

// ErrSessionInvocationNotActive reports a result or end for an invocation that
// is no longer active, for example a late or repeated report.
var ErrSessionInvocationNotActive = errors.New("session invocation is not active")

// ErrSessionTriggerConsumed reports that the trigger, for example a user turn,
// already has an accepted role result.
var ErrSessionTriggerConsumed = errors.New("session trigger already has an accepted role result")

// SessionInvocation is one pinned role invocation for an ideation session. It
// names its session and revision; it never names a run.
type SessionInvocation struct {
	// ID is the stable invocation identity.
	ID string
	// SessionID identifies the owning session. The store fills it.
	SessionID string
	// Role is the declared session role.
	Role string
	// TriggerID identifies what the invocation answers: the user turn for an
	// ideation role, or the handoff request for a planning role. At most one
	// invocation per trigger is accepted.
	TriggerID string
	// SessionRevision is the session revision the invocation serves. The store
	// assigns it from the write that starts the role.
	SessionRevision int64
	// BriefRevision is the brief revision the role received, or zero.
	BriefRevision int64
	// ContextRevision is the repository commit of the read-only snapshot.
	ContextRevision string
	// Harness is the selected harness.
	Harness string
	// Model is the selected model.
	Model string
	// ReasoningEffort is the selected reasoning effort, or empty.
	ReasoningEffort string
	// PromptVersion is the role prompt version.
	PromptVersion string
	// PromptSHA256 is the content identity of the role prompt body.
	PromptSHA256 string
	// Image is the worker image name.
	Image string
	// ImageDigest is the worker image digest. It pins the worker skill set.
	ImageDigest string
	// CredentialStoreID identifies the factory-managed credential store, or is
	// empty when the worker keeps its own credential.
	CredentialStoreID string
	// WorkerID identifies the session-role worker.
	WorkerID string
	// NativeSessionID is the harness-native session, once known.
	NativeSessionID string
	// ResumedFrom identifies the earlier invocation whose native session this
	// invocation continues, or is empty for a fresh native session.
	ResumedFrom string
	// SnapshotPath is the host path of the read-only repository snapshot.
	SnapshotPath string
	// InvocationDirectory is the host path of the frozen session packet.
	InvocationDirectory string
	// ResultDirectory is the host path of the report directory.
	ResultDirectory string
	// Status is the invocation lifecycle state.
	Status SessionInvocationStatus
	// FailureKind is the typed cause of a failed, rejected, or cancelled
	// invocation.
	FailureKind string
	// StatusReason optionally explains Status.
	StatusReason string
	// Payload is the validated role payload of an accepted invocation.
	Payload []byte
	// CreatedAt is when the invocation started.
	CreatedAt time.Time
	// UpdatedAt is when the invocation last changed.
	UpdatedAt time.Time
	// ResourcesReleasedAt is when the coordinator removed the worker, the
	// snapshot, and the Git projection of an ended invocation. It is zero
	// while the invocation is active or its cleanup is still outstanding.
	ResourcesReleasedAt time.Time
}

// SessionRoleResult is one validated role result for the coordinator to apply.
type SessionRoleResult struct {
	// InvocationID identifies the active invocation that produced the result.
	InvocationID string
	// Turn optionally appends the role's conversation turn.
	Turn *IdeationTurn
	// Brief optionally stores a new brief revision.
	Brief *Brief
	// Plan optionally stores a new plan revision. It must name the brief and
	// context revisions the invocation was pinned to.
	Plan *PlanRevision
	// Payload is the validated structured payload kept as evidence.
	Payload []byte
}

// SessionInvocationEnd closes one active invocation without a result.
type SessionInvocationEnd struct {
	// InvocationID identifies the active invocation.
	InvocationID string
	// Status is failed, rejected, or cancelled.
	Status SessionInvocationStatus
	// FailureKind is the typed cause.
	FailureKind string
	// Reason optionally explains the end.
	Reason string
	// At is the end time. Zero selects the current time.
	At time.Time
}

// sessionInvocationColumns is the column order scanSessionInvocation decodes.
const sessionInvocationColumns = `session_id, id, role, trigger_id, session_revision, brief_revision, context_revision,
	harness, model, reasoning_effort, prompt_version, prompt_sha256, image, image_digest, credential_store_id,
	worker_id, native_session_id, resumed_from, snapshot_path, invocation_directory, result_directory,
	status, failure_kind, status_reason, payload, created_at, updated_at, resources_released_at`

// sessionInvocationSchema creates the additive table of schema version 44.
// The partial indexes enforce one active invocation per session and one
// accepted result per trigger.
var sessionInvocationSchema = []string{
	`CREATE TABLE IF NOT EXISTS ideation_invocations (
		session_id TEXT NOT NULL,
		id TEXT PRIMARY KEY,
		role TEXT NOT NULL,
		trigger_id TEXT NOT NULL,
		session_revision INTEGER NOT NULL,
		brief_revision INTEGER NOT NULL DEFAULT 0,
		context_revision TEXT NOT NULL,
		harness TEXT NOT NULL,
		model TEXT NOT NULL DEFAULT '',
		reasoning_effort TEXT NOT NULL DEFAULT '',
		prompt_version TEXT NOT NULL,
		prompt_sha256 TEXT NOT NULL,
		image TEXT NOT NULL DEFAULT '',
		image_digest TEXT NOT NULL DEFAULT '',
		credential_store_id TEXT NOT NULL DEFAULT '',
		worker_id TEXT NOT NULL,
		native_session_id TEXT NOT NULL DEFAULT '',
		resumed_from TEXT NOT NULL DEFAULT '',
		snapshot_path TEXT NOT NULL DEFAULT '',
		invocation_directory TEXT NOT NULL DEFAULT '',
		result_directory TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL,
		failure_kind TEXT NOT NULL DEFAULT '',
		status_reason TEXT NOT NULL DEFAULT '',
		payload BLOB,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	"CREATE UNIQUE INDEX IF NOT EXISTS one_active_invocation_per_ideation_session ON ideation_invocations (session_id) WHERE status = 'active'",
	"CREATE UNIQUE INDEX IF NOT EXISTS one_accepted_result_per_ideation_trigger ON ideation_invocations (session_id, trigger_id) WHERE status = 'accepted'",
	"CREATE INDEX IF NOT EXISTS ideation_invocations_by_session ON ideation_invocations (session_id, role, created_at)",
}

// migrateSessionInvocationSchema applies sessionInvocationSchema inside the
// migration transaction.
func migrateSessionInvocationSchema(ctx context.Context, tx *sql.Tx) error {
	for _, statement := range sessionInvocationSchema {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// migrateSessionResourceReleaseSchema adds the resource-release marker of
// schema version 47. Invocations that ended before the upgrade stay
// unreleased, so the next reconcile pass repeats their idempotent cleanup.
func migrateSessionResourceReleaseSchema(ctx context.Context, tx *sql.Tx) error {
	found, err := tableHasColumn(ctx, tx, "ideation_invocations", "resources_released_at")
	if err != nil || found {
		return err
	}
	_, err = tx.ExecContext(ctx, "ALTER TABLE ideation_invocations ADD COLUMN resources_released_at TEXT")
	return err
}

// tableHasColumn reports whether table has column inside tx.
func tableHasColumn(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return false, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	found := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("read %s column: %w", table, err)
		}
		found = found || name == column
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return false, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	return found, nil
}

// StartSessionInvocation records invocation as the session's active role
// invocation and commits write, which normally moves the session to
// role_active. The invocation is pinned to the resulting session revision. It
// fails when another invocation is active or the trigger already has an
// accepted result.
func (s *Store) StartSessionInvocation(ctx context.Context, write IdeationWrite, invocation SessionInvocation) (IdeationSession, SessionInvocation, error) {
	if err := validateSessionInvocation(invocation); err != nil {
		return IdeationSession{}, SessionInvocation{}, err
	}
	tx, session, err := s.beginIdeationWrite(ctx, write)
	if err != nil {
		return IdeationSession{}, SessionInvocation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := checkIdeationRevision(session, write); err != nil {
		return IdeationSession{}, SessionInvocation{}, err
	}
	var active, consumed int
	if err := tx.QueryRowContext(ctx, `SELECT
		COUNT(CASE WHEN status = 'active' THEN 1 END),
		COUNT(CASE WHEN status = 'accepted' AND trigger_id = ? THEN 1 END)
		FROM ideation_invocations WHERE session_id = ?`, invocation.TriggerID, session.ID).Scan(&active, &consumed); err != nil {
		return IdeationSession{}, SessionInvocation{}, fmt.Errorf("inspect session invocations: %w", err)
	}
	if active != 0 {
		return IdeationSession{}, SessionInvocation{}, fmt.Errorf("%w: session %q", ErrSessionInvocationActive, session.ID)
	}
	if consumed != 0 {
		return IdeationSession{}, SessionInvocation{}, fmt.Errorf("%w: trigger %q", ErrSessionTriggerConsumed, invocation.TriggerID)
	}
	invocation.SessionID = session.ID
	invocation.SessionRevision = write.ExpectedRevision + 1
	invocation.Status = SessionInvocationActive
	invocation.FailureKind, invocation.StatusReason, invocation.Payload = "", "", nil
	invocation.CreatedAt = ideationWriteTime(write)
	invocation.UpdatedAt = invocation.CreatedAt
	if _, err := tx.ExecContext(ctx, `INSERT INTO ideation_invocations (`+sessionInvocationColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		invocation.SessionID, invocation.ID, invocation.Role, invocation.TriggerID, invocation.SessionRevision,
		invocation.BriefRevision, invocation.ContextRevision, invocation.Harness, invocation.Model,
		invocation.ReasoningEffort, invocation.PromptVersion, invocation.PromptSHA256, invocation.Image,
		invocation.ImageDigest, invocation.CredentialStoreID, invocation.WorkerID, invocation.NativeSessionID,
		invocation.ResumedFrom, invocation.SnapshotPath, invocation.InvocationDirectory, invocation.ResultDirectory,
		invocation.Status, invocation.FailureKind, invocation.StatusReason, invocation.Payload,
		formatIdeationTime(invocation.CreatedAt), formatIdeationTime(invocation.UpdatedAt)); err != nil {
		return IdeationSession{}, SessionInvocation{}, fmt.Errorf("start session invocation %q: %w", invocation.ID, err)
	}
	updated, err := commitIdeationWrite(ctx, tx, session, write)
	if err != nil {
		return IdeationSession{}, SessionInvocation{}, err
	}
	return updated, invocation, nil
}

// RecordSessionInvocationNativeSession records the native session identity of
// an active invocation after the harness revealed it.
func (s *Store) RecordSessionInvocationNativeSession(ctx context.Context, invocationID, nativeSessionID string) (SessionInvocation, error) {
	if err := validateIdeationText("native session id", nativeSessionID, maxIdeationIdentifierBytes, true); err != nil {
		return SessionInvocation{}, err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE ideation_invocations SET native_session_id = ?, updated_at = ? WHERE id = ? AND status = 'active'`,
		nativeSessionID, formatIdeationTime(time.Now()), invocationID)
	if err != nil {
		return SessionInvocation{}, fmt.Errorf("record native session of %q: %w", invocationID, err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return SessionInvocation{}, err
	} else if changed != 1 {
		return SessionInvocation{}, fmt.Errorf("%w: %q", ErrSessionInvocationNotActive, invocationID)
	}
	return s.SessionInvocation(ctx, invocationID)
}

// SessionInvocation returns one session invocation or
// ErrSessionInvocationNotFound.
func (s *Store) SessionInvocation(ctx context.Context, invocationID string) (SessionInvocation, error) {
	return scanSessionInvocation(s.db.QueryRowContext(ctx, `SELECT `+sessionInvocationColumns+` FROM ideation_invocations WHERE id = ?`, invocationID), invocationID)
}

// ActiveSessionInvocations returns every active session invocation, oldest
// first. Restart recovery inspects each of them.
func (s *Store) ActiveSessionInvocations(ctx context.Context) ([]SessionInvocation, error) {
	return s.querySessionInvocations(ctx, `SELECT `+sessionInvocationColumns+` FROM ideation_invocations WHERE status = 'active' ORDER BY created_at, id`)
}

// UnreleasedSessionInvocations returns every ended invocation whose worker,
// snapshot, or Git projection may still exist, oldest first. Each reconcile
// pass retries their cleanup.
func (s *Store) UnreleasedSessionInvocations(ctx context.Context) ([]SessionInvocation, error) {
	return s.querySessionInvocations(ctx, `SELECT `+sessionInvocationColumns+` FROM ideation_invocations
		WHERE status != 'active' AND resources_released_at IS NULL ORDER BY created_at, id`)
}

// MarkSessionInvocationResourcesReleased records that the coordinator removed
// the resources of an ended invocation. The coordinator calls it only after
// the cleanup succeeded. Marking a released invocation again keeps the first
// time; an active invocation is refused with ErrSessionInvocationActive.
func (s *Store) MarkSessionInvocationResourcesReleased(ctx context.Context, invocationID string, at time.Time) (SessionInvocation, error) {
	if at.IsZero() {
		at = time.Now()
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE ideation_invocations SET resources_released_at = ?
		WHERE id = ? AND status != 'active' AND resources_released_at IS NULL`, formatIdeationTime(at), invocationID); err != nil {
		return SessionInvocation{}, fmt.Errorf("mark resources of session invocation %q released: %w", invocationID, err)
	}
	invocation, err := s.SessionInvocation(ctx, invocationID)
	if err != nil {
		return SessionInvocation{}, err
	}
	if invocation.Status == SessionInvocationActive {
		return SessionInvocation{}, fmt.Errorf("%w: %q", ErrSessionInvocationActive, invocationID)
	}
	return invocation, nil
}

// SessionInvocations returns every invocation of one session, ended or
// active, in start order. Session pages read accepted payloads and failure
// causes from them.
func (s *Store) SessionInvocations(ctx context.Context, sessionID string) ([]SessionInvocation, error) {
	return s.querySessionInvocations(ctx, `SELECT `+sessionInvocationColumns+` FROM ideation_invocations WHERE session_id = ? ORDER BY created_at, id`, sessionID)
}

// AcceptSessionRoleResult applies one validated role result in a single
// transaction: it appends the role turn, the brief revision, and the plan
// revision, marks the invocation accepted, and commits write. It succeeds only
// while the invocation is active and the session is still at the revision the
// invocation was pinned to, so a late or stale report changes nothing.
func (s *Store) AcceptSessionRoleResult(ctx context.Context, write IdeationWrite, result SessionRoleResult) (IdeationSession, SessionInvocation, error) {
	var briefContent, planNodes string
	var err error
	if result.Turn != nil {
		if err := validateIdeationTurn(*result.Turn); err != nil {
			return IdeationSession{}, SessionInvocation{}, err
		}
	}
	if result.Brief != nil {
		if briefContent, err = encodeBrief(*result.Brief); err != nil {
			return IdeationSession{}, SessionInvocation{}, err
		}
	}
	if result.Plan != nil {
		if planNodes, err = encodePlanNodes(*result.Plan); err != nil {
			return IdeationSession{}, SessionInvocation{}, err
		}
	}
	tx, session, err := s.beginIdeationWrite(ctx, write)
	if err != nil {
		return IdeationSession{}, SessionInvocation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	invocation, err := activeSessionInvocationTx(ctx, tx, session.ID, result.InvocationID)
	if err != nil {
		return IdeationSession{}, SessionInvocation{}, err
	}
	if invocation.SessionRevision != write.ExpectedRevision {
		return IdeationSession{}, SessionInvocation{}, &IdeationRevisionConflictError{SessionID: session.ID, Expected: write.ExpectedRevision, Actual: invocation.SessionRevision}
	}
	if err := checkIdeationRevision(session, write); err != nil {
		return IdeationSession{}, SessionInvocation{}, err
	}
	if result.Turn != nil {
		if _, err := insertIdeationTurn(ctx, tx, session, write, *result.Turn); err != nil {
			return IdeationSession{}, SessionInvocation{}, err
		}
	}
	if result.Brief != nil {
		if _, err := insertBriefRevision(ctx, tx, &session, write, *result.Brief, briefContent); err != nil {
			return IdeationSession{}, SessionInvocation{}, err
		}
	}
	if result.Plan != nil {
		if result.Plan.ContextRevision != invocation.ContextRevision || result.Plan.SourceBriefRevision != invocation.BriefRevision {
			return IdeationSession{}, SessionInvocation{}, fmt.Errorf("plan names brief %d at %s; the invocation was pinned to brief %d at %s",
				result.Plan.SourceBriefRevision, result.Plan.ContextRevision, invocation.BriefRevision, invocation.ContextRevision)
		}
		if _, err := insertPlanRevision(ctx, tx, &session, write, *result.Plan, planNodes); err != nil {
			return IdeationSession{}, SessionInvocation{}, err
		}
	}
	invocation.Status = SessionInvocationAccepted
	invocation.Payload = append([]byte(nil), result.Payload...)
	invocation.UpdatedAt = ideationWriteTime(write)
	if err := updateSessionInvocationStatusTx(ctx, tx, invocation); err != nil {
		return IdeationSession{}, SessionInvocation{}, err
	}
	updated, err := commitIdeationWrite(ctx, tx, session, write)
	if err != nil {
		return IdeationSession{}, SessionInvocation{}, err
	}
	return updated, invocation, nil
}

// ReleaseSessionInvocation ends an active invocation without a result and
// commits write in the same transaction, which normally returns the session
// to the user with a reason. The session must still be at the invocation's
// pinned revision.
func (s *Store) ReleaseSessionInvocation(ctx context.Context, write IdeationWrite, end SessionInvocationEnd) (IdeationSession, SessionInvocation, error) {
	if err := validateSessionInvocationEnd(end); err != nil {
		return IdeationSession{}, SessionInvocation{}, err
	}
	tx, session, err := s.beginIdeationWrite(ctx, write)
	if err != nil {
		return IdeationSession{}, SessionInvocation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	invocation, err := activeSessionInvocationTx(ctx, tx, session.ID, end.InvocationID)
	if err != nil {
		return IdeationSession{}, SessionInvocation{}, err
	}
	if invocation.SessionRevision != write.ExpectedRevision {
		return IdeationSession{}, SessionInvocation{}, &IdeationRevisionConflictError{SessionID: session.ID, Expected: write.ExpectedRevision, Actual: invocation.SessionRevision}
	}
	if err := checkIdeationRevision(session, write); err != nil {
		return IdeationSession{}, SessionInvocation{}, err
	}
	invocation = endedSessionInvocation(invocation, end)
	if err := updateSessionInvocationStatusTx(ctx, tx, invocation); err != nil {
		return IdeationSession{}, SessionInvocation{}, err
	}
	updated, err := commitIdeationWrite(ctx, tx, session, write)
	if err != nil {
		return IdeationSession{}, SessionInvocation{}, err
	}
	return updated, invocation, nil
}

// CloseSessionInvocation ends an active invocation without changing its
// session. The coordinator uses it when the session already left the pinned
// revision, for example after a cancellation.
func (s *Store) CloseSessionInvocation(ctx context.Context, end SessionInvocationEnd) (SessionInvocation, error) {
	if err := validateSessionInvocationEnd(end); err != nil {
		return SessionInvocation{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SessionInvocation{}, fmt.Errorf("begin session invocation close: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := reserveWriteLock(ctx, tx, "ideation_invocations", end.InvocationID); err != nil {
		return SessionInvocation{}, fmt.Errorf("reserve session invocation %q: %w", end.InvocationID, err)
	}
	invocation, err := activeSessionInvocationTx(ctx, tx, "", end.InvocationID)
	if err != nil {
		return SessionInvocation{}, err
	}
	invocation = endedSessionInvocation(invocation, end)
	if err := updateSessionInvocationStatusTx(ctx, tx, invocation); err != nil {
		return SessionInvocation{}, err
	}
	if err := tx.Commit(); err != nil {
		return SessionInvocation{}, fmt.Errorf("commit session invocation close: %w", err)
	}
	return invocation, nil
}

// endedSessionInvocation returns invocation with the status, cause, and time
// of end.
func endedSessionInvocation(invocation SessionInvocation, end SessionInvocationEnd) SessionInvocation {
	invocation.Status = end.Status
	invocation.FailureKind = end.FailureKind
	invocation.StatusReason = end.Reason
	invocation.UpdatedAt = ideationWriteTime(IdeationWrite{At: end.At})
	return invocation
}

// activeSessionInvocationTx reads one invocation inside tx and requires it to
// be active and, when sessionID is set, to belong to that session.
func activeSessionInvocationTx(ctx context.Context, tx *sql.Tx, sessionID, invocationID string) (SessionInvocation, error) {
	invocation, err := scanSessionInvocation(tx.QueryRowContext(ctx, `SELECT `+sessionInvocationColumns+` FROM ideation_invocations WHERE id = ?`, invocationID), invocationID)
	if err != nil {
		return SessionInvocation{}, err
	}
	if sessionID != "" && invocation.SessionID != sessionID {
		return SessionInvocation{}, fmt.Errorf("%w: %q belongs to session %q", ErrSessionInvocationNotFound, invocationID, invocation.SessionID)
	}
	if invocation.Status != SessionInvocationActive {
		return SessionInvocation{}, fmt.Errorf("%w: %q is %s", ErrSessionInvocationNotActive, invocationID, invocation.Status)
	}
	return invocation, nil
}

// updateSessionInvocationStatusTx persists the end state of one active
// invocation inside tx.
func updateSessionInvocationStatusTx(ctx context.Context, tx *sql.Tx, invocation SessionInvocation) error {
	result, err := tx.ExecContext(ctx, `UPDATE ideation_invocations
		SET status = ?, failure_kind = ?, status_reason = ?, payload = ?, updated_at = ?
		WHERE id = ? AND status = 'active'`,
		invocation.Status, invocation.FailureKind, invocation.StatusReason, invocation.Payload,
		formatIdeationTime(invocation.UpdatedAt), invocation.ID)
	if err != nil {
		return fmt.Errorf("end session invocation %q: %w", invocation.ID, err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed != 1 {
		return fmt.Errorf("%w: %q", ErrSessionInvocationNotActive, invocation.ID)
	}
	return nil
}

// querySessionInvocations reads every row of one invocation query.
func (s *Store) querySessionInvocations(ctx context.Context, query string, args ...any) ([]SessionInvocation, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list session invocations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	invocations := []SessionInvocation{}
	for rows.Next() {
		invocation, err := scanSessionInvocation(rows, "")
		if err != nil {
			return nil, err
		}
		invocations = append(invocations, invocation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list session invocations: %w", err)
	}
	return invocations, nil
}

// scanSessionInvocation decodes one sessionInvocationColumns row. A missing
// row returns ErrSessionInvocationNotFound for invocationID.
func scanSessionInvocation(row interface{ Scan(...any) error }, invocationID string) (SessionInvocation, error) {
	var invocation SessionInvocation
	var createdAt, updatedAt string
	var releasedAt sql.NullString
	err := row.Scan(&invocation.SessionID, &invocation.ID, &invocation.Role, &invocation.TriggerID,
		&invocation.SessionRevision, &invocation.BriefRevision, &invocation.ContextRevision, &invocation.Harness,
		&invocation.Model, &invocation.ReasoningEffort, &invocation.PromptVersion, &invocation.PromptSHA256,
		&invocation.Image, &invocation.ImageDigest, &invocation.CredentialStoreID, &invocation.WorkerID,
		&invocation.NativeSessionID, &invocation.ResumedFrom, &invocation.SnapshotPath, &invocation.InvocationDirectory,
		&invocation.ResultDirectory, &invocation.Status, &invocation.FailureKind, &invocation.StatusReason,
		&invocation.Payload, &createdAt, &updatedAt, &releasedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionInvocation{}, fmt.Errorf("%w: %q", ErrSessionInvocationNotFound, invocationID)
	}
	if err != nil {
		return SessionInvocation{}, fmt.Errorf("read session invocation: %w", err)
	}
	if invocation.CreatedAt, err = parseIdeationTime(createdAt); err != nil {
		return SessionInvocation{}, err
	}
	if invocation.UpdatedAt, err = parseIdeationTime(updatedAt); err != nil {
		return SessionInvocation{}, err
	}
	if releasedAt.Valid {
		if invocation.ResourcesReleasedAt, err = parseIdeationTime(releasedAt.String); err != nil {
			return SessionInvocation{}, err
		}
	}
	return invocation, nil
}

// validateSessionInvocation checks the caller-supplied identity and pinned
// policy of a new invocation.
func validateSessionInvocation(invocation SessionInvocation) error {
	for label, value := range map[string]string{
		"invocation id": invocation.ID, "invocation role": invocation.Role, "invocation trigger": invocation.TriggerID,
		"invocation worker id": invocation.WorkerID, "invocation harness": invocation.Harness,
		"invocation prompt version": invocation.PromptVersion,
	} {
		if err := validateIdeationIdentifier(label, value); err != nil {
			return err
		}
	}
	if invocation.ResumedFrom != "" {
		if err := validateIdeationIdentifier("invocation resumed from", invocation.ResumedFrom); err != nil {
			return err
		}
	}
	if len(invocation.ContextRevision) != 40 && len(invocation.ContextRevision) != 64 || !isLowerHex(invocation.ContextRevision) {
		return errors.New("session invocation context revision must be a full lowercase commit identity")
	}
	if len(invocation.PromptSHA256) != 64 || !isLowerHex(invocation.PromptSHA256) {
		return errors.New("session invocation prompt identity must be a SHA-256 digest")
	}
	if invocation.BriefRevision < 0 {
		return errors.New("session invocation brief revision must not be negative")
	}
	for label, value := range map[string]string{
		"model": invocation.Model, "reasoning effort": invocation.ReasoningEffort, "image": invocation.Image,
		"image digest": invocation.ImageDigest, "credential store": invocation.CredentialStoreID,
		"native session id": invocation.NativeSessionID, "snapshot path": invocation.SnapshotPath,
		"invocation directory": invocation.InvocationDirectory, "result directory": invocation.ResultDirectory,
	} {
		if len(value) > maxSessionInvocationTextBytes || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("session invocation %s must be a bounded single line", label)
		}
	}
	return nil
}

// validateSessionInvocationEnd checks that end closes an invocation with a
// declared non-active, non-accepted status.
func validateSessionInvocationEnd(end SessionInvocationEnd) error {
	switch end.Status {
	case SessionInvocationFailed, SessionInvocationRejected, SessionInvocationCancelled:
	default:
		return fmt.Errorf("session invocation end status %q is not supported", end.Status)
	}
	if end.FailureKind != "" {
		if err := validateIdeationIdentifier("invocation failure kind", end.FailureKind); err != nil {
			return err
		}
	}
	return validateIdeationText("invocation end reason", end.Reason, MaxIdeationReasonBytes, false)
}
