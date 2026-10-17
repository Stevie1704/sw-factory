package webui_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// conversationSeed writes session-1 conversation state as the coordinator
// and its session roles would. It is a deterministic role fixture: the
// tests check structure, never model wording.
type conversationSeed struct {
	t       *testing.T
	store   *store.Store
	session store.IdeationSession
	at      time.Time
}

// seedConversation opens the fixture store for seeding session-1.
func seedConversation(t *testing.T, ui sessionUI) *conversationSeed {
	t.Helper()
	opened, err := store.Open(t.Context(), ui.storePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	session, err := opened.IdeationSession(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	return &conversationSeed{t: t, store: opened, session: session, at: fixtureNow.Add(-time.Hour)}
}

// write returns the guard of the next write that leaves the session in state.
func (s *conversationSeed) write(state store.IdeationState, reason string) store.IdeationWrite {
	s.at = s.at.Add(time.Minute)
	return store.IdeationWrite{SessionID: s.session.ID, ExpectedRevision: s.session.Revision, State: state, StateReason: reason, At: s.at}
}

// userTurn appends one user turn, optionally correcting an earlier one.
func (s *conversationSeed) userTurn(id, body, corrects string) {
	s.t.Helper()
	session, _, err := s.store.AppendIdeationTurn(s.t.Context(), s.write(store.IdeationStateAwaitingUser, ""), store.IdeationTurn{
		ID: id, Author: store.IdeationTurnAuthorUser, Body: body, Corrects: corrects,
	})
	if err != nil {
		s.t.Fatal(err)
	}
	s.session = session
}

// startRole starts an ideation invocation for the user turn trigger.
func (s *conversationSeed) startRole(id, trigger string, briefRevision int64) {
	s.t.Helper()
	session, _, err := s.store.StartSessionInvocation(s.t.Context(), s.write(store.IdeationStateRoleActive, ""), store.SessionInvocation{
		ID: id, Role: "ideation", TriggerID: trigger, BriefRevision: briefRevision, ContextRevision: baseSHA, Harness: "codex",
		PromptVersion: "ideation-v2", PromptSHA256: strings.Repeat("a", 64), WorkerID: "session-session-1-ideation",
	})
	if err != nil {
		s.t.Fatal(err)
	}
	s.session = session
}

// answer starts an ideation invocation for trigger and accepts handoff as
// its completed result.
func (s *conversationSeed) answer(id, trigger string, handoff report.IdeationHandoff) {
	s.t.Helper()
	s.startRole(id, trigger, s.session.BriefRevision)
	payload, err := json.Marshal(report.Report{
		SchemaVersion: report.SchemaVersion, InvocationID: id, SessionID: s.session.ID, SessionRevision: s.session.Revision,
		Harness: "codex", Role: "ideation", Stage: "ideation", Outcome: report.OutcomeCompleted, Summary: "turn done",
		Ideation: &handoff, ReportedAt: s.at,
	})
	if err != nil {
		s.t.Fatal(err)
	}
	result := store.SessionRoleResult{
		InvocationID: id, Payload: payload,
		Turn: &store.IdeationTurn{ID: "role-" + id, Author: store.IdeationTurnAuthorRole, Role: "ideation", InvocationID: id, Body: handoff.Message},
	}
	if handoff.Brief != nil {
		brief := sessioncommand.BriefJSON{
			Problem: handoff.Brief.Problem, Decisions: decisionsJSON(handoff.Brief.Decisions),
			Evidence: itemsJSON(handoff.Brief.Evidence), Assumptions: itemsJSON(handoff.Brief.Assumptions),
			NonGoals: itemsJSON(handoff.Brief.NonGoals), UnresolvedQuestions: itemsJSON(handoff.Brief.UnresolvedQuestions),
		}.Brief()
		result.Brief = &brief
	}
	session, _, err := s.store.AcceptSessionRoleResult(s.t.Context(), s.write(store.IdeationStateAwaitingUser, ""), result)
	if err != nil {
		s.t.Fatal(err)
	}
	s.session = session
}

// fail starts an ideation invocation for trigger and releases it with a
// typed failure, as the coordinator does for a refused credential.
func (s *conversationSeed) fail(id, trigger, kind, reason string) {
	s.t.Helper()
	s.startRole(id, trigger, s.session.BriefRevision)
	session, _, err := s.store.ReleaseSessionInvocation(s.t.Context(), s.write(store.IdeationStateAwaitingUser, reason), store.SessionInvocationEnd{
		InvocationID: id, Status: store.SessionInvocationFailed, FailureKind: kind, Reason: reason, At: s.at,
	})
	if err != nil {
		s.t.Fatal(err)
	}
	s.session = session
}

// transition moves the session to state, as a lifecycle command does.
func (s *conversationSeed) transition(state store.IdeationState, reason string) {
	s.t.Helper()
	session, err := s.store.TransitionIdeationSession(s.t.Context(), s.write(state, reason))
	if err != nil {
		s.t.Fatal(err)
	}
	s.session = session
}

// itemsJSON converts report brief items for the seed.
func itemsJSON(items []report.BriefItem) []sessioncommand.BriefItemJSON {
	converted := []sessioncommand.BriefItemJSON{}
	for _, item := range items {
		converted = append(converted, sessioncommand.BriefItemJSON{ID: item.ID, Text: item.Text})
	}
	return converted
}

// decisionsJSON converts report brief decisions for the seed.
func decisionsJSON(decisions []report.BriefDecision) []sessioncommand.BriefDecisionJSON {
	converted := []sessioncommand.BriefDecisionJSON{}
	for _, decision := range decisions {
		converted = append(converted, sessioncommand.BriefDecisionJSON{ID: decision.ID, Decision: decision.Decision, Rationale: decision.Rationale})
	}
	return converted
}

// groundedFirstRound is the deterministic first ideation answer: questions
// grounded in repository evidence, a decision, an unresolved question, and
// a readiness proposal.
func groundedFirstRound() report.IdeationHandoff {
	return report.IdeationHandoff{
		Message: "The worker image is pulled on every first start (worker/Dockerfile).",
		Questions: []report.Question{
			{ID: "q-audience", Prompt: "Who waits for the image? Recommended: new developers, because they start most workers."},
			{ID: "q-budget", Prompt: "What start time is acceptable? Recommended: under one minute."},
		},
		Brief: &report.Brief{
			Problem:             "Workers start slowly because the image is pulled on first start.",
			Decisions:           []report.BriefDecision{{ID: "d-cache", Decision: "Cache the worker image locally.", Rationale: "Pulls dominate the start time."}},
			Evidence:            []report.BriefItem{{ID: "ev-dockerfile", Text: "worker/Dockerfile pulls the base image at start."}},
			Assumptions:         []report.BriefItem{{ID: "as-disk", Text: "Hosts have 5 GB free disk."}},
			NonGoals:            []report.BriefItem{{ID: "ng-registry", Text: "Running a private registry."}},
			UnresolvedQuestions: []report.BriefItem{{ID: "uq-registry", Text: "Which registry hosts the base image?"}},
		},
		Readiness: &report.ReadinessProposal{Proposal: report.ReadinessInvestigationNeeded, Rationale: "The registry is unknown."},
	}
}

// sessionPage reads the detail page of session-1 and fails on a non-200
// status.
func sessionPage(t *testing.T, ui sessionUI) string {
	t.Helper()
	response := get(t, ui.handler, "/sessions/session-1")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /sessions/session-1 status = %d; body:\n%s", response.Code, response.Body)
	}
	return response.Body.String()
}

// assertLacks fails when body contains any unwanted text.
func assertLacks(t *testing.T, body string, unwanted ...string) {
	t.Helper()
	for _, text := range unwanted {
		if strings.Contains(body, text) {
			t.Fatalf("page contains %q:\n%s", text, body)
		}
	}
}

// TestSessionListShowsSessionsAndStartsANewOne verifies that the session
// list names every session with its state and offers a keyboard-operable
// form that starts a session from an idea.
func TestSessionListShowsSessionsAndStartsANewOne(t *testing.T) {
	t.Parallel()

	ui := newSessionUI(t, true)
	response := get(t, ui.handler, "/sessions")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /sessions status = %d; body:\n%s", response.Code, response.Body)
	}
	body := response.Body.String()
	assertContains(t, body, `href="/sessions/session-1"`, "Faster onboarding", "awaiting_user",
		`id="new-session"`, `<label for="new-session-idea"`, `name="idea"`, `<button type="submit"`, `src="/assets/sessions.js"`)
	assertNoExternalResources(t, body)
	if page := get(t, ui.handler, "/").Body.String(); !strings.Contains(page, `href="/sessions"`) {
		t.Fatalf("run list does not link the session list:\n%s", page)
	}
	if code := get(t, newUIFixture(t).handler, "/sessions").Code; code != http.StatusNotFound {
		t.Fatalf("GET /sessions without the session API status = %d, want 404", code)
	}
	if code := get(t, ui.handler, "/sessions/session-missing").Code; code != http.StatusNotFound {
		t.Fatalf("GET unknown session status = %d, want 404", code)
	}
}

// TestSessionPageShowsGroundedQuestionsBriefAndDecisions verifies the
// grounded-questions scenario: the page shows the conversation in rounds,
// the pending questions with an answer field each, the brief with
// decisions, rationale, non-goals, evidence, and unresolved questions apart
// from the conversation, and the readiness proposal bound to its brief
// revision with the user's decision forms.
func TestSessionPageShowsGroundedQuestionsBriefAndDecisions(t *testing.T) {
	t.Parallel()

	ui := newSessionUI(t, true)
	seed := seedConversation(t, ui)
	seed.userTurn("turn-1", "Workers start too slowly.", "")
	seed.answer("sinv-1", "turn-1", groundedFirstRound())
	revision := seed.session.Revision

	body := sessionPage(t, ui)
	assertContains(t, body,
		"Round 1", "Workers start too slowly.", "The worker image is pulled on every first start (worker/Dockerfile).",
		`data-question-id="q-audience"`, "Who waits for the image? Recommended: new developers",
		`data-question-id="q-budget"`, `data-session-command="submit_turn"`,
		`id="brief"`, "Brief revision 1", "Cache the worker image locally.", "Pulls dominate the start time.",
		"Running a private registry.", "worker/Dockerfile pulls the base image at start.", "Hosts have 5 GB free disk.",
		"Which registry hosts the base image?",
		`data-readiness="investigation_needed"`, "proposed for brief revision 1", "The registry is unknown.",
		`data-session-command="hand_off"`, `name="deferred_questions" value="uq-registry"`,
		`data-session-command="investigation_needed"`, `data-session-command="defer"`, `data-session-command="no_build"`,
		`data-brief-editor`,
	)
	if !strings.Contains(body, `data-expected-revision="`+strconv.FormatInt(revision, 10)+`"`) {
		t.Fatalf("forms do not carry session revision %d:\n%s", revision, body)
	}
	assertLacks(t, body, `data-session-command="resume"`)
	assertNoExternalResources(t, body)
}

// TestSessionPageMarksAStaleReadinessProposal verifies that a readiness
// proposal for an older brief revision is not shown as current, because
// the user decides for the displayed brief revision.
func TestSessionPageMarksAStaleReadinessProposal(t *testing.T) {
	t.Parallel()

	ui := newSessionUI(t, true)
	seed := seedConversation(t, ui)
	seed.userTurn("turn-1", "Workers start too slowly.", "")
	seed.answer("sinv-1", "turn-1", groundedFirstRound())
	brief := sessioncommand.BriefJSON{Problem: "Edited by the user."}.Brief()
	session, _, err := seed.store.SaveBriefRevision(t.Context(), seed.write(store.IdeationStateAwaitingUser, ""), brief)
	if err != nil {
		t.Fatal(err)
	}
	seed.session = session

	body := sessionPage(t, ui)
	assertContains(t, body, "proposed for brief revision 1", "the brief is now revision 2", "Edited by the user.")
	assertLacks(t, body, `name="deferred_questions"`)
}

// TestSessionPageShowsDecisionsAffectedByAChangedAnswer verifies the
// changed-answer scenario: the correction names the answer it changes, the
// decisions that the role named are shown as revised or withdrawn against
// the earlier brief, and a correction that names no decision is visible.
func TestSessionPageShowsDecisionsAffectedByAChangedAnswer(t *testing.T) {
	t.Parallel()

	ui := newSessionUI(t, true)
	seed := seedConversation(t, ui)
	seed.userTurn("turn-1", "Workers start too slowly.", "")
	first := groundedFirstRound()
	first.Brief.Decisions = append(first.Brief.Decisions, report.BriefDecision{ID: "d-prefetch", Decision: "Prefetch at login.", Rationale: "Developers log in daily."})
	seed.answer("sinv-1", "turn-1", first)
	seed.userTurn("turn-2", "New developers wait for the image.", "")
	seed.answer("sinv-2", "turn-2", report.IdeationHandoff{Message: "Noted.", Brief: first.Brief})
	seed.userTurn("turn-3", "Actually CI runners wait, not developers.", "turn-2")
	revised := *first.Brief
	revised.Decisions = []report.BriefDecision{{ID: "d-cache", Decision: "Cache the worker image on CI runners.", Rationale: "CI runners start most workers."}}
	seed.answer("sinv-3", "turn-3", report.IdeationHandoff{Message: "CI changes the plan.", Brief: &revised, AffectedDecisions: []string{"d-cache", "d-prefetch"}})
	seed.userTurn("turn-4", "And the budget is two minutes.", "turn-2")
	seed.answer("sinv-4", "turn-4", report.IdeationHandoff{Message: "That changes nothing."})

	body := sessionPage(t, ui)
	assertContains(t, body,
		`data-corrects="turn-2"`, "Actually CI runners wait, not developers.",
		`data-affected-decision="d-cache"`, "revised", "Cache the worker image on CI runners.",
		`data-affected-decision="d-prefetch"`, "withdrawn",
		"named no affected decision",
		`data-session-command="submit_turn"`, `name="corrects"`, `<option value="turn-1"`, `<option value="turn-2"`,
	)
}

// TestSessionPageRequiresExplicitDeferralOfUnknowns verifies the
// unresolved-requirement scenario: every unresolved question is shown in
// the handoff as a required deferral, and the investigation-needed outcome
// lists the unresolved questions as its investigation items without a
// backlog item.
func TestSessionPageRequiresExplicitDeferralOfUnknowns(t *testing.T) {
	t.Parallel()

	ui := newSessionUI(t, true)
	seed := seedConversation(t, ui)
	seed.userTurn("turn-1", "Workers start too slowly.", "")
	seed.answer("sinv-1", "turn-1", groundedFirstRound())

	body := sessionPage(t, ui)
	assertContains(t, body, `name="deferred_questions" value="uq-registry" data-list required`)

	seed.transition(store.IdeationStateInvestigationNeeded, "Registry unknown.")
	body = sessionPage(t, ui)
	assertContains(t, body, `data-state="investigation_needed"`, "Registry unknown.", "Investigation items",
		"Which registry hosts the base image?", `data-session-command="resume"`, "No backlog item")
	assertLacks(t, body, `data-session-command="submit_turn"`, `data-session-command="hand_off"`)
}

// TestSessionPageShowsTheNoBuildOutcome verifies the no-build scenario: the
// role's proposal is shown, and after the user's decision the session is
// closed with its reason and offers no further command.
func TestSessionPageShowsTheNoBuildOutcome(t *testing.T) {
	t.Parallel()

	ui := newSessionUI(t, true)
	seed := seedConversation(t, ui)
	seed.userTurn("turn-1", "Rewrite the CLI in another language.", "")
	seed.answer("sinv-1", "turn-1", report.IdeationHandoff{
		Message:   "The CLI meets every stated need.",
		Brief:     &report.Brief{Problem: "No user problem was named."},
		Readiness: &report.ReadinessProposal{Proposal: report.ReadinessNoBuild, Rationale: "No user benefit."},
	})
	assertContains(t, sessionPage(t, ui), `data-readiness="no_build"`, "No user benefit.", `data-session-command="no_build"`)

	seed.transition(store.IdeationStateNoBuild, "Not worth building.")
	body := sessionPage(t, ui)
	assertContains(t, body, `data-state="no_build"`, "Not worth building.", "Rewrite the CLI in another language.")
	assertLacks(t, body, `data-session-command=`)
	if list := get(t, ui.handler, "/sessions").Body.String(); !strings.Contains(list, "no_build") {
		t.Fatalf("session list does not show the no_build outcome:\n%s", list)
	}
}

// TestSessionPageShowsProgressAndReadableFailures verifies that an active
// role, a failed role with its typed cause and next step, and a stopped
// coordinator are shown as readable states.
func TestSessionPageShowsProgressAndReadableFailures(t *testing.T) {
	t.Parallel()

	ui := newSessionUI(t, true)
	seed := seedConversation(t, ui)
	seed.userTurn("turn-1", "Workers start too slowly.", "")
	seed.startRole("sinv-1", "turn-1", 0)
	body := sessionPage(t, ui)
	assertContains(t, body, `role="status"`, "The ideation role is working")
	assertLacks(t, body, `data-session-command="submit_turn"`)

	ui = newSessionUI(t, true)
	seed = seedConversation(t, ui)
	seed.userTurn("turn-1", "Workers start too slowly.", "")
	seed.fail("sinv-1", "turn-1", "authentication_expired", "ideation role could not start: credential refused")
	body = sessionPage(t, ui)
	assertContains(t, body, `role="alert"`, "authentication_expired", "credential refused", "factory auth", `data-session-command="submit_turn"`)

	stopped := newSessionUI(t, false)
	assertContains(t, get(t, stopped.handler, "/sessions/session-1").Body.String(), "factory start")
}

// TestSessionPageAfterReloadShowsAcceptedAnswersWithoutResubmission verifies
// that a reload while an answer waits in the inbox shows the accepted
// answer and offers no second answer form, so a reload never duplicates a
// turn.
func TestSessionPageAfterReloadShowsAcceptedAnswersWithoutResubmission(t *testing.T) {
	t.Parallel()

	ui := newSessionUI(t, true)
	if response := ui.post(t, commandBody("request-1", 1), nil); response.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d; body:\n%s", response.Code, response.Body)
	}
	body := sessionPage(t, ui)
	assertContains(t, body, "Operators wait for images.", "waits for the coordinator")
	assertLacks(t, body, `data-session-command="submit_turn"`)
}

// TestSessionPageLinksThePlanReviewOnceAPlanExists verifies that the
// session page links the plan review page only when the session has a plan
// revision.
func TestSessionPageLinksThePlanReviewOnceAPlanExists(t *testing.T) {
	t.Parallel()

	ui := newSessionUI(t, true)
	seed := seedConversation(t, ui)
	seed.userTurn("turn-1", "Workers start too slowly.", "")
	seed.answer("sinv-1", "turn-1", groundedFirstRound())
	assertLacks(t, sessionPage(t, ui), `href="/sessions/session-1/plan"`)

	seed.startRole("sinv-2", "handoff-1", seed.session.BriefRevision)
	session, _, err := seed.store.SavePlanRevision(t.Context(), seed.write(store.IdeationStateDraftReview, ""), store.PlanRevision{
		SourceBriefRevision: seed.session.BriefRevision, ContextRevision: baseSHA,
		Nodes: []store.PlanNode{{ID: "n-1", Kind: "task", Title: "Cache images", Body: "Cache.", AcceptanceCriteria: []string{"Cached."}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	seed.session = session
	assertContains(t, sessionPage(t, ui), `href="/sessions/session-1/plan"`, "Plan revision 1")
}
