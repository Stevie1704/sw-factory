package plangraph

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// graph is an index of a plan whose references the store has resolved.
type graph struct {
	nodes    []store.PlanNode
	index    map[string]int
	children map[string][]string
}

// newGraph indexes nodes by ID and collects the children of each node in plan
// order.
func newGraph(nodes []store.PlanNode) graph {
	value := graph{nodes: nodes, index: make(map[string]int, len(nodes)), children: map[string][]string{}}
	for position, node := range nodes {
		value.index[node.ID] = position
	}
	for _, node := range nodes {
		if node.ParentID != "" {
			value.children[node.ParentID] = append(value.children[node.ParentID], node.ID)
		}
	}
	return value
}

// node returns the node with id.
func (g graph) node(id string) store.PlanNode {
	return g.nodes[g.index[id]]
}

// structureProblems checks that only top-level parents have children, that
// each parent has enough children, and that no dependency repeats.
func (g graph) structureProblems() []Problem {
	var problems []Problem
	add := func(node store.PlanNode, format string, args ...any) {
		problems = append(problems, Problem{NodeID: node.ID, Rule: RuleStructure, Detail: fmt.Sprintf(format, args...)})
	}
	for _, node := range g.nodes {
		children := len(g.children[node.ID])
		switch {
		case node.Kind == KindParent && children < MinParentChildren:
			add(node, "parent %q has %d children; a parent needs at least %d, otherwise plan one %s", node.ID, children, MinParentChildren, KindTask)
		case node.Kind != KindParent && children != 0:
			add(node, "plan node %q is a %s and cannot have children; make it a %s or move its children", node.ID, node.Kind, KindParent)
		}
		if node.Kind == KindParent && node.ParentID != "" {
			add(node, "parent %q has parent %q; parents are top-level, so flatten the hierarchy", node.ID, node.ParentID)
		}
		seen := make(map[string]bool, len(node.DependsOn))
		for _, dependency := range node.DependsOn {
			if seen[dependency] {
				add(node, "plan node %q names dependency %q more than once", node.ID, dependency)
			}
			seen[dependency] = true
		}
	}
	return problems
}

// parentCycles reports each cycle of parent relationships once.
func (g graph) parentCycles() []Problem {
	var problems []Problem
	reported := map[string]bool{}
	for _, node := range g.nodes {
		chain := []string{node.ID}
		onChain := map[string]bool{node.ID: true}
		for current := node.ParentID; current != ""; current = g.node(current).ParentID {
			if onChain[current] {
				cycle := g.canonicalCycle(chain[slices.Index(chain, current):])
				if key := strings.Join(cycle, " "); !reported[key] {
					reported[key] = true
					problems = append(problems, Problem{NodeID: cycle[0], Rule: RuleCycle, Detail: fmt.Sprintf(
						"parent cycle %s; give the hierarchy one top-level parent", strings.Join(append(cycle, cycle[0]), " -> "))})
				}
				break
			}
			chain = append(chain, current)
			onChain[current] = true
		}
	}
	return problems
}

// orderingCycles reports each cycle of the completion order once. A node
// comes after its dependencies, and a parent comes after its children.
func (g graph) orderingCycles() []Problem {
	const (
		unvisited = iota
		visiting
		done
	)
	state := make(map[string]int, len(g.nodes))
	var stack []string
	var problems []Problem
	reported := map[string]bool{}
	var visit func(id string)
	visit = func(id string) {
		state[id] = visiting
		stack = append(stack, id)
		for _, next := range g.after(id) {
			switch state[next] {
			case unvisited:
				visit(next)
			case visiting:
				cycle := g.canonicalCycle(stack[slices.Index(stack, next):])
				if key := strings.Join(cycle, " "); !reported[key] {
					reported[key] = true
					problems = append(problems, Problem{NodeID: cycle[0], Rule: RuleCycle, Detail: g.describeOrderingCycle(cycle)})
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = done
	}
	for _, node := range g.nodes {
		if state[node.ID] == unvisited {
			visit(node.ID)
		}
	}
	return problems
}

// after returns the nodes that must be done before id: its children, then its
// dependencies.
func (g graph) after(id string) []string {
	return append(append([]string(nil), g.children[id]...), g.node(id).DependsOn...)
}

// describeOrderingCycle names each relationship of a completion-order cycle.
func (g graph) describeOrderingCycle(cycle []string) string {
	reasons := make([]string, 0, len(cycle))
	for position, id := range cycle {
		next := cycle[(position+1)%len(cycle)]
		if g.node(next).ParentID == id {
			reasons = append(reasons, id+" contains "+next)
		} else {
			reasons = append(reasons, id+" depends on "+next)
		}
	}
	return fmt.Sprintf("dependency cycle %s (%s); remove one of these relationships",
		strings.Join(append(append([]string(nil), cycle...), cycle[0]), " -> "), strings.Join(reasons, "; "))
}

// canonicalCycle rotates cycle so that it starts at its node that comes first
// in the plan, which makes the report independent of the search order.
func (g graph) canonicalCycle(cycle []string) []string {
	start := 0
	for position, id := range cycle {
		if g.index[id] < g.index[cycle[start]] {
			start = position
		}
	}
	return append(append([]string(nil), cycle[start:]...), cycle[:start]...)
}
