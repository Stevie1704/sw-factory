package store_test

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestCorrectionTurnLinksAnEarlierUserAnswer verifies that a user turn can
// name the earlier user answer it corrects, that the link survives a
// restart and a replay, and that a link to a missing, role, or own turn is
// refused.
func TestCorrectionTurnLinksAnEarlierUserAnswer(t *testing.T) {
	t.Parallel()

	opened, path := openIdeationStoreForTest(t)
	session := createIdeationSessionForTest(t, opened, "session-1")
	session, _ = appendUserTurnForTest(t, opened, session, "turn-a", "Only admins onboard.")
	session, _, err := opened.AppendIdeationTurn(t.Context(), ideationWrite(session, store.IdeationStateAwaitingUser), store.IdeationTurn{
		ID: "turn-b", Author: store.IdeationTurnAuthorRole, Role: "ideation", Body: "Who else onboards?",
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"missing": "turn-x", "role": "turn-b", "own": "turn-c"} {
		_, _, err := opened.AppendIdeationTurn(t.Context(), ideationWrite(session, store.IdeationStateAwaitingUser), store.IdeationTurn{
			ID: "turn-c", Author: store.IdeationTurnAuthorUser, Body: "Every developer onboards.", Corrects: target,
		})
		if err == nil {
			t.Fatalf("%s correction target error = nil, want a refusal", name)
		}
	}
	if _, _, err := opened.AppendIdeationTurn(t.Context(), ideationWrite(session, store.IdeationStateAwaitingUser), store.IdeationTurn{
		ID: "turn-r", Author: store.IdeationTurnAuthorRole, Role: "ideation", Body: "Noted.", Corrects: "turn-a",
	}); err == nil {
		t.Fatal("role turn correction error = nil, want a refusal")
	}
	correction := store.IdeationTurn{ID: "turn-c", Author: store.IdeationTurnAuthorUser, Body: "Every developer onboards.", Corrects: "turn-a"}
	if _, _, err := opened.AppendIdeationTurn(t.Context(), ideationWrite(session, store.IdeationStateAwaitingUser), correction); err != nil {
		t.Fatalf("AppendIdeationTurn(correction) error = %v", err)
	}

	reopened := reopenIdeationStoreForTest(t, opened, path)
	if _, replayed, err := reopened.AppendIdeationTurn(t.Context(), ideationWrite(session, store.IdeationStateAwaitingUser), correction); err != nil || replayed.Corrects != "turn-a" {
		t.Fatalf("replayed correction = %#v, %v; want the stored link", replayed, err)
	}
	changed := correction
	changed.Corrects = ""
	if _, _, err := reopened.AppendIdeationTurn(t.Context(), ideationWrite(session, store.IdeationStateAwaitingUser), changed); !errors.Is(err, store.ErrIdeationRequestReused) {
		t.Fatalf("replay without the link error = %v, want ErrIdeationRequestReused", err)
	}
	history, err := reopened.IdeationHistory(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Turns) != 3 || history.Turns[2].Corrects != "turn-a" || history.Turns[0].Corrects != "" {
		t.Fatalf("turns = %#v, want turn-c linked to turn-a", history.Turns)
	}
}

// TestSchema46MigrationAddsTheCorrectionLink verifies that a store at schema
// 45 keeps its turns and gains an empty correction link.
func TestSchema46MigrationAddsTheCorrectionLink(t *testing.T) {
	t.Parallel()

	opened, path := openIdeationStoreForTest(t)
	session := createIdeationSessionForTest(t, opened, "session-1")
	appendUserTurnForTest(t, opened, session, "turn-a", "Only admins onboard.")
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(t.Context(), "ALTER TABLE ideation_turns DROP COLUMN corrects; UPDATE schema_metadata SET version = 45 WHERE singleton = 1"); err != nil {
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
	history, err := migrated.IdeationHistory(t.Context(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if migrated.SchemaVersion() != store.CurrentSchemaVersion || len(history.Turns) != 1 || history.Turns[0].Corrects != "" {
		t.Fatalf("schema %d, turns %#v; want the current schema and the kept turn", migrated.SchemaVersion(), history.Turns)
	}
}
