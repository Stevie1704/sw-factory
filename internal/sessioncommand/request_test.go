package sessioncommand_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
)

// TestParseRequestAcceptsEveryDeclaredKind verifies that each declared
// command kind parses into an inbox command with a canonical payload, so a
// retried submission with other whitespace is the same command.
func TestParseRequestAcceptsEveryDeclaredKind(t *testing.T) {
	t.Parallel()

	payloads := map[sessioncommand.Kind]string{
		sessioncommand.KindCreateSession:   `{"title":"Faster onboarding"}`,
		sessioncommand.KindSubmitTurn:      `{"body":"Who uses this?"}`,
		sessioncommand.KindEditBrief:       `{"brief":{"problem":"Slow.","scope":[{"id":"scope-1","text":"The command."}],"decisions":[{"id":"d-1","decision":"Cache.","rationale":"Fast."}]}}`,
		sessioncommand.KindHandOff:         `{"brief_revision":2}`,
		sessioncommand.KindEditDraft:       `{"nodes":[{"id":"n-1","kind":"task","title":"Do it","body":"Body.","acceptance_criteria":["Works."],"brief_refs":["scope-1"]}]}`,
		sessioncommand.KindRegenerateDraft: `{"plan_revision":1}`,
		sessioncommand.KindPreviewPlan:     `{"plan_revision":1}`,
		sessioncommand.KindApprove:         `{"plan_revision":1,"payload_sha256":"` + strings.Repeat("a", 64) + `","relationship_mapping_sha256":"` + strings.Repeat("b", 64) + `","publish":true}`,
		sessioncommand.KindPublish:         `{}`,
		sessioncommand.KindReconcile:       `{"operation_id":"item/n-1","external_id":"42"}`,
		sessioncommand.KindDefer:           `{"reason":"Later."}`,
		sessioncommand.KindResume:          `{}`,
		sessioncommand.KindNoBuild:         `{"reason":"Not worth it."}`,
		sessioncommand.KindCancel:          `{}`,
	}
	if len(payloads) != len(sessioncommand.Kinds()) {
		t.Fatalf("test covers %d kinds, package declares %d", len(payloads), len(sessioncommand.Kinds()))
	}
	for kind, payload := range payloads {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			revision := "3"
			if kind == sessioncommand.KindCreateSession {
				revision = "0"
			}
			body := `{"request_id":"request-1","session_id":"session-1","kind":"` + string(kind) + `","expected_revision":` + revision + `,"payload": ` + payload + ` }`
			command, err := sessioncommand.ParseRequest([]byte(body))
			if err != nil {
				t.Fatalf("ParseRequest() error = %v", err)
			}
			if command.RequestID != "request-1" || command.SessionID != "session-1" || command.Kind != string(kind) {
				t.Fatalf("command = %#v", command)
			}
			spaced := strings.Replace(body, `"payload": {`, `"payload": {  `, 1)
			again, err := sessioncommand.ParseRequest([]byte(spaced))
			if err != nil || string(again.Payload) != string(command.Payload) {
				t.Fatalf("respaced payload = %s, %v; want the canonical %s", again.Payload, err, command.Payload)
			}
		})
	}
}

// TestParseRequestRefusesMalformedOrUnboundedInput verifies the strict,
// bounded decoding of browser input.
func TestParseRequestRefusesMalformedOrUnboundedInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{name: "not json", body: `{`},
		{name: "unknown envelope field", body: `{"request_id":"r","session_id":"s","kind":"cancel","expected_revision":1,"payload":{},"admin":true}`},
		{name: "unknown kind", body: `{"request_id":"r","session_id":"s","kind":"launch_harness","expected_revision":1,"payload":{}}`},
		{name: "unknown payload field", body: `{"request_id":"r","session_id":"s","kind":"submit_turn","expected_revision":1,"payload":{"body":"x","role":"ideation"}}`},
		{name: "missing payload", body: `{"request_id":"r","session_id":"s","kind":"cancel","expected_revision":1}`},
		{name: "unsafe request id", body: `{"request_id":"r 1","session_id":"s","kind":"cancel","expected_revision":1,"payload":{}}`},
		{name: "missing session", body: `{"request_id":"r","kind":"cancel","expected_revision":1,"payload":{}}`},
		{name: "create with revision", body: `{"request_id":"r","session_id":"s","kind":"create_session","expected_revision":1,"payload":{"title":"x"}}`},
		{name: "mutation without revision", body: `{"request_id":"r","session_id":"s","kind":"cancel","expected_revision":0,"payload":{}}`},
		{name: "empty turn", body: `{"request_id":"r","session_id":"s","kind":"submit_turn","expected_revision":1,"payload":{"body":"  "}}`},
		{name: "oversized turn", body: `{"request_id":"r","session_id":"s","kind":"submit_turn","expected_revision":1,"payload":{"body":"` + strings.Repeat("x", 64*1024+1) + `"}}`},
		{name: "bad digest", body: `{"request_id":"r","session_id":"s","kind":"approve","expected_revision":1,"payload":{"plan_revision":1,"payload_sha256":"x","relationship_mapping_sha256":"y"}}`},
		{name: "trailing data", body: `{"request_id":"r","session_id":"s","kind":"cancel","expected_revision":1,"payload":{}} {}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := sessioncommand.ParseRequest([]byte(test.body))
			var invalid *sessioncommand.InvalidRequestError
			if !errors.As(err, &invalid) {
				t.Fatalf("ParseRequest() error = %v, want InvalidRequestError", err)
			}
		})
	}
}
