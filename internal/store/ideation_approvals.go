package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

const (
	// MaxApprovalPayloadBytes bounds the rendered publication payload one
	// approval binds.
	MaxApprovalPayloadBytes = 1024 * 1024
	// MaxRelationshipMappingBytes bounds the relationship mapping one approval
	// binds.
	MaxRelationshipMappingBytes = 64 * 1024
	// maxApprovalTargetBytes bounds the publication target identity.
	maxApprovalTargetBytes = 512
)

// ErrPlanNotApprovable reports an approval for a plan revision that is not
// the session's current, fresh plan revision.
var ErrPlanNotApprovable = errors.New("plan revision is not the current fresh draft")

// PlanApproval binds one immutable plan revision to the exact publication the
// human reviewed: the target, the fully rendered payload, and the relationship
// mapping. Publication (#272) renders the payload and mapping; the store keeps
// the bytes and their digests and never re-renders them.
type PlanApproval struct {
	// SessionID identifies the owning session. The store fills it.
	SessionID string
	// ID is the approval identity and idempotency key, for example the browser
	// request ID of the approve action.
	ID string
	// PlanRevision is the approved plan revision.
	PlanRevision int64
	// Target identifies the tracker destination, for example a repository or
	// project.
	Target string
	// Payload is the fully rendered publication payload.
	Payload []byte
	// PayloadSHA256 is the lowercase hex digest of Payload. The store sets it.
	PayloadSHA256 string
	// RelationshipMapping is the rendered mapping from plan relationships to
	// tracker relationships.
	RelationshipMapping []byte
	// RelationshipMappingSHA256 is the lowercase hex digest of
	// RelationshipMapping. The store sets it.
	RelationshipMappingSHA256 string
	// ApprovedAt is when the approval was accepted.
	ApprovedAt time.Time
}

// Binds reports whether the approval covers exactly this plan revision,
// target, rendered payload, and relationship mapping. Publication calls it
// with its freshly rendered values; any difference requires a new approval.
func (approval PlanApproval) Binds(planRevision int64, target string, payload, relationshipMapping []byte) bool {
	return approval.PlanRevision == planRevision && approval.Target == target &&
		approval.PayloadSHA256 == sha256Hex(payload) && approval.RelationshipMappingSHA256 == sha256Hex(relationshipMapping)
}

// ApprovePlan records approval for the session's current plan revision. The
// plan must derive from the current brief revision. A stored approval with the
// same ID and binding is a replay, so a double submission records one
// approval.
func (s *Store) ApprovePlan(ctx context.Context, write IdeationWrite, approval PlanApproval) (IdeationSession, PlanApproval, error) {
	if err := validatePlanApproval(approval); err != nil {
		return IdeationSession{}, PlanApproval{}, err
	}
	if approval.RelationshipMapping == nil {
		approval.RelationshipMapping = []byte{}
	}
	approval.PayloadSHA256 = sha256Hex(approval.Payload)
	approval.RelationshipMappingSHA256 = sha256Hex(approval.RelationshipMapping)
	tx, session, err := s.beginIdeationWrite(ctx, write)
	if err != nil {
		return IdeationSession{}, PlanApproval{}, err
	}
	defer func() { _ = tx.Rollback() }()
	stored, err := scanPlanApproval(tx.QueryRowContext(ctx, `SELECT `+planApprovalColumns+` FROM ideation_approvals WHERE session_id = ? AND id = ?`, session.ID, approval.ID))
	switch {
	case err == nil:
		if !stored.Binds(approval.PlanRevision, approval.Target, approval.Payload, approval.RelationshipMapping) {
			return IdeationSession{}, PlanApproval{}, fmt.Errorf("%w: approval %q", ErrIdeationRequestReused, approval.ID)
		}
		return session, stored, nil
	case !errors.Is(err, sql.ErrNoRows):
		return IdeationSession{}, PlanApproval{}, err
	}
	if err := checkIdeationRevision(session, write); err != nil {
		return IdeationSession{}, PlanApproval{}, err
	}
	var sourceBrief int64
	err = tx.QueryRowContext(ctx, `SELECT source_brief_revision FROM ideation_plan_revisions WHERE session_id = ? AND revision = ?`, session.ID, approval.PlanRevision).Scan(&sourceBrief)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (approval.PlanRevision != session.PlanRevision || sourceBrief != session.BriefRevision)) {
		return IdeationSession{}, PlanApproval{}, fmt.Errorf("%w: plan revision %d (current plan %d, current brief %d)", ErrPlanNotApprovable, approval.PlanRevision, session.PlanRevision, session.BriefRevision)
	}
	if err != nil {
		return IdeationSession{}, PlanApproval{}, fmt.Errorf("read approved plan revision %d: %w", approval.PlanRevision, err)
	}
	approval.SessionID = session.ID
	approval.ApprovedAt = ideationWriteTime(write)
	if _, err := tx.ExecContext(ctx, `INSERT INTO ideation_approvals (`+planApprovalColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		approval.SessionID, approval.ID, approval.PlanRevision, approval.Target, approval.Payload, approval.PayloadSHA256,
		approval.RelationshipMapping, approval.RelationshipMappingSHA256, formatIdeationTime(approval.ApprovedAt)); err != nil {
		return IdeationSession{}, PlanApproval{}, fmt.Errorf("save approval %q: %w", approval.ID, err)
	}
	session.ApprovalID = approval.ID
	updated, err := commitIdeationWrite(ctx, tx, session, write)
	if err != nil {
		return IdeationSession{}, PlanApproval{}, err
	}
	return updated, approval, nil
}

// CurrentPlanApproval returns the approval that binds the session's current
// plan revision, or nil when a later brief or plan change cleared it.
func (s *Store) CurrentPlanApproval(ctx context.Context, sessionID string) (*PlanApproval, error) {
	approval, err := scanPlanApproval(s.db.QueryRowContext(ctx, `
		SELECT `+prefixedPlanApprovalColumns+`
		FROM ideation_approvals a JOIN ideation_sessions s ON s.id = a.session_id AND s.approval_id = a.id
		WHERE a.session_id = ?`, sessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &approval, nil
}

// planApprovalColumns is the column order that scanPlanApproval decodes.
const planApprovalColumns = `session_id, id, plan_revision, target, payload, payload_sha256,
	relationship_mapping, relationship_mapping_sha256, approved_at`

// prefixedPlanApprovalColumns is planApprovalColumns for the alias "a".
const prefixedPlanApprovalColumns = `a.session_id, a.id, a.plan_revision, a.target, a.payload, a.payload_sha256,
	a.relationship_mapping, a.relationship_mapping_sha256, a.approved_at`

// scanPlanApproval decodes one approval row. sql.ErrNoRows passes through
// unwrapped so callers can detect a missing approval.
func scanPlanApproval(row interface{ Scan(...any) error }) (PlanApproval, error) {
	var approval PlanApproval
	var approvedAt string
	if err := row.Scan(&approval.SessionID, &approval.ID, &approval.PlanRevision, &approval.Target, &approval.Payload,
		&approval.PayloadSHA256, &approval.RelationshipMapping, &approval.RelationshipMappingSHA256, &approvedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PlanApproval{}, err
		}
		return PlanApproval{}, fmt.Errorf("read approval: %w", err)
	}
	var err error
	approval.ApprovedAt, err = parseIdeationTime(approvedAt)
	return approval, err
}

// validatePlanApproval checks the caller-supplied binding of one approval.
func validatePlanApproval(approval PlanApproval) error {
	if err := validateIdeationIdentifier("approval id", approval.ID); err != nil {
		return err
	}
	if approval.PlanRevision < 1 {
		return errors.New("approval plan revision must be positive")
	}
	if err := validateIdeationText("approval target", approval.Target, maxApprovalTargetBytes, true); err != nil {
		return err
	}
	if len(bytes.TrimSpace(approval.Payload)) == 0 || len(approval.Payload) > MaxApprovalPayloadBytes {
		return fmt.Errorf("approval payload must contain 1 to %d bytes", MaxApprovalPayloadBytes)
	}
	if len(approval.RelationshipMapping) > MaxRelationshipMappingBytes {
		return fmt.Errorf("approval relationship mapping must not exceed %d bytes", MaxRelationshipMappingBytes)
	}
	return nil
}

// sha256Hex returns the lowercase hex SHA-256 digest of value.
func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
