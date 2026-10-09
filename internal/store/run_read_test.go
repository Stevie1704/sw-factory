package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/store"
)

// TestReadOnlyStoreListsRunsWithTheirInvocationsAndGateResults verifies the
// run read model works on a read-only store: runs newest update first, one
// run by identity, its invocations in update order, and every retained gate
// result ordered by phase, checkpoint, and declaration order.
func TestReadOnlyStoreListsRunsWithTheirInvocationsAndGateResults(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state", "factory.db")
	opened, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for index, status := range []store.Status{store.StatusComplete, store.StatusActive} {
		id := []string{"run-old", "run-new"}[index]
		if err := opened.SaveRun(t.Context(), store.Run{
			ID: id, RepositoryPath: "/repo", IssueNumber: 10 + index,
			Stage: store.StageImplementation, Status: status,
			Branch: "factory/" + id, Worktree: "/worktrees/" + id,
			CreatedAt: base, UpdatedAt: base.Add(time.Duration(index) * time.Hour),
		}); err != nil {
			t.Fatalf("SaveRun(%s) error = %v", id, err)
		}
	}
	for index, id := range []string{"inv-second", "inv-first"} {
		if err := opened.SaveInvocation(t.Context(), store.Invocation{
			ID: id, RunID: "run-new", Harness: "codex", Role: "implementation",
			Stage: store.StageImplementation, Status: store.InvocationStatusCompleted,
			CreatedAt: base, UpdatedAt: base.Add(time.Duration(1-index) * time.Minute),
		}); err != nil {
			t.Fatalf("SaveInvocation(%s) error = %v", id, err)
		}
	}
	firstSHA := "0123456789abcdef0123456789abcdef01234567"
	secondSHA := "abcdefabcdefabcdefabcdefabcdefabcdefabcd"
	if err := opened.SaveGateResults(t.Context(), []store.GateResult{
		{RunID: "run-new", CheckpointSHA: secondSHA, Phase: store.GatePhaseCheckpoint, Ordinal: 1, GateName: "test", Outcome: store.GateOutcomeFailed, Status: "failure", Blocking: true},
		{RunID: "run-new", CheckpointSHA: secondSHA, Phase: store.GatePhaseCheckpoint, Ordinal: 0, GateName: "format", Outcome: store.GateOutcomePassed, Status: "success", Blocking: true},
		{RunID: "run-new", CheckpointSHA: firstSHA, Phase: store.GatePhaseBaseline, Ordinal: 0, GateName: "format", Outcome: store.GateOutcomePassed, Status: "success", Blocking: true},
		{RunID: "run-old", CheckpointSHA: firstSHA, Phase: store.GatePhaseBaseline, Ordinal: 0, GateName: "format", Outcome: store.GateOutcomePassed, Status: "success", Blocking: true},
	}); err != nil {
		t.Fatalf("SaveGateResults() error = %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := store.OpenReadOnly(t.Context(), path)
	if err != nil {
		t.Fatalf("OpenReadOnly() error = %v", err)
	}
	defer func() { _ = reader.Close() }()

	runs, err := reader.ListRuns(t.Context())
	if err != nil {
		t.Fatalf("ListRuns() error = %v", err)
	}
	if len(runs) != 2 || runs[0].ID != "run-new" || runs[1].ID != "run-old" || runs[0].Branch != "factory/run-new" {
		t.Fatalf("ListRuns() = %#v, want run-new then run-old", runs)
	}
	run, err := reader.Run(t.Context(), "run-old")
	if err != nil || run == nil || run.IssueNumber != 10 {
		t.Fatalf("Run(run-old) = %#v, %v; want issue 10", run, err)
	}
	missing, err := reader.Run(t.Context(), "run-missing")
	if err != nil || missing != nil {
		t.Fatalf("Run(run-missing) = %#v, %v; want nil, nil", missing, err)
	}
	invocations, err := reader.Invocations(t.Context(), "run-new")
	if err != nil {
		t.Fatalf("Invocations() error = %v", err)
	}
	if len(invocations) != 2 || invocations[0].ID != "inv-first" || invocations[1].ID != "inv-second" {
		t.Fatalf("Invocations() = %#v, want inv-first then inv-second", invocations)
	}
	gates, err := reader.RunGateResults(t.Context(), "run-new")
	if err != nil {
		t.Fatalf("RunGateResults() error = %v", err)
	}
	want := []struct {
		phase store.GatePhase
		sha   string
		name  string
	}{
		{store.GatePhaseBaseline, firstSHA, "format"},
		{store.GatePhaseCheckpoint, secondSHA, "format"},
		{store.GatePhaseCheckpoint, secondSHA, "test"},
	}
	if len(gates) != len(want) {
		t.Fatalf("RunGateResults() = %#v, want %d results", gates, len(want))
	}
	for index, expected := range want {
		got := gates[index]
		if got.Phase != expected.phase || got.CheckpointSHA != expected.sha || got.GateName != expected.name {
			t.Fatalf("RunGateResults()[%d] = %#v, want %v", index, got, expected)
		}
	}
}

// TestReadOnlyStoreWaitsForACommittingWriter verifies a read-only reader
// waits for a short write lock instead of failing at once with SQLITE_BUSY,
// so a page read during a coordinator commit still succeeds.
func TestReadOnlyStoreWaitsForACommittingWriter(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state", "factory.db")
	opened, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := store.OpenReadOnly(t.Context(), path)
	if err != nil {
		t.Fatalf("OpenReadOnly() error = %v", err)
	}
	defer func() { _ = reader.Close() }()

	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	connection, err := writer.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if _, err := connection.ExecContext(t.Context(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := reader.ListRuns(context.Background())
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if _, err := connection.ExecContext(t.Context(), "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("ListRuns() during a write lock error = %v, want a wait for the writer", err)
	}
}
