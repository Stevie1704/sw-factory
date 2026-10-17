package webui

import (
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// supervisorJSON is the browser shape of the coordinator liveness.
type supervisorJSON struct {
	Live       bool   `json:"live"`
	ObservedAt string `json:"observed_at"`
	ExpiresAt  string `json:"expires_at,omitempty"`
}

// sessionJSON is the browser shape of one session.
type sessionJSON struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	State         string `json:"state"`
	StateReason   string `json:"state_reason"`
	Revision      int64  `json:"revision"`
	BriefRevision int64  `json:"brief_revision"`
	PlanRevision  int64  `json:"plan_revision"`
	ApprovalID    string `json:"approval_id"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// turnJSON is the browser shape of one accepted conversation turn.
type turnJSON struct {
	ID              string `json:"id"`
	Sequence        int64  `json:"sequence"`
	Author          string `json:"author"`
	Role            string `json:"role,omitempty"`
	Body            string `json:"body"`
	SessionRevision int64  `json:"session_revision"`
	CreatedAt       string `json:"created_at"`
}

// briefRevisionJSON is the browser shape of the current brief revision.
type briefRevisionJSON struct {
	Revision  int64                    `json:"revision"`
	Brief     sessioncommand.BriefJSON `json:"brief"`
	CreatedAt string                   `json:"created_at"`
}

// planRevisionJSON is the browser shape of the current plan revision.
type planRevisionJSON struct {
	Revision            int64                         `json:"revision"`
	SourceBriefRevision int64                         `json:"source_brief_revision"`
	ContextRevision     string                        `json:"context_revision"`
	Stale               bool                          `json:"stale"`
	Nodes               []sessioncommand.PlanNodeJSON `json:"nodes"`
	CreatedAt           string                        `json:"created_at"`
}

// approvalJSON is the browser shape of the current approval.
type approvalJSON struct {
	ID                        string `json:"id"`
	PlanRevision              int64  `json:"plan_revision"`
	Target                    string `json:"target"`
	PayloadSHA256             string `json:"payload_sha256"`
	RelationshipMappingSHA256 string `json:"relationship_mapping_sha256"`
	ApprovedAt                string `json:"approved_at"`
}

// receiptJSON is the browser shape of one publication receipt.
type receiptJSON struct {
	ID         string              `json:"id"`
	ApprovalID string              `json:"approval_id"`
	Outcome    string              `json:"outcome"`
	Items      []publishedItemJSON `json:"items"`
	Detail     string              `json:"detail"`
	RecordedAt string              `json:"recorded_at"`
}

// publishedItemJSON is the browser shape of one published tracker item.
type publishedItemJSON struct {
	NodeID     string `json:"node_id"`
	ExternalID string `json:"external_id"`
	URL        string `json:"url"`
}

// operationJSON is the browser shape of one publication ledger operation.
type operationJSON struct {
	ApprovalID string `json:"approval_id"`
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	NodeID     string `json:"node_id,omitempty"`
	State      string `json:"state"`
	ExternalID string `json:"external_id,omitempty"`
	URL        string `json:"url,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Attempts   int    `json:"attempts"`
	UpdatedAt  string `json:"updated_at"`
}

// activityJSON is the browser shape of the durable session state. It holds
// only application-owned structured state from the store.
type activityJSON struct {
	Supervisor supervisorJSON     `json:"supervisor"`
	Session    sessionJSON        `json:"session"`
	Turns      []turnJSON         `json:"turns"`
	Brief      *briefRevisionJSON `json:"brief"`
	Plan       *planRevisionJSON  `json:"plan"`
	Approval   *approvalJSON      `json:"approval"`
	Receipts   []receiptJSON      `json:"receipts"`
	Operations []operationJSON    `json:"operations"`
	Commands   []answer           `json:"commands"`
}

// newSupervisorJSON converts the supervisor view.
func newSupervisorJSON(view factory.SupervisorView) supervisorJSON {
	value := supervisorJSON{Live: view.Live, ObservedAt: jsonTime(view.ObservedAt)}
	if view.Heartbeat != nil {
		value.ExpiresAt = jsonTime(view.Heartbeat.ExpiresAt)
	}
	return value
}

// newSessionJSON converts one session.
func newSessionJSON(session store.IdeationSession) sessionJSON {
	return sessionJSON{
		ID: session.ID, Title: session.Title, State: string(session.State), StateReason: session.StateReason,
		Revision: session.Revision, BriefRevision: session.BriefRevision, PlanRevision: session.PlanRevision,
		ApprovalID: session.ApprovalID, CreatedAt: jsonTime(session.CreatedAt), UpdatedAt: jsonTime(session.UpdatedAt),
	}
}

// newActivityJSON converts the session activity. It shows the current brief,
// plan, and approval, every receipt and ledger operation, and the newest
// commands without their results; the command route returns a result.
func newActivityJSON(activity factory.SessionActivity) activityJSON {
	history := activity.History
	value := activityJSON{
		Supervisor: newSupervisorJSON(activity.Supervisor), Session: newSessionJSON(history.Session),
		Turns: []turnJSON{}, Receipts: []receiptJSON{}, Operations: []operationJSON{}, Commands: []answer{},
	}
	for _, turn := range history.Turns {
		value.Turns = append(value.Turns, turnJSON{
			ID: turn.ID, Sequence: turn.Sequence, Author: string(turn.Author), Role: turn.Role, Body: turn.Body,
			SessionRevision: turn.SessionRevision, CreatedAt: jsonTime(turn.CreatedAt),
		})
	}
	if count := len(history.Briefs); count > 0 {
		brief := history.Briefs[count-1]
		value.Brief = &briefRevisionJSON{Revision: brief.Revision, Brief: sessioncommand.BriefFromStore(brief.Brief), CreatedAt: jsonTime(brief.CreatedAt)}
	}
	if count := len(history.Plans); count > 0 {
		plan := history.Plans[count-1]
		value.Plan = &planRevisionJSON{
			Revision: plan.Revision, SourceBriefRevision: plan.SourceBriefRevision, ContextRevision: plan.ContextRevision,
			Stale: plan.Stale, Nodes: sessioncommand.PlanNodesFromStore(plan.Nodes), CreatedAt: jsonTime(plan.CreatedAt),
		}
	}
	for _, approval := range history.Approvals {
		if approval.ID == history.Session.ApprovalID {
			value.Approval = &approvalJSON{
				ID: approval.ID, PlanRevision: approval.PlanRevision, Target: approval.Target, PayloadSHA256: approval.PayloadSHA256,
				RelationshipMappingSHA256: approval.RelationshipMappingSHA256, ApprovedAt: jsonTime(approval.ApprovedAt),
			}
		}
	}
	for _, receipt := range history.Receipts {
		items := make([]publishedItemJSON, 0, len(receipt.Items))
		for _, item := range receipt.Items {
			items = append(items, publishedItemJSON{NodeID: item.NodeID, ExternalID: item.ExternalID, URL: item.URL})
		}
		value.Receipts = append(value.Receipts, receiptJSON{
			ID: receipt.ID, ApprovalID: receipt.ApprovalID, Outcome: string(receipt.Outcome), Items: items,
			Detail: receipt.Detail, RecordedAt: jsonTime(receipt.RecordedAt),
		})
	}
	for _, operation := range history.Operations {
		value.Operations = append(value.Operations, operationJSON{
			ApprovalID: operation.ApprovalID, ID: operation.ID, Kind: string(operation.Kind), NodeID: operation.NodeID,
			State: string(operation.State), ExternalID: operation.ExternalID, URL: operation.URL, Detail: operation.Detail,
			Attempts: operation.Attempts, UpdatedAt: jsonTime(operation.UpdatedAt),
		})
	}
	for _, command := range activity.Commands {
		summary := commandAnswer(command)
		summary.Result = nil
		value.Commands = append(value.Commands, summary)
	}
	return value
}
