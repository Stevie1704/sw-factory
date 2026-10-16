package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MaxIdeationTurnBytes bounds the body of one conversation turn.
const MaxIdeationTurnBytes = 64 * 1024

// ErrIdeationRequestReused reports that a caller reused a turn, approval, or
// receipt ID for different content. A retry with identical content is a
// replay and returns the stored record instead.
var ErrIdeationRequestReused = errors.New("ideation request id reused with different content")

// IdeationTurnAuthor identifies who wrote one conversation turn.
type IdeationTurnAuthor string

const (
	// IdeationTurnAuthorUser is a turn the local operator submitted.
	IdeationTurnAuthorUser IdeationTurnAuthor = "user"
	// IdeationTurnAuthorRole is a turn taken from an accepted role report.
	IdeationTurnAuthorRole IdeationTurnAuthor = "role"
)

// IdeationTurn is one accepted conversation turn. Turns are append-only.
type IdeationTurn struct {
	// SessionID identifies the owning session. The store fills it.
	SessionID string
	// ID is the stable turn identity and the idempotency key. A browser
	// submission uses its request ID; a role result uses a key derived from
	// its invocation. Appending an existing ID with the same content replays
	// the stored turn.
	ID string
	// Sequence is the 1-based conversation order. The store assigns it.
	Sequence int64
	// Author identifies the user or a role.
	Author IdeationTurnAuthor
	// Role names the factory role for a role turn and is empty for a user turn.
	Role string
	// InvocationID optionally identifies the role invocation that produced the
	// turn.
	InvocationID string
	// Body is the turn text.
	Body string
	// SessionRevision is the session revision that accepted the turn. The
	// store assigns it.
	SessionRevision int64
	// CreatedAt is when the turn was accepted. The store assigns it from the
	// write time.
	CreatedAt time.Time
}

// AppendIdeationTurn appends turn after the newest turn when the session still
// has write.ExpectedRevision. A turn whose ID is already stored is a replay:
// the stored turn and the current session return unchanged, independent of the
// expected revision, so a retried submission never appends twice.
func (s *Store) AppendIdeationTurn(ctx context.Context, write IdeationWrite, turn IdeationTurn) (IdeationSession, IdeationTurn, error) {
	if err := validateIdeationTurn(turn); err != nil {
		return IdeationSession{}, IdeationTurn{}, err
	}
	tx, session, err := s.beginIdeationWrite(ctx, write)
	if err != nil {
		return IdeationSession{}, IdeationTurn{}, err
	}
	defer func() { _ = tx.Rollback() }()
	stored, err := scanIdeationTurn(tx.QueryRowContext(ctx, `SELECT `+ideationTurnColumns+` FROM ideation_turns WHERE session_id = ? AND id = ?`, session.ID, turn.ID))
	switch {
	case err == nil:
		if stored.Author != turn.Author || stored.Role != turn.Role || stored.InvocationID != turn.InvocationID || stored.Body != turn.Body {
			return IdeationSession{}, IdeationTurn{}, fmt.Errorf("%w: turn %q", ErrIdeationRequestReused, turn.ID)
		}
		return session, stored, nil
	case !errors.Is(err, sql.ErrNoRows):
		return IdeationSession{}, IdeationTurn{}, err
	}
	if err := checkIdeationRevision(session, write); err != nil {
		return IdeationSession{}, IdeationTurn{}, err
	}
	if turn, err = insertIdeationTurn(ctx, tx, session, write, turn); err != nil {
		return IdeationSession{}, IdeationTurn{}, err
	}
	updated, err := commitIdeationWrite(ctx, tx, session, write)
	if err != nil {
		return IdeationSession{}, IdeationTurn{}, err
	}
	return updated, turn, nil
}

// insertIdeationTurn appends turn after the newest turn of session inside tx.
// The caller validated the turn, checked the session revision, and commits
// the write that accepts the turn.
func insertIdeationTurn(ctx context.Context, tx *sql.Tx, session IdeationSession, write IdeationWrite, turn IdeationTurn) (IdeationTurn, error) {
	turn.SessionID = session.ID
	turn.SessionRevision = write.ExpectedRevision + 1
	turn.CreatedAt = ideationWriteTime(write)
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence), 0) + 1 FROM ideation_turns WHERE session_id = ?", session.ID).Scan(&turn.Sequence); err != nil {
		return IdeationTurn{}, fmt.Errorf("order ideation turn %q: %w", turn.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ideation_turns (`+ideationTurnColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		turn.SessionID, turn.ID, turn.Sequence, turn.Author, turn.Role, turn.InvocationID, turn.Body,
		turn.SessionRevision, formatIdeationTime(turn.CreatedAt)); err != nil {
		return IdeationTurn{}, fmt.Errorf("append ideation turn %q: %w", turn.ID, err)
	}
	return turn, nil
}

// ideationTurnColumns is the column order that scanIdeationTurn decodes.
const ideationTurnColumns = `session_id, id, sequence, author, role, invocation_id, body, session_revision, created_at`

// scanIdeationTurn decodes one ideationTurnColumns row.
func scanIdeationTurn(row interface{ Scan(...any) error }) (IdeationTurn, error) {
	var turn IdeationTurn
	var createdAt string
	if err := row.Scan(&turn.SessionID, &turn.ID, &turn.Sequence, &turn.Author, &turn.Role, &turn.InvocationID,
		&turn.Body, &turn.SessionRevision, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return IdeationTurn{}, err
		}
		return IdeationTurn{}, fmt.Errorf("read ideation turn: %w", err)
	}
	var err error
	turn.CreatedAt, err = parseIdeationTime(createdAt)
	return turn, err
}

// validateIdeationTurn checks the caller-supplied fields of one turn.
func validateIdeationTurn(turn IdeationTurn) error {
	if err := validateIdeationIdentifier("turn id", turn.ID); err != nil {
		return err
	}
	switch turn.Author {
	case IdeationTurnAuthorUser:
		if turn.Role != "" || turn.InvocationID != "" {
			return errors.New("ideation user turn must not name a role or invocation")
		}
	case IdeationTurnAuthorRole:
		if err := validateIdeationIdentifier("turn role", turn.Role); err != nil {
			return err
		}
		if turn.InvocationID != "" {
			if err := validateIdeationIdentifier("turn invocation id", turn.InvocationID); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("ideation turn author %q is not declared", turn.Author)
	}
	return validateIdeationText("turn body", turn.Body, MaxIdeationTurnBytes, true)
}
