package publication_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/publication"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/tracker/trackertest"
	"github.com/Stevie1704/sw-factory/internal/workflow"
)

// errStoreCrashed is the failure a crashed test store returns.
var errStoreCrashed = errors.New("store crashed")

// faultyStoreForTest wraps the real store. It fails the first ledger write
// that failOn selects without persisting it. With crash set, every later call
// fails too, as if the coordinator process died at that point.
type faultyStoreForTest struct {
	*store.Store
	failOn func(store.PublicationOperation) bool
	crash  bool
	// failReceipts counts the receipt writes that fail before one passes.
	failReceipts int
	mu           sync.Mutex
	crashed      bool
	fired        bool
}

// down reports whether the simulated process is dead.
func (f *faultyStoreForTest) down() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.crashed {
		return errStoreCrashed
	}
	return nil
}

// RecordPublicationOperation fails the selected write once.
func (f *faultyStoreForTest) RecordPublicationOperation(ctx context.Context, operation store.PublicationOperation) (store.PublicationOperation, error) {
	if err := f.down(); err != nil {
		return store.PublicationOperation{}, err
	}
	f.mu.Lock()
	fire := !f.fired && f.failOn(operation)
	if fire {
		f.fired, f.crashed = true, f.crash
	}
	f.mu.Unlock()
	if fire {
		return store.PublicationOperation{}, errors.New("injected ledger write failure")
	}
	return f.Store.RecordPublicationOperation(ctx, operation)
}

// IdeationSession fails after a crash.
func (f *faultyStoreForTest) IdeationSession(ctx context.Context, id string) (store.IdeationSession, error) {
	if err := f.down(); err != nil {
		return store.IdeationSession{}, err
	}
	return f.Store.IdeationSession(ctx, id)
}

// IdeationHistory fails after a crash.
func (f *faultyStoreForTest) IdeationHistory(ctx context.Context, id string) (store.IdeationHistory, error) {
	if err := f.down(); err != nil {
		return store.IdeationHistory{}, err
	}
	return f.Store.IdeationHistory(ctx, id)
}

// TransitionIdeationSession fails after a crash.
func (f *faultyStoreForTest) TransitionIdeationSession(ctx context.Context, write store.IdeationWrite) (store.IdeationSession, error) {
	if err := f.down(); err != nil {
		return store.IdeationSession{}, err
	}
	return f.Store.TransitionIdeationSession(ctx, write)
}

// RecordPublicationReceipt fails after a crash, and fails the first
// failReceipts receipt writes without persisting them.
func (f *faultyStoreForTest) RecordPublicationReceipt(ctx context.Context, write store.IdeationWrite, receipt store.PublicationReceipt, settled ...store.PublicationOperation) (store.IdeationSession, store.PublicationReceipt, error) {
	if err := f.down(); err != nil {
		return store.IdeationSession{}, store.PublicationReceipt{}, err
	}
	f.mu.Lock()
	fail := f.failReceipts > 0
	if fail {
		f.failReceipts--
	}
	f.mu.Unlock()
	if fail {
		return store.IdeationSession{}, store.PublicationReceipt{}, errors.New("injected receipt write failure")
	}
	return f.Store.RecordPublicationReceipt(ctx, write, receipt, settled...)
}

// StartPublicationLedger fails after a crash.
func (f *faultyStoreForTest) StartPublicationLedger(ctx context.Context, sessionID, approvalID string, operations []store.PublicationOperation, at time.Time) ([]store.PublicationOperation, error) {
	if err := f.down(); err != nil {
		return nil, err
	}
	return f.Store.StartPublicationLedger(ctx, sessionID, approvalID, operations, at)
}

// recordOf selects the ledger write of operation id in state.
func recordOf(id string, state store.PublicationOperationState) func(store.PublicationOperation) bool {
	return func(operation store.PublicationOperation) bool {
		return operation.ID == id && operation.State == state
	}
}

// publish runs one attempt and fails the test on an error.
func (h *harness) publish(attemptID string) publication.Result {
	h.t.Helper()
	result, err := h.publisher().Publish(h.t.Context(), publication.Request{SessionID: h.sessionID, AttemptID: attemptID})
	if err != nil {
		h.t.Fatalf("Publish(%s) error = %v", attemptID, err)
	}
	return result
}

// assertPublished verifies a complete publication with exactly one tracker
// item per node and three relationships.
func (h *harness) assertPublished(result publication.Result) {
	h.t.Helper()
	if result.Receipt.Outcome != store.PublicationOutcomePublished || result.Session.State != store.IdeationStatePublished {
		h.t.Fatalf("result = %#v, want published", result)
	}
	if items := h.itemsByNode(); len(items) != 3 {
		h.t.Fatalf("created items = %#v, want one per node", items)
	}
	if links := h.fake.Relationships(); len(links) != 3 {
		h.t.Fatalf("relationships = %#v, want three without duplicates", links)
	}
	h.assertInertTracker()
}

// TestFailureBeforeCreateRetriesWithoutDuplicates verifies two failures
// before the tracker write: a typed no-change error from the provider and a
// failed ledger reservation. Both leave a resumable state, and the retry
// creates each item once.
func TestFailureBeforeCreateRetriesWithoutDuplicates(t *testing.T) {
	t.Parallel()

	t.Run("provider refuses the create", func(t *testing.T) {
		t.Parallel()
		h := newApprovedHarness(t)
		h.fake.Inject(trackertest.Fault{Op: trackertest.OpCreateItem, Key: "n-2", Err: tracker.NewPlanError(tracker.PlanErrorPermission, "no write access")})
		first := h.publish("attempt-1")
		if first.Receipt.Outcome != store.PublicationOutcomeIncomplete || first.Session.State != store.IdeationStatePublicationIncomplete {
			t.Fatalf("first attempt = %#v, want incomplete", first)
		}
		if len(first.Receipt.Items) != 1 || first.Receipt.Items[0].NodeID != "n-1" || !strings.Contains(first.Receipt.Detail, "item/n-2") {
			t.Fatalf("receipt = %#v, want n-1 and a detail naming item/n-2", first.Receipt)
		}
		if got := h.operation("item/n-2"); got.State != store.PublicationOperationFailed || got.Attempts != 1 {
			t.Fatalf("item/n-2 = %#v, want failed after one attempt", got)
		}
		h.assertPublished(h.publish("attempt-2"))
	})

	t.Run("ledger reservation fails", func(t *testing.T) {
		t.Parallel()
		h := newApprovedHarness(t)
		faulty := &faultyStoreForTest{Store: h.store, failOn: recordOf("item/n-2", store.PublicationOperationAttempted)}
		if _, err := h.publisherWith(faulty, h.fake).Publish(t.Context(), publication.Request{SessionID: h.sessionID, AttemptID: "attempt-1"}); err == nil {
			t.Fatal("Publish() hid the ledger failure")
		}
		if h.fake.Writes() != 1 {
			t.Fatalf("tracker writes = %d, want only the create of n-1", h.fake.Writes())
		}
		if got := h.operation("item/n-2"); got.State != store.PublicationOperationPending || got.Attempts != 0 {
			t.Fatalf("item/n-2 = %#v, want pending without an attempt", got)
		}
		if state := h.session().State; state != store.IdeationStatePublicationIncomplete {
			t.Fatalf("session state = %s, want publication_incomplete", state)
		}
		h.assertPublished(h.publish("attempt-2"))
	})
}

// TestRecoveryAfterRemoteSuccessBeforeLocalReceipt verifies that a create
// whose result was not recorded before the process died is found by its
// correlation after restart and adopted, not created again.
func TestRecoveryAfterRemoteSuccessBeforeLocalReceipt(t *testing.T) {
	t.Parallel()

	h := newApprovedHarness(t)
	faulty := &faultyStoreForTest{Store: h.store, failOn: recordOf("item/n-2", store.PublicationOperationSucceeded), crash: true}
	if _, err := h.publisherWith(faulty, h.fake).Publish(t.Context(), publication.Request{SessionID: h.sessionID, AttemptID: "attempt-1"}); !errors.Is(err, errStoreCrashed) {
		t.Fatalf("Publish() error = %v, want the crash", err)
	}
	if len(h.fake.Items()) != 2 {
		t.Fatalf("tracker items before restart = %d, want n-1 and n-2", len(h.fake.Items()))
	}

	h.reopen()
	if state := h.session().State; state != store.IdeationStatePublishing {
		t.Fatalf("session state after crash = %s, want publishing", state)
	}
	if got := h.operation("item/n-2"); got.State != store.PublicationOperationAttempted || got.ExternalID != "" {
		t.Fatalf("item/n-2 after crash = %#v, want attempted without identity", got)
	}
	result := h.publish("attempt-2")
	h.assertPublished(result)
	adopted := h.operation("item/n-2")
	if adopted.ExternalID != h.itemsByNode()["n-2"].Ref.ExternalID || adopted.Attempts != 1 || adopted.Detail != "found by correlation lookup" {
		t.Fatalf("item/n-2 after recovery = %#v, want the adopted item after one attempt", adopted)
	}
	if h.fake.Writes() != 6 {
		t.Fatalf("tracker writes = %d, want 3 creates and 3 links", h.fake.Writes())
	}
}

// TestAmbiguousOutcomesStopForExplicitReconciliation verifies that an unknown
// create outcome is adopted when a lookup finds exactly one item, that it
// stops when the lookup finds none or several, and that explicit
// reconciliation resolves both cases without a blind create.
func TestAmbiguousOutcomesStopForExplicitReconciliation(t *testing.T) {
	t.Parallel()

	t.Run("timeout without a remote item needs confirmed absence", func(t *testing.T) {
		t.Parallel()
		h := newApprovedHarness(t)
		h.fake.Inject(trackertest.Fault{Op: trackertest.OpCreateItem, Key: "n-2", Err: context.DeadlineExceeded})
		h.publish("attempt-1")
		if got := h.operation("item/n-2"); got.State != store.PublicationOperationAmbiguous {
			t.Fatalf("item/n-2 = %#v, want ambiguous", got)
		}
		writes := h.fake.Writes()
		second := h.publish("attempt-2")
		if second.Receipt.Outcome != store.PublicationOutcomeIncomplete || h.fake.Writes() != writes {
			t.Fatalf("retry = %#v with %d writes; want a stop without a create", second.Receipt, h.fake.Writes())
		}
		if _, err := h.publisher().Reconcile(t.Context(), publication.Reconciliation{SessionID: h.sessionID, OperationID: "item/n-2", ExternalID: "999"}); !errors.Is(err, publication.ErrReconciliationRefused) {
			t.Fatalf("Reconcile(adopt unknown) error = %v, want refused", err)
		}
		resolved, err := h.publisher().Reconcile(t.Context(), publication.Reconciliation{SessionID: h.sessionID, OperationID: "item/n-2"})
		if err != nil || resolved.State != store.PublicationOperationFailed {
			t.Fatalf("Reconcile(confirm absence) = %#v, %v", resolved, err)
		}
		h.assertPublished(h.publish("attempt-3"))
	})

	t.Run("lost response is adopted on retry", func(t *testing.T) {
		t.Parallel()
		h := newApprovedHarness(t)
		h.fake.Inject(trackertest.Fault{Op: trackertest.OpCreateItem, Key: "n-2", AfterWrite: true, Err: tracker.NewPlanError(tracker.PlanErrorAmbiguous, "timeout")})
		h.publish("attempt-1")
		if got := h.operation("item/n-2"); got.State != store.PublicationOperationAmbiguous {
			t.Fatalf("item/n-2 = %#v, want ambiguous", got)
		}
		h.assertPublished(h.publish("attempt-2"))
	})

	t.Run("multiple candidates need an explicit choice", func(t *testing.T) {
		t.Parallel()
		h := newApprovedHarness(t)
		h.fake.Inject(trackertest.Fault{Op: trackertest.OpCreateItem, Key: "n-2", AfterWrite: true, Err: errors.New("connection reset")})
		h.publish("attempt-1")
		var rendered tracker.RenderedPlanItem
		for _, item := range h.fake.Items() {
			if item.Rendered.NodeID == "n-2" {
				rendered = item.Rendered
			}
		}
		duplicate := h.fake.Seed(rendered)
		second := h.publish("attempt-2")
		if got := h.operation("item/n-2"); got.State != store.PublicationOperationAmbiguous || !strings.Contains(got.Detail, "2 tracker items") {
			t.Fatalf("item/n-2 = %#v, want ambiguous with two candidates", got)
		}
		if second.Receipt.Outcome != store.PublicationOutcomeIncomplete {
			t.Fatalf("retry receipt = %#v, want incomplete", second.Receipt)
		}
		if _, err := h.publisher().Reconcile(t.Context(), publication.Reconciliation{SessionID: h.sessionID, OperationID: "item/n-2"}); !errors.Is(err, publication.ErrReconciliationRefused) {
			t.Fatalf("Reconcile(confirm absence) with candidates error = %v, want refused", err)
		}
		if _, err := h.publisher().Reconcile(t.Context(), publication.Reconciliation{SessionID: h.sessionID, OperationID: "item/n-1"}); !errors.Is(err, publication.ErrReconciliationRefused) {
			t.Fatalf("Reconcile(succeeded operation) error = %v, want refused", err)
		}
		adopted, err := h.publisher().Reconcile(t.Context(), publication.Reconciliation{SessionID: h.sessionID, OperationID: "item/n-2", ExternalID: duplicate.ExternalID})
		if err != nil || adopted.State != store.PublicationOperationSucceeded || adopted.ExternalID != duplicate.ExternalID {
			t.Fatalf("Reconcile(adopt) = %#v, %v", adopted, err)
		}
		final := h.publish("attempt-3")
		if final.Receipt.Outcome != store.PublicationOutcomePublished || len(h.fake.Items()) != 4 {
			t.Fatalf("final = %#v with %d items; the extra candidate stays and nothing is created again", final.Receipt, len(h.fake.Items()))
		}
		abandoned := ""
		for _, item := range h.fake.Items() {
			if item.Rendered.NodeID == "n-2" && item.Ref != duplicate {
				abandoned = item.Ref.ExternalID
			}
		}
		for _, link := range h.fake.Relationships() {
			if link.From.ExternalID == abandoned || link.To.ExternalID == abandoned {
				t.Fatalf("link %#v uses the candidate that was not adopted", link)
			}
		}
	})
}

// TestRelationshipRetriesVerifyLinksAndKeepAPreciseReceipt verifies that a
// failure during links leaves a receipt with every created item and link, and
// that a retry verifies a link whose response was lost instead of creating it
// again.
func TestRelationshipRetriesVerifyLinksAndKeepAPreciseReceipt(t *testing.T) {
	t.Parallel()

	h := newApprovedHarness(t)
	h.fake.Inject(trackertest.Fault{Op: trackertest.OpCreateRelationship, Key: "depends_on:3->2", Err: errors.New("server error")})
	first := h.publish("attempt-1")
	if first.Receipt.Outcome != store.PublicationOutcomeIncomplete || len(first.Receipt.Items) != 3 || len(first.Receipt.Relationships) != 1 ||
		first.Receipt.Relationships[0] != (store.PublishedRelationship{FromNodeID: "n-3", ToNodeID: "n-1", Kind: "parent"}) {
		t.Fatalf("receipt = %#v, want three items and the first link", first.Receipt)
	}
	if !strings.Contains(first.Receipt.Detail, "link/depends_on/n-3/n-2") {
		t.Fatalf("receipt detail = %q, want the stopped link", first.Receipt.Detail)
	}

	h.fake.Inject(trackertest.Fault{Op: trackertest.OpCreateRelationship, Key: "parent:2->1", AfterWrite: true, Err: errors.New("timeout")})
	second := h.publish("attempt-2")
	if second.Receipt.Outcome != store.PublicationOutcomeIncomplete || len(h.fake.Relationships()) != 3 {
		t.Fatalf("second attempt = %#v with %d links", second.Receipt, len(h.fake.Relationships()))
	}
	h.assertPublished(h.publish("attempt-3"))
	if got := h.operation("link/parent/n-2/n-1"); got.Detail != "verified an existing relationship" || got.Attempts != 1 {
		t.Fatalf("link/parent/n-2/n-1 = %#v, want verified after one attempt", got)
	}
}

// TestRetryAfterRestartResumesTheSameApprovedPlan verifies that after a
// partial publication and a restart the session cannot be edited or
// regenerated and the retry continues the stored ledger of the same approval.
func TestRetryAfterRestartResumesTheSameApprovedPlan(t *testing.T) {
	t.Parallel()

	h := newApprovedHarness(t)
	h.fake.Inject(trackertest.Fault{Op: trackertest.OpCreateItem, Key: "n-3", Err: tracker.NewPlanError(tracker.PlanErrorUnsupportedCapability, "rate limited")})
	h.publish("attempt-1")
	before := h.ledger()

	h.reopen()
	session := h.session()
	if _, err := workflow.DefaultSessionLifecycle().Write(session, workflow.SessionEventEdited, "", publicationNow); err == nil {
		t.Fatal("the lifecycle allows an edit of a partially published plan")
	}
	if _, err := workflow.DefaultSessionLifecycle().Write(session, workflow.SessionEventRoleStarted, "", publicationNow); err == nil {
		t.Fatal("the lifecycle allows regeneration of a partially published plan")
	}
	h.assertPublished(h.publish("attempt-2"))
	after := h.ledger()
	for index := range before {
		if before[index].ID != after[index].ID || before[index].Sequence != after[index].Sequence {
			t.Fatalf("ledger changed from %#v to %#v", before, after)
		}
	}
	history, err := h.store.IdeationHistory(t.Context(), h.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Plans) != 1 || len(history.Approvals) != 1 || len(history.Receipts) != 2 {
		t.Fatalf("history = %d plans, %d approvals, %d receipts; want 1, 1, 2", len(history.Plans), len(history.Approvals), len(history.Receipts))
	}
}

// afterFirstCreateForTest runs hook once after the first successful item
// create.
type afterFirstCreateForTest struct {
	*trackertest.PlanTracker
	hook func()
	once sync.Once
}

// CreatePlanItem creates through the fake and then runs the hook once.
func (a *afterFirstCreateForTest) CreatePlanItem(ctx context.Context, repository tracker.Repository, item tracker.RenderedPlanItem) (tracker.PlanItemRef, error) {
	ref, err := a.PlanTracker.CreatePlanItem(ctx, repository, item)
	if err == nil {
		a.once.Do(a.hook)
	}
	return ref, err
}

// TestCancellationStopsFutureWorkWithoutDeletingItems verifies that an
// interrupted attempt and a cancelled session stop before the next write,
// keep the created item and its identity, and never delete it.
func TestCancellationStopsFutureWorkWithoutDeletingItems(t *testing.T) {
	t.Parallel()

	t.Run("interrupted attempt resumes", func(t *testing.T) {
		t.Parallel()
		h := newApprovedHarness(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		interrupting := &afterFirstCreateForTest{PlanTracker: h.fake, hook: cancel}
		result, err := h.publisherWith(h.store, interrupting).Publish(ctx, publication.Request{SessionID: h.sessionID, AttemptID: "attempt-1"})
		if err != nil {
			t.Fatalf("Publish() error = %v", err)
		}
		if result.Receipt.Outcome != store.PublicationOutcomeIncomplete || !strings.Contains(result.Receipt.Detail, "interrupted") ||
			len(h.fake.Items()) != 1 || h.fake.Writes() != 1 {
			t.Fatalf("interrupted result = %#v with %d writes", result.Receipt, h.fake.Writes())
		}
		h.assertPublished(h.publish("attempt-2"))
	})

	t.Run("cancelled session keeps created items", func(t *testing.T) {
		t.Parallel()
		h := newApprovedHarness(t)
		cancelling := &afterFirstCreateForTest{PlanTracker: h.fake}
		cancelling.hook = func() {
			if _, err := h.store.TransitionIdeationSession(t.Context(), h.write(h.session(), workflow.SessionEventCancelled)); err != nil {
				t.Errorf("cancel session: %v", err)
			}
		}
		result, err := h.publisherWith(h.store, cancelling).Publish(t.Context(), publication.Request{SessionID: h.sessionID, AttemptID: "attempt-1"})
		if !errors.Is(err, publication.ErrPublicationCancelled) {
			t.Fatalf("Publish() error = %v, want ErrPublicationCancelled", err)
		}
		if result.Session.State != store.IdeationStateCancelled || len(h.fake.Items()) != 1 || h.fake.Writes() != 1 {
			t.Fatalf("cancelled result = %#v with %d writes", result.Session, h.fake.Writes())
		}
		if got := h.operation("item/n-1"); got.State != store.PublicationOperationSucceeded || got.ExternalID != h.fake.Items()[0].Ref.ExternalID {
			t.Fatalf("item/n-1 = %#v, want its identity kept", got)
		}
		recorded := h.receipt("attempt-1")
		if recorded.Outcome != store.PublicationOutcomeIncomplete || len(recorded.Items) != 1 || recorded.Items[0].NodeID != "n-1" ||
			recorded.Items[0].ExternalID != h.fake.Items()[0].Ref.ExternalID || result.Receipt.ID != "attempt-1" {
			t.Fatalf("receipt = %#v, result receipt = %#v; want an incomplete receipt that lists the created n-1", recorded, result.Receipt)
		}
		for _, want := range []string{"cancelled", "not run: item/n-2, item/n-3, link/parent/n-3/n-1, link/depends_on/n-3/n-2, link/parent/n-2/n-1"} {
			if !strings.Contains(recorded.Detail, want) {
				t.Fatalf("receipt detail = %q, want %q", recorded.Detail, want)
			}
		}
		if _, err := h.publisher().Publish(t.Context(), publication.Request{SessionID: h.sessionID, AttemptID: "attempt-2"}); !errors.Is(err, publication.ErrNotPublishable) {
			t.Fatalf("Publish() after cancel error = %v, want ErrNotPublishable", err)
		}
		h.assertInertTracker()
	})

	t.Run("cancelled attempt records its receipt on resume", func(t *testing.T) {
		t.Parallel()
		h := newApprovedHarness(t)
		faulty := &faultyStoreForTest{Store: h.store, failOn: func(store.PublicationOperation) bool { return false }, failReceipts: 1}
		cancelling := &afterFirstCreateForTest{PlanTracker: h.fake}
		cancelling.hook = func() {
			if _, err := h.store.TransitionIdeationSession(t.Context(), h.write(h.session(), workflow.SessionEventCancelled)); err != nil {
				t.Errorf("cancel session: %v", err)
			}
		}
		if _, err := h.publisherWith(faulty, cancelling).Publish(t.Context(), publication.Request{SessionID: h.sessionID, AttemptID: "attempt-1"}); err == nil {
			t.Fatal("Publish() hid the receipt failure")
		}
		resumed, err := h.publisher().Publish(t.Context(), publication.Request{SessionID: h.sessionID, AttemptID: "attempt-1"})
		if !errors.Is(err, publication.ErrPublicationCancelled) {
			t.Fatalf("resumed Publish() error = %v, want ErrPublicationCancelled", err)
		}
		if resumed.Receipt.ID != "attempt-1" || len(resumed.Receipt.Items) != 1 || h.fake.Writes() != 1 {
			t.Fatalf("resumed receipt = %#v with %d writes; want the n-1 receipt and no new write", resumed.Receipt, h.fake.Writes())
		}
		if state := h.session().State; state != store.IdeationStateCancelled {
			t.Fatalf("session state = %s, want cancelled", state)
		}
	})
}

// receipt returns the recorded receipt with id.
func (h *harness) receipt(id string) store.PublicationReceipt {
	h.t.Helper()
	history, err := h.store.IdeationHistory(h.t.Context(), h.sessionID)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, receipt := range history.Receipts {
		if receipt.ID == id {
			return receipt
		}
	}
	h.t.Fatalf("receipts = %#v, want %q", history.Receipts, id)
	return store.PublicationReceipt{}
}

// TestCancelledSessionReconcilesAnAmbiguousCreate verifies that a session
// cancelled after an ambiguous create can still adopt the remote item or
// confirm that none exists. Reconciliation records the identity in the ledger
// and in a receipt, keeps the session cancelled, and never enables Publish.
func TestCancelledSessionReconcilesAnAmbiguousCreate(t *testing.T) {
	t.Parallel()

	cancelAfterAmbiguousCreate := func(t *testing.T, fault trackertest.Fault) *harness {
		t.Helper()
		h := newApprovedHarness(t)
		h.fake.Inject(fault)
		h.publish("attempt-1")
		if got := h.operation("item/n-2"); got.State != store.PublicationOperationAmbiguous {
			t.Fatalf("item/n-2 = %#v, want ambiguous", got)
		}
		if _, err := h.store.TransitionIdeationSession(t.Context(), h.write(h.session(), workflow.SessionEventCancelled)); err != nil {
			t.Fatal(err)
		}
		return h
	}
	receipt := func(t *testing.T, h *harness, id string) store.PublicationReceipt {
		t.Helper()
		history, err := h.store.IdeationHistory(t.Context(), h.sessionID)
		if err != nil {
			t.Fatal(err)
		}
		for _, receipt := range history.Receipts {
			if receipt.ID == id {
				return receipt
			}
		}
		t.Fatalf("receipts = %#v, want %q", history.Receipts, id)
		return store.PublicationReceipt{}
	}

	t.Run("adopt records the item", func(t *testing.T) {
		t.Parallel()
		h := cancelAfterAmbiguousCreate(t, trackertest.Fault{Op: trackertest.OpCreateItem, Key: "n-2", AfterWrite: true, Err: tracker.NewPlanError(tracker.PlanErrorAmbiguous, "timeout")})
		var created tracker.PlanItemRef
		for _, item := range h.fake.Items() {
			if item.Rendered.NodeID == "n-2" {
				created = item.Ref
			}
		}
		writes := h.fake.Writes()
		adopted, err := h.publisher().Reconcile(t.Context(), publication.Reconciliation{
			SessionID: h.sessionID, OperationID: "item/n-2", ExternalID: created.ExternalID, ReceiptID: "reconcile-1",
		})
		if err != nil || adopted.State != store.PublicationOperationSucceeded || adopted.ExternalID != created.ExternalID {
			t.Fatalf("Reconcile(adopt) = %#v, %v; want the item adopted", adopted, err)
		}
		recorded := receipt(t, h, "reconcile-1")
		found := false
		for _, item := range recorded.Items {
			found = found || (item.NodeID == "n-2" && item.ExternalID == created.ExternalID)
		}
		if !found || recorded.Outcome != store.PublicationOutcomeIncomplete {
			t.Fatalf("receipt = %#v, want an incomplete receipt that lists the adopted n-2", recorded)
		}
		if session := h.session(); session.State != store.IdeationStateCancelled {
			t.Fatalf("session = %#v, want it to stay cancelled", session)
		}
		if _, err := h.publisher().Publish(t.Context(), publication.Request{SessionID: h.sessionID, AttemptID: "attempt-2"}); !errors.Is(err, publication.ErrNotPublishable) {
			t.Fatalf("Publish() after reconcile error = %v, want ErrNotPublishable", err)
		}
		if h.fake.Writes() != writes {
			t.Fatalf("tracker writes = %d, want %d: reconciliation never writes to the tracker", h.fake.Writes(), writes)
		}
	})

	t.Run("failed receipt write leaves the operation for a retry", func(t *testing.T) {
		t.Parallel()
		h := cancelAfterAmbiguousCreate(t, trackertest.Fault{Op: trackertest.OpCreateItem, Key: "n-2", Err: context.DeadlineExceeded})
		faulty := &faultyStoreForTest{Store: h.store, failOn: func(store.PublicationOperation) bool { return false }, failReceipts: 1}
		request := publication.Reconciliation{SessionID: h.sessionID, OperationID: "item/n-2", ReceiptID: "reconcile-1"}
		if _, err := h.publisherWith(faulty, h.fake).Reconcile(t.Context(), request); err == nil {
			t.Fatal("Reconcile() hid the receipt failure")
		}
		if got := h.operation("item/n-2"); got.State != store.PublicationOperationAmbiguous {
			t.Fatalf("item/n-2 after the failed receipt = %#v, want it still ambiguous", got)
		}
		resolved, err := h.publisher().Reconcile(t.Context(), request)
		if err != nil || resolved.State != store.PublicationOperationFailed {
			t.Fatalf("retried Reconcile() = %#v, %v; want not applied", resolved, err)
		}
		if recorded := receipt(t, h, "reconcile-1"); !strings.Contains(recorded.Detail, "item/n-2") {
			t.Fatalf("receipt = %#v, want the reconciled operation named", recorded)
		}
	})

	t.Run("confirm absent records not applied", func(t *testing.T) {
		t.Parallel()
		h := cancelAfterAmbiguousCreate(t, trackertest.Fault{Op: trackertest.OpCreateItem, Key: "n-2", Err: context.DeadlineExceeded})
		resolved, err := h.publisher().Reconcile(t.Context(), publication.Reconciliation{SessionID: h.sessionID, OperationID: "item/n-2", ReceiptID: "reconcile-1"})
		if err != nil || resolved.State != store.PublicationOperationFailed {
			t.Fatalf("Reconcile(confirm absence) = %#v, %v; want not applied", resolved, err)
		}
		recorded := receipt(t, h, "reconcile-1")
		for _, item := range recorded.Items {
			if item.NodeID == "n-2" {
				t.Fatalf("receipt = %#v, want no n-2 item", recorded)
			}
		}
		if !strings.Contains(recorded.Detail, "item/n-2") || h.session().State != store.IdeationStateCancelled {
			t.Fatalf("receipt detail = %q, session = %s; want the reconciled operation named and the session cancelled", recorded.Detail, h.session().State)
		}
		if _, err := h.publisher().Publish(t.Context(), publication.Request{SessionID: h.sessionID, AttemptID: "attempt-2"}); !errors.Is(err, publication.ErrNotPublishable) {
			t.Fatalf("Publish() after reconcile error = %v, want ErrNotPublishable", err)
		}
		if _, err := h.publisher().Reconcile(t.Context(), publication.Reconciliation{SessionID: h.sessionID, OperationID: "item/n-2", ReceiptID: "reconcile-2"}); !errors.Is(err, publication.ErrReconciliationRefused) {
			t.Fatalf("second Reconcile() error = %v, want refused for a settled operation", err)
		}
	})
}

// TestInterruptDerivesTheOutcomeFromTheLedger verifies that an attempt
// whose every operation succeeded but whose receipt write failed settles as
// published, not incomplete, when the coordinator interrupts it.
func TestInterruptDerivesTheOutcomeFromTheLedger(t *testing.T) {
	t.Parallel()

	h := newApprovedHarness(t)
	faulty := &faultyStoreForTest{Store: h.store, failOn: func(store.PublicationOperation) bool { return false }, failReceipts: 1}
	if _, err := h.publisherWith(faulty, h.fake).Publish(t.Context(), publication.Request{SessionID: h.sessionID, AttemptID: "attempt-1"}); err == nil {
		t.Fatal("Publish() hid the receipt failure")
	}
	if state := h.session().State; state != store.IdeationStatePublishing {
		t.Fatalf("session state = %s, want publishing", state)
	}
	session, err := h.publisher().Interrupt(t.Context(), publication.Request{SessionID: h.sessionID, AttemptID: "attempt-1"}, "the receipt write failed")
	if err != nil {
		t.Fatalf("Interrupt() error = %v", err)
	}
	recorded := h.receipt("attempt-1")
	if session.State != store.IdeationStatePublished || recorded.Outcome != store.PublicationOutcomePublished || len(recorded.Items) != 3 || len(recorded.Relationships) != 3 {
		t.Fatalf("session = %s, receipt = %#v; want published with three items and three links", session.State, recorded)
	}
}

// boundedLookupForTest is a tracker whose correlation lookup reads only
// part of the tracker.
type boundedLookupForTest struct {
	*trackertest.PlanTracker
}

// PlanLookupBound describes the scripted lookup window.
func (boundedLookupForTest) PlanLookupBound() string {
	return "the 1,000 newest items"
}

// TestAmbiguousCreateNamesTheLookupBoundBeforeConfirmation verifies that an
// unknown create outcome with no match tells the operator which items the
// lookup read, so that the operator checks older items before confirming
// that none exists.
func TestAmbiguousCreateNamesTheLookupBoundBeforeConfirmation(t *testing.T) {
	t.Parallel()

	h := newApprovedHarness(t)
	h.fake.Inject(trackertest.Fault{Op: trackertest.OpCreateItem, Key: "n-2", Err: context.DeadlineExceeded})
	if _, err := h.publisherWith(h.store, boundedLookupForTest{h.fake}).Publish(t.Context(), publication.Request{SessionID: h.sessionID, AttemptID: "attempt-1"}); err != nil {
		t.Fatal(err)
	}
	if got := h.operation("item/n-2"); !strings.Contains(got.Detail, "the lookup reads only the 1,000 newest items") {
		t.Fatalf("item/n-2 after the timeout = %#v, want the lookup bound named", got)
	}
	if _, err := h.publisherWith(h.store, boundedLookupForTest{h.fake}).Publish(t.Context(), publication.Request{SessionID: h.sessionID, AttemptID: "attempt-2"}); err != nil {
		t.Fatal(err)
	}
	if got := h.operation("item/n-2"); got.State != store.PublicationOperationAmbiguous || !strings.Contains(got.Detail, "the lookup reads only the 1,000 newest items") {
		t.Fatalf("item/n-2 = %#v, want the lookup bound named before confirmation", got)
	}
}
