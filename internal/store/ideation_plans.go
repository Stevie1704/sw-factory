package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// MaxBriefBytes bounds the serialized content of one brief revision.
	MaxBriefBytes = 256 * 1024
	// MaxPlanBytes bounds the serialized nodes of one plan revision. Node
	// count and per-node limits belong to plan validation (internal/plangraph).
	MaxPlanBytes = 1024 * 1024
	// maxPlanTitleBytes bounds one plan-node title.
	maxPlanTitleBytes = 256
)

// ErrStalePlanSource reports a plan revision whose source brief revision is
// not the current brief revision. Such a draft would be stale on arrival.
var ErrStalePlanSource = errors.New("plan source brief revision is not current")

// BriefItem is one traceable statement inside a brief. Its ID is unique inside
// the brief revision, so plan nodes can reference it.
type BriefItem struct {
	// ID is the stable item identity inside the brief.
	ID string
	// Text is the statement.
	Text string
}

// BriefDecision is one decision with its rationale.
type BriefDecision struct {
	// ID is the stable item identity inside the brief.
	ID string
	// Decision states what was decided.
	Decision string
	// Rationale states why.
	Rationale string
}

// Brief is the structured statement of the problem a session explores.
type Brief struct {
	// Problem states the problem to solve.
	Problem string
	// Audience names who has the problem.
	Audience string
	// SuccessCriteria lists observable outcomes.
	SuccessCriteria []BriefItem
	// Scope lists what is in scope.
	Scope []BriefItem
	// NonGoals lists what is explicitly out of scope.
	NonGoals []BriefItem
	// Decisions lists resolved decisions with rationale.
	Decisions []BriefDecision
	// Assumptions lists statements taken as true without proof.
	Assumptions []BriefItem
	// Evidence lists supporting facts and their sources.
	Evidence []BriefItem
	// UnresolvedQuestions lists open questions. They stay visible; a plan must
	// not hide them.
	UnresolvedQuestions []BriefItem
}

// itemIDs returns every item ID in the brief, or an error for a missing,
// malformed, duplicate, or empty item.
func (brief Brief) itemIDs() (map[string]bool, error) {
	ids := map[string]bool{}
	add := func(id, text string) error {
		if err := validateIdeationIdentifier("brief item id", id); err != nil {
			return err
		}
		if ids[id] {
			return fmt.Errorf("ideation brief item id %q is declared more than once", id)
		}
		ids[id] = true
		return validateIdeationText("brief item "+id, text, MaxBriefBytes, true)
	}
	for _, list := range [][]BriefItem{brief.SuccessCriteria, brief.Scope, brief.NonGoals, brief.Assumptions, brief.Evidence, brief.UnresolvedQuestions} {
		for _, item := range list {
			if err := add(item.ID, item.Text); err != nil {
				return nil, err
			}
		}
	}
	for _, decision := range brief.Decisions {
		if err := add(decision.ID, decision.Decision); err != nil {
			return nil, err
		}
		if err := validateIdeationText("decision rationale "+decision.ID, decision.Rationale, MaxBriefBytes, true); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// BriefRevision is one immutable version of a session's brief.
type BriefRevision struct {
	// SessionID identifies the owning session.
	SessionID string
	// Revision is the 1-based brief version. The store assigns it.
	Revision int64
	// Brief is the content of this version.
	Brief Brief
	// CreatedAt is when the revision was accepted.
	CreatedAt time.Time
}

// PlanNode is one provider-neutral work item of a plan graph.
type PlanNode struct {
	// ID is the stable local node identity. It is independent of any external
	// issue number.
	ID string
	// Kind is the provider-neutral work-item kind. Plan validation
	// (internal/plangraph) owns the accepted vocabulary; the store requires an
	// identifier.
	Kind string
	// Title is the work-item title.
	Title string
	// Body is the work-item description.
	Body string
	// AcceptanceCriteria lists observable completion checks.
	AcceptanceCriteria []string
	// ParentID optionally names the parent node.
	ParentID string
	// DependsOn names nodes that must be done first.
	DependsOn []string
	// BriefRefs names the source brief items this node traces to.
	BriefRefs []string
}

// PlanRevision is one immutable version of a session's plan graph.
type PlanRevision struct {
	// SessionID identifies the owning session.
	SessionID string
	// Revision is the 1-based plan version. The store assigns it.
	Revision int64
	// SourceBriefRevision is the exact brief revision the plan derives from.
	SourceBriefRevision int64
	// ContextRevision is the repository commit the plan was made against.
	ContextRevision string
	// Nodes is the work-item graph.
	Nodes []PlanNode
	// Stale is derived on read: the source brief revision is no longer the
	// session's current brief revision. The stored content never changes.
	Stale bool
	// CreatedAt is when the revision was accepted.
	CreatedAt time.Time
}

// SaveBriefRevision stores brief as the next brief revision. A new brief makes
// every existing plan stale and clears the current approval in the same
// transaction, so approved content never silently follows a brief change.
func (s *Store) SaveBriefRevision(ctx context.Context, write IdeationWrite, brief Brief) (IdeationSession, BriefRevision, error) {
	content, err := encodeBrief(brief)
	if err != nil {
		return IdeationSession{}, BriefRevision{}, err
	}
	tx, session, err := s.beginIdeationWrite(ctx, write)
	if err != nil {
		return IdeationSession{}, BriefRevision{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := checkIdeationRevision(session, write); err != nil {
		return IdeationSession{}, BriefRevision{}, err
	}
	revision, err := insertBriefRevision(ctx, tx, &session, write, brief, content)
	if err != nil {
		return IdeationSession{}, BriefRevision{}, err
	}
	updated, err := commitIdeationWrite(ctx, tx, session, write)
	if err != nil {
		return IdeationSession{}, BriefRevision{}, err
	}
	return updated, revision, nil
}

// insertBriefRevision inserts the encoded brief as the next revision inside
// tx and records it, with a cleared approval, on session. The caller checked
// the session revision and commits the write.
func insertBriefRevision(ctx context.Context, tx *sql.Tx, session *IdeationSession, write IdeationWrite, brief Brief, content string) (BriefRevision, error) {
	revision := BriefRevision{SessionID: session.ID, Revision: session.BriefRevision + 1, Brief: brief, CreatedAt: ideationWriteTime(write)}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ideation_brief_revisions (session_id, revision, brief, created_at) VALUES (?, ?, ?, ?)`,
		revision.SessionID, revision.Revision, content, formatIdeationTime(revision.CreatedAt)); err != nil {
		return BriefRevision{}, fmt.Errorf("save brief revision %d: %w", revision.Revision, err)
	}
	session.BriefRevision = revision.Revision
	session.ApprovalID = ""
	return revision, nil
}

// SavePlanRevision stores plan as the next plan revision. The plan must derive
// from the current brief revision, and its references must resolve. A new
// plan revision clears the current approval; older revisions stay unchanged.
func (s *Store) SavePlanRevision(ctx context.Context, write IdeationWrite, plan PlanRevision) (IdeationSession, PlanRevision, error) {
	nodes, err := encodePlanNodes(plan)
	if err != nil {
		return IdeationSession{}, PlanRevision{}, err
	}
	tx, session, err := s.beginIdeationWrite(ctx, write)
	if err != nil {
		return IdeationSession{}, PlanRevision{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := checkIdeationRevision(session, write); err != nil {
		return IdeationSession{}, PlanRevision{}, err
	}
	plan, err = insertPlanRevision(ctx, tx, &session, write, plan, nodes)
	if err != nil {
		return IdeationSession{}, PlanRevision{}, err
	}
	updated, err := commitIdeationWrite(ctx, tx, session, write)
	if err != nil {
		return IdeationSession{}, PlanRevision{}, err
	}
	return updated, plan, nil
}

// encodePlanNodes checks the context revision of plan and returns its bounded
// stored node JSON.
func encodePlanNodes(plan PlanRevision) (string, error) {
	if !isLowerHex(plan.ContextRevision) || len(plan.ContextRevision) < 7 || len(plan.ContextRevision) > 64 {
		return "", errors.New("plan context revision must be a lowercase hexadecimal commit identity")
	}
	nodes, err := json.Marshal(plan.Nodes)
	if err != nil {
		return "", fmt.Errorf("encode plan nodes: %w", err)
	}
	if len(nodes) > MaxPlanBytes {
		return "", fmt.Errorf("plan is %d bytes; the limit is %d", len(nodes), MaxPlanBytes)
	}
	return string(nodes), nil
}

// insertPlanRevision validates plan against the current brief revision of
// session, inserts it inside tx as the next plan revision, and records it,
// with a cleared approval, on session. The caller checked the session
// revision and commits the write.
func insertPlanRevision(ctx context.Context, tx *sql.Tx, session *IdeationSession, write IdeationWrite, plan PlanRevision, nodes string) (PlanRevision, error) {
	if plan.SourceBriefRevision != session.BriefRevision || session.BriefRevision == 0 {
		return PlanRevision{}, fmt.Errorf("%w: plan names brief revision %d, current is %d", ErrStalePlanSource, plan.SourceBriefRevision, session.BriefRevision)
	}
	var content string
	if err := tx.QueryRowContext(ctx, `SELECT brief FROM ideation_brief_revisions WHERE session_id = ? AND revision = ?`, session.ID, session.BriefRevision).Scan(&content); err != nil {
		return PlanRevision{}, fmt.Errorf("read source brief revision %d: %w", session.BriefRevision, err)
	}
	var brief Brief
	if err := json.Unmarshal([]byte(content), &brief); err != nil {
		return PlanRevision{}, fmt.Errorf("decode source brief revision %d: %w", session.BriefRevision, err)
	}
	briefItems, err := brief.itemIDs()
	if err != nil {
		return PlanRevision{}, err
	}
	if err := validatePlanNodes(plan.Nodes, briefItems); err != nil {
		return PlanRevision{}, err
	}
	plan.SessionID = session.ID
	plan.Revision = session.PlanRevision + 1
	plan.Stale = false
	plan.CreatedAt = ideationWriteTime(write)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ideation_plan_revisions (session_id, revision, source_brief_revision, context_revision, nodes, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		plan.SessionID, plan.Revision, plan.SourceBriefRevision, plan.ContextRevision, nodes, formatIdeationTime(plan.CreatedAt)); err != nil {
		return PlanRevision{}, fmt.Errorf("save plan revision %d: %w", plan.Revision, err)
	}
	session.PlanRevision = plan.Revision
	session.ApprovalID = ""
	return plan, nil
}

// encodeBrief validates brief and returns its bounded stored JSON.
func encodeBrief(brief Brief) (string, error) {
	if _, err := brief.itemIDs(); err != nil {
		return "", err
	}
	content, err := json.Marshal(brief)
	if err != nil {
		return "", fmt.Errorf("encode brief: %w", err)
	}
	if len(content) > MaxBriefBytes {
		return "", fmt.Errorf("brief is %d bytes; the limit is %d", len(content), MaxBriefBytes)
	}
	if strings.ContainsRune(string(content), '\x00') {
		return "", errors.New("brief contains a NUL byte")
	}
	return string(content), nil
}

// validatePlanNodes checks the structure the store owns: the integrity of
// ValidatePlanIntegrity and brief references that resolve in the source brief.
func validatePlanNodes(nodes []PlanNode, briefItems map[string]bool) error {
	if err := ValidatePlanIntegrity(nodes); err != nil {
		return err
	}
	for _, node := range nodes {
		for _, reference := range node.BriefRefs {
			if !briefItems[reference] {
				return fmt.Errorf("plan node %q brief reference %q names no item of the source brief", node.ID, reference)
			}
		}
	}
	return nil
}

// ValidatePlanIntegrity checks the identity and reference integrity the store
// owns for a plan graph: at least one node, safe unique IDs, a kind and a
// title on every node, bounded text, and parent and dependency references
// that name another node of the plan. It does not check brief references,
// which need the source brief. Cycle, hierarchy-shape, content, and size
// policy belong to plan validation (internal/plangraph).
func ValidatePlanIntegrity(nodes []PlanNode) error {
	if len(nodes) == 0 {
		return errors.New("plan must contain at least one node")
	}
	ids := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		if err := validateIdeationIdentifier("plan node id", node.ID); err != nil {
			return err
		}
		if ids[node.ID] {
			return fmt.Errorf("plan node id %q is declared more than once", node.ID)
		}
		ids[node.ID] = true
	}
	for _, node := range nodes {
		if err := validateIdeationIdentifier("plan node "+node.ID+" kind", node.Kind); err != nil {
			return err
		}
		if err := validateIdeationText("plan node "+node.ID+" title", node.Title, maxPlanTitleBytes, true); err != nil {
			return err
		}
		if err := validateIdeationText("plan node "+node.ID+" body", node.Body, MaxPlanBytes, false); err != nil {
			return err
		}
		for _, criterion := range node.AcceptanceCriteria {
			if err := validateIdeationText("plan node "+node.ID+" acceptance criterion", criterion, MaxPlanBytes, true); err != nil {
				return err
			}
		}
		if node.ParentID != "" && (node.ParentID == node.ID || !ids[node.ParentID]) {
			return fmt.Errorf("plan node %q parent %q must name another node of the plan", node.ID, node.ParentID)
		}
		for _, dependency := range node.DependsOn {
			if dependency == node.ID || !ids[dependency] {
				return fmt.Errorf("plan node %q dependency %q must name another node of the plan", node.ID, dependency)
			}
		}
	}
	return nil
}

// scanBriefRevision decodes one brief revision row.
func scanBriefRevision(row interface{ Scan(...any) error }) (BriefRevision, error) {
	var revision BriefRevision
	var content, createdAt string
	if err := row.Scan(&revision.SessionID, &revision.Revision, &content, &createdAt); err != nil {
		return BriefRevision{}, fmt.Errorf("read brief revision: %w", err)
	}
	if err := json.Unmarshal([]byte(content), &revision.Brief); err != nil {
		return BriefRevision{}, fmt.Errorf("decode brief revision %d: %w", revision.Revision, err)
	}
	var err error
	revision.CreatedAt, err = parseIdeationTime(createdAt)
	return revision, err
}

// scanPlanRevision decodes one plan revision row. Stale is set by the caller.
func scanPlanRevision(row interface{ Scan(...any) error }) (PlanRevision, error) {
	var plan PlanRevision
	var nodes, createdAt string
	if err := row.Scan(&plan.SessionID, &plan.Revision, &plan.SourceBriefRevision, &plan.ContextRevision, &nodes, &createdAt); err != nil {
		return PlanRevision{}, fmt.Errorf("read plan revision: %w", err)
	}
	if err := json.Unmarshal([]byte(nodes), &plan.Nodes); err != nil {
		return PlanRevision{}, fmt.Errorf("decode plan revision %d: %w", plan.Revision, err)
	}
	var err error
	plan.CreatedAt, err = parseIdeationTime(createdAt)
	return plan, err
}
