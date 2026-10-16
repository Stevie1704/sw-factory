package plangraph_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/plangraph"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// taskForTest returns a complete implementable item with id.
func taskForTest(id string) store.PlanNode {
	return store.PlanNode{
		ID: id, Kind: plangraph.KindTask, Title: "Item " + id, Body: "Problem, outcome, and context of " + id + ".",
		AcceptanceCriteria: []string{id + " is observable"}, BriefRefs: []string{"scope-1"},
	}
}

// TestValidateAcceptsASingleFix verifies that one standalone task is a
// complete plan.
func TestValidateAcceptsASingleFix(t *testing.T) {
	t.Parallel()

	if err := plangraph.Validate([]store.PlanNode{taskForTest("fix")}); err != nil {
		t.Fatalf("Validate(single fix) error = %v, want nil", err)
	}
}

// TestValidateAcceptsAnInvestigationFirstPlan verifies that a brief with a
// material unknown can plan an investigation item first, with the work that
// needs its answer depending on it, and without a parent.
func TestValidateAcceptsAnInvestigationFirstPlan(t *testing.T) {
	t.Parallel()

	spike := taskForTest("measure")
	spike.Kind = plangraph.KindInvestigation
	work := taskForTest("cache")
	work.DependsOn = []string{"measure"}
	if err := plangraph.Validate([]store.PlanNode{spike, work}); err != nil {
		t.Fatalf("Validate(investigation first) error = %v, want nil", err)
	}
}

// TestValidateRejectsReferencesTheStoreCannotResolve verifies that the
// store's identity and reference integrity is part of plan validation, so a
// dangling, self, or duplicate reference fails before acceptance.
func TestValidateRejectsReferencesTheStoreCannotResolve(t *testing.T) {
	t.Parallel()

	dangling := taskForTest("a")
	dangling.DependsOn = []string{"missing"}
	self := taskForTest("b")
	self.ParentID = "b"
	tests := map[string][]store.PlanNode{
		"dangling dependency": {dangling},
		"self parent":         {self},
		"duplicate id":        {taskForTest("c"), taskForTest("c")},
	}
	for name, nodes := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := plangraph.Validate(nodes)
			var invalid *plangraph.ValidationError
			if !errors.As(err, &invalid) || invalid.Problems[0].Rule != plangraph.RuleIntegrity {
				t.Fatalf("Validate() error = %v, want an integrity ValidationError", err)
			}
		})
	}
}

// assertRule fails t unless err is a ValidationError with a problem for rule
// whose detail contains want.
func assertRule(t *testing.T, err error, rule plangraph.Rule, want string) {
	t.Helper()
	var invalid *plangraph.ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("Validate() error = %v, want a ValidationError", err)
	}
	for _, problem := range invalid.Problems {
		if problem.Rule == rule && strings.Contains(problem.Detail, want) {
			return
		}
	}
	t.Fatalf("Validate() problems = %#v, want rule %q with %q", invalid.Problems, rule, want)
}

// TestValidateRejectsEmptyRequiredContentAndUnknownKinds verifies that every
// item must state its problem and outcome, carry observable acceptance
// criteria, and use a declared kind.
func TestValidateRejectsEmptyRequiredContentAndUnknownKinds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*store.PlanNode)
		rule   plangraph.Rule
		want   string
	}{
		{"blank body", func(node *store.PlanNode) { node.Body = " \n" }, plangraph.RuleRequiredContent, `"a" has no body`},
		{"no acceptance criteria", func(node *store.PlanNode) { node.AcceptanceCriteria = nil }, plangraph.RuleRequiredContent, `"a" has no acceptance criteria`},
		{"unknown kind", func(node *store.PlanNode) { node.Kind = "epic" }, plangraph.RuleKind, `kind "epic"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			node := taskForTest("a")
			test.mutate(&node)
			assertRule(t, plangraph.Validate([]store.PlanNode{node}), test.rule, test.want)
		})
	}
}

// featureForTest returns a parent with three children: an investigation that
// the two tasks depend on, and one task that depends on the other.
func featureForTest() []store.PlanNode {
	parent := taskForTest("export")
	parent.Kind = plangraph.KindParent
	spike := taskForTest("format")
	spike.Kind, spike.ParentID = plangraph.KindInvestigation, "export"
	writer := taskForTest("writer")
	writer.ParentID, writer.DependsOn = "export", []string{"format"}
	command := taskForTest("command")
	command.ParentID, command.DependsOn = "export", []string{"writer", "format"}
	return []store.PlanNode{parent, spike, writer, command}
}

// TestValidateAcceptsAFeatureWithDependencies verifies that a parent with
// implementable children, an investigation item, and acyclic dependencies is
// a valid plan, and that dependencies on a whole parent are allowed.
func TestValidateAcceptsAFeatureWithDependencies(t *testing.T) {
	t.Parallel()

	nodes := append(featureForTest(), taskForTest("docs"))
	nodes[len(nodes)-1].DependsOn = []string{"export"}
	if err := plangraph.Validate(nodes); err != nil {
		t.Fatalf("Validate(feature) error = %v, want nil", err)
	}
}

// TestValidateRejectsCyclesAndContradictoryStructure verifies that a plan
// graph is refused when its dependencies or parents form a cycle or when its
// hierarchy contradicts the item kinds.
func TestValidateRejectsCyclesAndContradictoryStructure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func([]store.PlanNode) []store.PlanNode
		rule   plangraph.Rule
		want   string
	}{
		{"dependency cycle", func(nodes []store.PlanNode) []store.PlanNode {
			nodes[2].DependsOn = []string{"command"}
			return nodes
		}, plangraph.RuleCycle, "writer -> command -> writer"},
		{"child depends on its parent", func(nodes []store.PlanNode) []store.PlanNode {
			nodes[2].DependsOn = []string{"export"}
			return nodes
		}, plangraph.RuleCycle, "export -> writer -> export"},
		{"parent cycle", func(nodes []store.PlanNode) []store.PlanNode {
			nodes[0].ParentID = "writer"
			nodes[2].Kind = plangraph.KindParent
			return nodes
		}, plangraph.RuleCycle, "parent cycle export -> writer -> export"},
		{"parent without children", func(nodes []store.PlanNode) []store.PlanNode {
			lone := taskForTest("lone")
			lone.Kind = plangraph.KindParent
			return append(nodes, lone)
		}, plangraph.RuleStructure, `parent "lone" has 0 children`},
		{"parent with one child", func(nodes []store.PlanNode) []store.PlanNode {
			return []store.PlanNode{nodes[0], nodes[1]}
		}, plangraph.RuleStructure, `parent "export" has 1 children`},
		{"task with children", func(nodes []store.PlanNode) []store.PlanNode {
			nodes[1].Kind = plangraph.KindTask
			nodes[2].ParentID = "format"
			nodes[3].ParentID = "format"
			return nodes
		}, plangraph.RuleStructure, `"format" is a task`},
		{"nested parent", func(nodes []store.PlanNode) []store.PlanNode {
			top := taskForTest("top")
			top.Kind = plangraph.KindParent
			nodes[0].ParentID = "top"
			sibling := taskForTest("sibling")
			sibling.ParentID = "top"
			return append(nodes, top, sibling)
		}, plangraph.RuleStructure, `parent "export" has parent "top"`},
		{"repeated dependency", func(nodes []store.PlanNode) []store.PlanNode {
			nodes[2].DependsOn = []string{"format", "format"}
			return nodes
		}, plangraph.RuleStructure, `names dependency "format" more than once`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assertRule(t, plangraph.Validate(test.mutate(featureForTest())), test.rule, test.want)
		})
	}
}

// TestValidateRefusesExcessivePlansWithActionableLimits verifies that a plan
// over an explicit limit is refused as a whole with the limit and the size it
// found, instead of being truncated.
func TestValidateRefusesExcessivePlansWithActionableLimits(t *testing.T) {
	t.Parallel()

	tooMany := make([]store.PlanNode, 0, plangraph.MaxPlanNodes+1)
	for position := range plangraph.MaxPlanNodes + 1 {
		tooMany = append(tooMany, taskForTest(fmt.Sprintf("item-%d", position)))
	}
	assertRule(t, plangraph.Validate(tooMany), plangraph.RuleLimit, fmt.Sprintf("plan has %d nodes; the limit is %d", plangraph.MaxPlanNodes+1, plangraph.MaxPlanNodes))

	criteria := taskForTest("criteria")
	for len(criteria.AcceptanceCriteria) <= plangraph.MaxAcceptanceCriteria {
		criteria.AcceptanceCriteria = append(criteria.AcceptanceCriteria, "another check")
	}
	assertRule(t, plangraph.Validate([]store.PlanNode{criteria}), plangraph.RuleLimit, fmt.Sprintf("%d acceptance criteria; the limit is %d", plangraph.MaxAcceptanceCriteria+1, plangraph.MaxAcceptanceCriteria))

	nodes := []store.PlanNode{taskForTest("wide")}
	for position := range plangraph.MaxDependencies + 1 {
		dependency := taskForTest(fmt.Sprintf("dep-%d", position))
		nodes = append(nodes, dependency)
		nodes[0].DependsOn = append(nodes[0].DependsOn, dependency.ID)
	}
	assertRule(t, plangraph.Validate(nodes), plangraph.RuleLimit, fmt.Sprintf("%d dependencies; the limit is %d", plangraph.MaxDependencies+1, plangraph.MaxDependencies))
}
