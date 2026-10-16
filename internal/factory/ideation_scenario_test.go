package factory_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	"github.com/Stevie1704/sw-factory/internal/report"
	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// ideationScenario drives one session through browser commands and the
// session roles, with deterministic role reports in place of a model. Its
// checks read structured state only, never model wording.
type ideationScenario struct {
	*sessionFixture
	roles   *factory.SessionRoles
	planned []string
}

// StartIdeation starts the ideation role for the turn, as the coordinator's
// command loop does.
func (s *ideationScenario) StartIdeation(ctx context.Context, sessionID, turnID string) error {
	_, err := s.roles.StartIdeationTurn(ctx, sessionID, turnID)
	return err
}

// StartPlanning records the planning start; the scenarios end at the
// handoff decision.
func (s *ideationScenario) StartPlanning(_ context.Context, _ string, requestID string) error {
	s.planned = append(s.planned, requestID)
	return nil
}

// newIdeationScenario returns a scenario over a fresh session fixture.
func newIdeationScenario(t *testing.T) *ideationScenario {
	t.Helper()
	fixture := newSessionFixture(t, config.HarnessCodex)
	return &ideationScenario{sessionFixture: fixture, roles: fixture.roles()}
}

// command sends one browser command at the current session revision through
// the inbox and the dispatcher, and returns its stored outcome.
func (s *ideationScenario) command(requestID string, kind sessioncommand.Kind, payload string) store.IdeationCommand {
	s.t.Helper()
	body := fmt.Sprintf(`{"request_id":%q,"session_id":"session-1","kind":%q,"expected_revision":%d,"payload":%s}`, requestID, kind, s.session().Revision, payload)
	parsed, err := sessioncommand.ParseRequest([]byte(body))
	if err != nil {
		s.t.Fatalf("ParseRequest(%s) error = %v", body, err)
	}
	if _, _, err := s.store.AcceptIdeationCommand(s.t.Context(), parsed); err != nil {
		s.t.Fatal(err)
	}
	dispatcher := sessioncommand.NewDispatcher(sessioncommand.Env{
		Store: s.store, Lifecycle: workflow.DefaultSessionLifecycle(), Roles: s, RepositoryPath: s.repo,
		Now: func() time.Time { return time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC) },
	})
	if err := dispatcher.Drain(s.t.Context()); err != nil {
		s.t.Fatal(err)
	}
	dispatcher.Wait()
	command, err := s.store.IdeationCommand(s.t.Context(), requestID)
	if err != nil {
		s.t.Fatal(err)
	}
	return command
}

// respond writes handoff as the report of the active ideation invocation and
// lets the coordinator accept it.
func (s *ideationScenario) respond(handoff report.IdeationHandoff) store.SessionInvocation {
	s.t.Helper()
	active, err := s.store.ActiveSessionInvocations(s.t.Context())
	if err != nil || len(active) != 1 {
		s.t.Fatalf("active invocations = %#v, %v; want one ideation invocation", active, err)
	}
	s.report(active[0], report.Report{Outcome: report.OutcomeCompleted, Ideation: &handoff})
	if err := s.roles.Reconcile(s.t.Context()); err != nil {
		s.t.Fatal(err)
	}
	return active[0]
}

// history reads the session history.
func (s *ideationScenario) history() store.IdeationHistory {
	s.t.Helper()
	history, err := s.store.IdeationHistory(s.t.Context(), "session-1")
	if err != nil {
		s.t.Fatal(err)
	}
	return history
}

// requireApplied fails unless command was applied.
func requireApplied(t *testing.T, command store.IdeationCommand) {
	t.Helper()
	if command.Outcome != store.IdeationCommandApplied {
		t.Fatalf("command %s outcome = %q (%s), want applied", command.RequestID, command.Outcome, command.Detail)
	}
}

// groundedRound is a deterministic ideation result whose questions rest on
// repository evidence and whose brief keeps an unknown open.
func groundedRound() report.IdeationHandoff {
	return report.IdeationHandoff{
		Message:   "internal/cli has no export command.",
		Questions: []report.Question{{ID: "q-reader", Prompt: "Who reads the export? Recommended: support engineers."}},
		Brief: &report.Brief{
			Problem:             "Operators cannot export run history.",
			Decisions:           []report.BriefDecision{{ID: "d-format", Decision: "Export JSON.", Rationale: "Operators script with jq."}},
			Evidence:            []report.BriefItem{{ID: "ev-cli", Text: "internal/cli/cli.go has no export command."}},
			UnresolvedQuestions: []report.BriefItem{{ID: "uq-volume", Text: "How many runs does one export hold?"}},
		},
		Readiness: &report.ReadinessProposal{Proposal: report.ReadinessInvestigationNeeded, Rationale: "The volume is unknown."},
	}
}

// TestIdeationScenarioGroundedQuestionsAndChangedAnswer verifies the
// grounded-questions and changed-answer scenarios end to end: the role's
// questions and evidence reach the session, a correction reaches the role
// packet marked as a correction, and the role's revision of the affected
// decision is stored in the next brief revision.
func TestIdeationScenarioGroundedQuestionsAndChangedAnswer(t *testing.T) {
	scenario := newIdeationScenario(t)
	if _, err := scenario.roles.StartIdeationTurn(t.Context(), "session-1", "turn-1"); err != nil {
		t.Fatal(err)
	}
	first := scenario.respond(groundedRound())
	history := scenario.history()
	brief := history.Briefs[len(history.Briefs)-1].Brief
	if len(brief.Evidence) != 1 || brief.Evidence[0].ID != "ev-cli" || history.Session.State != store.IdeationStateAwaitingUser {
		t.Fatalf("history = %#v, want the grounded brief and a session waiting for the user", history)
	}
	accepted := scenario.invocation(first.ID)
	var stored report.Report
	if err := json.Unmarshal(accepted.Payload, &stored); err != nil || len(stored.Ideation.Questions) != 1 || stored.Ideation.Questions[0].ID != "q-reader" {
		t.Fatalf("accepted payload = %s, %v; want the question q-reader", accepted.Payload, err)
	}

	requireApplied(t, scenario.command("turn-2", sessioncommand.KindSubmitTurn, `{"body":"Operators read it."}`))
	scenario.respond(report.IdeationHandoff{Message: "Noted.", Brief: groundedRound().Brief})
	requireApplied(t, scenario.command("turn-3", sessioncommand.KindSubmitTurn, `{"body":"Auditors read it, and they need CSV.","corrects":"turn-2"}`))
	active, err := scenario.store.ActiveSessionInvocations(t.Context())
	if err != nil || len(active) != 1 {
		t.Fatalf("active = %#v, %v", active, err)
	}
	packet, err := os.ReadFile(filepath.Join(active[0].InvocationDirectory, "session.json"))
	if err != nil || !strings.Contains(string(packet), `"corrects": "turn-2"`) {
		t.Fatalf("packet = %s, %v; want the correction marked", packet, err)
	}
	revised := *groundedRound().Brief
	revised.Decisions = []report.BriefDecision{{ID: "d-format", Decision: "Export CSV.", Rationale: "Auditors open it in spreadsheets."}}
	scenario.respond(report.IdeationHandoff{Message: "CSV replaces JSON.", Brief: &revised, AffectedDecisions: []string{"d-format"}})
	history = scenario.history()
	current := history.Briefs[len(history.Briefs)-1].Brief
	if len(current.Decisions) != 1 || current.Decisions[0].Decision != "Export CSV." || history.Turns[len(history.Turns)-2].Corrects != "turn-2" {
		t.Fatalf("history = %#v, want the revised decision after the correction", history)
	}
}

// TestIdeationScenarioUnresolvedRequirementAndNoBuild verifies the
// unresolved-requirement and no-build scenarios: a handoff that does not
// defer the open unknown is refused, investigation-needed parks the session
// on it, and the user can resume and close the session as no-build. No
// command creates a backlog item or starts planning without the user's
// explicit deferral.
func TestIdeationScenarioUnresolvedRequirementAndNoBuild(t *testing.T) {
	scenario := newIdeationScenario(t)
	if _, err := scenario.roles.StartIdeationTurn(t.Context(), "session-1", "turn-1"); err != nil {
		t.Fatal(err)
	}
	scenario.respond(groundedRound())
	revision := scenario.session().BriefRevision

	refused := scenario.command("handoff-1", sessioncommand.KindHandOff, fmt.Sprintf(`{"brief_revision":%d}`, revision))
	if refused.Outcome != store.IdeationCommandRefused || !strings.Contains(refused.Detail, "uq-volume") || len(scenario.planned) != 0 {
		t.Fatalf("handoff = %#v, planned %v; want a refusal that names uq-volume", refused, scenario.planned)
	}
	requireApplied(t, scenario.command("investigate-1", sessioncommand.KindInvestigationNeeded, `{"reason":"Measure export volume."}`))
	if state := scenario.session().State; state != store.IdeationStateInvestigationNeeded {
		t.Fatalf("state = %q, want investigation_needed", state)
	}
	requireApplied(t, scenario.command("resume-1", sessioncommand.KindResume, `{}`))
	requireApplied(t, scenario.command("no-build-1", sessioncommand.KindNoBuild, `{"reason":"Volume is too small to matter."}`))
	session := scenario.session()
	if session.State != store.IdeationStateNoBuild || session.PlanRevision != 0 || len(scenario.planned) != 0 {
		t.Fatalf("session = %#v, planned %v; want no_build without a plan", session, scenario.planned)
	}
}
