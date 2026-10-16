package webui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"

	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/plangraph"
	"github.com/Stevie1704/sw-factory/internal/publication"
	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// pagePlan is the plan review, preview, and publication page of a session.
const pagePlan page = "plan"

// planPage is the model of the plan review page. It holds only
// application-owned structured state from the store; the actions it offers
// follow the session lifecycle, and the coordinator checks every command
// again.
type planPage struct {
	// Supervisor is the coordinator liveness at the read.
	Supervisor *factory.SupervisorView
	// Session is the reviewed session.
	Session store.IdeationSession
	// Plan is the current plan revision, or nil before the first draft.
	Plan *store.PlanRevision
	// Items are the plan items with their problems and reverse references.
	Items []planItemView
	// PlanProblems are the validation problems of the whole plan.
	PlanProblems []plangraph.Problem
	// Valid reports that the current plan passes plan validation.
	Valid bool
	// Draft is the editor model of the current plan.
	Draft planDraft
	// Pending lists the commands the coordinator has not completed.
	Pending []store.IdeationCommand
	// Recent lists the newest completed commands of the session.
	Recent []store.IdeationCommand
	// Preview is the newest rendered preview of the current plan revision.
	Preview *publicationView
	// PreviewRefusal explains why the newest preview of the current plan
	// revision was not rendered.
	PreviewRefusal *store.IdeationCommand
	// Approved is the publication the current approval binds.
	Approved *publicationView
	// Approval is the current approval.
	Approval *store.PlanApproval
	// Progress is the publication ledger of the current or newest approval.
	Progress *publicationProgress
	// Actions are the commands the page offers now.
	Actions planActions
}

// planItemView is one plan item on the review page.
type planItemView struct {
	// Node is the stored plan node.
	Node store.PlanNode
	// Problems are the validation problems of this node.
	Problems []plangraph.Problem
	// Children lists the nodes whose parent this node is.
	Children []string
	// Dependents lists the nodes that depend on this node.
	Dependents []string
}

// planDraft is the editor model the page hands to plan.js as JSON.
type planDraft struct {
	SessionID        string                        `json:"session_id"`
	ExpectedRevision int64                         `json:"expected_revision"`
	PlanRevision     int64                         `json:"plan_revision"`
	Kinds            []string                      `json:"kinds"`
	Nodes            []sessioncommand.PlanNodeJSON `json:"nodes"`
}

// publicationView is one rendered publication: a preview or the approved
// payload.
type publicationView struct {
	// RequestID is the preview command that rendered it, or the approval ID.
	RequestID string
	// PlanRevision is the rendered plan revision.
	PlanRevision int64
	// Target is the provider target.
	Target string
	// PayloadSHA256 is the digest of the rendered items.
	PayloadSHA256 string
	// RelationshipMappingSHA256 is the digest of the rendered relationships.
	RelationshipMappingSHA256 string
	// Items are the rendered items in provider order.
	Items []renderedItemView
	// Relationships are the rendered relationships.
	Relationships []tracker.PlanRelationshipMapping
	// TextualRelationships reports that at least one relationship uses the
	// textual fallback.
	TextualRelationships bool
}

// renderedItemView is one rendered tracker item.
type renderedItemView struct {
	tracker.RenderedPlanItem
	// Intermediate reports an item the provider requires that is not a plan
	// node, such as an intermediate hierarchy level.
	Intermediate bool
	// FieldList holds the provider fields in name order.
	FieldList []fieldView
}

// fieldView is one provider field.
type fieldView struct {
	Name  string
	Value string
}

// publicationProgress is the ledger of one approval with its receipts.
type publicationProgress struct {
	// ApprovalID identifies the approval.
	ApprovalID string
	// Operations are the ledger rows in execution order.
	Operations []operationView
	// Receipts are the attempts of the approval, newest first.
	Receipts []store.PublicationReceipt
	// Created counts succeeded item creates.
	Created int
	// Linked counts succeeded relationship creates.
	Linked int
	// Pending counts operations that have not run or not finished.
	Pending int
	// Failed counts operations that made no change.
	Failed int
	// Ambiguous counts operations with an unknown outcome.
	Ambiguous int
}

// operationView is one ledger row.
type operationView struct {
	store.PublicationOperation
	// Title is the rendered item title of an item create.
	Title string
	// Intermediate reports an item the provider requires that is not a plan
	// node.
	Intermediate bool
	// Reconcilable reports an item create that needs an operator decision.
	Reconcilable bool
}

// planActions are the commands the page offers.
type planActions struct {
	// Edit offers the draft editor.
	Edit bool
	// Regenerate offers a new draft from the planning role.
	Regenerate bool
	// Preview offers a provider preview of the current plan revision.
	Preview bool
	// Approve offers approve-and-publish of the displayed preview.
	Approve bool
	// Publish offers publication or a retry of the current approval.
	Publish bool
	// Retry reports that Publish resumes an incomplete publication, or one
	// that stopped without a recorded result.
	Retry bool
	// Publishing reports that a pending command is publishing now.
	Publishing bool
	// Reconcile offers reconciliation of ambiguous item creates.
	Reconcile bool
	// Cancel offers cancellation of approval or publication.
	Cancel bool
}

// planRoutes serves the plan review page.
type planRoutes struct {
	ui       *server
	sessions Sessions
}

// register adds the plan routes to mux.
func (routes *planRoutes) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /sessions/{id}/plan", routes.review)
}

// review renders the plan review page of one session.
func (routes *planRoutes) review(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if !validRunID(sessionID) {
		routes.ui.render(w, http.StatusBadRequest, pageError, errorPage{Title: "Invalid session", Message: "The session identity is empty or contains control characters."})
		return
	}
	activity, err := routes.sessions.SessionActivity(r.Context(), sessionID)
	if errors.Is(err, store.ErrIdeationSessionNotFound) {
		routes.ui.render(w, http.StatusNotFound, pageError, errorPage{Title: "Session not found", Message: "No session has the identity " + sessionID + "."})
		return
	}
	if err != nil {
		routes.ui.renderStoreError(w, err)
		return
	}
	routes.ui.render(w, http.StatusOK, pagePlan, newPlanPage(activity))
}

// newPlanPage builds the review page model from the session activity.
func newPlanPage(activity factory.SessionActivity) planPage {
	history := activity.History
	supervisor := activity.Supervisor
	model := planPage{Supervisor: &supervisor, Session: history.Session}
	if count := len(history.Plans); count > 0 {
		model.Plan = &history.Plans[count-1]
		model.Items, model.PlanProblems = planItems(model.Plan.Nodes)
		model.Valid = len(model.PlanProblems) == 0 && !slices.ContainsFunc(model.Items, func(item planItemView) bool { return len(item.Problems) > 0 })
	}
	model.Draft = newPlanDraft(history.Session, model.Plan)
	for _, command := range activity.Commands {
		if command.State == store.IdeationCommandCompleted {
			model.Recent = append(model.Recent, command)
		} else {
			model.Pending = append(model.Pending, command)
		}
	}
	if model.Plan != nil {
		model.Preview, model.PreviewRefusal = currentPreview(activity.Commands, model.Plan)
	}
	model.Approval, model.Approved = currentApproval(history)
	model.Progress = newProgress(history)
	model.Actions = newPlanActions(model)
	return model
}

// planItems returns the item views of nodes and the problems that name no
// node.
func planItems(nodes []store.PlanNode) ([]planItemView, []plangraph.Problem) {
	items := make([]planItemView, 0, len(nodes))
	index := map[string]int{}
	for position, node := range nodes {
		index[node.ID] = position
		items = append(items, planItemView{Node: node})
	}
	for _, node := range nodes {
		if position, ok := index[node.ParentID]; ok {
			items[position].Children = append(items[position].Children, node.ID)
		}
		for _, dependency := range node.DependsOn {
			if position, ok := index[dependency]; ok {
				items[position].Dependents = append(items[position].Dependents, node.ID)
			}
		}
	}
	var planProblems []plangraph.Problem
	var invalid *plangraph.ValidationError
	if err := plangraph.Validate(nodes); errors.As(err, &invalid) {
		for _, problem := range invalid.Problems {
			if position, ok := index[problem.NodeID]; ok {
				items[position].Problems = append(items[position].Problems, problem)
			} else {
				planProblems = append(planProblems, problem)
			}
		}
	} else if err != nil {
		planProblems = append(planProblems, plangraph.Problem{Rule: plangraph.RuleIntegrity, Detail: err.Error()})
	}
	return items, planProblems
}

// newPlanDraft returns the editor model of plan at the session revision.
func newPlanDraft(session store.IdeationSession, plan *store.PlanRevision) planDraft {
	draft := planDraft{
		SessionID: session.ID, ExpectedRevision: session.Revision,
		Kinds: []string{plangraph.KindTask, plangraph.KindInvestigation, plangraph.KindParent},
		Nodes: []sessioncommand.PlanNodeJSON{},
	}
	if plan != nil {
		draft.PlanRevision = plan.Revision
		draft.Nodes = sessioncommand.PlanNodesFromStore(plan.Nodes)
	}
	return draft
}

// currentPreview returns the newest rendered preview of plan, and the newest
// refused preview of plan when it is newer than that rendering. commands are
// newest first.
func currentPreview(commands []store.IdeationCommand, plan *store.PlanRevision) (*publicationView, *store.IdeationCommand) {
	var refusal *store.IdeationCommand
	for index, command := range commands {
		if sessioncommand.Kind(command.Kind) != sessioncommand.KindPreviewPlan || command.State != store.IdeationCommandCompleted {
			continue
		}
		if command.Outcome != store.IdeationCommandApplied {
			if refusal == nil && previewedRevision(command) == plan.Revision {
				refusal = &commands[index]
			}
			continue
		}
		var result sessioncommand.PreviewResult
		if err := json.Unmarshal(command.Result, &result); err != nil || result.PlanRevision != plan.Revision {
			continue
		}
		view, err := newPublicationView(command.RequestID, result.PlanRevision, result.Payload, result.RelationshipMapping, plan.Nodes)
		if err != nil {
			continue
		}
		return view, refusal
	}
	return nil, refusal
}

// previewedRevision returns the plan revision a preview command named.
func previewedRevision(command store.IdeationCommand) int64 {
	var payload sessioncommand.PlanRevisionPayload
	if err := json.Unmarshal(command.Payload, &payload); err != nil {
		return 0
	}
	return payload.PlanRevision
}

// newPublicationView decodes a rendered publication. An item whose node is
// not in nodes is an intermediate item the provider requires.
func newPublicationView(requestID string, planRevision int64, payload, mapping []byte, nodes []store.PlanNode) (*publicationView, error) {
	preview, err := publication.DecodePreview(payload, mapping)
	if err != nil {
		return nil, err
	}
	view := &publicationView{
		RequestID: requestID, PlanRevision: planRevision, Target: preview.Target,
		PayloadSHA256: digestOf(payload), RelationshipMappingSHA256: digestOf(mapping), Relationships: preview.Relationships,
	}
	for _, item := range preview.Items {
		rendered := renderedItemView{RenderedPlanItem: item, Intermediate: !hasNode(nodes, item.NodeID)}
		for name, value := range item.Fields {
			rendered.FieldList = append(rendered.FieldList, fieldView{Name: name, Value: value})
		}
		sort.Slice(rendered.FieldList, func(i, j int) bool { return rendered.FieldList[i].Name < rendered.FieldList[j].Name })
		view.Items = append(view.Items, rendered)
	}
	for _, relationship := range preview.Relationships {
		if relationship.Mode == tracker.PlanRelationshipTextual {
			view.TextualRelationships = true
		}
	}
	return view, nil
}

// hasNode reports whether nodes holds id.
func hasNode(nodes []store.PlanNode, id string) bool {
	return slices.ContainsFunc(nodes, func(node store.PlanNode) bool { return node.ID == id })
}

// currentApproval returns the current approval and the publication it binds.
func currentApproval(history store.IdeationHistory) (*store.PlanApproval, *publicationView) {
	for index, approval := range history.Approvals {
		if approval.ID != history.Session.ApprovalID {
			continue
		}
		var nodes []store.PlanNode
		for _, plan := range history.Plans {
			if plan.Revision == approval.PlanRevision {
				nodes = plan.Nodes
			}
		}
		view, err := newPublicationView(approval.ID, approval.PlanRevision, approval.Payload, approval.RelationshipMapping, nodes)
		if err != nil {
			return &history.Approvals[index], nil
		}
		return &history.Approvals[index], view
	}
	return nil, nil
}

// newProgress returns the ledger of the current approval, or of the newest
// approval with a ledger when no approval is current, for example after a
// cancellation. It returns nil when nothing was published.
func newProgress(history store.IdeationHistory) *publicationProgress {
	approvalID := history.Session.ApprovalID
	if !slices.ContainsFunc(history.Operations, func(operation store.PublicationOperation) bool { return operation.ApprovalID == approvalID }) {
		if count := len(history.Operations); count > 0 {
			approvalID = history.Operations[count-1].ApprovalID
		}
	}
	if approvalID == "" {
		return nil
	}
	approved, nodes := approvedItems(history, approvalID)
	progress := &publicationProgress{ApprovalID: approvalID}
	for _, operation := range history.Operations {
		if operation.ApprovalID != approvalID {
			continue
		}
		view := operationView{PublicationOperation: operation}
		if operation.Kind == store.PublicationOperationCreateItem {
			view.Title = approved[operation.NodeID]
			view.Intermediate = !hasNode(nodes, operation.NodeID)
			view.Reconcilable = history.Session.State == store.IdeationStatePublicationIncomplete &&
				(operation.State == store.PublicationOperationAmbiguous || operation.State == store.PublicationOperationAttempted)
		}
		progress.count(operation)
		progress.Operations = append(progress.Operations, view)
	}
	for _, receipt := range history.Receipts {
		if receipt.ApprovalID == approvalID {
			progress.Receipts = append([]store.PublicationReceipt{receipt}, progress.Receipts...)
		}
	}
	if len(progress.Operations) == 0 && len(progress.Receipts) == 0 {
		return nil
	}
	return progress
}

// count adds operation to the progress counters.
func (progress *publicationProgress) count(operation store.PublicationOperation) {
	switch operation.State {
	case store.PublicationOperationSucceeded:
		if operation.Kind == store.PublicationOperationCreateItem {
			progress.Created++
		} else {
			progress.Linked++
		}
	case store.PublicationOperationFailed:
		progress.Failed++
	case store.PublicationOperationAmbiguous:
		progress.Ambiguous++
	default:
		progress.Pending++
	}
}

// approvedItems returns the rendered item titles of one approval by node and
// the plan nodes it was rendered from.
func approvedItems(history store.IdeationHistory, approvalID string) (map[string]string, []store.PlanNode) {
	titles := map[string]string{}
	var nodes []store.PlanNode
	for _, approval := range history.Approvals {
		if approval.ID != approvalID {
			continue
		}
		if preview, err := publication.DecodePreview(approval.Payload, approval.RelationshipMapping); err == nil {
			for _, item := range preview.Items {
				titles[item.NodeID] = item.Title
			}
		}
		for _, plan := range history.Plans {
			if plan.Revision == approval.PlanRevision {
				nodes = plan.Nodes
			}
		}
	}
	return titles, nodes
}

// newPlanActions decides which commands the page offers. A pending command
// hides the tracker actions, so a second click or a second tab does not
// queue a second approval or publication behind the first.
func newPlanActions(model planPage) planActions {
	lifecycle := workflow.DefaultSessionLifecycle()
	allows := func(event workflow.SessionEvent) bool {
		_, err := lifecycle.Next(model.Session.State, event)
		return err == nil
	}
	busy := len(model.Pending) > 0
	state := model.Session.State
	hasPlan := model.Plan != nil
	current := hasPlan && !model.Plan.Stale
	reviewable := current && model.Valid && allows(workflow.SessionEventApproved)
	actions := planActions{
		Edit:       current && allows(workflow.SessionEventEdited),
		Regenerate: hasPlan && allows(workflow.SessionEventRoleStarted),
		Preview:    reviewable && !busy,
		Approve:    reviewable && !busy && model.Preview != nil,
		Publish:    !busy && model.Approval != nil && allows(workflow.SessionEventPublicationStarted),
		Retry:      state == store.IdeationStatePublicationIncomplete || state == store.IdeationStatePublishing,
		Cancel: allows(workflow.SessionEventCancelled) &&
			(state == store.IdeationStateApproved || state == store.IdeationStatePublishing || state == store.IdeationStatePublicationIncomplete),
	}
	actions.Reconcile = !busy && state == store.IdeationStatePublicationIncomplete
	actions.Publishing = busy && state == store.IdeationStatePublishing
	return actions
}

// digestOf returns the lowercase SHA-256 of value, as an approval names it.
func digestOf(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
