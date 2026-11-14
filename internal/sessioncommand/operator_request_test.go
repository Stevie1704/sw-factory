package sessioncommand_test

import (
	"github.com/Stevie1704/sw-factory/internal/sessioncommand"
	"testing"
)

// TestOperatorCommandsHaveIndependentStrictSubjects rejects synthetic session and forged provenance.
func TestOperatorCommandsHaveIndependentStrictSubjects(t *testing.T) {
	source := []byte(`{"request_id":"preview-1","scope":"backlog","subject_id":"repository","kind":"preview_execution","expected_revision":0,"payload":{"selected_issue_ids":["local-00112233445566778899aabbccddeeff"]}}`)
	command, err := sessioncommand.ParseRequest(source)
	if err != nil {
		t.Fatal(err)
	}
	if command.Scope != "backlog" || command.SubjectID != "repository" || command.SessionID != "" {
		t.Fatalf("command=%#v", command)
	}
	for _, bad := range []string{
		`{"request_id":"r","scope":"run","subject_id":"run-1","kind":"preview_execution","expected_revision":1,"payload":{"selected_issue_ids":[]}}`,
		`{"request_id":"r","scope":"backlog","subject_id":"repository","session_id":"terminal-session","kind":"preview_execution","expected_revision":0,"payload":{"selected_issue_ids":["local-00112233445566778899aabbccddeeff"]}}`,
		`{"request_id":"r","scope":"backlog","subject_id":"repository","kind":"preview_execution","expected_revision":0,"payload":{"operator_name":"attacker","selected_issue_ids":["local-00112233445566778899aabbccddeeff"]}}`,
	} {
		if _, err := sessioncommand.ParseRequest([]byte(bad)); err == nil {
			t.Fatal("forged/cross-scope request accepted")
		}
	}
}
