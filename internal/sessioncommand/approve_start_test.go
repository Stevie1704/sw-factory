package sessioncommand_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestPublicationApprovalNeverImplicitlyCreatesExecutionAuthority separates the two deliberate decisions.
func TestPublicationApprovalNeverImplicitlyCreatesExecutionAuthority(t *testing.T) {
	for _, kind := range []sessioncommand.Kind{sessioncommand.KindApprove, sessioncommand.KindApproveStart} {
		t.Run(string(kind), func(t *testing.T) {
			c := newCoordinatorForTest(t)
			c.createSession("session")
			revision := c.draftPlan("session")
			preview := c.preview("preview", "session", revision)
			payload := approvePayload(preview)
			if kind == sessioncommand.KindApproveStart {
				payload = strings.TrimSuffix(payload, "}") + `,"selected_issue_ids":["local-00112233445566778899aabbccddeeff"]}`
			}
			c.submit("decision", "session", kind, revision, payload)
			dispatcher := c.dispatcher()
			authorizations := 0
			dispatcher.Env.AuthorizePlan = func(ctx context.Context, command store.IdeationCommand, selected []string) error {
				history, err := c.store.IdeationHistory(ctx, "session")
				if err != nil {
					t.Fatal(err)
				}
				if history.Session.ApprovalID != command.RequestID || len(history.Approvals) != 1 || len(c.fake.Items()) != 0 {
					t.Fatal("execution decision was not separate between exact approval and publication")
				}
				authorizations++
				return nil
			}
			if err := dispatcher.Drain(t.Context()); err != nil {
				t.Fatal(err)
			}
			dispatcher.Wait()
			c.requireOutcome("decision", store.IdeationCommandApplied)
			want := 0
			if kind == sessioncommand.KindApproveStart {
				want = 1
			}
			if authorizations != want || len(c.fake.Items()) != 3 {
				t.Fatalf("authorizations=%d,published=%d", authorizations, len(c.fake.Items()))
			}
			if err := dispatcher.Drain(t.Context()); err != nil {
				t.Fatal(err)
			}
			dispatcher.Wait()
			if authorizations != want {
				t.Fatal("command replay created another execution decision")
			}
		})
	}
}

// TestApproveStartIsUnsupportedWithoutAnExplicitLocalAuthorizer protects legacy remote approvals.
func TestApproveStartIsUnsupportedWithoutAnExplicitLocalAuthorizer(t *testing.T) {
	c := newCoordinatorForTest(t)
	c.createSession("session")
	revision := c.draftPlan("session")
	preview := c.preview("preview", "session", revision)
	payload := strings.TrimSuffix(approvePayload(preview), "}") + `,"selected_issue_ids":["local-00112233445566778899aabbccddeeff"]}`
	c.submit("decision", "session", sessioncommand.KindApproveStart, revision, payload)
	c.drain()
	c.requireOutcome("decision", store.IdeationCommandUnsupported)
	if len(c.fake.Items()) != 0 || len(c.history("session").Approvals) != 0 {
		t.Fatal("unsupported command published or approved work")
	}
}
