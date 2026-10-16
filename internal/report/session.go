package report

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// MaxIdeationMessageRunes bounds the conversational text of one ideation
	// report.
	MaxIdeationMessageRunes = 16000
	// MaxPlanDraftNodes is the transport bound on the nodes of one draft plan.
	// Plan validation owns the planning limits; this bound only keeps one
	// report inside MaxReportBytes.
	MaxPlanDraftNodes = 256
	// maxBriefItems bounds each list of a brief update.
	maxBriefItems = 64
	// maxPlanNodeBodyRunes bounds the description of one plan node.
	maxPlanNodeBodyRunes = 8000
	// maxPlanNodeTitleRunes bounds the title of one plan node.
	maxPlanNodeTitleRunes = 256
	// maxPlanNodeReferences bounds each reference list of one plan node.
	maxPlanNodeReferences = 64
)

// Readiness is the closed set of readiness proposals an ideation role may
// make. A proposal never changes the session: the user decides the handoff.
type Readiness string

const (
	// ReadinessReadyForPlanning proposes that the brief is ready for planning.
	ReadinessReadyForPlanning Readiness = "ready_for_planning"
	// ReadinessInvestigationNeeded proposes that material unknowns need
	// investigation before planning.
	ReadinessInvestigationNeeded Readiness = "investigation_needed"
	// ReadinessNoBuild proposes that the idea should not be built.
	ReadinessNoBuild Readiness = "no_build"
)

// ReadinessProposal is the ideation role's proposal for the next session
// step, with its reason.
type ReadinessProposal struct {
	// Proposal is the proposed next step.
	Proposal Readiness `json:"proposal"`
	// Rationale explains the proposal.
	Rationale string `json:"rationale"`
}

// BriefItem is one traceable brief statement with an ID that is unique in the
// brief.
type BriefItem struct {
	// ID is the stable item identity.
	ID string `json:"id"`
	// Text is the statement.
	Text string `json:"text"`
}

// BriefDecision is one resolved decision with its rationale.
type BriefDecision struct {
	// ID is the stable item identity.
	ID string `json:"id"`
	// Decision states what was decided.
	Decision string `json:"decision"`
	// Rationale states why.
	Rationale string `json:"rationale"`
}

// Brief is a complete proposed brief revision. Its fields match the stored
// brief of an ideation session.
type Brief struct {
	// Problem states the problem to solve.
	Problem string `json:"problem"`
	// Audience names who has the problem.
	Audience string `json:"audience,omitempty"`
	// SuccessCriteria lists observable outcomes.
	SuccessCriteria []BriefItem `json:"success_criteria,omitempty"`
	// Scope lists what is in scope.
	Scope []BriefItem `json:"scope,omitempty"`
	// NonGoals lists what is out of scope.
	NonGoals []BriefItem `json:"non_goals,omitempty"`
	// Decisions lists resolved decisions.
	Decisions []BriefDecision `json:"decisions,omitempty"`
	// Assumptions lists statements taken as true without proof.
	Assumptions []BriefItem `json:"assumptions,omitempty"`
	// Evidence lists supporting facts and their sources.
	Evidence []BriefItem `json:"evidence,omitempty"`
	// UnresolvedQuestions lists questions that stay open.
	UnresolvedQuestions []BriefItem `json:"unresolved_questions,omitempty"`
}

// IdeationHandoff is the structured result of one ideation turn.
type IdeationHandoff struct {
	// Message is the role's conversational reply. It may span lines.
	Message string `json:"message"`
	// Questions are the next targeted questions for the user.
	Questions []Question `json:"questions,omitempty"`
	// Brief optionally proposes a complete new brief revision.
	Brief *Brief `json:"brief,omitempty"`
	// Readiness optionally proposes the next session step.
	Readiness *ReadinessProposal `json:"readiness,omitempty"`
	// AffectedDecisions names the brief decisions that a changed user answer
	// affects. The brief of the same report shows each one revised or
	// removed, so no conclusion that contradicts the new answer stays.
	AffectedDecisions []string `json:"affected_decisions,omitempty"`
}

// PlanNode is one provider-neutral work item of a draft plan. Its fields match
// the stored plan node of an ideation session.
type PlanNode struct {
	// ID is the stable local node identity.
	ID string `json:"id"`
	// Kind is the provider-neutral work-item kind.
	Kind string `json:"kind"`
	// Title is the work-item title.
	Title string `json:"title"`
	// Body is the work-item description. It may span lines.
	Body string `json:"body,omitempty"`
	// AcceptanceCriteria lists observable completion checks.
	AcceptanceCriteria []string `json:"acceptance_criteria,omitempty"`
	// ParentID optionally names the parent node.
	ParentID string `json:"parent_id,omitempty"`
	// DependsOn names nodes that must be done first.
	DependsOn []string `json:"depends_on,omitempty"`
	// BriefRefs names the source brief items of the node.
	BriefRefs []string `json:"brief_refs,omitempty"`
}

// PlanDraft is the structured result of one planning invocation. The
// coordinator, not the role, records the source brief and context revisions.
type PlanDraft struct {
	// Nodes is the draft work-item graph.
	Nodes []PlanNode `json:"nodes"`
}

// isSessionReport reports whether the envelope claims session ownership or
// carries a session-only payload.
func isSessionReport(value Report) bool {
	return value.SessionID != "" || value.SessionRevision != 0 || value.Ideation != nil || value.PlanDraft != nil
}

// sessionRoleKind returns the coordinator-resolved session report contract,
// or the role name when a standalone reader has no coordinator context.
func sessionRoleKind(value Report, context ValidationContext) string {
	if context.RoleKind != "" {
		return context.RoleKind
	}
	return value.Role
}

// validateSessionReport applies the identity and payload rules of an ideation
// or planning report. A session report never names a run.
func validateSessionReport(value Report, context ValidationContext) error {
	if context.RunID != "" || value.RunID != "" {
		return fmt.Errorf("session report run_id %q must be empty", value.RunID)
	}
	identityFields := []struct{ field, expected, actual string }{
		{"invocation_id", context.InvocationID, value.InvocationID},
		{"session_id", context.SessionID, value.SessionID},
		{"harness", context.Harness, value.Harness},
		{"role", context.Role, value.Role},
		{"stage", context.Stage, value.Stage},
	}
	for _, identity := range identityFields {
		if strings.TrimSpace(identity.expected) == "" || identity.actual != identity.expected {
			return fmt.Errorf("report %s %q does not match expected %q", identity.field, identity.actual, identity.expected)
		}
	}
	if context.SessionRevision < 1 || value.SessionRevision != context.SessionRevision {
		return fmt.Errorf("report session_revision %d does not match expected %d", value.SessionRevision, context.SessionRevision)
	}
	if !IsSafeIdentifier(value.InvocationID) || !IsSafeIdentifier(value.SessionID) {
		return errors.New("report identifiers contain unsafe characters")
	}
	if err := validateText("report summary", value.Summary, maxSummaryRunes, true); err != nil {
		return err
	}
	if value.ReportedAt.IsZero() {
		return errors.New("report reported_at is required")
	}
	if value.Handoff != nil || value.TestHandoff != nil || value.ReviewHandoff != nil || value.PullRequestSummary != nil || value.TestObjectionResponse != nil {
		return errors.New("session report must not contain a run handoff")
	}
	if err := validateSessionOutcome(value, sessionRoleKind(value, context)); err != nil {
		return err
	}
	if value.NativeSessionID != "" && !IsSafeIdentifier(value.NativeSessionID) {
		return errors.New("native_session_id contains unsafe characters")
	}
	if value.Usage != nil {
		if err := validateUsage(*value.Usage); err != nil {
			return err
		}
	}
	return validateEvaluationSignals(value)
}

// validateSessionOutcome checks that each outcome carries exactly the payload
// of its role kind.
func validateSessionOutcome(value Report, kind string) error {
	if kind != "ideation" && kind != "planning" {
		return fmt.Errorf("report role kind %q is not a session role", kind)
	}
	switch value.Outcome {
	case OutcomeCompleted:
		if len(value.Questions) != 0 || len(value.Evidence) != 0 {
			return errors.New("completed session report must contain only its role payload")
		}
		if kind == "ideation" {
			if value.PlanDraft != nil || value.Ideation == nil {
				return errors.New("completed ideation report requires only an ideation payload")
			}
			return validateIdeationHandoff(*value.Ideation)
		}
		if value.Ideation != nil || value.PlanDraft == nil {
			return errors.New("completed planning report requires only a plan_draft payload")
		}
		return validatePlanDraft(*value.PlanDraft)
	case OutcomeNeedsClarification:
		if kind != "planning" {
			return errors.New("ideation report carries its questions in the ideation payload")
		}
		if len(value.Questions) == 0 || value.PlanDraft != nil || value.Ideation != nil || len(value.Evidence) != 0 {
			return errors.New("needs_clarification planning report requires only questions")
		}
		return validateQuestions(value.Questions)
	case OutcomeCannotProceed:
		if len(value.Evidence) == 0 || value.PlanDraft != nil || value.Ideation != nil || len(value.Questions) != 0 {
			return errors.New("cannot_proceed session report requires only evidence")
		}
		return validateEvidence(value.Evidence)
	default:
		return fmt.Errorf("unsupported report outcome %q", value.Outcome)
	}
}

// validateIdeationHandoff checks the message, questions, brief, and readiness
// proposal of one ideation turn.
func validateIdeationHandoff(value IdeationHandoff) error {
	if err := validateMultilineText("ideation.message", value.Message, MaxIdeationMessageRunes, true); err != nil {
		return err
	}
	if err := validateQuestions(value.Questions); err != nil {
		return err
	}
	if value.Brief != nil {
		if err := validateBrief(*value.Brief); err != nil {
			return err
		}
	}
	if len(value.AffectedDecisions) > maxBriefItems {
		return fmt.Errorf("ideation.affected_decisions exceeds %d entries", maxBriefItems)
	}
	affected := map[string]bool{}
	for _, id := range value.AffectedDecisions {
		if !IsSafeIdentifier(id) || affected[id] {
			return fmt.Errorf("ideation.affected_decisions entry %q must be a unique, safe decision ID", id)
		}
		affected[id] = true
	}
	if value.Readiness != nil {
		switch value.Readiness.Proposal {
		case ReadinessReadyForPlanning, ReadinessInvestigationNeeded, ReadinessNoBuild:
		default:
			return fmt.Errorf("ideation.readiness.proposal %q is not supported", value.Readiness.Proposal)
		}
		if err := validateMultilineText("ideation.readiness.rationale", value.Readiness.Rationale, maxTextRunes, true); err != nil {
			return err
		}
	}
	return nil
}

// validateBrief checks a proposed brief: bounded text and unique, safe item
// IDs across every list.
func validateBrief(value Brief) error {
	if err := validateMultilineText("brief.problem", value.Problem, maxTextRunes, true); err != nil {
		return err
	}
	if err := validateMultilineText("brief.audience", value.Audience, maxTextRunes, false); err != nil {
		return err
	}
	seen := map[string]bool{}
	checkID := func(field, id string) error {
		if !IsSafeIdentifier(id) {
			return fmt.Errorf("%s.id is required and must be safe", field)
		}
		if seen[id] {
			return fmt.Errorf("%s.id %q is duplicated in the brief", field, id)
		}
		seen[id] = true
		return nil
	}
	lists := []struct {
		field string
		items []BriefItem
	}{
		{"brief.success_criteria", value.SuccessCriteria}, {"brief.scope", value.Scope}, {"brief.non_goals", value.NonGoals},
		{"brief.assumptions", value.Assumptions}, {"brief.evidence", value.Evidence}, {"brief.unresolved_questions", value.UnresolvedQuestions},
	}
	for _, list := range lists {
		if len(list.items) > maxBriefItems {
			return fmt.Errorf("%s exceeds %d entries", list.field, maxBriefItems)
		}
		for index, item := range list.items {
			field := fmt.Sprintf("%s[%d]", list.field, index)
			if err := checkID(field, item.ID); err != nil {
				return err
			}
			if err := validateMultilineText(field+".text", item.Text, maxTextRunes, true); err != nil {
				return err
			}
		}
	}
	if len(value.Decisions) > maxBriefItems {
		return fmt.Errorf("brief.decisions exceeds %d entries", maxBriefItems)
	}
	for index, decision := range value.Decisions {
		field := fmt.Sprintf("brief.decisions[%d]", index)
		if err := checkID(field, decision.ID); err != nil {
			return err
		}
		if err := validateMultilineText(field+".decision", decision.Decision, maxTextRunes, true); err != nil {
			return err
		}
		if err := validateMultilineText(field+".rationale", decision.Rationale, maxTextRunes, true); err != nil {
			return err
		}
	}
	return nil
}

// validatePlanDraft checks the transport shape of a draft plan: bounded node
// count, unique safe IDs, required titles, and safe references. Graph rules
// such as cycles and hierarchy shape belong to plan validation.
func validatePlanDraft(value PlanDraft) error {
	if len(value.Nodes) == 0 {
		return errors.New("plan_draft requires at least one node")
	}
	if len(value.Nodes) > MaxPlanDraftNodes {
		return fmt.Errorf("plan_draft has %d nodes; the transport limit is %d", len(value.Nodes), MaxPlanDraftNodes)
	}
	seen := make(map[string]bool, len(value.Nodes))
	for index, node := range value.Nodes {
		field := fmt.Sprintf("plan_draft.nodes[%d]", index)
		if !IsSafeIdentifier(node.ID) || !IsSafeIdentifier(node.Kind) {
			return fmt.Errorf("%s id and kind are required and must be safe", field)
		}
		if seen[node.ID] {
			return fmt.Errorf("%s.id %q is duplicated", field, node.ID)
		}
		seen[node.ID] = true
		if err := validateText(field+".title", node.Title, maxPlanNodeTitleRunes, true); err != nil {
			return err
		}
		if err := validateMultilineText(field+".body", node.Body, maxPlanNodeBodyRunes, false); err != nil {
			return err
		}
		if node.ParentID != "" && !IsSafeIdentifier(node.ParentID) {
			return fmt.Errorf("%s.parent_id must be safe", field)
		}
		for name, references := range map[string][]string{"depends_on": node.DependsOn, "brief_refs": node.BriefRefs} {
			if len(references) > maxPlanNodeReferences {
				return fmt.Errorf("%s.%s exceeds %d entries", field, name, maxPlanNodeReferences)
			}
			for _, reference := range references {
				if !IsSafeIdentifier(reference) {
					return fmt.Errorf("%s.%s contains an unsafe reference", field, name)
				}
			}
		}
		if len(node.AcceptanceCriteria) > maxPlanNodeReferences {
			return fmt.Errorf("%s.acceptance_criteria exceeds %d entries", field, maxPlanNodeReferences)
		}
		for criterion, text := range node.AcceptanceCriteria {
			if err := validateText(fmt.Sprintf("%s.acceptance_criteria[%d]", field, criterion), text, maxTextRunes, true); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateMultilineText enforces bounded, valid UTF-8 text that may span
// lines but contains no NUL byte.
func validateMultilineText(field, value string, maxRunes int, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", field)
	}
	if strings.ContainsRune(value, '\x00') || !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8 without NUL characters", field)
	}
	if utf8.RuneCountInString(value) > maxRunes {
		return fmt.Errorf("%s exceeds %d characters", field, maxRunes)
	}
	return nil
}
