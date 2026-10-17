package store_test

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// commandForTest returns one accepted browser command for session.
func commandForTest(requestID, sessionID string, expected int64) store.IdeationCommand {
	return store.IdeationCommand{
		RequestID:        requestID,
		SessionID:        sessionID,
		Kind:             "submit_turn",
		ExpectedRevision: expected,
		Payload:          []byte(`{"body":"Who uses this?"}`),
		AcceptedAt:       ideationNow,
	}
}

// TestIdeationCommandIsAcceptedOnceAndReplayedAcrossRestart verifies that the
// inbox records a command durably, that a repeated request ID returns the
// stored command, and that the same ID with other content is refused.
func TestIdeationCommandIsAcceptedOnceAndReplayedAcrossRestart(t *testing.T) {
	t.Parallel()

	opened, path := openIdeationStoreForTest(t)
	accepted, replayed, err := opened.AcceptIdeationCommand(t.Context(), commandForTest("request-1", "session-1", 3))
	if err != nil {
		t.Fatalf("AcceptIdeationCommand() error = %v", err)
	}
	if replayed || accepted.State != store.IdeationCommandAccepted || accepted.Sequence != 1 || accepted.Outcome != "" {
		t.Fatalf("accepted = %#v, replayed = %t; want a new accepted command at sequence 1", accepted, replayed)
	}

	reopened := reopenIdeationStoreForTest(t, opened, path)
	again, replayed, err := reopened.AcceptIdeationCommand(t.Context(), commandForTest("request-1", "session-1", 3))
	if err != nil {
		t.Fatalf("repeated AcceptIdeationCommand() error = %v", err)
	}
	if !replayed || again.Sequence != 1 || string(again.Payload) != `{"body":"Who uses this?"}` {
		t.Fatalf("repeated = %#v, replayed = %t; want the stored command", again, replayed)
	}

	changed := commandForTest("request-1", "session-1", 4)
	if _, _, err := reopened.AcceptIdeationCommand(t.Context(), changed); !errors.Is(err, store.ErrIdeationRequestReused) {
		t.Fatalf("reused request error = %v, want ErrIdeationRequestReused", err)
	}
	pending, err := reopened.PendingIdeationCommands(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].RequestID != "request-1" {
		t.Fatalf("pending = %#v, want only request-1", pending)
	}
}

// TestIdeationCommandRejectsUnboundedOrMalformedInput verifies that the inbox
// refuses identities and payloads that a browser could abuse.
func TestIdeationCommandRejectsUnboundedOrMalformedInput(t *testing.T) {
	t.Parallel()

	opened, _ := openIdeationStoreForTest(t)
	tests := []struct {
		name   string
		mutate func(*store.IdeationCommand)
	}{
		{name: "empty request id", mutate: func(command *store.IdeationCommand) { command.RequestID = "" }},
		{name: "unsafe session id", mutate: func(command *store.IdeationCommand) { command.SessionID = "../x" }},
		{name: "unsafe kind", mutate: func(command *store.IdeationCommand) { command.Kind = "a b" }},
		{name: "negative revision", mutate: func(command *store.IdeationCommand) { command.ExpectedRevision = -1 }},
		{name: "invalid json", mutate: func(command *store.IdeationCommand) { command.Payload = []byte("{") }},
		{name: "oversized payload", mutate: func(command *store.IdeationCommand) {
			command.Payload = append(append([]byte(`"`), make([]byte, store.MaxIdeationCommandPayloadBytes)...), '"')
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := commandForTest("request-x", "session-1", 1)
			test.mutate(&command)
			if _, _, err := opened.AcceptIdeationCommand(t.Context(), command); err == nil {
				t.Fatal("AcceptIdeationCommand() error = nil, want a validation error")
			}
		})
	}
}

// TestIdeationCommandCompletionKeepsTheFirstOutcome verifies that a command
// moves from accepted through applying to one completed outcome, and that a
// later completion cannot change that outcome.
func TestIdeationCommandCompletionKeepsTheFirstOutcome(t *testing.T) {
	t.Parallel()

	opened, _ := openIdeationStoreForTest(t)
	if _, _, err := opened.AcceptIdeationCommand(t.Context(), commandForTest("request-1", "session-1", 1)); err != nil {
		t.Fatal(err)
	}
	applying, err := opened.BeginIdeationCommand(t.Context(), "request-1")
	if err != nil {
		t.Fatalf("BeginIdeationCommand() error = %v", err)
	}
	if applying.State != store.IdeationCommandApplying || applying.Attempts != 1 {
		t.Fatalf("applying = %#v, want applying at attempt 1", applying)
	}
	completed, err := opened.CompleteIdeationCommand(t.Context(), store.IdeationCommandResult{
		RequestID: "request-1", Outcome: store.IdeationCommandConflict, Detail: "session is at revision 4", SessionRevision: 4,
		Result: []byte(`{"current":4}`), At: ideationNow.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("CompleteIdeationCommand() error = %v", err)
	}
	if completed.State != store.IdeationCommandCompleted || completed.Outcome != store.IdeationCommandConflict ||
		completed.SessionRevision != 4 || !completed.CompletedAt.Equal(ideationNow.Add(time.Minute)) {
		t.Fatalf("completed = %#v, want the conflict outcome", completed)
	}
	again, err := opened.CompleteIdeationCommand(t.Context(), store.IdeationCommandResult{RequestID: "request-1", Outcome: store.IdeationCommandApplied})
	if err != nil {
		t.Fatal(err)
	}
	if again.Outcome != store.IdeationCommandConflict {
		t.Fatalf("second completion outcome = %q, want the first outcome", again.Outcome)
	}
	if _, err := opened.BeginIdeationCommand(t.Context(), "request-1"); !errors.Is(err, store.ErrIdeationCommandSettled) {
		t.Fatalf("BeginIdeationCommand() on a completed command error = %v, want ErrIdeationCommandSettled", err)
	}
	if _, err := opened.IdeationCommand(t.Context(), "missing"); !errors.Is(err, store.ErrIdeationCommandNotFound) {
		t.Fatalf("IdeationCommand(missing) error = %v, want ErrIdeationCommandNotFound", err)
	}
	pending, err := opened.PendingIdeationCommands(t.Context())
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending = %#v, %v; want none", pending, err)
	}
}

// TestIdeationWriteCompletesItsCommandAtomically verifies that a session
// mutation that names its command completes that command in the same
// transaction, so a coordinator restart never applies it twice.
func TestIdeationWriteCompletesItsCommandAtomically(t *testing.T) {
	t.Parallel()

	opened, _ := openIdeationStoreForTest(t)
	session := createIdeationSessionForTest(t, opened, "session-1")
	if _, _, err := opened.AcceptIdeationCommand(t.Context(), commandForTest("request-1", session.ID, session.Revision)); err != nil {
		t.Fatal(err)
	}
	write := ideationWrite(session, store.IdeationStateAwaitingUser)
	write.CommandID = "request-1"
	updated, _, err := opened.AppendIdeationTurn(t.Context(), write, store.IdeationTurn{ID: "request-1", Author: store.IdeationTurnAuthorUser, Body: "Who uses this?"})
	if err != nil {
		t.Fatalf("AppendIdeationTurn() error = %v", err)
	}
	command, err := opened.IdeationCommand(t.Context(), "request-1")
	if err != nil {
		t.Fatal(err)
	}
	if command.State != store.IdeationCommandCompleted || command.Outcome != store.IdeationCommandApplied || command.SessionRevision != updated.Revision {
		t.Fatalf("command = %#v, want applied at session revision %d", command, updated.Revision)
	}

	second := ideationWrite(updated, store.IdeationStateDeferred)
	second.CommandID = "request-1"
	if _, err := opened.TransitionIdeationSession(t.Context(), second); !errors.Is(err, store.ErrIdeationCommandSettled) {
		t.Fatalf("second write for a completed command error = %v, want ErrIdeationCommandSettled", err)
	}
	current, err := opened.IdeationSession(t.Context(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Revision != updated.Revision || current.State != store.IdeationStateAwaitingUser {
		t.Fatalf("session = %#v, want the refused write rolled back", current)
	}
}

// TestIdeationCommandsListsSessionActivityNewestFirst verifies the per-session
// activity read that a reconnecting browser uses.
func TestIdeationCommandsListsSessionActivityNewestFirst(t *testing.T) {
	t.Parallel()

	opened, _ := openIdeationStoreForTest(t)
	for _, command := range []store.IdeationCommand{
		commandForTest("request-1", "session-1", 1),
		commandForTest("request-2", "session-2", 1),
		commandForTest("request-3", "session-1", 2),
	} {
		if _, _, err := opened.AcceptIdeationCommand(t.Context(), command); err != nil {
			t.Fatal(err)
		}
	}
	commands, err := opened.IdeationCommands(t.Context(), "session-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 || commands[0].RequestID != "request-3" || commands[1].RequestID != "request-1" {
		t.Fatalf("commands = %#v, want request-3 then request-1", commands)
	}
}

// TestCommandInboxAppendsWithoutMigratingTheStore verifies that the narrow
// browser write handle opens only an existing, current store, and that its
// only write is a command row.
func TestCommandInboxAppendsWithoutMigratingTheStore(t *testing.T) {
	t.Parallel()

	opened, path := openIdeationStoreForTest(t)
	session := createIdeationSessionForTest(t, opened, "session-1")
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	inbox, err := store.OpenCommandInbox(t.Context(), path)
	if err != nil {
		t.Fatalf("OpenCommandInbox() error = %v", err)
	}
	read, err := inbox.IdeationSession(t.Context(), session.ID)
	if err != nil || read.Revision != session.Revision {
		t.Fatalf("inbox session = %#v, %v", read, err)
	}
	if _, _, err := inbox.AcceptIdeationCommand(t.Context(), commandForTest("request-1", session.ID, 1)); err != nil {
		t.Fatalf("inbox AcceptIdeationCommand() error = %v", err)
	}
	if err := inbox.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := store.OpenReadOnly(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	if command, err := reader.IdeationCommand(t.Context(), "request-1"); err != nil || command.State != store.IdeationCommandAccepted {
		t.Fatalf("stored command = %#v, %v", command, err)
	}

	missing := filepath.Join(t.TempDir(), "data", "missing.db")
	if _, err := store.OpenCommandInbox(t.Context(), missing); err == nil {
		t.Fatal("OpenCommandInbox(missing) error = nil, want a refusal")
	}
	if _, err := os.Stat(filepath.Dir(missing)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenCommandInbox created %s: %v", filepath.Dir(missing), err)
	}

	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(t.Context(), "UPDATE schema_metadata SET version = 44 WHERE singleton = 1"); err != nil {
		t.Fatal(err)
	}
	_ = database.Close()
	if _, err := store.OpenCommandInbox(t.Context(), path); err == nil {
		t.Fatal("OpenCommandInbox(older schema) error = nil, want a migration refusal")
	}
}

// TestSchema45MigrationCreatesTheCommandInbox verifies that an older store
// gains an empty command inbox through the normal migration path.
func TestSchema45MigrationCreatesTheCommandInbox(t *testing.T) {
	t.Parallel()

	opened, path := openIdeationStoreForTest(t)
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(t.Context(), "DROP TABLE ideation_commands; UPDATE schema_metadata SET version = 44 WHERE singleton = 1"); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open() migration error = %v", err)
	}
	defer func() { _ = migrated.Close() }()
	if migrated.SchemaVersion() != store.CurrentSchemaVersion {
		t.Fatalf("SchemaVersion() = %d, want %d", migrated.SchemaVersion(), store.CurrentSchemaVersion)
	}
	pending, err := migrated.PendingIdeationCommands(t.Context())
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending = %#v, %v; want an empty inbox", pending, err)
	}
}
