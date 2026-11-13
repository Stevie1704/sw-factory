package sessioncommand_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestPersistentDispatcherPublishesAfterSessionPreview keeps one supervisor dispatcher across rounds.
func TestPersistentDispatcherPublishesAfterSessionPreview(t *testing.T) {
	c := newCoordinatorForTest(t)
	c.createSession("session")
	revision := c.draftPlan("session")
	dispatcher := c.dispatcher()
	c.submit("preview", "session", sessioncommand.KindPreviewPlan, revision, `{"plan_revision":1}`)
	if err := dispatcher.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.Wait(); err != nil {
		t.Fatal(err)
	}
	command := c.requireOutcome("preview", store.IdeationCommandApplied)
	var preview previewResult
	if err := json.Unmarshal(command.Result, &preview); err != nil {
		t.Fatal(err)
	}
	c.submit("approve", "session", sessioncommand.KindApprove, revision, approvePayload(preview))
	if err := dispatcher.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.Wait(); err != nil {
		t.Fatal(err)
	}
	c.requireOutcome("approve", store.IdeationCommandApplied)
	if len(c.fake.Items()) != 3 {
		t.Fatalf("published %d items; want three", len(c.fake.Items()))
	}
}

// subjectReleaseOperator serves scoped commands through the coordinator extension seam.
type subjectReleaseOperator struct{}

// ApplyOperatorCommand returns a durable result for the displayed repository subject.
func (subjectReleaseOperator) ApplyOperatorCommand(context.Context, store.IdeationCommand) (sessioncommand.Outcome, error) {
	return sessioncommand.Outcome{Code: store.IdeationCommandApplied, SessionRevision: 1, Detail: "scoped coordinator command applied"}, nil
}

// TestPersistentDispatcherAuthorizesAfterBacklogPreview frees a background backlog subject between rounds.
func TestPersistentDispatcherAuthorizesAfterBacklogPreview(t *testing.T) {
	c := newCoordinatorForTest(t)
	dispatcher := c.dispatcher()
	dispatcher.Env.Operator = subjectReleaseOperator{}
	selected := `["local-00112233445566778899aabbccddeeff"]`
	requests := []struct {
		id       string
		kind     sessioncommand.Kind
		revision int64
		payload  string
	}{
		{"preview", sessioncommand.KindPreviewExecution, 0, `{"selected_issue_ids":` + selected + `}`},
		{"authorize", sessioncommand.KindAuthorizeExecution, 1, `{"preview_id":"preview","payload_sha256":"sha256:` + strings.Repeat("a", 64) + `","relationship_mapping_sha256":"sha256:` + strings.Repeat("b", 64) + `","graph_digest":"sha256:` + strings.Repeat("c", 64) + `","selected_issue_ids":` + selected + `}`},
	}
	for _, request := range requests {
		raw := fmt.Sprintf(`{"request_id":%q,"scope":"backlog","subject_id":"repository","kind":%q,"expected_revision":%d,"payload":%s}`, request.id, request.kind, request.revision, request.payload)
		command, err := sessioncommand.ParseRequest([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := c.store.AcceptIdeationCommand(t.Context(), command); err != nil {
			t.Fatal(err)
		}
		if err := dispatcher.Drain(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := dispatcher.Wait(); err != nil {
			t.Fatal(err)
		}
		c.requireOutcome(request.id, store.IdeationCommandApplied)
	}
}
