// Package sessioncommand defines the commands that the local UI sends to the
// coordinator for ideation sessions, and dispatches them in the coordinator
// (ADR 0024). The UI parses and validates a request and appends it to the
// durable command inbox; the coordinator is the only process that applies
// it. Each command kind has one handler, so session roles, plan validation,
// and review pages can supply behavior without a second command path.
package sessioncommand

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// Kind names one session command.
type Kind string

const (
	// KindCreateSession creates a session with the request's session ID.
	KindCreateSession Kind = "create_session"
	// KindSubmitTurn appends one user turn. The request ID is the turn ID.
	KindSubmitTurn Kind = "submit_turn"
	// KindEditBrief saves a user-edited brief as the next brief revision.
	KindEditBrief Kind = "edit_brief"
	// KindHandOff hands the displayed brief revision to planning.
	KindHandOff Kind = "hand_off"
	// KindEditDraft saves a user-edited plan draft as the next plan revision.
	KindEditDraft Kind = "edit_draft"
	// KindRegenerateDraft asks planning for a new draft of the displayed plan
	// revision.
	KindRegenerateDraft Kind = "regenerate_draft"
	// KindPreviewPlan renders the tracker publication preview of the displayed
	// plan revision. It changes no session state.
	KindPreviewPlan Kind = "preview_plan"
	// KindApprove approves the previewed publication and publishes it in the
	// same command: approval and publication are one user action.
	KindApprove Kind = "approve"
	// KindPublish starts or retries publication of the current approval.
	KindPublish Kind = "publish"
	// KindReconcile resolves one ambiguous item create of an incomplete
	// publication.
	KindReconcile Kind = "reconcile"
	// KindDefer parks the session.
	KindDefer Kind = "defer"
	// KindResume resumes a parked session.
	KindResume Kind = "resume"
	// KindInvestigationNeeded records the decision that material unknowns
	// need investigation before planning. It creates no backlog item.
	KindInvestigationNeeded Kind = "investigation_needed"
	// KindNoBuild records the decision not to build.
	KindNoBuild Kind = "no_build"
	// KindCancel cancels the session. Cancellation never deletes tracker
	// items.
	KindCancel Kind = "cancel"
)

// MaxRequestBytes bounds one request body. It leaves room for the largest
// payload and the envelope.
const MaxRequestBytes = store.MaxIdeationCommandPayloadBytes + 4096

// maxDeferredQuestions bounds the deferred questions of one handoff. It is
// above any brief the store accepts in practice.
const maxDeferredQuestions = 256

// maxTitleBytes bounds a session title. It matches the store limit.
const maxTitleBytes = 200

// InvalidRequestError reports browser input that does not form a valid
// command. Nothing was appended to the inbox.
type InvalidRequestError struct {
	// Reason explains what is wrong.
	Reason string
}

// Error describes the invalid request.
func (e *InvalidRequestError) Error() string { return "invalid session command: " + e.Reason }

// invalid returns an InvalidRequestError with a formatted reason.
func invalid(format string, arguments ...any) error {
	return &InvalidRequestError{Reason: fmt.Sprintf(format, arguments...)}
}

// request is the JSON envelope of one browser command.
type request struct {
	RequestID        string          `json:"request_id"`
	SessionID        string          `json:"session_id"`
	Kind             Kind            `json:"kind"`
	ExpectedRevision int64           `json:"expected_revision"`
	Payload          json.RawMessage `json:"payload"`
}

// CreateSessionPayload is the payload of KindCreateSession.
type CreateSessionPayload struct {
	// Title is an optional short operator label.
	Title string `json:"title"`
}

// TurnPayload is the payload of KindSubmitTurn.
type TurnPayload struct {
	// Body is the user's answer or idea.
	Body string `json:"body"`
	// Corrects optionally names the earlier user turn whose answer this turn
	// changes.
	Corrects string `json:"corrects,omitempty"`
}

// BriefPayload is the payload of KindEditBrief.
type BriefPayload struct {
	// Brief is the complete edited brief.
	Brief BriefJSON `json:"brief"`
}

// HandOffPayload is the payload of KindHandOff.
type HandOffPayload struct {
	// BriefRevision is the brief revision the user saw and hands off.
	BriefRevision int64 `json:"brief_revision"`
	// DeferredQuestions names every unresolved question of that brief
	// revision. The user defers each one explicitly to planning, which may
	// turn it into an investigation item but must not answer it.
	DeferredQuestions []string `json:"deferred_questions,omitempty"`
}

// DraftPayload is the payload of KindEditDraft.
type DraftPayload struct {
	// Nodes is the complete edited plan graph.
	Nodes []PlanNodeJSON `json:"nodes"`
}

// PlanRevisionPayload is the payload of KindRegenerateDraft and
// KindPreviewPlan.
type PlanRevisionPayload struct {
	// PlanRevision is the plan revision the user saw.
	PlanRevision int64 `json:"plan_revision"`
}

// ApprovePayload is the payload of KindApprove. The digests bind the
// approval to the exact preview the user reviewed.
type ApprovePayload struct {
	// PlanRevision is the previewed plan revision.
	PlanRevision int64 `json:"plan_revision"`
	// PayloadSHA256 is the digest of the previewed rendered items.
	PayloadSHA256 string `json:"payload_sha256"`
	// RelationshipMappingSHA256 is the digest of the previewed relationships.
	RelationshipMappingSHA256 string `json:"relationship_mapping_sha256"`
}

// PublishPayload is the payload of KindPublish. It has no fields.
type PublishPayload struct{}

// ReconcilePayload is the payload of KindReconcile.
type ReconcilePayload struct {
	// OperationID identifies the ambiguous item create.
	OperationID string `json:"operation_id"`
	// ExternalID adopts this tracker item; empty confirms that none exists.
	ExternalID string `json:"external_id"`
}

// ReasonPayload is the payload of KindDefer, KindResume,
// KindInvestigationNeeded, KindNoBuild, and KindCancel.
type ReasonPayload struct {
	// Reason optionally explains the decision.
	Reason string `json:"reason"`
}

// BriefItemJSON is the browser shape of store.BriefItem.
type BriefItemJSON struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// BriefDecisionJSON is the browser shape of store.BriefDecision.
type BriefDecisionJSON struct {
	ID        string `json:"id"`
	Decision  string `json:"decision"`
	Rationale string `json:"rationale"`
}

// BriefJSON is the browser shape of store.Brief.
type BriefJSON struct {
	Problem             string              `json:"problem"`
	Audience            string              `json:"audience"`
	SuccessCriteria     []BriefItemJSON     `json:"success_criteria"`
	Scope               []BriefItemJSON     `json:"scope"`
	NonGoals            []BriefItemJSON     `json:"non_goals"`
	Decisions           []BriefDecisionJSON `json:"decisions"`
	Assumptions         []BriefItemJSON     `json:"assumptions"`
	Evidence            []BriefItemJSON     `json:"evidence"`
	UnresolvedQuestions []BriefItemJSON     `json:"unresolved_questions"`
}

// PlanNodeJSON is the browser shape of store.PlanNode.
type PlanNodeJSON struct {
	ID                 string   `json:"id"`
	Kind               string   `json:"kind"`
	Title              string   `json:"title"`
	Body               string   `json:"body"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	ParentID           string   `json:"parent_id"`
	DependsOn          []string `json:"depends_on"`
	BriefRefs          []string `json:"brief_refs"`
}

// payload is a decoded command payload that checks its own bounds.
type payload interface{ validate() error }

// kindSpec declares one command kind: the payload it carries, the handler
// that applies it in the coordinator, and whether that handler calls the
// tracker.
type kindSpec struct {
	kind    Kind
	payload func() payload
	handler Handler
	tracker bool
}

// kindTable is the one declaration of every command kind, in declaration
// order. Payload decoding, the default handlers, and the dispatcher's
// background work all read it. It is a function, not a variable, because
// the handlers decode their payloads through it.
func kindTable() []kindSpec {
	reason := func() payload { return &ReasonPayload{} }
	planRevision := func() payload { return &PlanRevisionPayload{} }
	return []kindSpec{
		{kind: KindCreateSession, payload: func() payload { return &CreateSessionPayload{} }, handler: createSession},
		{kind: KindSubmitTurn, payload: func() payload { return &TurnPayload{} }, handler: submitTurn},
		{kind: KindEditBrief, payload: func() payload { return &BriefPayload{} }, handler: editBrief},
		{kind: KindHandOff, payload: func() payload { return &HandOffPayload{} }, handler: handOff},
		{kind: KindEditDraft, payload: func() payload { return &DraftPayload{} }, handler: editDraft},
		{kind: KindRegenerateDraft, payload: planRevision, handler: regenerateDraft},
		{kind: KindPreviewPlan, payload: planRevision, handler: previewPlan, tracker: true},
		{kind: KindApprove, payload: func() payload { return &ApprovePayload{} }, handler: approve, tracker: true},
		{kind: KindPublish, payload: func() payload { return &PublishPayload{} }, handler: publish, tracker: true},
		{kind: KindReconcile, payload: func() payload { return &ReconcilePayload{} }, handler: reconcile, tracker: true},
		{kind: KindDefer, payload: reason, handler: Transition(workflow.SessionEventDeferred)},
		{kind: KindResume, payload: reason, handler: transitionBy(resumeEvent)},
		{kind: KindInvestigationNeeded, payload: reason, handler: investigationNeeded},
		{kind: KindNoBuild, payload: reason, handler: Transition(workflow.SessionEventNoBuild)},
		{kind: KindCancel, payload: reason, handler: Transition(workflow.SessionEventCancelled)},
	}
}

// lookupKind returns the declaration of kind.
func lookupKind(kind Kind) (kindSpec, bool) {
	for _, spec := range kindTable() {
		if spec.kind == kind {
			return spec, true
		}
	}
	return kindSpec{}, false
}

// Kinds returns every declared command kind.
func Kinds() []Kind {
	table := kindTable()
	kinds := make([]Kind, 0, len(table))
	for _, spec := range table {
		kinds = append(kinds, spec.kind)
	}
	return kinds
}

// ParseRequest decodes one browser request strictly, validates its envelope
// and its kind's payload, and returns the inbox command with a canonical
// payload. Unknown fields, unknown kinds, and oversized values return an
// InvalidRequestError. The session revision is checked later, against the
// store.
func ParseRequest(body []byte) (store.IdeationCommand, error) {
	if len(body) > MaxRequestBytes {
		return store.IdeationCommand{}, invalid("the request is %d bytes; the limit is %d", len(body), MaxRequestBytes)
	}
	var envelope request
	if err := decodeStrict(body, &envelope); err != nil {
		return store.IdeationCommand{}, invalid("%v", err)
	}
	if envelope.Payload == nil {
		return store.IdeationCommand{}, invalid("payload is required")
	}
	if !safeIdentifier(envelope.RequestID) || !safeIdentifier(envelope.SessionID) {
		return store.IdeationCommand{}, invalid("request_id and session_id must be 1 to 128 letters, digits, '.', '_', or '-'")
	}
	if envelope.Kind == KindCreateSession && envelope.ExpectedRevision != 0 {
		return store.IdeationCommand{}, invalid("create_session must use expected_revision 0")
	}
	if envelope.Kind != KindCreateSession && envelope.ExpectedRevision < 1 {
		return store.IdeationCommand{}, invalid("expected_revision must be the session revision the page shows")
	}
	payload, err := DecodePayload(envelope.Kind, envelope.Payload)
	if err != nil {
		return store.IdeationCommand{}, err
	}
	canonical, err := json.Marshal(payload)
	if err != nil {
		return store.IdeationCommand{}, fmt.Errorf("encode session command payload: %w", err)
	}
	return store.IdeationCommand{
		RequestID: envelope.RequestID, SessionID: envelope.SessionID, Kind: string(envelope.Kind),
		ExpectedRevision: envelope.ExpectedRevision, Payload: canonical,
	}, nil
}

// DecodePayload decodes and validates the payload of kind strictly. It
// returns a pointer to the kind's payload type.
func DecodePayload(kind Kind, raw []byte) (any, error) {
	spec, ok := lookupKind(kind)
	if !ok {
		return nil, invalid("kind %q is not declared", kind)
	}
	payload := spec.payload()
	if err := decodeStrict(raw, payload); err != nil {
		return nil, invalid("%s payload: %v", kind, err)
	}
	if err := payload.validate(); err != nil {
		return nil, invalid("%s payload: %v", kind, err)
	}
	return payload, nil
}

// validate checks the bounded title.
func (p *CreateSessionPayload) validate() error {
	return boundedText("title", p.Title, maxTitleBytes, false)
}

// validate checks the required, bounded turn body and the optional
// corrected turn identity. The store checks that the corrected turn exists.
func (p *TurnPayload) validate() error {
	if p.Corrects != "" && !safeIdentifier(p.Corrects) {
		return errors.New("corrects must be the ID of an earlier turn")
	}
	return boundedText("body", p.Body, store.MaxIdeationTurnBytes, true)
}

// validate checks the bounded brief. The store checks item identities.
func (p *BriefPayload) validate() error {
	encoded, err := json.Marshal(p.Brief)
	if err != nil {
		return err
	}
	if len(encoded) > store.MaxBriefBytes {
		return fmt.Errorf("brief is %d bytes; the limit is %d", len(encoded), store.MaxBriefBytes)
	}
	return nil
}

// validate checks the handed-off brief revision and the deferred question
// identities. The handler checks them against the brief.
func (p *HandOffPayload) validate() error {
	if len(p.DeferredQuestions) > maxDeferredQuestions {
		return fmt.Errorf("deferred_questions has %d entries; the limit is %d", len(p.DeferredQuestions), maxDeferredQuestions)
	}
	for _, id := range p.DeferredQuestions {
		if !safeIdentifier(id) {
			return errors.New("deferred_questions must hold brief item IDs")
		}
	}
	return positive("brief_revision", p.BriefRevision)
}

// validate checks the bounded draft. The store checks node references, and
// plan validation checks the graph shape.
func (p *DraftPayload) validate() error {
	if len(p.Nodes) == 0 {
		return errors.New("nodes must not be empty")
	}
	encoded, err := json.Marshal(p.Nodes)
	if err != nil {
		return err
	}
	if len(encoded) > store.MaxPlanBytes {
		return fmt.Errorf("plan is %d bytes; the limit is %d", len(encoded), store.MaxPlanBytes)
	}
	return nil
}

// validate checks the displayed plan revision.
func (p *PlanRevisionPayload) validate() error { return positive("plan_revision", p.PlanRevision) }

// validate checks the plan revision and both digests.
func (p *ApprovePayload) validate() error {
	if err := positive("plan_revision", p.PlanRevision); err != nil {
		return err
	}
	if !sha256Hex(p.PayloadSHA256) || !sha256Hex(p.RelationshipMappingSHA256) {
		return errors.New("payload_sha256 and relationship_mapping_sha256 must be lowercase SHA-256 digests of the preview")
	}
	return nil
}

// validate accepts the empty publish payload.
func (*PublishPayload) validate() error { return nil }

// validate checks the operation and the optional external identity.
func (p *ReconcilePayload) validate() error {
	if p.OperationID == "" || len(p.OperationID) > 512 || strings.ContainsFunc(p.OperationID, isControl) {
		return errors.New("operation_id must be a ledger operation identity")
	}
	return boundedText("external_id", p.ExternalID, 256, false)
}

// validate checks the bounded reason.
func (p *ReasonPayload) validate() error {
	return boundedText("reason", p.Reason, store.MaxIdeationReasonBytes, false)
}

// Brief converts the browser brief into the stored brief.
func (b BriefJSON) Brief() store.Brief {
	items := func(values []BriefItemJSON) []store.BriefItem {
		converted := make([]store.BriefItem, 0, len(values))
		for _, value := range values {
			converted = append(converted, store.BriefItem{ID: value.ID, Text: value.Text})
		}
		return converted
	}
	decisions := make([]store.BriefDecision, 0, len(b.Decisions))
	for _, decision := range b.Decisions {
		decisions = append(decisions, store.BriefDecision{ID: decision.ID, Decision: decision.Decision, Rationale: decision.Rationale})
	}
	return store.Brief{
		Problem: b.Problem, Audience: b.Audience, SuccessCriteria: items(b.SuccessCriteria), Scope: items(b.Scope),
		NonGoals: items(b.NonGoals), Decisions: decisions, Assumptions: items(b.Assumptions), Evidence: items(b.Evidence),
		UnresolvedQuestions: items(b.UnresolvedQuestions),
	}
}

// BriefFromStore converts a stored brief into the browser shape.
func BriefFromStore(brief store.Brief) BriefJSON {
	items := func(values []store.BriefItem) []BriefItemJSON {
		converted := make([]BriefItemJSON, 0, len(values))
		for _, value := range values {
			converted = append(converted, BriefItemJSON{ID: value.ID, Text: value.Text})
		}
		return converted
	}
	decisions := make([]BriefDecisionJSON, 0, len(brief.Decisions))
	for _, decision := range brief.Decisions {
		decisions = append(decisions, BriefDecisionJSON{ID: decision.ID, Decision: decision.Decision, Rationale: decision.Rationale})
	}
	return BriefJSON{
		Problem: brief.Problem, Audience: brief.Audience, SuccessCriteria: items(brief.SuccessCriteria), Scope: items(brief.Scope),
		NonGoals: items(brief.NonGoals), Decisions: decisions, Assumptions: items(brief.Assumptions), Evidence: items(brief.Evidence),
		UnresolvedQuestions: items(brief.UnresolvedQuestions),
	}
}

// PlanNodes converts browser plan nodes into stored plan nodes.
func PlanNodes(nodes []PlanNodeJSON) []store.PlanNode {
	converted := make([]store.PlanNode, 0, len(nodes))
	for _, node := range nodes {
		converted = append(converted, store.PlanNode{
			ID: node.ID, Kind: node.Kind, Title: node.Title, Body: node.Body, AcceptanceCriteria: node.AcceptanceCriteria,
			ParentID: node.ParentID, DependsOn: node.DependsOn, BriefRefs: node.BriefRefs,
		})
	}
	return converted
}

// PlanNodesFromStore converts stored plan nodes into the browser shape.
func PlanNodesFromStore(nodes []store.PlanNode) []PlanNodeJSON {
	converted := make([]PlanNodeJSON, 0, len(nodes))
	for _, node := range nodes {
		converted = append(converted, PlanNodeJSON{
			ID: node.ID, Kind: node.Kind, Title: node.Title, Body: node.Body, AcceptanceCriteria: node.AcceptanceCriteria,
			ParentID: node.ParentID, DependsOn: node.DependsOn, BriefRefs: node.BriefRefs,
		})
	}
	return converted
}

// decodeStrict decodes exactly one JSON value into destination and rejects
// unknown fields and trailing data.
func decodeStrict(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}

// safeIdentifier reports whether value is 1 to 128 letters, digits, '.', '_',
// or '-', as the store requires for ideation identities.
func safeIdentifier(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// boundedText checks an optional or required text against a byte bound.
func boundedText(label, value string, maxBytes int, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", label)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%s is %d bytes; the limit is %d", label, len(value), maxBytes)
	}
	if strings.ContainsRune(value, 0) {
		return fmt.Errorf("%s contains a NUL byte", label)
	}
	return nil
}

// positive checks a 1-based revision.
func positive(label string, value int64) error {
	if value < 1 {
		return fmt.Errorf("%s must be at least 1", label)
	}
	return nil
}

// sha256Hex reports whether value is a lowercase hexadecimal SHA-256 digest.
func sha256Hex(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// isControl reports a control character.
func isControl(r rune) bool { return r < 0x20 || r == 0x7f }
