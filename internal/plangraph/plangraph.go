// Package plangraph owns the deterministic structural policy of a draft plan
// graph: the accepted item kinds, required content, the hierarchy shape,
// acyclic dependencies, and explicit size limits. The operational store owns
// identity and reference integrity (store.ValidatePlanIntegrity); this package
// builds the structural rules on top of it. Planning craft, such as when to
// decompose, lives in the versioned planning prompt, not here.
package plangraph

import (
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// Accepted provider-neutral item kinds. Publication maps them to tracker
// types; the vocabulary is closed so a typo cannot reach a tracker.
const (
	// KindTask is one implementable item. It has no children.
	KindTask = "task"
	// KindInvestigation is one research item that answers a material unknown
	// of the brief before implementation depends on it. It has no children.
	KindInvestigation = "investigation"
	// KindParent groups implementable children when decomposition adds value.
	// It is a top-level item with at least MinParentChildren children.
	KindParent = "parent"
)

// Explicit size limits of one draft plan. A plan over a limit is refused as a
// whole with an actionable problem; it is never truncated. The limits keep a
// draft small enough for one person to review. The report transport bounds
// are separate: report.MaxPlanDraftNodes is larger, and a report must also fit
// report.MaxReportBytes as a whole, which a plan inside these limits can
// exceed when its texts are at their field limits.
const (
	// MaxPlanNodes bounds the items of one plan.
	MaxPlanNodes = 25
	// MaxAcceptanceCriteria bounds the acceptance criteria of one item.
	MaxAcceptanceCriteria = 12
	// MaxDependencies bounds the dependencies of one item.
	MaxDependencies = 10
)

// MinParentChildren is the smallest number of children that makes a parent
// worth its own item. With fewer children one item is sufficient.
const MinParentChildren = 2

// Rule names one structural plan rule that a problem violates.
type Rule string

const (
	// RuleIntegrity is the store-owned identity and reference integrity:
	// unique IDs and parent and dependency references that resolve to another
	// node of the plan.
	RuleIntegrity Rule = "integrity"
	// RuleKind requires a declared item kind.
	RuleKind Rule = "kind"
	// RuleRequiredContent requires a body and observable acceptance criteria
	// on every item, so each item can stand alone.
	RuleRequiredContent Rule = "required_content"
	// RuleCycle refuses a parent cycle and an ordering cycle. A parent is done
	// only after its children, so a cycle may run through dependencies and
	// parent relationships together.
	RuleCycle Rule = "cycle"
	// RuleStructure refuses a hierarchy or dependency list that contradicts
	// the item kinds: children below a non-parent, a parent with too few
	// children, a nested parent, or a repeated dependency.
	RuleStructure Rule = "structure"
	// RuleLimit refuses a plan over an explicit size limit.
	RuleLimit Rule = "limit"
)

// Problem is one actionable rule violation of a plan graph.
type Problem struct {
	// NodeID names the node the problem is about. It is empty for a problem of
	// the whole plan.
	NodeID string
	// Rule is the violated rule.
	Rule Rule
	// Detail states the violation and what to change.
	Detail string
}

// ValidationError reports every problem that makes a plan graph invalid. The
// plan is refused as a whole; nothing is dropped or truncated.
type ValidationError struct {
	// Problems lists the violations in a stable order.
	Problems []Problem
}

// Error lists the problems in one line.
func (e *ValidationError) Error() string {
	details := make([]string, 0, len(e.Problems))
	for _, problem := range e.Problems {
		details = append(details, problem.Detail)
	}
	return fmt.Sprintf("plan graph is invalid (%d problems): %s", len(e.Problems), strings.Join(details, "; "))
}

// Validate checks nodes against the store integrity and the structural plan
// rules. It returns a *ValidationError that lists every problem, or nil for a
// valid plan.
func Validate(nodes []store.PlanNode) error {
	if err := store.ValidatePlanIntegrity(nodes); err != nil {
		return &ValidationError{Problems: []Problem{{Rule: RuleIntegrity, Detail: err.Error()}}}
	}
	var problems []Problem
	if len(nodes) > MaxPlanNodes {
		problems = append(problems, Problem{Rule: RuleLimit, Detail: fmt.Sprintf(
			"plan has %d nodes; the limit is %d. Plan the first deliverable and leave later work in the brief, or merge closely related items", len(nodes), MaxPlanNodes)})
	}
	for _, node := range nodes {
		problems = append(problems, contentProblems(node)...)
		problems = append(problems, limitProblems(node)...)
	}
	graph := newGraph(nodes)
	problems = append(problems, graph.structureProblems()...)
	if parentCycles := graph.parentCycles(); len(parentCycles) != 0 {
		problems = append(problems, parentCycles...)
	} else {
		problems = append(problems, graph.orderingCycles()...)
	}
	if len(problems) != 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

// contentProblems checks the kind and the required content of one node.
func contentProblems(node store.PlanNode) []Problem {
	var problems []Problem
	switch node.Kind {
	case KindTask, KindInvestigation, KindParent:
	default:
		problems = append(problems, Problem{NodeID: node.ID, Rule: RuleKind, Detail: fmt.Sprintf(
			"plan node %q has kind %q; use %s, %s, or %s", node.ID, node.Kind, KindTask, KindInvestigation, KindParent)})
	}
	if strings.TrimSpace(node.Body) == "" {
		problems = append(problems, Problem{NodeID: node.ID, Rule: RuleRequiredContent, Detail: fmt.Sprintf(
			"plan node %q has no body; state the problem, outcome, scope, and relevant context", node.ID)})
	}
	if len(node.AcceptanceCriteria) == 0 {
		problems = append(problems, Problem{NodeID: node.ID, Rule: RuleRequiredContent, Detail: fmt.Sprintf(
			"plan node %q has no acceptance criteria; add at least one observable check", node.ID)})
	}
	return problems
}

// limitProblems checks the per-item size limits of one node.
func limitProblems(node store.PlanNode) []Problem {
	var problems []Problem
	if count := len(node.AcceptanceCriteria); count > MaxAcceptanceCriteria {
		problems = append(problems, Problem{NodeID: node.ID, Rule: RuleLimit, Detail: fmt.Sprintf(
			"plan node %q has %d acceptance criteria; the limit is %d. Merge overlapping checks or split the item", node.ID, count, MaxAcceptanceCriteria)})
	}
	if count := len(node.DependsOn); count > MaxDependencies {
		problems = append(problems, Problem{NodeID: node.ID, Rule: RuleLimit, Detail: fmt.Sprintf(
			"plan node %q has %d dependencies; the limit is %d. Depend on a parent instead of each of its children", node.ID, count, MaxDependencies)})
	}
	return problems
}
