package webui

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// sessionListPage is the model of the session list.
type sessionListPage struct {
	// Supervisor is the coordinator liveness at the read.
	Supervisor *factory.SupervisorView
	// Sessions holds every session of the repository, newest update first.
	Sessions []store.IdeationSession
}

// sessionPage is the model of one session: the conversation in rounds, the
// pending questions, the brief, the role's readiness proposal, and the
// commands the user can send in the current state.
type sessionPage struct {
	// Supervisor is the coordinator liveness at the read.
	Supervisor *factory.SupervisorView
	// Session is the current session projection.
	Session store.IdeationSession
	// Rounds is the conversation; a round starts with the user's turns and
	// ends with the role's answer.
	Rounds []conversationRound
	// Pending holds the questions of the newest role turn while the user has
	// not answered them.
	Pending []report.Question
	// Brief is the current brief revision, or nil.
	Brief *store.BriefRevision
	// Readiness is the newest readiness proposal of the ideation role, or nil.
	Readiness *readinessView
	// Progress names the active role invocation, or is nil.
	Progress *roleProgress
	// Failure explains the newest role invocation when it ended without a
	// result, or is nil.
	Failure *roleFailure
	// PendingCommands holds the commands that wait for the coordinator.
	PendingCommands []pendingCommand
	// Outcomes holds the newest completed commands that were not applied.
	Outcomes []store.IdeationCommand
	// Answers lists the earlier user answers that a correction can name.
	Answers []answerOption
	// Actions are the commands the page offers in the current state.
	Actions sessionActions
}

// conversationRound is one question round of the conversation.
type conversationRound struct {
	// Number is the 1-based round number.
	Number int
	// Turns holds the user turns and the role answer of the round.
	Turns []turnView
}

// turnView is one turn with the structured role payload that belongs to it.
type turnView struct {
	store.IdeationTurn
	// Questions are the questions of a role turn.
	Questions []report.Question
	// Readiness is the readiness proposal of a role turn, or nil.
	Readiness *report.ReadinessProposal
	// Correction lists the decisions that a role turn revisited for a
	// corrected answer, or is nil.
	Correction *correctionView
}

// correctionView is what a role answer did with the decisions that a
// corrected answer affects.
type correctionView struct {
	// Corrects is the user turn whose answer was changed.
	Corrects string
	// Affected holds each decision the role named, with its result.
	Affected []affectedDecision
}

// affectedDecision is one decision that a changed answer affects.
type affectedDecision struct {
	// ID is the brief decision identity.
	ID string
	// Result is revised, withdrawn, unchanged, or added, from the brief
	// before and after the role answer.
	Result string
	// Decision is the decision text after the answer, or before it when the
	// decision was withdrawn.
	Decision string
}

// readinessView is a readiness proposal bound to the brief revision it was
// made for.
type readinessView struct {
	report.ReadinessProposal
	// BriefRevision is the brief revision the proposal was made for.
	BriefRevision int64
	// CurrentBriefRevision is the brief revision the page shows.
	CurrentBriefRevision int64
}

// Current reports whether the proposal was made for the displayed brief.
func (r readinessView) Current() bool { return r.BriefRevision == r.CurrentBriefRevision }

// roleProgress is an active role invocation.
type roleProgress struct {
	// Role is the session role.
	Role string
	// Since is when the invocation started.
	Since time.Time
}

// roleFailure is a role invocation that ended without a result.
type roleFailure struct {
	// Role is the session role.
	Role string
	// Kind is the typed failure cause.
	Kind string
	// Reason explains the failure.
	Reason string
	// Advice tells the user what to do next.
	Advice string
	// At is when the invocation ended.
	At time.Time
}

// pendingCommand is one command that waits for the coordinator.
type pendingCommand struct {
	store.IdeationCommand
	// Body is the answer text of a pending submit_turn command.
	Body string
}

// answerOption is one earlier user answer that a correction can name.
type answerOption struct {
	// ID is the user turn identity.
	ID string
	// Round is the round of the answer.
	Round int
	// Excerpt is the start of the answer.
	Excerpt string
}

// sessionActions are the commands the page offers.
type sessionActions struct {
	// Answer offers the answer and correction forms.
	Answer bool
	// EditBrief offers the brief editor.
	EditBrief bool
	// HandOff offers the handoff of the displayed brief revision.
	HandOff bool
	// Investigate offers the investigation-needed outcome.
	Investigate bool
	// Defer, NoBuild, Resume, and Cancel offer the lifecycle commands.
	Defer, NoBuild, Resume, Cancel bool
}

// Any reports whether the page offers a command.
func (a sessionActions) Any() bool {
	return a.Answer || a.EditBrief || a.HandOff || a.Investigate || a.Defer || a.NoBuild || a.Resume || a.Cancel
}

// failureAdvice is the next step for each typed role failure.
var failureAdvice = map[string]string{
	factory.SessionFailureAuthenticationExpired: "The harness credential was refused. Refresh it with factory auth, then send your answer again.",
	factory.SessionFailureRateLimited:           "The harness reported a capacity limit. Wait, then send your answer again.",
	factory.SessionFailureCredentials:           "The coordinator could not give the harness credential to the worker. Run factory doctor, then send your answer again.",
	factory.SessionFailureReportRejected:        "The role's report was not valid. Send your answer again to start a new turn.",
	factory.SessionFailurePlanRejected:          "Plan validation refused the draft. Edit the brief or hand it off again.",
	factory.SessionFailureCancelled:             "The role was cancelled. Send your answer again to start a new turn.",
}

// sessionList renders every session of the repository.
func (ui *server) sessionList(w http.ResponseWriter, r *http.Request) {
	overview, err := ui.options.Sessions.SessionOverview(r.Context())
	if err != nil {
		ui.renderStoreError(w, err)
		return
	}
	ui.render(w, http.StatusOK, pageSessions, sessionListPage{Supervisor: &overview.Supervisor, Sessions: overview.Sessions})
}

// sessionDetail renders one session, or a not-found page.
func (ui *server) sessionDetail(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if !validRunID(sessionID) {
		ui.render(w, http.StatusBadRequest, pageError, errorPage{Title: "Invalid session", Message: "The session identity is empty or contains control characters."})
		return
	}
	activity, err := ui.options.Sessions.SessionActivity(r.Context(), sessionID)
	if errors.Is(err, store.ErrIdeationSessionNotFound) {
		ui.render(w, http.StatusNotFound, pageError, errorPage{Title: "Session not found", Message: "No ideation session has the identity " + sessionID + "."})
		return
	}
	if err != nil {
		ui.renderStoreError(w, err)
		return
	}
	ui.render(w, http.StatusOK, pageSession, newSessionPage(activity))
}

// newSessionPage builds the session page model from the durable activity.
func newSessionPage(activity factory.SessionActivity) sessionPage {
	history := activity.History
	page := sessionPage{Supervisor: &activity.Supervisor, Session: history.Session}
	if count := len(history.Briefs); count > 0 {
		page.Brief = &history.Briefs[count-1]
	}
	invocations := map[string]store.SessionInvocation{}
	for _, invocation := range activity.Invocations {
		invocations[invocation.ID] = invocation
	}
	page.Rounds, page.Answers = conversationRounds(history, invocations)
	page.Pending = pendingQuestions(page.Rounds)
	page.Readiness = newestReadiness(activity.Invocations, history.Session.BriefRevision)
	page.Progress, page.Failure = roleState(activity.Invocations, history.Session)
	for _, command := range activity.Commands {
		switch {
		case command.State != store.IdeationCommandCompleted:
			page.PendingCommands = append(page.PendingCommands, newPendingCommand(command))
		case command.Outcome != store.IdeationCommandApplied && len(page.Outcomes) < 3:
			page.Outcomes = append(page.Outcomes, command)
		}
	}
	page.Actions = actionsFor(page)
	return page
}

// conversationRounds groups the turns into rounds and decodes the role
// payload of each role turn. It also returns the user answers a correction
// can name.
func conversationRounds(history store.IdeationHistory, invocations map[string]store.SessionInvocation) ([]conversationRound, []answerOption) {
	var rounds []conversationRound
	var answers []answerOption
	turns := map[string]store.IdeationTurn{}
	for _, turn := range history.Turns {
		turns[turn.ID] = turn
		view := turnView{IdeationTurn: turn}
		if turn.Author == store.IdeationTurnAuthorUser {
			if count := len(rounds); count == 0 || rounds[count-1].Turns[len(rounds[count-1].Turns)-1].Author == store.IdeationTurnAuthorRole {
				rounds = append(rounds, conversationRound{Number: count + 1})
			}
			answers = append(answers, answerOption{ID: turn.ID, Round: len(rounds), Excerpt: excerpt(turn.Body)})
		} else if len(rounds) == 0 {
			rounds = append(rounds, conversationRound{Number: 1})
		}
		if invocation, ok := invocations[turn.InvocationID]; ok && turn.Author == store.IdeationTurnAuthorRole {
			addRolePayload(&view, invocation, turns[invocation.TriggerID], history.Briefs)
		}
		last := &rounds[len(rounds)-1]
		last.Turns = append(last.Turns, view)
	}
	return rounds, answers
}

// addRolePayload adds the questions, readiness, and correction result of an
// accepted ideation invocation to its role turn.
func addRolePayload(view *turnView, invocation store.SessionInvocation, trigger store.IdeationTurn, briefs []store.BriefRevision) {
	handoff := ideationHandoff(invocation)
	if handoff == nil {
		return
	}
	view.Questions, view.Readiness = handoff.Questions, handoff.Readiness
	if trigger.Corrects == "" {
		return
	}
	before := briefAt(briefs, invocation.BriefRevision)
	after := before
	if handoff.Brief != nil {
		after = briefAt(briefs, invocation.BriefRevision+1)
	}
	correction := &correctionView{Corrects: trigger.Corrects}
	for _, id := range handoff.AffectedDecisions {
		correction.Affected = append(correction.Affected, compareDecision(id, before, after))
	}
	view.Correction = correction
}

// ideationHandoff decodes the ideation payload of an accepted invocation, or
// returns nil.
func ideationHandoff(invocation store.SessionInvocation) *report.IdeationHandoff {
	if invocation.Status != store.SessionInvocationAccepted || invocation.Role != workflow.RoleIdeation || len(invocation.Payload) == 0 {
		return nil
	}
	var value report.Report
	if err := json.Unmarshal(invocation.Payload, &value); err != nil {
		return nil
	}
	return value.Ideation
}

// briefAt returns the brief of revision, or an empty brief.
func briefAt(briefs []store.BriefRevision, revision int64) store.Brief {
	for _, brief := range briefs {
		if brief.Revision == revision {
			return brief.Brief
		}
	}
	return store.Brief{}
}

// compareDecision reports what happened to decision id between two briefs.
func compareDecision(id string, before, after store.Brief) affectedDecision {
	find := func(brief store.Brief) *store.BriefDecision {
		for index := range brief.Decisions {
			if brief.Decisions[index].ID == id {
				return &brief.Decisions[index]
			}
		}
		return nil
	}
	old, current := find(before), find(after)
	switch {
	case current == nil && old == nil:
		return affectedDecision{ID: id, Result: "withdrawn"}
	case current == nil:
		return affectedDecision{ID: id, Result: "withdrawn", Decision: old.Decision}
	case old == nil:
		return affectedDecision{ID: id, Result: "added", Decision: current.Decision}
	case *old != *current:
		return affectedDecision{ID: id, Result: "revised", Decision: current.Decision}
	default:
		return affectedDecision{ID: id, Result: "unchanged", Decision: current.Decision}
	}
}

// pendingQuestions returns the questions of the newest turn when it is a
// role turn, because the user has not answered them yet.
func pendingQuestions(rounds []conversationRound) []report.Question {
	if len(rounds) == 0 {
		return nil
	}
	turns := rounds[len(rounds)-1].Turns
	last := turns[len(turns)-1]
	if last.Author != store.IdeationTurnAuthorRole {
		return nil
	}
	return last.Questions
}

// newestReadiness returns the newest readiness proposal of an accepted
// ideation invocation, bound to the brief revision it was made for: the
// brief it proposed, or the brief it received when it proposed none.
func newestReadiness(invocations []store.SessionInvocation, currentBrief int64) *readinessView {
	for index := len(invocations) - 1; index >= 0; index-- {
		invocation := invocations[index]
		handoff := ideationHandoff(invocation)
		if handoff == nil {
			continue
		}
		if handoff.Readiness == nil {
			return nil
		}
		revision := invocation.BriefRevision
		if handoff.Brief != nil {
			revision++
		}
		return &readinessView{ReadinessProposal: *handoff.Readiness, BriefRevision: revision, CurrentBriefRevision: currentBrief}
	}
	return nil
}

// roleState returns the active role invocation, or the newest invocation
// when it ended without a result while the session waits for the user.
func roleState(invocations []store.SessionInvocation, session store.IdeationSession) (*roleProgress, *roleFailure) {
	if len(invocations) == 0 {
		return nil, nil
	}
	newest := invocations[len(invocations)-1]
	switch {
	case newest.Status == store.SessionInvocationActive:
		return &roleProgress{Role: newest.Role, Since: newest.CreatedAt}, nil
	case newest.Status == store.SessionInvocationAccepted || store.IsTerminalIdeationState(session.State):
		return nil, nil
	case newest.FailureKind == factory.SessionFailureSessionMoved:
		return nil, nil
	}
	advice, ok := failureAdvice[newest.FailureKind]
	if !ok {
		advice = "Send your answer again to retry. Run factory doctor when the failure repeats."
	}
	return nil, &roleFailure{Role: newest.Role, Kind: newest.FailureKind, Reason: newest.StatusReason, Advice: advice, At: newest.UpdatedAt}
}

// newPendingCommand adds the answer text of a pending submit_turn command.
func newPendingCommand(command store.IdeationCommand) pendingCommand {
	pending := pendingCommand{IdeationCommand: command}
	if sessioncommand.Kind(command.Kind) == sessioncommand.KindSubmitTurn {
		var payload sessioncommand.TurnPayload
		if json.Unmarshal(command.Payload, &payload) == nil {
			pending.Body = payload.Body
		}
	}
	return pending
}

// actionsFor decides which commands the page offers. A command that waits
// for the coordinator hides every form, so a reload cannot send the same
// decision twice. The lifecycle decides the rest.
func actionsFor(page sessionPage) sessionActions {
	if len(page.PendingCommands) > 0 || page.Progress != nil {
		return sessionActions{}
	}
	lifecycle := workflow.DefaultSessionLifecycle()
	allows := func(event workflow.SessionEvent) bool {
		_, err := lifecycle.Next(page.Session.State, event)
		return err == nil
	}
	hasUnknowns := page.Brief != nil && len(page.Brief.Brief.UnresolvedQuestions) > 0
	return sessionActions{
		Answer:      allows(workflow.SessionEventTurnAccepted),
		EditBrief:   allows(workflow.SessionEventEdited) && page.Session.State == store.IdeationStateAwaitingUser,
		HandOff:     allows(workflow.SessionEventRoleStarted) && page.Brief != nil && page.Session.State == store.IdeationStateAwaitingUser,
		Investigate: allows(workflow.SessionEventInvestigationNeeded) && hasUnknowns,
		Defer:       allows(workflow.SessionEventDeferred),
		NoBuild:     allows(workflow.SessionEventNoBuild),
		Resume:      allows(workflow.SessionEventResumed),
		Cancel:      allows(workflow.SessionEventCancelled),
	}
}

// excerpt returns the first line of text, shortened for a selection list.
func excerpt(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if runes := []rune(line); len(runes) > 80 {
		return string(runes[:79]) + "…"
	}
	return line
}

// briefListView is one item list of a brief, with the key of its JSON field
// and the prefix that new item IDs of the list get.
type briefListView struct {
	// Key is the JSON field of the list.
	Key string
	// Label is the heading of the list.
	Label string
	// Prefix starts the ID of a new item in the editor.
	Prefix string
	// Items holds the list items.
	Items []store.BriefItem
}

// briefLists returns the item lists of brief in display order. Decisions
// have their own shape and are not in the result.
func briefLists(brief store.Brief) []briefListView {
	return []briefListView{
		{Key: "success_criteria", Label: "Success criteria", Prefix: "sc", Items: brief.SuccessCriteria},
		{Key: "scope", Label: "Scope", Prefix: "scope", Items: brief.Scope},
		{Key: "non_goals", Label: "Non-goals", Prefix: "ng", Items: brief.NonGoals},
		{Key: "evidence", Label: "Evidence", Prefix: "ev", Items: brief.Evidence},
		{Key: "assumptions", Label: "Assumptions", Prefix: "as", Items: brief.Assumptions},
		{Key: "unresolved_questions", Label: "Unresolved questions", Prefix: "uq", Items: brief.UnresolvedQuestions},
	}
}

// editableBrief returns the brief of revision, or an empty brief when the
// session has none yet.
func editableBrief(revision *store.BriefRevision) store.Brief {
	if revision == nil {
		return store.Brief{}
	}
	return revision.Brief
}

// reasonFormView is the model of one lifecycle command form with an
// optional reason.
type reasonFormView struct {
	// Session is the session the command changes.
	Session store.IdeationSession
	// Kind is the command kind.
	Kind string
	// Heading, Explanation, and Button are the visible texts of the form.
	Heading, Explanation, Button string
}

// reasonForm builds a lifecycle command form for the session page.
func reasonForm(page sessionPage, kind, heading, explanation, button string) reasonFormView {
	return reasonFormView{Session: page.Session, Kind: kind, Heading: heading, Explanation: explanation, Button: button}
}
