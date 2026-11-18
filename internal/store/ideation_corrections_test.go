package store_test

import (
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
