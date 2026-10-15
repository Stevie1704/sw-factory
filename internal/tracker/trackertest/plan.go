// Package trackertest provides in-memory fakes of the tracker ports for tests
// in other packages. The fakes keep all state in memory and support explicit
// failure injection; they never reach a provider.
package trackertest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// Operation names one PlanPublisher method for failure injection.
type Operation string

const (
	// OpPreflight selects PreflightPlan.
	OpPreflight Operation = "preflight"
	// OpCreateItem selects CreatePlanItem.
	OpCreateItem Operation = "create_item"
	// OpFindItems selects FindPlanItems.
	OpFindItems Operation = "find_items"
	// OpCreateRelationship selects CreatePlanRelationship.
	OpCreateRelationship Operation = "create_relationship"
	// OpFindRelationship selects FindPlanRelationship.
	OpFindRelationship Operation = "find_relationship"
)

// Fault is one injected failure. A fault is consumed by the first call it
// matches.
type Fault struct {
	// Op selects the method.
	Op Operation
	// Key optionally selects the call: the NodeID for OpCreateItem, the
	// correlation for OpFindItems, and RelationshipKey for the relationship
	// operations. An empty Key matches every call of Op.
	Key string
	// AfterWrite applies the write before the call returns Err, as a lost
	// response does. It applies only to the create operations.
	AfterWrite bool
	// Err is the error the call returns.
	Err error
}

// PlanItem is one item the fake holds.
type PlanItem struct {
	// Ref is the identity the fake assigned.
	Ref tracker.PlanItemRef
	// Rendered is the content the item was created with.
	Rendered tracker.RenderedPlanItem
}

// PlanTracker is an in-memory tracker.PlanPublisher. Configure its exported
// fields before the first call; they model the provider configuration that
// preflight resolves. Changing them after an approval models provider drift.
type PlanTracker struct {
	// Types maps a plan kind to a provider type. Nil maps every kind to
	// itself; a non-nil map without the kind fails preflight with
	// tracker.PlanErrorInvalidMapping.
	Types map[string]string
	// Labels lists the operator-mapped labels every item receives.
	Labels []string
	// Mode is the relationship representation. Empty selects
	// tracker.PlanRelationshipNative.
	Mode tracker.PlanRelationshipMode

	mu            sync.Mutex
	faults        []Fault
	items         []PlanItem
	relationships []tracker.PlanRelationship
	writes        int
}

var _ tracker.PlanPublisher = (*PlanTracker)(nil)

// NewPlanTracker returns an empty fake with the default configuration.
func NewPlanTracker() *PlanTracker {
	return &PlanTracker{}
}

// Inject queues fault. Faults are matched in the order they were queued.
func (f *PlanTracker) Inject(fault Fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = append(f.faults, fault)
}

// Seed adds an item without a call, for example a duplicate that another
// writer created.
func (f *PlanTracker) Seed(item tracker.RenderedPlanItem) tracker.PlanItemRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addItem(item)
}

// Items returns every item in creation order.
func (f *PlanTracker) Items() []PlanItem {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.items)
}

// Relationships returns every relationship in creation order.
func (f *PlanTracker) Relationships() []tracker.PlanRelationship {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.relationships)
}

// Writes returns the number of create calls received, including calls that
// failed before the write.
func (f *PlanTracker) Writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes
}

// RelationshipKey returns the fault key of a relationship:
// "<kind>:<from external id>-><to external id>".
func RelationshipKey(relationship tracker.PlanRelationship) string {
	return string(relationship.Kind) + ":" + relationship.From.ExternalID + "->" + relationship.To.ExternalID
}

// PreflightPlan renders graph deterministically and makes no write.
func (f *PlanTracker) PreflightPlan(_ context.Context, repository tracker.Repository, graph tracker.PlanGraph) (tracker.PlanPreview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fault, ok := f.takeFault(OpPreflight, ""); ok {
		return tracker.PlanPreview{}, fault.Err
	}
	for _, label := range f.Labels {
		if tracker.IsFactoryStateLabel(label) {
			return tracker.PlanPreview{}, tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "label %q is a factory state label", label)
		}
	}
	mode := f.Mode
	if mode == "" {
		mode = tracker.PlanRelationshipNative
	}
	preview := tracker.PlanPreview{Target: "fake:" + repository.String()}
	for _, item := range graph.Items {
		itemType := item.Kind
		if f.Types != nil {
			mapped, ok := f.Types[item.Kind]
			if !ok {
				return tracker.PlanPreview{}, tracker.NewPlanError(tracker.PlanErrorInvalidMapping, "no type is mapped for kind %q", item.Kind)
			}
			itemType = mapped
		}
		preview.Items = append(preview.Items, tracker.RenderedPlanItem{
			NodeID: item.NodeID, Type: itemType, Title: item.Title, Body: renderBody(item),
			Labels: slices.Clone(f.Labels), Correlation: item.Correlation,
		})
		if item.ParentID != "" {
			preview.Relationships = append(preview.Relationships, tracker.PlanRelationshipMapping{
				Kind: tracker.PlanRelationshipParent, FromNodeID: item.NodeID, ToNodeID: item.ParentID, Mode: mode,
			})
		}
		for _, dependency := range item.DependsOn {
			preview.Relationships = append(preview.Relationships, tracker.PlanRelationshipMapping{
				Kind: tracker.PlanRelationshipDependsOn, FromNodeID: item.NodeID, ToNodeID: dependency, Mode: mode,
			})
		}
	}
	return preview, nil
}

// CreatePlanItem creates item unless an injected fault stops it first.
func (f *PlanTracker) CreatePlanItem(_ context.Context, _ tracker.Repository, item tracker.RenderedPlanItem) (tracker.PlanItemRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	fault, ok := f.takeFault(OpCreateItem, item.NodeID)
	if ok && !fault.AfterWrite {
		return tracker.PlanItemRef{}, fault.Err
	}
	ref := f.addItem(item)
	if ok {
		return tracker.PlanItemRef{}, fault.Err
	}
	return ref, nil
}

// FindPlanItems returns every item with correlation.
func (f *PlanTracker) FindPlanItems(_ context.Context, _ tracker.Repository, correlation string) ([]tracker.PlanItemRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fault, ok := f.takeFault(OpFindItems, correlation); ok {
		return nil, fault.Err
	}
	found := []tracker.PlanItemRef{}
	for _, item := range f.items {
		if item.Rendered.Correlation == correlation {
			found = append(found, item.Ref)
		}
	}
	return found, nil
}

// CreatePlanRelationship creates relationship between two existing items
// unless an injected fault stops it first.
func (f *PlanTracker) CreatePlanRelationship(_ context.Context, _ tracker.Repository, relationship tracker.PlanRelationship) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	fault, ok := f.takeFault(OpCreateRelationship, RelationshipKey(relationship))
	if ok && !fault.AfterWrite {
		return fault.Err
	}
	if !f.hasItem(relationship.From) || !f.hasItem(relationship.To) {
		return errors.New("relationship endpoint does not exist")
	}
	f.relationships = append(f.relationships, relationship)
	if ok {
		return fault.Err
	}
	return nil
}

// FindPlanRelationship reports whether relationship exists.
func (f *PlanTracker) FindPlanRelationship(_ context.Context, _ tracker.Repository, relationship tracker.PlanRelationship) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fault, ok := f.takeFault(OpFindRelationship, RelationshipKey(relationship)); ok {
		return false, fault.Err
	}
	return slices.Contains(f.relationships, relationship), nil
}

// takeFault removes and returns the first queued fault that matches op and
// key.
func (f *PlanTracker) takeFault(op Operation, key string) (Fault, bool) {
	for index, fault := range f.faults {
		if fault.Op == op && (fault.Key == "" || fault.Key == key) {
			f.faults = slices.Delete(f.faults, index, index+1)
			return fault, true
		}
	}
	return Fault{}, false
}

// addItem stores item under the next item number.
func (f *PlanTracker) addItem(item tracker.RenderedPlanItem) tracker.PlanItemRef {
	number := strconv.Itoa(len(f.items) + 1)
	ref := tracker.PlanItemRef{ExternalID: number, URL: "https://tracker.invalid/items/" + number}
	f.items = append(f.items, PlanItem{Ref: ref, Rendered: item})
	return ref
}

// hasItem reports whether ref names an item the fake holds.
func (f *PlanTracker) hasItem(ref tracker.PlanItemRef) bool {
	for _, item := range f.items {
		if item.Ref == ref {
			return true
		}
	}
	return false
}

// renderBody renders the plan body, the acceptance criteria, the brief
// references, and the hidden correlation marker.
func renderBody(item tracker.PlanGraphItem) string {
	var body strings.Builder
	body.WriteString(item.Body)
	if len(item.AcceptanceCriteria) > 0 {
		body.WriteString("\n\n## Acceptance criteria\n")
		for _, criterion := range item.AcceptanceCriteria {
			fmt.Fprintf(&body, "- [ ] %s\n", criterion)
		}
	}
	if len(item.BriefRefs) > 0 {
		fmt.Fprintf(&body, "\nTraces to: %s\n", strings.Join(item.BriefRefs, ", "))
	}
	fmt.Fprintf(&body, "\n<!-- factory-plan: %s -->", item.Correlation)
	return body.String()
}
