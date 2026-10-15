package publication_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/publication"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/tracker/trackertest"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// publicationNow is the fixed clock of the publication tests.
var publicationNow = time.Date(2026, time.October, 15, 10, 0, 0, 0, time.UTC)

// conversationSecret is user turn text that must never reach the tracker.
const conversationSecret = "PRIVATE-TURN-TEXT-7f3a"

// testRepository is the registered repository of the tests.
var testRepository = tracker.Repository{Owner: "owner", Name: "repo"}

// harness is one approved session in a real SQLite store with a fake
// tracker.
type harness struct {
	t         *testing.T
	path      string
	store     *store.Store
	fake      *trackertest.PlanTracker
	sessionID string
}

// newHarness creates a session with a three-node plan: n-1 is the parent of
// n-2 and n-3, and n-3 depends on n-2. The plan lists n-3 first, so the
// publisher must order creates itself. It does not approve the plan.
func newHarness(t *testing.T) *harness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "factory.db")
	opened, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, path: path, store: opened, fake: trackertest.NewPlanTracker(), sessionID: "session-1"}
	t.Cleanup(func() { _ = h.store.Close() })
	lifecycle := workflow.DefaultSessionLifecycle()
	session, err := opened.CreateIdeationSession(t.Context(), store.IdeationSession{
		ID: h.sessionID, RepositoryPath: "/repos/factory", State: lifecycle.InitialState(), CreatedAt: publicationNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, _, err = opened.AppendIdeationTurn(t.Context(), h.write(session, workflow.SessionEventTurnAccepted), store.IdeationTurn{
		ID: "turn-1", Author: store.IdeationTurnAuthorUser, Body: conversationSecret,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, brief, err := opened.SaveBriefRevision(t.Context(), h.write(session, workflow.SessionEventEdited), store.Brief{
		Problem: "Onboarding is slow.", Scope: []store.BriefItem{{ID: "scope-1", Text: "The onboarding command."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err = opened.TransitionIdeationSession(t.Context(), h.write(session, workflow.SessionEventRoleStarted))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = opened.SavePlanRevision(t.Context(), h.write(session, workflow.SessionEventPlanDrafted), store.PlanRevision{
		SourceBriefRevision: brief.Revision, ContextRevision: "0123456789abcdef0123456789abcdef01234567",
		Nodes: []store.PlanNode{
			{ID: "n-3", Kind: "task", Title: "Document setup", Body: "Docs.", ParentID: "n-1", DependsOn: []string{"n-2"}},
			{ID: "n-1", Kind: "spec", Title: "Faster onboarding", Body: "Parent.", AcceptanceCriteria: []string{"One command."}, BriefRefs: []string{"scope-1"}},
			{ID: "n-2", Kind: "task", Title: "Cache images", Body: "Child.", ParentID: "n-1"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return h
}

// newApprovedHarness returns a harness whose current plan is approved with
// the rendered preview as approval-1.
func newApprovedHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.approve("approval-1")
	return h
}

// publisher returns a publisher over the harness store and fake.
func (h *harness) publisher() *publication.Publisher {
	return h.publisherWith(h.store, h.fake)
}

// publisherWith returns a publisher over the given seams.
func (h *harness) publisherWith(seam publication.Store, planTracker tracker.PlanPublisher) *publication.Publisher {
	return &publication.Publisher{
		Store: seam, Tracker: planTracker, Repository: testRepository,
		Lifecycle: workflow.DefaultSessionLifecycle(), Now: func() time.Time { return publicationNow },
	}
}

// session reads the current session.
func (h *harness) session() store.IdeationSession {
	h.t.Helper()
	session, err := h.store.IdeationSession(h.t.Context(), h.sessionID)
	if err != nil {
		h.t.Fatal(err)
	}
	return session
}

// write resolves event for session through the session lifecycle.
func (h *harness) write(session store.IdeationSession, event workflow.SessionEvent) store.IdeationWrite {
	h.t.Helper()
	write, err := workflow.DefaultSessionLifecycle().Write(session, event, "", publicationNow)
	if err != nil {
		h.t.Fatal(err)
	}
	return write
}

// approve renders the current preview and approves it as id.
func (h *harness) approve(id string) publication.Binding {
	h.t.Helper()
	binding, err := h.publisher().Preview(h.t.Context(), h.sessionID)
	if err != nil {
		h.t.Fatalf("Preview() error = %v", err)
	}
	if _, _, err := h.store.ApprovePlan(h.t.Context(), h.write(h.session(), workflow.SessionEventApproved), binding.Approval(id)); err != nil {
		h.t.Fatalf("ApprovePlan() error = %v", err)
	}
	return binding
}

// reopen closes the store and opens it again, as a coordinator restart does.
func (h *harness) reopen() {
	h.t.Helper()
	_ = h.store.Close()
	reopened, err := store.Open(h.t.Context(), h.path)
	if err != nil {
		h.t.Fatalf("reopen store: %v", err)
	}
	h.store = reopened
}

// ledger returns the publication ledger of approval-1.
func (h *harness) ledger() []store.PublicationOperation {
	h.t.Helper()
	ledger, err := h.store.PublicationLedger(h.t.Context(), h.sessionID, "approval-1")
	if err != nil {
		h.t.Fatal(err)
	}
	return ledger
}

// operation returns the ledger operation with id.
func (h *harness) operation(id string) store.PublicationOperation {
	h.t.Helper()
	for _, operation := range h.ledger() {
		if operation.ID == id {
			return operation
		}
	}
	h.t.Fatalf("ledger has no operation %q: %#v", id, h.ledger())
	return store.PublicationOperation{}
}

// assertInertTracker verifies decision D6 and the raw-conversation rule: no
// created item carries a factory state label or conversation text, and the
// store holds no run.
func (h *harness) assertInertTracker() {
	h.t.Helper()
	for _, item := range h.fake.Items() {
		for _, label := range item.Rendered.Labels {
			if tracker.IsFactoryStateLabel(label) {
				h.t.Fatalf("item %s carries factory label %q", item.Ref.ExternalID, label)
			}
		}
		if strings.Contains(item.Rendered.Body, conversationSecret) || strings.Contains(item.Rendered.Title, conversationSecret) {
			h.t.Fatalf("item %s contains raw conversation text", item.Ref.ExternalID)
		}
	}
	if run, err := h.store.CurrentRun(h.t.Context()); err != nil || run != nil {
		h.t.Fatalf("CurrentRun() = %#v, %v; publication must not start a run", run, err)
	}
}

// itemsByNode returns the created items keyed by node, failing on a
// duplicate node.
func (h *harness) itemsByNode() map[string]trackertest.PlanItem {
	h.t.Helper()
	items := map[string]trackertest.PlanItem{}
	for _, item := range h.fake.Items() {
		if _, ok := items[item.Rendered.NodeID]; ok {
			h.t.Fatalf("node %s was created more than once: %#v", item.Rendered.NodeID, h.fake.Items())
		}
		items[item.Rendered.NodeID] = item
	}
	return items
}
