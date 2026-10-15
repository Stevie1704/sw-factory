package publication

import (
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// itemOperationID returns the ledger identity of the create of node. Node
// identities never contain '/', so the identity is unambiguous.
func itemOperationID(nodeID string) string {
	return "item/" + nodeID
}

// relationshipOperationID returns the ledger identity of one relationship.
func relationshipOperationID(relationship tracker.PlanRelationshipMapping) string {
	return "link/" + string(relationship.Kind) + "/" + relationship.FromNodeID + "/" + relationship.ToNodeID
}

// orderOperations validates a rendered preview and returns its ledger in
// execution order: every item after the items it needs (its parent and its
// dependencies), in preview order where the graph allows, and then every
// relationship in preview order. A relationship therefore starts only after
// both of its items exist.
func orderOperations(preview tracker.PlanPreview) ([]store.PublicationOperation, error) {
	if err := validatePreview(preview); err != nil {
		return nil, err
	}
	position := make(map[string]int, len(preview.Items))
	for index, item := range preview.Items {
		position[item.NodeID] = index
	}
	needs := make([]int, len(preview.Items))
	unlocks := make([][]int, len(preview.Items))
	for _, relationship := range preview.Relationships {
		from, to := position[relationship.FromNodeID], position[relationship.ToNodeID]
		needs[from]++
		unlocks[to] = append(unlocks[to], from)
	}
	done := make([]bool, len(preview.Items))
	operations := make([]store.PublicationOperation, 0, len(preview.Items)+len(preview.Relationships))
	for len(operations) < len(preview.Items) {
		next := -1
		for index := range preview.Items {
			if !done[index] && needs[index] == 0 {
				next = index
				break
			}
		}
		if next < 0 {
			return nil, fmt.Errorf("%w: the relationships form a cycle", ErrInvalidPayload)
		}
		done[next] = true
		for _, dependent := range unlocks[next] {
			needs[dependent]--
		}
		item := preview.Items[next]
		operations = append(operations, store.PublicationOperation{
			ID: itemOperationID(item.NodeID), Kind: store.PublicationOperationCreateItem,
			NodeID: item.NodeID, Correlation: item.Correlation,
		})
	}
	for _, relationship := range preview.Relationships {
		operations = append(operations, store.PublicationOperation{
			ID: relationshipOperationID(relationship), Kind: store.PublicationOperationCreateRelationship,
			RelationshipKind: string(relationship.Kind), FromNodeID: relationship.FromNodeID, ToNodeID: relationship.ToNodeID,
		})
	}
	return operations, nil
}

// validatePreview refuses a rendering the factory must not approve or
// publish: missing or repeated identities, a factory state label (decision
// D6), or a relationship with an unknown kind, mode, or endpoint.
func validatePreview(preview tracker.PlanPreview) error {
	if strings.TrimSpace(preview.Target) == "" {
		return fmt.Errorf("%w: the preview has no target", ErrInvalidPayload)
	}
	if len(preview.Items) == 0 {
		return fmt.Errorf("%w: the preview has no items", ErrInvalidPayload)
	}
	nodes := make(map[string]bool, len(preview.Items))
	correlations := make(map[string]bool, len(preview.Items))
	for _, item := range preview.Items {
		if item.NodeID == "" || strings.Contains(item.NodeID, "/") || nodes[item.NodeID] {
			return fmt.Errorf("%w: item node %q is empty, contains '/', or repeats", ErrInvalidPayload, item.NodeID)
		}
		nodes[item.NodeID] = true
		if item.Correlation == "" || correlations[item.Correlation] {
			return fmt.Errorf("%w: item %q correlation %q is empty or repeats", ErrInvalidPayload, item.NodeID, item.Correlation)
		}
		correlations[item.Correlation] = true
		if strings.TrimSpace(item.Title) == "" || strings.TrimSpace(item.Type) == "" {
			return fmt.Errorf("%w: item %q has no title or type", ErrInvalidPayload, item.NodeID)
		}
		for _, label := range item.Labels {
			if tracker.IsFactoryStateLabel(label) {
				return fmt.Errorf("%w: item %q carries factory state label %q; publication never authorizes execution", ErrInvalidPayload, item.NodeID, label)
			}
		}
	}
	relationships := make(map[string]bool, len(preview.Relationships))
	for _, relationship := range preview.Relationships {
		if relationship.Kind != tracker.PlanRelationshipParent && relationship.Kind != tracker.PlanRelationshipDependsOn {
			return fmt.Errorf("%w: relationship kind %q is not declared", ErrInvalidPayload, relationship.Kind)
		}
		if relationship.Mode != tracker.PlanRelationshipNative && relationship.Mode != tracker.PlanRelationshipTextual {
			return fmt.Errorf("%w: relationship mode %q is not declared", ErrInvalidPayload, relationship.Mode)
		}
		if !nodes[relationship.FromNodeID] || !nodes[relationship.ToNodeID] || relationship.FromNodeID == relationship.ToNodeID {
			return fmt.Errorf("%w: relationship %s must join two different items of the preview", ErrInvalidPayload, relationshipOperationID(relationship))
		}
		id := relationshipOperationID(relationship)
		if relationships[id] {
			return fmt.Errorf("%w: relationship %s repeats", ErrInvalidPayload, id)
		}
		relationships[id] = true
	}
	return nil
}
