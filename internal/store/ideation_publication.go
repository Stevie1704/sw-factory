package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// maxPublicationOperationIDBytes bounds one ledger operation identity. It is
// larger than other ideation identities because it joins node identities.
const maxPublicationOperationIDBytes = 512

// ErrPublicationLedgerMismatch reports a ledger start whose operations differ
// from the ledger already planned for the same approval. An approved plan is
// published exactly once in one order; it is never regenerated.
var ErrPublicationLedgerMismatch = errors.New("publication ledger differs from the planned ledger")

// ErrPublicationOperationNotFound reports a ledger update for an operation
// that the ledger does not hold.
var ErrPublicationOperationNotFound = errors.New("publication operation not found")

// ErrPublicationOperationSettled reports an update that would change a
// succeeded operation or a recorded external identity.
var ErrPublicationOperationSettled = errors.New("publication operation is settled")

// PublicationOperationKind classifies one ledger operation.
type PublicationOperationKind string

const (
	// PublicationOperationCreateItem creates one tracker item for one plan
	// node.
	PublicationOperationCreateItem PublicationOperationKind = "create_item"
	// PublicationOperationCreateRelationship creates one relationship between
	// two created items.
	PublicationOperationCreateRelationship PublicationOperationKind = "create_relationship"
)

// PublicationOperationState is the recovery state of one ledger operation.
type PublicationOperationState string

const (
	// PublicationOperationPending means the operation has not been attempted,
	// or an earlier attempt is known to have made no change.
	PublicationOperationPending PublicationOperationState = "pending"
	// PublicationOperationAttempted means the write was reserved before it was
	// sent. Found at startup, it means the outcome is unknown.
	PublicationOperationAttempted PublicationOperationState = "attempted"
	// PublicationOperationFailed means the last attempt failed in a way that
	// proves the tracker made no change. A retry may repeat it after a
	// lookup.
	PublicationOperationFailed PublicationOperationState = "failed"
	// PublicationOperationAmbiguous means the outcome is unknown or several
	// tracker items match. Publication stops until a lookup finds exactly one
	// item or the operator reconciles the operation.
	PublicationOperationAmbiguous PublicationOperationState = "ambiguous"
	// PublicationOperationSucceeded means the tracker holds the result. For an
	// item, ExternalID records it. The state is final.
	PublicationOperationSucceeded PublicationOperationState = "succeeded"
)

// PublicationOperation is one row of the per-session, per-approval
// publication ledger (ADR 0022). The ledger is the recovery authority of
// publication: each external identity is recorded here before a dependent
// operation starts.
type PublicationOperation struct {
	// SessionID identifies the owning session. The store fills it.
	SessionID string
	// ApprovalID identifies the approval being published. The store fills it.
	ApprovalID string
	// ID is the stable operation identity inside the approval.
	ID string
	// Sequence is the 1-based execution order. The store assigns it.
	Sequence int
	// Kind classifies the operation.
	Kind PublicationOperationKind
	// NodeID is the plan node a create_item operation publishes.
	NodeID string
	// Correlation is the correlation key that finds the item of a create_item
	// operation in the tracker.
	Correlation string
	// RelationshipKind is the plan relationship kind of a create_relationship
	// operation.
	RelationshipKind string
	// FromNodeID is the start node of a create_relationship operation.
	FromNodeID string
	// ToNodeID is the end node of a create_relationship operation.
	ToNodeID string
	// State is the recovery state.
	State PublicationOperationState
	// ExternalID is the tracker identity of a created item. Once recorded it
	// never changes.
	ExternalID string
	// URL optionally locates the created item.
	URL string
	// Detail optionally explains the state, for example the last error or the
	// operator's reconciliation.
	Detail string
	// Attempts counts the writes sent for this operation.
	Attempts int
	// UpdatedAt is when the row last changed.
	UpdatedAt time.Time
}

// StartPublicationLedger plans the ledger of the session's current approval.
// The first call stores operations in order as pending. A later call with the
// same operations returns the stored ledger unchanged, so a retry or a restart
// resumes the same plan. Different operations return
// ErrPublicationLedgerMismatch.
func (s *Store) StartPublicationLedger(ctx context.Context, sessionID, approvalID string, operations []PublicationOperation, at time.Time) ([]PublicationOperation, error) {
	if err := validatePublicationOperations(operations); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin publication ledger of session %q: %w", sessionID, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := reserveWriteLock(ctx, tx, "ideation_sessions", sessionID); err != nil {
		return nil, fmt.Errorf("reserve ideation session %q: %w", sessionID, err)
	}
	session, err := scanIdeationSession(tx.QueryRowContext(ctx, `SELECT `+ideationSessionColumns+` FROM ideation_sessions WHERE id = ?`, sessionID), sessionID)
	if err != nil {
		return nil, err
	}
	if session.ApprovalID == "" || session.ApprovalID != approvalID {
		return nil, fmt.Errorf("%w: ledger names %q, current is %q", ErrApprovalNotCurrent, approvalID, session.ApprovalID)
	}
	stored, err := publicationLedger(ctx, tx, sessionID, approvalID)
	if err != nil {
		return nil, err
	}
	if len(stored) > 0 {
		if !samePlannedLedger(stored, operations) {
			return nil, fmt.Errorf("%w: approval %q", ErrPublicationLedgerMismatch, approvalID)
		}
		return stored, nil
	}
	when := formatIdeationTime(at)
	for index, operation := range operations {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ideation_publication_operations (`+publicationOperationColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', '', 0, ?)`,
			sessionID, approvalID, operation.ID, index+1, operation.Kind, operation.NodeID, operation.Correlation,
			operation.RelationshipKind, operation.FromNodeID, operation.ToNodeID, PublicationOperationPending, when); err != nil {
			return nil, fmt.Errorf("save publication operation %q: %w", operation.ID, err)
		}
	}
	stored, err = publicationLedger(ctx, tx, sessionID, approvalID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit publication ledger of session %q: %w", sessionID, err)
	}
	return stored, nil
}

// PublicationLedger returns the ledger of one approval in execution order. It
// is empty before publication starts.
func (s *Store) PublicationLedger(ctx context.Context, sessionID, approvalID string) ([]PublicationOperation, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin publication ledger read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	return publicationLedger(ctx, tx, sessionID, approvalID)
}

// RecordPublicationOperation stores the state, external identity, detail, and
// attempt count of one planned operation. A succeeded operation and a recorded
// external identity are final: a change returns ErrPublicationOperationSettled,
// and a replay of the same values succeeds. The write does not change the
// session revision, because only the coordinator writes the ledger.
func (s *Store) RecordPublicationOperation(ctx context.Context, operation PublicationOperation) (PublicationOperation, error) {
	if err := validatePublicationResult(operation); err != nil {
		return PublicationOperation{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PublicationOperation{}, fmt.Errorf("begin publication operation %q: %w", operation.ID, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := reserveWriteLock(ctx, tx, "ideation_sessions", operation.SessionID); err != nil {
		return PublicationOperation{}, fmt.Errorf("reserve publication operation %q: %w", operation.ID, err)
	}
	stored, err := scanPublicationOperation(tx.QueryRowContext(ctx, `SELECT `+publicationOperationColumns+`
		FROM ideation_publication_operations WHERE session_id = ? AND approval_id = ? AND id = ?`,
		operation.SessionID, operation.ApprovalID, operation.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return PublicationOperation{}, fmt.Errorf("%w: %q", ErrPublicationOperationNotFound, operation.ID)
	}
	if err != nil {
		return PublicationOperation{}, err
	}
	if stored.Kind == PublicationOperationCreateItem && operation.State == PublicationOperationSucceeded && strings.TrimSpace(operation.ExternalID) == "" {
		return PublicationOperation{}, fmt.Errorf("publication operation %q succeeded without an external identity", operation.ID)
	}
	if settledChange(stored, operation) {
		return PublicationOperation{}, fmt.Errorf("%w: %q is %s with external identity %q", ErrPublicationOperationSettled, operation.ID, stored.State, stored.ExternalID)
	}
	stored.State, stored.ExternalID, stored.URL = operation.State, operation.ExternalID, operation.URL
	stored.Detail, stored.Attempts = operation.Detail, operation.Attempts
	stored.UpdatedAt = ideationWriteTime(IdeationWrite{At: operation.UpdatedAt})
	if _, err := tx.ExecContext(ctx, `
		UPDATE ideation_publication_operations
		SET state = ?, external_id = ?, url = ?, detail = ?, attempts = ?, updated_at = ?
		WHERE session_id = ? AND approval_id = ? AND id = ?`,
		stored.State, stored.ExternalID, stored.URL, stored.Detail, stored.Attempts, formatIdeationTime(stored.UpdatedAt),
		stored.SessionID, stored.ApprovalID, stored.ID); err != nil {
		return PublicationOperation{}, fmt.Errorf("save publication operation %q: %w", operation.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return PublicationOperation{}, fmt.Errorf("commit publication operation %q: %w", operation.ID, err)
	}
	return stored, nil
}

// settledChange reports whether update would change a succeeded operation or
// a recorded external identity.
func settledChange(stored, update PublicationOperation) bool {
	if stored.ExternalID != "" && update.ExternalID != stored.ExternalID {
		return true
	}
	return stored.State == PublicationOperationSucceeded &&
		(update.State != stored.State || update.URL != stored.URL)
}

// samePlannedLedger reports whether stored holds exactly operations in order.
func samePlannedLedger(stored, operations []PublicationOperation) bool {
	if len(stored) != len(operations) {
		return false
	}
	for index, operation := range operations {
		planned := stored[index]
		if planned.ID != operation.ID || planned.Kind != operation.Kind || planned.NodeID != operation.NodeID ||
			planned.Correlation != operation.Correlation || planned.RelationshipKind != operation.RelationshipKind ||
			planned.FromNodeID != operation.FromNodeID || planned.ToNodeID != operation.ToNodeID {
			return false
		}
	}
	return true
}

// validatePublicationOperations checks the planned identity of a ledger.
func validatePublicationOperations(operations []PublicationOperation) error {
	if len(operations) == 0 {
		return errors.New("publication ledger must contain at least one operation")
	}
	seen := make(map[string]bool, len(operations))
	for _, operation := range operations {
		if err := validatePublishedReference("operation id", operation.ID, true); err != nil || len(operation.ID) > maxPublicationOperationIDBytes {
			return fmt.Errorf("publication operation id %q must be one line of 1 to %d bytes", operation.ID, maxPublicationOperationIDBytes)
		}
		if seen[operation.ID] {
			return fmt.Errorf("publication operation id %q is declared more than once", operation.ID)
		}
		seen[operation.ID] = true
		switch operation.Kind {
		case PublicationOperationCreateItem:
			if err := validateIdeationIdentifier("publication node id", operation.NodeID); err != nil {
				return err
			}
			if err := validatePublishedReference("correlation", operation.Correlation, true); err != nil {
				return err
			}
		case PublicationOperationCreateRelationship:
			for _, value := range []string{operation.RelationshipKind, operation.FromNodeID, operation.ToNodeID} {
				if err := validateIdeationIdentifier("publication relationship field", value); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("publication operation kind %q is not declared", operation.Kind)
		}
	}
	return nil
}

// validatePublicationResult checks the mutable fields of one ledger update.
func validatePublicationResult(operation PublicationOperation) error {
	switch operation.State {
	case PublicationOperationPending, PublicationOperationAttempted, PublicationOperationFailed,
		PublicationOperationAmbiguous, PublicationOperationSucceeded:
	default:
		return fmt.Errorf("publication operation state %q is not declared", operation.State)
	}
	if err := validatePublishedReference("external id", operation.ExternalID, false); err != nil {
		return err
	}
	if err := validatePublishedReference("url", operation.URL, false); err != nil {
		return err
	}
	if operation.Attempts < 0 {
		return errors.New("publication operation attempts must not be negative")
	}
	return validateIdeationText("publication operation detail", operation.Detail, MaxIdeationReasonBytes, false)
}

// publicationOperationColumns is the column order that
// scanPublicationOperation decodes.
const publicationOperationColumns = `session_id, approval_id, id, sequence, kind, node_id, correlation,
	relationship_kind, from_node_id, to_node_id, state, external_id, url, detail, attempts, updated_at`

// prefixedPublicationOperationColumns is publicationOperationColumns for the
// alias "o".
const prefixedPublicationOperationColumns = `o.session_id, o.approval_id, o.id, o.sequence, o.kind, o.node_id,
	o.correlation, o.relationship_kind, o.from_node_id, o.to_node_id, o.state, o.external_id, o.url, o.detail,
	o.attempts, o.updated_at`

// publicationLedger reads the ledger of one approval inside tx.
func publicationLedger(ctx context.Context, tx *sql.Tx, sessionID, approvalID string) ([]PublicationOperation, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+publicationOperationColumns+`
		FROM ideation_publication_operations WHERE session_id = ? AND approval_id = ? ORDER BY sequence`, sessionID, approvalID)
	if err != nil {
		return nil, fmt.Errorf("read publication ledger: %w", err)
	}
	defer func() { _ = rows.Close() }()
	operations := []PublicationOperation{}
	for rows.Next() {
		operation, err := scanPublicationOperation(rows)
		if err != nil {
			return nil, err
		}
		operations = append(operations, operation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read publication ledger: %w", err)
	}
	return operations, nil
}

// scanPublicationOperation decodes one ledger row. sql.ErrNoRows passes
// through unwrapped so callers can detect a missing operation.
func scanPublicationOperation(row interface{ Scan(...any) error }) (PublicationOperation, error) {
	var operation PublicationOperation
	var updatedAt string
	if err := row.Scan(&operation.SessionID, &operation.ApprovalID, &operation.ID, &operation.Sequence, &operation.Kind,
		&operation.NodeID, &operation.Correlation, &operation.RelationshipKind, &operation.FromNodeID, &operation.ToNodeID,
		&operation.State, &operation.ExternalID, &operation.URL, &operation.Detail, &operation.Attempts, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PublicationOperation{}, err
		}
		return PublicationOperation{}, fmt.Errorf("read publication operation: %w", err)
	}
	var err error
	operation.UpdatedAt, err = parseIdeationTime(updatedAt)
	return operation, err
}

// publicationLedgerSchema creates the additive ledger table of schema version
// 43. One row per operation keeps the one-row-per-run pending_effects journal
// unchanged (ADR 0022).
const publicationLedgerSchema = `CREATE TABLE IF NOT EXISTS ideation_publication_operations (
	session_id TEXT NOT NULL,
	approval_id TEXT NOT NULL,
	id TEXT NOT NULL,
	sequence INTEGER NOT NULL,
	kind TEXT NOT NULL,
	node_id TEXT NOT NULL DEFAULT '',
	correlation TEXT NOT NULL DEFAULT '',
	relationship_kind TEXT NOT NULL DEFAULT '',
	from_node_id TEXT NOT NULL DEFAULT '',
	to_node_id TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL,
	external_id TEXT NOT NULL DEFAULT '',
	url TEXT NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT '',
	attempts INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (session_id, approval_id, id),
	UNIQUE (session_id, approval_id, sequence)
)`
