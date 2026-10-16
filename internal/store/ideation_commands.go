package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

const (
	// MaxIdeationCommandPayloadBytes bounds the JSON payload of one session
	// command. It leaves room for a complete plan draft (MaxPlanBytes).
	MaxIdeationCommandPayloadBytes = 2 * 1024 * 1024
	// MaxIdeationCommandResultBytes bounds the JSON result of one session
	// command, for example a rendered publication preview.
	MaxIdeationCommandResultBytes = 4 * 1024 * 1024
)

// ErrIdeationCommandNotFound reports that the inbox holds no command with the
// requested ID.
var ErrIdeationCommandNotFound = errors.New("ideation command not found")

// ErrIdeationCommandSettled reports a change to a command that already has
// its outcome. A command completes exactly once.
var ErrIdeationCommandSettled = errors.New("ideation command is already completed")

// IdeationCommandState is the processing state of one inbox command.
type IdeationCommandState string

const (
	// IdeationCommandAccepted means the command is durable and waits for the
	// coordinator. Acceptance does not mean that the command was applied.
	IdeationCommandAccepted IdeationCommandState = "accepted"
	// IdeationCommandApplying means the coordinator started to apply the
	// command. Found after a restart, it means the coordinator resumes it.
	IdeationCommandApplying IdeationCommandState = "applying"
	// IdeationCommandCompleted means the command has its final outcome.
	IdeationCommandCompleted IdeationCommandState = "completed"
)

// IdeationCommandOutcome is the final result of one completed command.
type IdeationCommandOutcome string

const (
	// IdeationCommandApplied means the coordinator applied the command.
	IdeationCommandApplied IdeationCommandOutcome = "applied"
	// IdeationCommandConflict means the command named a session revision that
	// was no longer current. SessionRevision names the current revision.
	IdeationCommandConflict IdeationCommandOutcome = "conflict"
	// IdeationCommandRefused means the coordinator refused the command, for
	// example because the session state does not allow it.
	IdeationCommandRefused IdeationCommandOutcome = "refused"
	// IdeationCommandUnsupported means this build has no behavior for the
	// command, for example no tracker planning capability.
	IdeationCommandUnsupported IdeationCommandOutcome = "unsupported"
	// IdeationCommandFailed means the coordinator could not apply the command
	// after repeated local failures.
	IdeationCommandFailed IdeationCommandOutcome = "failed"
)

// IdeationCommand is one browser request in the durable session command
// inbox. The local UI appends it; only the coordinator applies it.
type IdeationCommand struct {
	// RequestID is the browser-chosen idempotency key. It is unique in the
	// inbox.
	RequestID string
	// SessionID identifies the target session. A create command names the
	// session to create.
	SessionID string
	// Kind names the command. The session command package owns the
	// vocabulary; the store requires an identifier.
	Kind string
	// ExpectedRevision is the session revision the browser showed. A create
	// command uses zero.
	ExpectedRevision int64
	// Payload is the validated JSON payload of the command.
	Payload []byte
	// Sequence is the inbox order. The store assigns it.
	Sequence int64
	// State is the processing state.
	State IdeationCommandState
	// Attempts counts how often the coordinator started the command.
	Attempts int
	// Outcome is the final result. It is empty until State is completed.
	Outcome IdeationCommandOutcome
	// Detail explains the outcome.
	Detail string
	// SessionRevision is the session revision after an applied command, or
	// the current revision of a conflict.
	SessionRevision int64
	// Result is optional JSON output of the command, for example a preview.
	Result []byte
	// AcceptedAt is when the inbox accepted the command.
	AcceptedAt time.Time
	// CompletedAt is when the command completed, or zero.
	CompletedAt time.Time
}

// IdeationCommandResult is the outcome the coordinator records for one
// command.
type IdeationCommandResult struct {
	// RequestID identifies the command.
	RequestID string
	// Outcome is the final result.
	Outcome IdeationCommandOutcome
	// Detail explains the outcome.
	Detail string
	// SessionRevision is the session revision after the command.
	SessionRevision int64
	// Result is optional JSON output.
	Result []byte
	// At is the completion time. Zero selects the current time.
	At time.Time
}

// ideationCommandColumns is the column order that scanIdeationCommand decodes.
const ideationCommandColumns = `request_id, session_id, kind, expected_revision, payload, sequence, state, attempts,
	outcome, detail, session_revision, result, accepted_at, completed_at`

// AcceptIdeationCommand appends command to the inbox in the accepted state. A
// stored command with the same request ID and content is a replay: it returns
// the stored command, with any outcome, and true. The same ID with other
// content returns ErrIdeationRequestReused.
func (s *Store) AcceptIdeationCommand(ctx context.Context, command IdeationCommand) (IdeationCommand, bool, error) {
	if err := validateIdeationCommand(command); err != nil {
		return IdeationCommand{}, false, err
	}
	if command.AcceptedAt.IsZero() {
		command.AcceptedAt = time.Now()
	}
	command.AcceptedAt = command.AcceptedAt.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IdeationCommand{}, false, fmt.Errorf("begin ideation command %q: %w", command.RequestID, err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO ideation_commands (request_id, session_id, kind, expected_revision, payload, state, attempts,
			outcome, detail, session_revision, result, accepted_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, '', '', 0, '', ?, '')
		ON CONFLICT(request_id) DO NOTHING`,
		command.RequestID, command.SessionID, command.Kind, command.ExpectedRevision, string(command.Payload),
		IdeationCommandAccepted, formatIdeationTime(command.AcceptedAt))
	if err != nil {
		return IdeationCommand{}, false, fmt.Errorf("accept ideation command %q: %w", command.RequestID, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return IdeationCommand{}, false, fmt.Errorf("inspect ideation command %q: %w", command.RequestID, err)
	}
	stored, err := scanIdeationCommand(tx.QueryRowContext(ctx, `SELECT `+ideationCommandColumns+` FROM ideation_commands WHERE request_id = ?`, command.RequestID), command.RequestID)
	if err != nil {
		return IdeationCommand{}, false, err
	}
	if inserted == 0 && (stored.SessionID != command.SessionID || stored.Kind != command.Kind ||
		stored.ExpectedRevision != command.ExpectedRevision || !bytes.Equal(stored.Payload, command.Payload)) {
		return IdeationCommand{}, false, fmt.Errorf("%w: command %q", ErrIdeationRequestReused, command.RequestID)
	}
	if err := tx.Commit(); err != nil {
		return IdeationCommand{}, false, fmt.Errorf("commit ideation command %q: %w", command.RequestID, err)
	}
	return stored, inserted == 0, nil
}

// IdeationCommand returns one inbox command or ErrIdeationCommandNotFound.
func (s *Store) IdeationCommand(ctx context.Context, requestID string) (IdeationCommand, error) {
	return scanIdeationCommand(s.db.QueryRowContext(ctx, `SELECT `+ideationCommandColumns+` FROM ideation_commands WHERE request_id = ?`, requestID), requestID)
}

// PendingIdeationCommands returns every accepted or applying command in inbox
// order. The coordinator applies them in this order.
func (s *Store) PendingIdeationCommands(ctx context.Context) ([]IdeationCommand, error) {
	return s.queryIdeationCommands(ctx, `SELECT `+ideationCommandColumns+` FROM ideation_commands WHERE state IN (?, ?) ORDER BY sequence`,
		IdeationCommandAccepted, IdeationCommandApplying)
}

// IdeationCommands returns at most limit commands of one session, newest
// first. A reconnecting browser reads them to show accepted and completed
// requests.
func (s *Store) IdeationCommands(ctx context.Context, sessionID string, limit int) ([]IdeationCommand, error) {
	return s.queryIdeationCommands(ctx, `SELECT `+ideationCommandColumns+` FROM ideation_commands WHERE session_id = ? ORDER BY sequence DESC LIMIT ?`,
		sessionID, limit)
}

// BeginIdeationCommand marks a pending command as applying and counts the
// attempt. A completed command returns ErrIdeationCommandSettled.
func (s *Store) BeginIdeationCommand(ctx context.Context, requestID string) (IdeationCommand, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE ideation_commands SET state = ?, attempts = attempts + 1
		WHERE request_id = ? AND state IN (?, ?)`,
		IdeationCommandApplying, requestID, IdeationCommandAccepted, IdeationCommandApplying)
	if err != nil {
		return IdeationCommand{}, fmt.Errorf("begin ideation command %q: %w", requestID, err)
	}
	command, err := s.IdeationCommand(ctx, requestID)
	if err != nil {
		return IdeationCommand{}, err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return IdeationCommand{}, fmt.Errorf("inspect ideation command %q: %w", requestID, err)
	} else if changed != 1 {
		return IdeationCommand{}, fmt.Errorf("%w: %q", ErrIdeationCommandSettled, requestID)
	}
	return command, nil
}

// CompleteIdeationCommand records the outcome of a pending command. A command
// that already completed keeps its first outcome and returns unchanged, so a
// coordinator that resumes after a restart cannot overwrite it.
func (s *Store) CompleteIdeationCommand(ctx context.Context, outcome IdeationCommandResult) (IdeationCommand, error) {
	if err := validateIdeationCommandResult(outcome); err != nil {
		return IdeationCommand{}, err
	}
	if _, err := s.db.ExecContext(ctx, completeIdeationCommandStatement, completeIdeationCommandArguments(outcome)...); err != nil {
		return IdeationCommand{}, fmt.Errorf("complete ideation command %q: %w", outcome.RequestID, err)
	}
	return s.IdeationCommand(ctx, outcome.RequestID)
}

// completeIdeationCommandStatement completes one pending command.
const completeIdeationCommandStatement = `
	UPDATE ideation_commands
	SET state = ?, outcome = ?, detail = ?, session_revision = ?, result = ?, completed_at = ?
	WHERE request_id = ? AND state IN (?, ?)`

// completeIdeationCommandArguments returns the arguments of
// completeIdeationCommandStatement for outcome.
func completeIdeationCommandArguments(outcome IdeationCommandResult) []any {
	at := outcome.At
	if at.IsZero() {
		at = time.Now()
	}
	return []any{IdeationCommandCompleted, outcome.Outcome, outcome.Detail, outcome.SessionRevision, string(outcome.Result),
		formatIdeationTime(at), outcome.RequestID, IdeationCommandAccepted, IdeationCommandApplying}
}

// completeCommandInWrite completes the command a session write names, in the
// write's transaction. The command must still be pending, so one command
// changes the session at most once.
func completeCommandInWrite(ctx context.Context, tx *sql.Tx, session IdeationSession, write IdeationWrite) error {
	if write.CommandID == "" {
		return nil
	}
	result, err := tx.ExecContext(ctx, completeIdeationCommandStatement, completeIdeationCommandArguments(IdeationCommandResult{
		RequestID: write.CommandID, Outcome: IdeationCommandApplied, SessionRevision: session.Revision, At: session.UpdatedAt,
	})...)
	if err != nil {
		return fmt.Errorf("complete ideation command %q: %w", write.CommandID, err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("inspect ideation command %q: %w", write.CommandID, err)
	} else if changed != 1 {
		return fmt.Errorf("%w: %q", ErrIdeationCommandSettled, write.CommandID)
	}
	return nil
}

// queryIdeationCommands reads every command row of query.
func (s *Store) queryIdeationCommands(ctx context.Context, query string, arguments ...any) ([]IdeationCommand, error) {
	rows, err := s.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("read ideation commands: %w", err)
	}
	defer func() { _ = rows.Close() }()
	commands := []IdeationCommand{}
	for rows.Next() {
		command, err := scanIdeationCommand(rows, "")
		if err != nil {
			return nil, err
		}
		commands = append(commands, command)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read ideation commands: %w", err)
	}
	return commands, nil
}

// scanIdeationCommand decodes one ideationCommandColumns row. A missing row
// returns ErrIdeationCommandNotFound for requestID.
func scanIdeationCommand(row interface{ Scan(...any) error }, requestID string) (IdeationCommand, error) {
	var command IdeationCommand
	var payload, result, acceptedAt, completedAt string
	err := row.Scan(&command.RequestID, &command.SessionID, &command.Kind, &command.ExpectedRevision, &payload,
		&command.Sequence, &command.State, &command.Attempts, &command.Outcome, &command.Detail,
		&command.SessionRevision, &result, &acceptedAt, &completedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return IdeationCommand{}, fmt.Errorf("%w: %q", ErrIdeationCommandNotFound, requestID)
	}
	if err != nil {
		return IdeationCommand{}, fmt.Errorf("read ideation command: %w", err)
	}
	command.Payload = []byte(payload)
	if result != "" {
		command.Result = []byte(result)
	}
	if command.AcceptedAt, err = parseIdeationTime(acceptedAt); err != nil {
		return IdeationCommand{}, err
	}
	if completedAt != "" {
		if command.CompletedAt, err = parseIdeationTime(completedAt); err != nil {
			return IdeationCommand{}, err
		}
	}
	return command, nil
}

// validateIdeationCommand checks the browser-supplied fields of one command.
func validateIdeationCommand(command IdeationCommand) error {
	if err := validateIdeationIdentifier("command request id", command.RequestID); err != nil {
		return err
	}
	if err := validateIdeationIdentifier("command session id", command.SessionID); err != nil {
		return err
	}
	if err := validateIdeationIdentifier("command kind", command.Kind); err != nil {
		return err
	}
	if command.ExpectedRevision < 0 {
		return fmt.Errorf("ideation command expected revision %d is negative", command.ExpectedRevision)
	}
	return validateCommandJSON("payload", command.Payload, MaxIdeationCommandPayloadBytes, true)
}

// validateIdeationCommandResult checks one coordinator outcome.
func validateIdeationCommandResult(outcome IdeationCommandResult) error {
	if err := validateIdeationIdentifier("command request id", outcome.RequestID); err != nil {
		return err
	}
	switch outcome.Outcome {
	case IdeationCommandApplied, IdeationCommandConflict, IdeationCommandRefused, IdeationCommandUnsupported, IdeationCommandFailed:
	default:
		return fmt.Errorf("ideation command outcome %q is not declared", outcome.Outcome)
	}
	if err := validateIdeationText("command detail", outcome.Detail, MaxIdeationReasonBytes, false); err != nil {
		return err
	}
	return validateCommandJSON("result", outcome.Result, MaxIdeationCommandResultBytes, false)
}

// validateCommandJSON checks that value is bounded, valid JSON. An optional
// value may be empty.
func validateCommandJSON(label string, value []byte, maxBytes int, required bool) error {
	if len(value) == 0 && !required {
		return nil
	}
	if len(value) > maxBytes {
		return fmt.Errorf("ideation command %s is %d bytes; the limit is %d", label, len(value), maxBytes)
	}
	if !json.Valid(value) {
		return fmt.Errorf("ideation command %s is not valid JSON", label)
	}
	return nil
}

// ideationCommandSchema creates the additive command inbox of schema version
// 45 (ADR 0024).
var ideationCommandSchema = []string{
	`CREATE TABLE IF NOT EXISTS ideation_commands (
		sequence INTEGER PRIMARY KEY AUTOINCREMENT,
		request_id TEXT NOT NULL UNIQUE,
		session_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		expected_revision INTEGER NOT NULL,
		payload TEXT NOT NULL,
		state TEXT NOT NULL,
		attempts INTEGER NOT NULL DEFAULT 0,
		outcome TEXT NOT NULL DEFAULT '',
		detail TEXT NOT NULL DEFAULT '',
		session_revision INTEGER NOT NULL DEFAULT 0,
		result TEXT NOT NULL DEFAULT '',
		accepted_at TEXT NOT NULL,
		completed_at TEXT NOT NULL DEFAULT ''
	)`,
	"CREATE INDEX IF NOT EXISTS ideation_commands_by_session ON ideation_commands (session_id, sequence)",
	"CREATE INDEX IF NOT EXISTS ideation_commands_by_state ON ideation_commands (state, sequence)",
}

// CommandInbox is the narrow write handle of the local UI. It can append a
// command and read what a command submission needs. It cannot change
// sessions, runs, or any other record, and opening it never creates,
// migrates, or backs up the store.
type CommandInbox struct {
	store *Store
}

// OpenCommandInbox opens an existing, current operational store for command
// submission. It applies the same file checks as OpenReadOnly and refuses a
// store that needs a migration.
func OpenCommandInbox(ctx context.Context, path string) (*CommandInbox, error) {
	absolutePath, err := checkExistingStoreFile(path)
	if err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: absolutePath, RawQuery: "mode=rw&_pragma=busy_timeout(5000)"}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open command inbox: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	version, err := readSchemaVersion(ctx, database, absolutePath)
	if err != nil {
		_ = database.Close()
		return nil, err
	}
	if version > CurrentSchemaVersion {
		_ = database.Close()
		return nil, &UnknownSchemaVersionError{Version: version, Supported: CurrentSchemaVersion}
	}
	if version < CurrentSchemaVersion {
		_ = database.Close()
		return nil, errors.New("operational store schema requires migration")
	}
	return &CommandInbox{store: &Store{db: database, path: absolutePath, schemaVersion: version}}, nil
}

// AcceptIdeationCommand appends one command; see Store.AcceptIdeationCommand.
func (inbox *CommandInbox) AcceptIdeationCommand(ctx context.Context, command IdeationCommand) (IdeationCommand, bool, error) {
	return inbox.store.AcceptIdeationCommand(ctx, command)
}

// IdeationCommand reads one command; see Store.IdeationCommand.
func (inbox *CommandInbox) IdeationCommand(ctx context.Context, requestID string) (IdeationCommand, error) {
	return inbox.store.IdeationCommand(ctx, requestID)
}

// IdeationSession reads one session; see Store.IdeationSession.
func (inbox *CommandInbox) IdeationSession(ctx context.Context, sessionID string) (IdeationSession, error) {
	return inbox.store.IdeationSession(ctx, sessionID)
}

// ReadSupervisorHeartbeat reads the coordinator heartbeat; see
// Store.ReadSupervisorHeartbeat.
func (inbox *CommandInbox) ReadSupervisorHeartbeat(ctx context.Context) (*SupervisorHeartbeat, error) {
	return inbox.store.ReadSupervisorHeartbeat(ctx)
}

// Close closes the handle.
func (inbox *CommandInbox) Close() error {
	return inbox.store.Close()
}

// checkExistingStoreFile returns the absolute path of an existing, private,
// regular, non-empty store file. It changes nothing on disk.
func checkExistingStoreFile(path string) (string, error) {
	if path == "" {
		return "", errors.New("operational store path is required")
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve operational store path: %w", err)
	}
	storeDirectory := filepath.Dir(absolutePath)
	if info, err := os.Stat(storeDirectory); err != nil {
		return "", fmt.Errorf("inspect operational store directory: %w", err)
	} else if !info.IsDir() {
		return "", errors.New("operational store parent is not a directory")
	} else if info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("operational store directory is not private")
	}
	info, err := os.Lstat(absolutePath)
	if err != nil {
		return "", fmt.Errorf("inspect operational store: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("operational store is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o400 == 0 {
		return "", errors.New("operational store file is not private and owner-readable")
	}
	if info.Size() == 0 {
		return "", &UnversionedDatabaseError{Path: absolutePath, Err: errors.New("database file is empty")}
	}
	return absolutePath, nil
}
