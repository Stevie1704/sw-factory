package tracker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// PlanPublisher is the optional, provider-neutral tracker capability that
// publishes an approved plan graph (ADR 0022). An adapter implements it with
// its own provider commands and credentials; the coordinator, agents, and the
// browser never issue them. The publication service drives the methods in
// this order: PreflightPlan before approval and again before the first write,
// FindPlanItems before every CreatePlanItem, and FindPlanRelationship before
// every CreatePlanRelationship.
//
// Contract for every adapter:
//
//   - PreflightPlan is read-only. It resolves the target, the work-item types,
//     the required fields, and the relationship capabilities, and it renders
//     the complete content of every item. The same graph against an unchanged
//     provider configuration renders byte-identical previews, so the approval
//     digests stay valid. A capability that is missing must fail with a
//     PlanError before approval; it must never be replaced silently by a
//     different representation.
//   - CreatePlanItem and CreatePlanRelationship publish exactly the rendered
//     content. They never add a label or tag from FactoryStateLabels, never
//     start a run, and never edit a tracker item that the plan did not create.
//   - A failure that proves that the provider made no change returns a
//     PlanError of kind PlanErrorPermission, PlanErrorUnsupportedCapability,
//     or PlanErrorInvalidMapping. Any other error, and a PlanError of kind
//     PlanErrorAmbiguous, means that the outcome is unknown: the provider may
//     have applied the write, for example after a timeout. The publication
//     service then reconciles and never repeats the write blindly.
//   - FindPlanItems returns every item whose correlation metadata matches,
//     in any state. It must use a provider read that sees a just-created item
//     where the provider offers one (for example a listing, not an
//     eventually-consistent search index).
//   - FindPlanRelationship reports whether the relationship already exists, so
//     that a retry does not create a duplicate link.
type PlanPublisher interface {
	PreflightPlan(context.Context, Repository, PlanGraph) (PlanPreview, error)
	CreatePlanItem(context.Context, Repository, RenderedPlanItem) (PlanItemRef, error)
	FindPlanItems(context.Context, Repository, string) ([]PlanItemRef, error)
	CreatePlanRelationship(context.Context, Repository, PlanRelationship) error
	FindPlanRelationship(context.Context, Repository, PlanRelationship) (bool, error)
}

// BoundedPlanLookup is an optional PlanPublisher capability. An adapter whose
// FindPlanItems reads only part of the tracker implements it, so that
// publication tells the operator which items an empty lookup covered before
// the operator confirms that no item exists.
type BoundedPlanLookup interface {
	// PlanLookupBound describes the items that FindPlanItems reads, for
	// example "the 1,000 newest issues that the authenticated account
	// created".
	PlanLookupBound() string
}

// PlanGraph is the provider-neutral input of PreflightPlan: one approved or
// reviewable plan revision. It carries plan content and traceability
// identities only, never conversation turns.
type PlanGraph struct {
	// SessionID identifies the ideation session that owns the plan.
	SessionID string
	// PlanRevision is the plan revision the graph renders.
	PlanRevision int64
	// ContextRevision is the repository commit the plan was made against.
	ContextRevision string
	// Items lists the plan nodes in plan order.
	Items []PlanGraphItem
}

// PlanGraphItem is one provider-neutral plan node.
type PlanGraphItem struct {
	// NodeID is the stable local node identity.
	NodeID string
	// Kind is the provider-neutral work-item kind, for example spec or task.
	// The adapter maps it to a provider work-item type.
	Kind string
	// Title is the work-item title.
	Title string
	// Body is the work-item description written in the plan.
	Body string
	// AcceptanceCriteria lists observable completion checks.
	AcceptanceCriteria []string
	// ParentID optionally names the parent node.
	ParentID string
	// DependsOn names nodes that must be done first.
	DependsOn []string
	// BriefRefs names the brief items the node traces to.
	BriefRefs []string
	// Correlation is the stable correlation key of the node; see
	// PlanCorrelation. The adapter must render it into the published item so
	// that FindPlanItems can find the item again.
	Correlation string
}

// PlanCorrelation returns the stable correlation key of one plan node:
// "<session>/<plan revision>/<node>". The key does not depend on an external
// identity, so it is known before the item exists. Adapters render it as
// provider metadata, for example a hidden body marker or a namespaced tag,
// and FindPlanItems looks items up by it.
func PlanCorrelation(sessionID string, planRevision int64, nodeID string) string {
	return sessionID + "/" + strconv.FormatInt(planRevision, 10) + "/" + nodeID
}

// PlanPreview is the deterministic result of PreflightPlan. It is exactly what
// the human reviews and what the approval binds: Target binds the approval
// target, Items render the payload, and Relationships render the relationship
// mapping. Publication creates the stored approved preview; it never creates
// a fresh rendering.
type PlanPreview struct {
	// Target identifies the provider destination, for example
	// "github:owner/name" or "azure-devops:org/project".
	Target string
	// Items lists every item to create, in plan order. An adapter may add
	// required intermediate items; they have their own NodeID and
	// Correlation and appear in the preview like any other item.
	Items []RenderedPlanItem
	// Relationships lists every relationship to create.
	Relationships []PlanRelationshipMapping
}

// RenderedPlanItem is the complete content of one tracker item as the adapter
// will create it.
type RenderedPlanItem struct {
	// NodeID is the local node identity, or the identity the adapter assigned
	// to a required intermediate item.
	NodeID string
	// Type is the resolved provider work-item type, for example "issue",
	// "Epic", or "User Story".
	Type string
	// Title is the final item title.
	Title string
	// Body is the final, fully rendered item description, including the
	// traceability metadata and any approved textual relationship fallback.
	Body string
	// Fields holds additional provider fields with their final values, for
	// example required Azure DevOps fields. It is empty for GitHub.
	Fields map[string]string
	// Labels lists the labels or tags the item receives. It must not contain a
	// label from FactoryStateLabels.
	Labels []string
	// Correlation is the correlation key rendered into the item.
	Correlation string
}

// PlanRelationshipKind is the provider-neutral kind of a plan relationship.
type PlanRelationshipKind string

const (
	// PlanRelationshipParent links a child item (From) to its parent item
	// (To).
	PlanRelationshipParent PlanRelationshipKind = "parent"
	// PlanRelationshipDependsOn links a dependent item (From) to the item it
	// depends on (To): To must be done before From.
	PlanRelationshipDependsOn PlanRelationshipKind = "depends_on"
)

// PlanRelationshipMode is how the adapter represents a relationship. The mode
// is part of the approved relationship mapping and must not change after
// approval.
type PlanRelationshipMode string

const (
	// PlanRelationshipNative uses the provider's native relationship, for
	// example a GitHub sub-issue or an Azure DevOps hierarchy link.
	PlanRelationshipNative PlanRelationshipMode = "native"
	// PlanRelationshipTextual uses an explicit textual fallback, for example a
	// task list or a backlink, that the preview shows.
	PlanRelationshipTextual PlanRelationshipMode = "textual"
)

// PlanRelationshipMapping is one relationship of the preview between two
// local nodes.
type PlanRelationshipMapping struct {
	// Kind is the relationship kind.
	Kind PlanRelationshipKind
	// FromNodeID is the local node at the start of the relationship.
	FromNodeID string
	// ToNodeID is the local node at the end of the relationship.
	ToNodeID string
	// Mode is the approved representation.
	Mode PlanRelationshipMode
}

// PlanItemRef identifies one tracker item.
type PlanItemRef struct {
	// ExternalID is the provider identity, for example an issue number or a
	// work-item ID.
	ExternalID string
	// URL locates the item for a human.
	URL string
}

// PlanRelationship is one relationship between two created tracker items.
type PlanRelationship struct {
	// Kind is the relationship kind.
	Kind PlanRelationshipKind
	// Mode is the approved representation.
	Mode PlanRelationshipMode
	// From is the item at the start of the relationship.
	From PlanItemRef
	// To is the item at the end of the relationship.
	To PlanItemRef
}

// PlanErrorKind classifies a planning failure that the caller must handle
// differently from a generic error.
type PlanErrorKind string

const (
	// PlanErrorPermission means the authenticated account may not perform the
	// operation. The provider made no change.
	PlanErrorPermission PlanErrorKind = "permission"
	// PlanErrorUnsupportedCapability means the target lacks a required
	// capability, for example native sub-issues or a hierarchy level. The
	// provider made no change.
	PlanErrorUnsupportedCapability PlanErrorKind = "unsupported_capability"
	// PlanErrorInvalidMapping means the configured mapping does not fit the
	// target, for example an unknown work-item type or a missing required
	// field. The provider made no change.
	PlanErrorInvalidMapping PlanErrorKind = "invalid_mapping"
	// PlanErrorAmbiguous means the outcome of a write is unknown, for example
	// after a timeout. The provider may have applied it.
	PlanErrorAmbiguous PlanErrorKind = "ambiguous"
	// PlanErrorMultipleCandidates means a correlation lookup found more than
	// one item, so the caller cannot pick one without explicit reconciliation.
	PlanErrorMultipleCandidates PlanErrorKind = "multiple_candidates"
)

// PlanError is a planning failure of a known kind.
type PlanError struct {
	// Kind classifies the failure.
	Kind PlanErrorKind
	// Err is the underlying failure.
	Err error
}

// Error describes the failure.
func (e *PlanError) Error() string {
	if e.Err == nil {
		return string(e.Kind)
	}
	return fmt.Sprintf("%s: %v", e.Kind, e.Err)
}

// Unwrap returns the underlying failure.
func (e *PlanError) Unwrap() error { return e.Err }

// NewPlanError returns a PlanError of kind with a formatted cause.
func NewPlanError(kind PlanErrorKind, format string, args ...any) error {
	return &PlanError{Kind: kind, Err: fmt.Errorf(format, args...)}
}

// PlanErrorKindOf returns the kind of the first PlanError in err's chain.
func PlanErrorKindOf(err error) (PlanErrorKind, bool) {
	var planErr *PlanError
	if errors.As(err, &planErr) {
		return planErr.Kind, true
	}
	return "", false
}

// WriteNotApplied reports whether err proves that a create made no change:
// a PlanError of kind permission, unsupported capability, or invalid mapping.
// Every other error leaves the outcome unknown.
func WriteNotApplied(err error) bool {
	kind, ok := PlanErrorKindOf(err)
	return ok && (kind == PlanErrorPermission || kind == PlanErrorUnsupportedCapability || kind == PlanErrorInvalidMapping)
}

// IsFactoryStateLabel reports whether label is one of FactoryStateLabels,
// ignoring case. Published plan items must never carry such a label.
func IsFactoryStateLabel(label string) bool {
	for _, owned := range FactoryStateLabels {
		if strings.EqualFold(strings.TrimSpace(label), owned) {
			return true
		}
	}
	return false
}
