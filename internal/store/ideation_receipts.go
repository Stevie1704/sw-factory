package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

const (
	// MaxPublicationReceiptBytes bounds the serialized items and relationships
	// of one receipt.
	MaxPublicationReceiptBytes = 256 * 1024
	// maxPublishedReferenceBytes bounds one external identity or URL.
	maxPublishedReferenceBytes = 2048
)

// ErrApprovalNotCurrent reports a receipt for an approval that is not the
// session's current approval.
var ErrApprovalNotCurrent = errors.New("approval is not the current approval")

// PublicationOutcome classifies one publication attempt.
type PublicationOutcome string

const (
	// PublicationOutcomePublished means every approved item and relationship
	// exists in the tracker.
	PublicationOutcomePublished PublicationOutcome = "published"
	// PublicationOutcomeIncomplete means the attempt stopped before every
	// operation succeeded. The receipt lists what is known to exist.
	PublicationOutcomeIncomplete PublicationOutcome = "incomplete"
)

// PublishedItem maps one plan node to the tracker item created for it.
type PublishedItem struct {
	// NodeID is the local plan-node identity.
	NodeID string
	// ExternalID is the tracker identity, for example an issue number.
	ExternalID string
	// URL optionally locates the tracker item.
	URL string
}

// PublishedRelationship records one tracker relationship known to exist.
type PublishedRelationship struct {
	// FromNodeID is the local node at the start of the relationship.
	FromNodeID string
	// ToNodeID is the local node at the end of the relationship.
	ToNodeID string
	// Kind is the plan relationship kind, for example parent or depends_on.
	Kind string
}

// PublicationReceipt is append-only evidence of one publication attempt for
// an approval. The per-operation ledger of #272 is the recovery authority;
// the receipt is the inspectable summary.
type PublicationReceipt struct {
	// SessionID identifies the owning session. The store fills it.
	SessionID string
	// ID is the receipt identity and idempotency key.
	ID string
	// ApprovalID identifies the approval the attempt published.
	ApprovalID string
	// PlanRevision is the approved plan revision. The store copies it from the
	// approval.
	PlanRevision int64
	// Outcome classifies the attempt.
	Outcome PublicationOutcome
	// Items lists tracker items known to exist after the attempt.
	Items []PublishedItem
	// Relationships lists tracker relationships known to exist after the
	// attempt.
	Relationships []PublishedRelationship
	// Detail optionally explains an incomplete attempt and the next action.
	Detail string
	// RecordedAt is when the receipt was accepted.
	RecordedAt time.Time
}

// RecordPublicationReceipt appends receipt for the session's current
// approval. A stored receipt with the same ID and content is a replay.
func (s *Store) RecordPublicationReceipt(ctx context.Context, write IdeationWrite, receipt PublicationReceipt) (IdeationSession, PublicationReceipt, error) {
	items, relationships, err := encodePublicationReceipt(receipt)
	if err != nil {
		return IdeationSession{}, PublicationReceipt{}, err
	}
	tx, session, err := s.beginIdeationWrite(ctx, write)
	if err != nil {
		return IdeationSession{}, PublicationReceipt{}, err
	}
	defer func() { _ = tx.Rollback() }()
	stored, err := scanPublicationReceipt(tx.QueryRowContext(ctx, `SELECT `+publicationReceiptColumns+` FROM ideation_publication_receipts r JOIN ideation_approvals a ON a.session_id = r.session_id AND a.id = r.approval_id WHERE r.session_id = ? AND r.id = ?`, session.ID, receipt.ID))
	switch {
	case err == nil:
		if !sameReceiptContent(stored, receipt) {
			return IdeationSession{}, PublicationReceipt{}, fmt.Errorf("%w: receipt %q", ErrIdeationRequestReused, receipt.ID)
		}
		return session, stored, nil
	case !errors.Is(err, sql.ErrNoRows):
		return IdeationSession{}, PublicationReceipt{}, err
	}
	if err := checkIdeationRevision(session, write); err != nil {
		return IdeationSession{}, PublicationReceipt{}, err
	}
	if session.ApprovalID == "" || receipt.ApprovalID != session.ApprovalID {
		return IdeationSession{}, PublicationReceipt{}, fmt.Errorf("%w: receipt names %q, current is %q", ErrApprovalNotCurrent, receipt.ApprovalID, session.ApprovalID)
	}
	if err := tx.QueryRowContext(ctx, `SELECT plan_revision FROM ideation_approvals WHERE session_id = ? AND id = ?`, session.ID, receipt.ApprovalID).Scan(&receipt.PlanRevision); err != nil {
		return IdeationSession{}, PublicationReceipt{}, fmt.Errorf("read published approval %q: %w", receipt.ApprovalID, err)
	}
	receipt.SessionID = session.ID
	receipt.RecordedAt = ideationWriteTime(write)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ideation_publication_receipts (session_id, id, approval_id, outcome, items, relationships, detail, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		receipt.SessionID, receipt.ID, receipt.ApprovalID, receipt.Outcome, items, relationships, receipt.Detail,
		formatIdeationTime(receipt.RecordedAt)); err != nil {
		return IdeationSession{}, PublicationReceipt{}, fmt.Errorf("save publication receipt %q: %w", receipt.ID, err)
	}
	updated, err := commitIdeationWrite(ctx, tx, session, write)
	if err != nil {
		return IdeationSession{}, PublicationReceipt{}, err
	}
	return updated, receipt, nil
}

// publicationReceiptColumns is the column order that scanPublicationReceipt
// decodes, for receipt alias "r" joined with approval alias "a".
const publicationReceiptColumns = `r.session_id, r.id, r.approval_id, a.plan_revision, r.outcome, r.items,
	r.relationships, r.detail, r.recorded_at`

// scanPublicationReceipt decodes one receipt row. sql.ErrNoRows passes through
// unwrapped so callers can detect a missing receipt.
func scanPublicationReceipt(row interface{ Scan(...any) error }) (PublicationReceipt, error) {
	var receipt PublicationReceipt
	var items, relationships, recordedAt string
	if err := row.Scan(&receipt.SessionID, &receipt.ID, &receipt.ApprovalID, &receipt.PlanRevision, &receipt.Outcome,
		&items, &relationships, &receipt.Detail, &recordedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PublicationReceipt{}, err
		}
		return PublicationReceipt{}, fmt.Errorf("read publication receipt: %w", err)
	}
	if err := json.Unmarshal([]byte(items), &receipt.Items); err != nil {
		return PublicationReceipt{}, fmt.Errorf("decode publication receipt %q items: %w", receipt.ID, err)
	}
	if err := json.Unmarshal([]byte(relationships), &receipt.Relationships); err != nil {
		return PublicationReceipt{}, fmt.Errorf("decode publication receipt %q relationships: %w", receipt.ID, err)
	}
	var err error
	receipt.RecordedAt, err = parseIdeationTime(recordedAt)
	return receipt, err
}

// sameReceiptContent reports whether a retried receipt carries the stored
// content.
func sameReceiptContent(stored, retried PublicationReceipt) bool {
	normalize := func(receipt PublicationReceipt) PublicationReceipt {
		if receipt.Items == nil {
			receipt.Items = []PublishedItem{}
		}
		if receipt.Relationships == nil {
			receipt.Relationships = []PublishedRelationship{}
		}
		return PublicationReceipt{ID: receipt.ID, ApprovalID: receipt.ApprovalID, Outcome: receipt.Outcome,
			Items: receipt.Items, Relationships: receipt.Relationships, Detail: receipt.Detail}
	}
	return reflect.DeepEqual(normalize(stored), normalize(retried))
}

// encodePublicationReceipt validates receipt and returns its stored item and
// relationship JSON.
func encodePublicationReceipt(receipt PublicationReceipt) (string, string, error) {
	if err := validateIdeationIdentifier("receipt id", receipt.ID); err != nil {
		return "", "", err
	}
	if err := validateIdeationIdentifier("receipt approval id", receipt.ApprovalID); err != nil {
		return "", "", err
	}
	if receipt.Outcome != PublicationOutcomePublished && receipt.Outcome != PublicationOutcomeIncomplete {
		return "", "", fmt.Errorf("publication outcome %q is not declared", receipt.Outcome)
	}
	if err := validateIdeationText("receipt detail", receipt.Detail, maxIdeationReasonBytes, false); err != nil {
		return "", "", err
	}
	for _, item := range receipt.Items {
		if err := validateIdeationIdentifier("published node id", item.NodeID); err != nil {
			return "", "", err
		}
		if err := validatePublishedReference("external id", item.ExternalID, true); err != nil {
			return "", "", err
		}
		if err := validatePublishedReference("url", item.URL, false); err != nil {
			return "", "", err
		}
	}
	for _, relationship := range receipt.Relationships {
		for _, value := range []string{relationship.FromNodeID, relationship.ToNodeID, relationship.Kind} {
			if err := validateIdeationIdentifier("published relationship field", value); err != nil {
				return "", "", err
			}
		}
	}
	items, err := sliceJSON(receipt.Items)
	if err != nil {
		return "", "", fmt.Errorf("encode publication receipt items: %w", err)
	}
	relationships, err := sliceJSON(receipt.Relationships)
	if err != nil {
		return "", "", fmt.Errorf("encode publication receipt relationships: %w", err)
	}
	if len(items)+len(relationships) > MaxPublicationReceiptBytes {
		return "", "", fmt.Errorf("publication receipt is %d bytes; the limit is %d", len(items)+len(relationships), MaxPublicationReceiptBytes)
	}
	return items, relationships, nil
}

// validatePublishedReference checks one bounded single-line tracker reference.
func validatePublishedReference(label, value string, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("published item %s is required", label)
	}
	if len(value) > maxPublishedReferenceBytes || strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("published item %s must be one line of at most %d bytes", label, maxPublishedReferenceBytes)
	}
	return nil
}
