// Package publication publishes an approved ideation plan graph to the work
// tracker through the tracker.PlanPublisher port (ADR 0022). It is a
// session-owned durable effect: it needs no Run and never starts one. Each
// tracker write is reserved in the per-operation publication ledger before it
// is sent, and its result is recorded before a dependent write starts, so a
// restart resumes the same approved plan and reconciles before it repeats a
// create.
package publication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// payloadSchemaVersion versions the rendered payload and relationship mapping
// bytes that an approval binds.
const payloadSchemaVersion = 1

// ErrNotApproved reports a publication request without a current approval.
var ErrNotApproved = errors.New("plan has no current approval")

// ErrStaleApproval reports that the approval no longer binds what the tracker
// would publish now, for example after a provider mapping or capability
// changed. Publication made no tracker write; a fresh review and approval are
// needed.
var ErrStaleApproval = errors.New("approval does not bind the current rendered publication")

// ErrNotPublishable reports a session whose lifecycle state does not allow
// publication, for example a published or cancelled session.
var ErrNotPublishable = errors.New("session state does not allow publication")

// ErrInvalidPayload reports a rendered publication that the factory refuses
// to approve or publish, for example one with a factory state label.
var ErrInvalidPayload = errors.New("rendered publication is invalid")

// ErrPublicationCancelled reports that the session was cancelled during
// publication. Created tracker items stay; the ledger keeps their identities.
var ErrPublicationCancelled = errors.New("publication stopped because the session was cancelled")

// ErrReconciliationRefused reports a reconciliation that does not fit the
// ledger or the tracker state.
var ErrReconciliationRefused = errors.New("reconciliation refused")

// Store is the operational-store seam of publication.
type Store interface {
	IdeationSession(context.Context, string) (store.IdeationSession, error)
	IdeationHistory(context.Context, string) (store.IdeationHistory, error)
	TransitionIdeationSession(context.Context, store.IdeationWrite) (store.IdeationSession, error)
	RecordPublicationReceipt(context.Context, store.IdeationWrite, store.PublicationReceipt) (store.IdeationSession, store.PublicationReceipt, error)
	StartPublicationLedger(context.Context, string, string, []store.PublicationOperation, time.Time) ([]store.PublicationOperation, error)
	RecordPublicationOperation(context.Context, store.PublicationOperation) (store.PublicationOperation, error)
}

var _ Store = (*store.Store)(nil)

// Publisher previews, publishes, and reconciles the plan of one ideation
// session. Only the coordinator runs it.
type Publisher struct {
	// Store is the operational store.
	Store Store
	// Tracker is the adapter's planning capability.
	Tracker tracker.PlanPublisher
	// Repository is the registered repository the tracker serves.
	Repository tracker.Repository
	// Lifecycle resolves every session state change.
	Lifecycle workflow.SessionLifecycle
	// Now returns the coordinator time. Nil selects time.Now.
	Now func() time.Time
}

// Binding is the rendered publication of one plan revision: the preview the
// human reviews and the exact bytes an approval binds.
type Binding struct {
	// PlanRevision is the rendered plan revision.
	PlanRevision int64
	// Target is the provider target of the preview.
	Target string
	// Payload is the canonical rendering of the items.
	Payload []byte
	// RelationshipMapping is the canonical rendering of the relationships.
	RelationshipMapping []byte
	// Preview is the structured preview for display.
	Preview tracker.PlanPreview
}

// Approval returns the approval record that binds this rendering under id.
// The caller records it with store.ApprovePlan.
func (b Binding) Approval(id string) store.PlanApproval {
	return store.PlanApproval{
		ID: id, PlanRevision: b.PlanRevision, Target: b.Target,
		Payload: bytes.Clone(b.Payload), RelationshipMapping: bytes.Clone(b.RelationshipMapping),
	}
}

// Preview preflights and renders the session's current plan revision. It
// makes no tracker write. A provider capability or permission problem
// returns the adapter's tracker.PlanError.
func (p *Publisher) Preview(ctx context.Context, sessionID string) (Binding, error) {
	history, err := p.Store.IdeationHistory(ctx, sessionID)
	if err != nil {
		return Binding{}, err
	}
	if history.Session.PlanRevision == 0 {
		return Binding{}, fmt.Errorf("session %q has no plan to preview", sessionID)
	}
	return p.render(ctx, history, history.Session.PlanRevision)
}

// render preflights one stored plan revision and encodes the preview.
func (p *Publisher) render(ctx context.Context, history store.IdeationHistory, planRevision int64) (Binding, error) {
	plan, ok := findPlan(history.Plans, planRevision)
	if !ok {
		return Binding{}, fmt.Errorf("session %q has no plan revision %d", history.Session.ID, planRevision)
	}
	preview, err := p.Tracker.PreflightPlan(ctx, p.Repository, planGraph(history.Session.ID, plan))
	if err != nil {
		return Binding{}, fmt.Errorf("preflight plan revision %d: %w", planRevision, err)
	}
	if _, err := orderOperations(preview); err != nil {
		return Binding{}, err
	}
	payload, mapping, err := encodePreview(preview)
	if err != nil {
		return Binding{}, err
	}
	return Binding{PlanRevision: planRevision, Target: preview.Target, Payload: payload, RelationshipMapping: mapping, Preview: preview}, nil
}

// findPlan returns the plan revision with number revision.
func findPlan(plans []store.PlanRevision, revision int64) (store.PlanRevision, bool) {
	for _, plan := range plans {
		if plan.Revision == revision {
			return plan, true
		}
	}
	return store.PlanRevision{}, false
}

// planGraph converts a stored plan revision into the provider-neutral graph.
// It copies plan content only; conversation turns never enter it.
func planGraph(sessionID string, plan store.PlanRevision) tracker.PlanGraph {
	graph := tracker.PlanGraph{SessionID: sessionID, PlanRevision: plan.Revision, ContextRevision: plan.ContextRevision}
	for _, node := range plan.Nodes {
		graph.Items = append(graph.Items, tracker.PlanGraphItem{
			NodeID: node.ID, Kind: node.Kind, Title: node.Title, Body: node.Body,
			AcceptanceCriteria: node.AcceptanceCriteria, ParentID: node.ParentID, DependsOn: node.DependsOn,
			BriefRefs: node.BriefRefs, Correlation: tracker.PlanCorrelation(sessionID, plan.Revision, node.ID),
		})
	}
	return graph
}

// renderedPayload is the stored shape of the approved items. New stored
// payloads use explicit JSON tags and a schema version (ADR 0022).
type renderedPayload struct {
	SchemaVersion int                `json:"schema_version"`
	Target        string             `json:"target"`
	Items         []renderedItemJSON `json:"items"`
}

// renderedItemJSON is the stored shape of one approved item.
type renderedItemJSON struct {
	NodeID      string            `json:"node_id"`
	Type        string            `json:"type"`
	Title       string            `json:"title"`
	Body        string            `json:"body"`
	Fields      map[string]string `json:"fields"`
	Labels      []string          `json:"labels"`
	Correlation string            `json:"correlation"`
}

// renderedMapping is the stored shape of the approved relationships.
type renderedMapping struct {
	SchemaVersion int                    `json:"schema_version"`
	Relationships []renderedRelationJSON `json:"relationships"`
}

// renderedRelationJSON is the stored shape of one approved relationship.
type renderedRelationJSON struct {
	Kind       string `json:"kind"`
	FromNodeID string `json:"from_node_id"`
	ToNodeID   string `json:"to_node_id"`
	Mode       string `json:"mode"`
}

// encodePreview returns the canonical payload and relationship mapping bytes
// of preview. Map keys are sorted by encoding/json, so the bytes are
// deterministic.
func encodePreview(preview tracker.PlanPreview) ([]byte, []byte, error) {
	payload := renderedPayload{SchemaVersion: payloadSchemaVersion, Target: preview.Target, Items: []renderedItemJSON{}}
	for _, item := range preview.Items {
		fields := item.Fields
		if fields == nil {
			fields = map[string]string{}
		}
		labels := item.Labels
		if labels == nil {
			labels = []string{}
		}
		payload.Items = append(payload.Items, renderedItemJSON{
			NodeID: item.NodeID, Type: item.Type, Title: item.Title, Body: item.Body,
			Fields: fields, Labels: labels, Correlation: item.Correlation,
		})
	}
	mapping := renderedMapping{SchemaVersion: payloadSchemaVersion, Relationships: []renderedRelationJSON{}}
	for _, relationship := range preview.Relationships {
		mapping.Relationships = append(mapping.Relationships, renderedRelationJSON{
			Kind: string(relationship.Kind), FromNodeID: relationship.FromNodeID, ToNodeID: relationship.ToNodeID, Mode: string(relationship.Mode),
		})
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("encode publication payload: %w", err)
	}
	mappingBytes, err := json.Marshal(mapping)
	if err != nil {
		return nil, nil, fmt.Errorf("encode relationship mapping: %w", err)
	}
	return payloadBytes, mappingBytes, nil
}

// decodeApproval returns the approved preview from the bytes the approval
// binds. Publication creates exactly this content.
func decodeApproval(approval store.PlanApproval) (tracker.PlanPreview, error) {
	preview, err := DecodePreview(approval.Payload, approval.RelationshipMapping)
	if err != nil {
		return tracker.PlanPreview{}, fmt.Errorf("approval %q: %w", approval.ID, err)
	}
	return preview, nil
}

// DecodePreview returns the structured preview that a canonical payload and
// relationship mapping encode, as a preview command returns them and an
// approval binds them. Bytes of another schema version return
// ErrInvalidPayload.
func DecodePreview(payloadBytes, mappingBytes []byte) (tracker.PlanPreview, error) {
	var payload renderedPayload
	if err := decodeStrict(payloadBytes, &payload); err != nil || payload.SchemaVersion != payloadSchemaVersion {
		return tracker.PlanPreview{}, fmt.Errorf("%w: payload is not schema version %d: %v", ErrInvalidPayload, payloadSchemaVersion, err)
	}
	var mapping renderedMapping
	if err := decodeStrict(mappingBytes, &mapping); err != nil || mapping.SchemaVersion != payloadSchemaVersion {
		return tracker.PlanPreview{}, fmt.Errorf("%w: relationship mapping is not schema version %d: %v", ErrInvalidPayload, payloadSchemaVersion, err)
	}
	preview := tracker.PlanPreview{Target: payload.Target}
	for _, item := range payload.Items {
		preview.Items = append(preview.Items, tracker.RenderedPlanItem{
			NodeID: item.NodeID, Type: item.Type, Title: item.Title, Body: item.Body,
			Fields: item.Fields, Labels: item.Labels, Correlation: item.Correlation,
		})
	}
	for _, relationship := range mapping.Relationships {
		preview.Relationships = append(preview.Relationships, tracker.PlanRelationshipMapping{
			Kind: tracker.PlanRelationshipKind(relationship.Kind), FromNodeID: relationship.FromNodeID,
			ToNodeID: relationship.ToNodeID, Mode: tracker.PlanRelationshipMode(relationship.Mode),
		})
	}
	return preview, nil
}

// decodeStrict decodes data into destination and rejects unknown fields.
func decodeStrict(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(destination)
}

// now returns the coordinator time.
func (p *Publisher) now() time.Time {
	if p.Now == nil {
		return time.Now().UTC()
	}
	return p.Now().UTC()
}
