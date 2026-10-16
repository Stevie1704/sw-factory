package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// IdeationState is the persisted lifecycle state of one ideation session. The
// set is closed in the store; the workflow package declares which transitions
// between these states are legal (ADR 0021).
type IdeationState string

const (
	// IdeationStateAwaitingUser means the session waits for the next user turn
	// or brief edit. No role invocation is in flight.
	IdeationStateAwaitingUser IdeationState = "awaiting_user"
	// IdeationStateRoleActive means an ideation or planning role invocation is
	// in flight for the current session revision.
	IdeationStateRoleActive IdeationState = "role_active"
	// IdeationStateDraftReview means a plan draft waits for human review.
	IdeationStateDraftReview IdeationState = "draft_review"
	// IdeationStateApproved means an approval binds the current plan revision
	// and publication has not started.
	IdeationStateApproved IdeationState = "approved"
	// IdeationStatePublishing means publication of the approved payload is in
	// progress.
	IdeationStatePublishing IdeationState = "publishing"
	// IdeationStatePublicationIncomplete means publication stopped before every
	// operation succeeded. Some tracker items can exist; retry or
	// reconciliation continues the same approval.
	IdeationStatePublicationIncomplete IdeationState = "publication_incomplete"
	// IdeationStatePublished means every approved item and relationship was
	// published. The state is terminal.
	IdeationStatePublished IdeationState = "published"
	// IdeationStateDeferred means the user parked the session. It can resume.
	IdeationStateDeferred IdeationState = "deferred"
	// IdeationStateInvestigationNeeded means the user decided that material
	// unknowns need investigation before planning. The unresolved questions
	// of the current brief are the investigation items. No backlog item is
	// created. The session can resume.
	IdeationStateInvestigationNeeded IdeationState = "investigation_needed"
	// IdeationStateNoBuild means the user decided not to build. The state is
	// terminal.
	IdeationStateNoBuild IdeationState = "no_build"
	// IdeationStateCancelled means the user cancelled the session. The state is
	// terminal.
	IdeationStateCancelled IdeationState = "cancelled"
)

// IdeationStates returns every declared ideation state in lifecycle order.
func IdeationStates() []IdeationState {
	return []IdeationState{
		IdeationStateAwaitingUser, IdeationStateRoleActive, IdeationStateDraftReview,
		IdeationStateApproved, IdeationStatePublishing, IdeationStatePublicationIncomplete,
		IdeationStatePublished, IdeationStateDeferred, IdeationStateInvestigationNeeded, IdeationStateNoBuild,
		IdeationStateCancelled,
	}
}

// IsTerminalIdeationState reports whether a session in state accepts no further
// lifecycle transition.
func IsTerminalIdeationState(state IdeationState) bool {
	return state == IdeationStatePublished || state == IdeationStateNoBuild || state == IdeationStateCancelled
}

// validIdeationState reports whether state is one of the declared states.
func validIdeationState(state IdeationState) bool {
	for _, declared := range IdeationStates() {
		if state == declared {
			return true
		}
	}
	return false
}

// maxIdeationIdentifierBytes bounds every caller-chosen ideation identity.
const maxIdeationIdentifierBytes = 128

// maxIdeationTitleBytes bounds the short operator label of one session.
const maxIdeationTitleBytes = 200

// MaxIdeationReasonBytes bounds a session state reason, a command outcome
// detail, a publication detail, and an invocation end reason.
const MaxIdeationReasonBytes = 2000

// TruncateUTF8 returns the longest prefix of value that has at most limit
// bytes and ends at a rune boundary. Writers use it to keep text inside a
// store bound without splitting a character.
func TruncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

// ErrIdeationSessionNotFound reports that no session has the requested ID.
var ErrIdeationSessionNotFound = errors.New("ideation session not found")

// ErrIdeationSessionExists reports that a session with the same ID exists.
var ErrIdeationSessionExists = errors.New("ideation session already exists")

// ErrIdeationRevisionConflict reports that a writer used a session revision
// that is no longer current, for example a second browser tab or a late role
// report.
var ErrIdeationRevisionConflict = errors.New("ideation session revision conflict")

// IdeationRevisionConflictError is the typed stale-writer error. It names the
// revision the writer expected and the revision that is current.
type IdeationRevisionConflictError struct {
	// SessionID identifies the session.
	SessionID string
	// Expected is the revision the writer read before it changed the session.
	Expected int64
	// Actual is the current persisted revision.
	Actual int64
}

// Error describes the stale write.
func (e *IdeationRevisionConflictError) Error() string {
	return fmt.Sprintf("ideation session %q is at revision %d, not the expected revision %d", e.SessionID, e.Actual, e.Expected)
}

// Unwrap lets callers classify the error with ErrIdeationRevisionConflict.
func (*IdeationRevisionConflictError) Unwrap() error { return ErrIdeationRevisionConflict }

// IdeationSession is the repository-scoped aggregate for work that exists
// before an issue or implementation run. It is not a Run and owns no branch,
// worktree, or remote issue.
type IdeationSession struct {
	// ID is the stable caller-chosen session identity.
	ID string
	// RepositoryPath is the registered repository that owns the session.
	RepositoryPath string
	// Title is an optional short operator label.
	Title string
	// State is the current lifecycle state.
	State IdeationState
	// StateReason optionally explains the current state, for example why a
	// role stopped or why the user deferred the session.
	StateReason string
	// Revision is the optimistic-concurrency token. Every accepted write
	// advances it by one.
	Revision int64
	// BriefRevision is the newest brief revision, or zero when no brief exists.
	BriefRevision int64
	// PlanRevision is the newest plan revision, or zero when no plan exists.
	PlanRevision int64
	// ApprovalID identifies the approval that binds the current plan revision.
	// It is empty when no current approval exists.
	ApprovalID string
	// CreatedAt is when the session was created.
	CreatedAt time.Time
	// UpdatedAt is when the last accepted write happened.
	UpdatedAt time.Time
}

// IdeationWrite is the guard and lifecycle result of one session mutation. The
// write succeeds only while the session still has ExpectedRevision, and it
// commits State in the same transaction as the content change.
type IdeationWrite struct {
	// SessionID identifies the session to change.
	SessionID string
	// ExpectedRevision is the session revision the writer read.
	ExpectedRevision int64
	// State is the lifecycle state after the write. The caller resolves it
	// through the workflow session lifecycle.
	State IdeationState
	// StateReason optionally explains State.
	StateReason string
	// At is the write time. Zero selects the current time.
	At time.Time
	// CommandID optionally names the pending inbox command that causes the
	// write. The store then completes that command as applied in the same
	// transaction, so a resumed coordinator never applies it twice.
	CommandID string
}

// ideationSessionColumns is the column order that scanIdeationSession decodes.
const ideationSessionColumns = `id, repository_path, title, state, state_reason, revision,
	brief_revision, plan_revision, approval_id, created_at, updated_at`

// CreateIdeationSession persists a new session at revision one. It needs no
// remote issue and no run. A reused ID returns ErrIdeationSessionExists.
func (s *Store) CreateIdeationSession(ctx context.Context, session IdeationSession) (IdeationSession, error) {
	if err := validateIdeationIdentifier("session id", session.ID); err != nil {
		return IdeationSession{}, err
	}
	if strings.TrimSpace(session.RepositoryPath) == "" || strings.ContainsAny(session.RepositoryPath, "\x00\r\n") {
		return IdeationSession{}, errors.New("ideation session repository path is required and must be single-line")
	}
	if err := validateIdeationText("session title", session.Title, maxIdeationTitleBytes, false); err != nil {
		return IdeationSession{}, err
	}
	if err := validateIdeationStateChange(session.State, session.StateReason); err != nil {
		return IdeationSession{}, err
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = time.Now()
	}
	session.CreatedAt = session.CreatedAt.UTC()
	session.UpdatedAt = session.CreatedAt
	session.Revision = 1
	session.BriefRevision, session.PlanRevision, session.ApprovalID = 0, 0, ""
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO ideation_sessions (`+ideationSessionColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, 0, 0, '', ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		session.ID, session.RepositoryPath, session.Title, session.State, session.StateReason, session.Revision,
		formatIdeationTime(session.CreatedAt), formatIdeationTime(session.UpdatedAt))
	if err != nil {
		return IdeationSession{}, fmt.Errorf("create ideation session %q: %w", session.ID, err)
	}
	if inserted, err := result.RowsAffected(); err != nil {
		return IdeationSession{}, fmt.Errorf("inspect ideation session %q creation: %w", session.ID, err)
	} else if inserted != 1 {
		return IdeationSession{}, fmt.Errorf("%w: %q", ErrIdeationSessionExists, session.ID)
	}
	return session, nil
}

// IdeationSession returns one session or ErrIdeationSessionNotFound.
func (s *Store) IdeationSession(ctx context.Context, sessionID string) (IdeationSession, error) {
	return scanIdeationSession(s.db.QueryRowContext(ctx, `SELECT `+ideationSessionColumns+` FROM ideation_sessions WHERE id = ?`, sessionID), sessionID)
}

// IdeationSessions returns every session of one repository, newest update
// first and then by ID.
func (s *Store) IdeationSessions(ctx context.Context, repositoryPath string) ([]IdeationSession, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+ideationSessionColumns+`
		FROM ideation_sessions
		WHERE repository_path = ?
		ORDER BY updated_at DESC, id ASC`, repositoryPath)
	if err != nil {
		return nil, fmt.Errorf("list ideation sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	sessions := []IdeationSession{}
	for rows.Next() {
		session, err := scanIdeationSession(rows, "")
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list ideation sessions: %w", err)
	}
	return sessions, nil
}

// scanIdeationSession decodes one ideationSessionColumns row. A missing row
// returns ErrIdeationSessionNotFound for sessionID.
func scanIdeationSession(row interface{ Scan(...any) error }, sessionID string) (IdeationSession, error) {
	var session IdeationSession
	var createdAt, updatedAt string
	err := row.Scan(&session.ID, &session.RepositoryPath, &session.Title, &session.State, &session.StateReason,
		&session.Revision, &session.BriefRevision, &session.PlanRevision, &session.ApprovalID, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return IdeationSession{}, fmt.Errorf("%w: %q", ErrIdeationSessionNotFound, sessionID)
	}
	if err != nil {
		return IdeationSession{}, fmt.Errorf("read ideation session: %w", err)
	}
	if session.CreatedAt, err = parseIdeationTime(createdAt); err != nil {
		return IdeationSession{}, err
	}
	if session.UpdatedAt, err = parseIdeationTime(updatedAt); err != nil {
		return IdeationSession{}, err
	}
	return session, nil
}

// beginIdeationWrite opens a write transaction, reserves SQLite's write lock
// before it reads, and returns the current session. The early reservation
// keeps two processes from both reading and then failing to upgrade a lock.
func (s *Store) beginIdeationWrite(ctx context.Context, write IdeationWrite) (*sql.Tx, IdeationSession, error) {
	if err := validateIdeationIdentifier("session id", write.SessionID); err != nil {
		return nil, IdeationSession{}, err
	}
	if err := validateIdeationStateChange(write.State, write.StateReason); err != nil {
		return nil, IdeationSession{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, IdeationSession{}, fmt.Errorf("begin ideation session %q write: %w", write.SessionID, err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE ideation_sessions SET id = id WHERE id = ?", write.SessionID); err != nil {
		_ = tx.Rollback()
		return nil, IdeationSession{}, fmt.Errorf("reserve ideation session %q: %w", write.SessionID, err)
	}
	session, err := scanIdeationSession(tx.QueryRowContext(ctx, `SELECT `+ideationSessionColumns+` FROM ideation_sessions WHERE id = ?`, write.SessionID), write.SessionID)
	if err != nil {
		_ = tx.Rollback()
		return nil, IdeationSession{}, err
	}
	return tx, session, nil
}

// checkIdeationRevision returns the typed conflict when the writer's expected
// revision is not the current session revision.
func checkIdeationRevision(session IdeationSession, write IdeationWrite) error {
	if session.Revision != write.ExpectedRevision {
		return &IdeationRevisionConflictError{SessionID: session.ID, Expected: write.ExpectedRevision, Actual: session.Revision}
	}
	return nil
}

// commitIdeationWrite advances the session revision, records the lifecycle
// state of write, and commits tx. The caller has already applied its content
// change to session and to tx.
func commitIdeationWrite(ctx context.Context, tx *sql.Tx, session IdeationSession, write IdeationWrite) (IdeationSession, error) {
	defer func() { _ = tx.Rollback() }()
	session.State = write.State
	session.StateReason = write.StateReason
	session.Revision = write.ExpectedRevision + 1
	session.UpdatedAt = ideationWriteTime(write)
	result, err := tx.ExecContext(ctx, `
		UPDATE ideation_sessions
		SET state = ?, state_reason = ?, revision = ?, brief_revision = ?, plan_revision = ?,
			approval_id = ?, updated_at = ?
		WHERE id = ? AND revision = ?`,
		session.State, session.StateReason, session.Revision, session.BriefRevision, session.PlanRevision,
		session.ApprovalID, formatIdeationTime(session.UpdatedAt), session.ID, write.ExpectedRevision)
	if err != nil {
		return IdeationSession{}, fmt.Errorf("save ideation session %q: %w", session.ID, err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return IdeationSession{}, fmt.Errorf("inspect ideation session %q update: %w", session.ID, err)
	} else if changed != 1 {
		return IdeationSession{}, &IdeationRevisionConflictError{SessionID: session.ID, Expected: write.ExpectedRevision, Actual: -1}
	}
	if err := completeCommandInWrite(ctx, tx, session, write); err != nil {
		return IdeationSession{}, err
	}
	if err := tx.Commit(); err != nil {
		return IdeationSession{}, fmt.Errorf("commit ideation session %q: %w", session.ID, err)
	}
	return session, nil
}

// TransitionIdeationSession records a lifecycle change with no content change,
// for example cancel, defer, no-build, or role start.
func (s *Store) TransitionIdeationSession(ctx context.Context, write IdeationWrite) (IdeationSession, error) {
	tx, session, err := s.beginIdeationWrite(ctx, write)
	if err != nil {
		return IdeationSession{}, err
	}
	if err := checkIdeationRevision(session, write); err != nil {
		_ = tx.Rollback()
		return IdeationSession{}, err
	}
	return commitIdeationWrite(ctx, tx, session, write)
}

// validateIdeationStateChange checks a declared state and its bounded reason.
func validateIdeationStateChange(state IdeationState, reason string) error {
	if !validIdeationState(state) {
		return fmt.Errorf("ideation state %q is not declared", state)
	}
	return validateIdeationText("state reason", reason, MaxIdeationReasonBytes, false)
}

// validateIdeationIdentifier checks one bounded, command-safe identity.
func validateIdeationIdentifier(label, value string) error {
	if len(value) > maxIdeationIdentifierBytes || !safeQuestionIdentifier(value) {
		return fmt.Errorf("ideation %s %q must be 1 to %d letters, digits, '.', '_', or '-'", label, value, maxIdeationIdentifierBytes)
	}
	return nil
}

// validateIdeationText checks optional or required text against a byte bound
// and rejects NUL bytes. Multi-line text is allowed.
func validateIdeationText(label, value string, maxBytes int, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("ideation %s is required", label)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("ideation %s is %d bytes; the limit is %d", label, len(value), maxBytes)
	}
	if strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("ideation %s contains a NUL byte", label)
	}
	return nil
}

// formatIdeationTime serializes a timestamp so text order is time order.
func formatIdeationTime(value time.Time) string {
	return value.UTC().Format(runTimestampLayout)
}

// parseIdeationTime decodes one persisted ideation timestamp.
func parseIdeationTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse ideation timestamp %q: %w", value, err)
	}
	return parsed, nil
}

// ideationSchema creates the additive ideation tables of schema version 42.
// No existing table changes, so stored runs stay readable. SQL foreign keys
// are not used, as in the rest of the store; child rows carry session_id.
var ideationSchema = []string{
	`CREATE TABLE IF NOT EXISTS ideation_sessions (
		id TEXT PRIMARY KEY,
		repository_path TEXT NOT NULL,
		title TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL,
		state_reason TEXT NOT NULL DEFAULT '',
		revision INTEGER NOT NULL,
		brief_revision INTEGER NOT NULL DEFAULT 0,
		plan_revision INTEGER NOT NULL DEFAULT 0,
		approval_id TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	"CREATE INDEX IF NOT EXISTS ideation_sessions_by_repository ON ideation_sessions (repository_path, updated_at)",
	`CREATE TABLE IF NOT EXISTS ideation_turns (
		session_id TEXT NOT NULL,
		id TEXT NOT NULL,
		sequence INTEGER NOT NULL,
		author TEXT NOT NULL,
		role TEXT NOT NULL DEFAULT '',
		invocation_id TEXT NOT NULL DEFAULT '',
		body TEXT NOT NULL,
		session_revision INTEGER NOT NULL,
		created_at TEXT NOT NULL,
		PRIMARY KEY (session_id, id),
		UNIQUE (session_id, sequence)
	)`,
	`CREATE TABLE IF NOT EXISTS ideation_brief_revisions (
		session_id TEXT NOT NULL,
		revision INTEGER NOT NULL,
		brief TEXT NOT NULL,
		created_at TEXT NOT NULL,
		PRIMARY KEY (session_id, revision)
	)`,
	`CREATE TABLE IF NOT EXISTS ideation_plan_revisions (
		session_id TEXT NOT NULL,
		revision INTEGER NOT NULL,
		source_brief_revision INTEGER NOT NULL,
		context_revision TEXT NOT NULL,
		nodes TEXT NOT NULL,
		created_at TEXT NOT NULL,
		PRIMARY KEY (session_id, revision)
	)`,
	`CREATE TABLE IF NOT EXISTS ideation_approvals (
		session_id TEXT NOT NULL,
		id TEXT NOT NULL,
		plan_revision INTEGER NOT NULL,
		target TEXT NOT NULL,
		payload BLOB NOT NULL,
		payload_sha256 TEXT NOT NULL,
		relationship_mapping BLOB NOT NULL,
		relationship_mapping_sha256 TEXT NOT NULL,
		approved_at TEXT NOT NULL,
		PRIMARY KEY (session_id, id)
	)`,
	`CREATE TABLE IF NOT EXISTS ideation_publication_receipts (
		session_id TEXT NOT NULL,
		id TEXT NOT NULL,
		approval_id TEXT NOT NULL,
		outcome TEXT NOT NULL,
		items TEXT NOT NULL,
		relationships TEXT NOT NULL,
		detail TEXT NOT NULL DEFAULT '',
		recorded_at TEXT NOT NULL,
		PRIMARY KEY (session_id, id)
	)`,
}

// migrateIdeationSchema applies ideationSchema inside the migration transaction.
func migrateIdeationSchema(ctx context.Context, tx *sql.Tx) error {
	for _, statement := range ideationSchema {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// ideationWriteTime returns the UTC write time of write, or the current time
// when the caller left it zero.
func ideationWriteTime(write IdeationWrite) time.Time {
	if write.At.IsZero() {
		return time.Now().UTC()
	}
	return write.At.UTC()
}

// IdeationHistory is the complete inspectable record of one session.
type IdeationHistory struct {
	// Session is the current session projection.
	Session IdeationSession
	// Turns contains every accepted turn in conversation order.
	Turns []IdeationTurn
	// Briefs contains every brief revision, oldest first.
	Briefs []BriefRevision
	// Plans contains every plan revision, oldest first, with Stale derived
	// from the current brief revision.
	Plans []PlanRevision
	// Approvals contains every approval, oldest first. Only the one named by
	// Session.ApprovalID is current.
	Approvals []PlanApproval
	// Receipts contains every publication receipt, oldest first.
	Receipts []PublicationReceipt
	// Operations contains the publication ledger of every approval, by
	// approval time and then in execution order.
	Operations []PublicationOperation
}

// IdeationHistory reads one session and its complete history in one read
// transaction, so the parts are consistent with each other.
func (s *Store) IdeationHistory(ctx context.Context, sessionID string) (IdeationHistory, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return IdeationHistory{}, fmt.Errorf("begin ideation history read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var history IdeationHistory
	if history.Session, err = scanIdeationSession(tx.QueryRowContext(ctx, `SELECT `+ideationSessionColumns+` FROM ideation_sessions WHERE id = ?`, sessionID), sessionID); err != nil {
		return IdeationHistory{}, err
	}
	if history.Turns, err = queryIdeationRows(ctx, tx, `SELECT `+ideationTurnColumns+` FROM ideation_turns WHERE session_id = ? ORDER BY sequence`, sessionID, scanIdeationTurn); err != nil {
		return IdeationHistory{}, err
	}
	if history.Briefs, err = queryIdeationRows(ctx, tx, `SELECT session_id, revision, brief, created_at FROM ideation_brief_revisions WHERE session_id = ? ORDER BY revision`, sessionID, scanBriefRevision); err != nil {
		return IdeationHistory{}, err
	}
	if history.Plans, err = queryIdeationRows(ctx, tx, `SELECT session_id, revision, source_brief_revision, context_revision, nodes, created_at FROM ideation_plan_revisions WHERE session_id = ? ORDER BY revision`, sessionID, scanPlanRevision); err != nil {
		return IdeationHistory{}, err
	}
	if history.Approvals, err = queryIdeationRows(ctx, tx, `SELECT `+planApprovalColumns+` FROM ideation_approvals WHERE session_id = ? ORDER BY approved_at, id`, sessionID, scanPlanApproval); err != nil {
		return IdeationHistory{}, err
	}
	if history.Receipts, err = queryIdeationRows(ctx, tx, `SELECT `+publicationReceiptColumns+` FROM ideation_publication_receipts r JOIN ideation_approvals a ON a.session_id = r.session_id AND a.id = r.approval_id WHERE r.session_id = ? ORDER BY r.recorded_at, r.id`, sessionID, scanPublicationReceipt); err != nil {
		return IdeationHistory{}, err
	}
	if history.Operations, err = queryIdeationRows(ctx, tx, `SELECT `+prefixedPublicationOperationColumns+` FROM ideation_publication_operations o JOIN ideation_approvals a ON a.session_id = o.session_id AND a.id = o.approval_id WHERE o.session_id = ? ORDER BY a.approved_at, o.approval_id, o.sequence`, sessionID, scanPublicationOperation); err != nil {
		return IdeationHistory{}, err
	}
	for index := range history.Plans {
		history.Plans[index].Stale = history.Plans[index].SourceBriefRevision != history.Session.BriefRevision
	}
	return history, nil
}

// queryIdeationRows reads every row of one session-scoped query with scan.
func queryIdeationRows[T any](ctx context.Context, tx *sql.Tx, query, sessionID string, scan func(interface{ Scan(...any) error }) (T, error)) ([]T, error) {
	rows, err := tx.QueryContext(ctx, query, sessionID)
	if err != nil {
		return nil, fmt.Errorf("read ideation history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	values := []T{}
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read ideation history: %w", err)
	}
	return values, nil
}
