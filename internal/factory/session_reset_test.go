package factory_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

// RemoveContextSnapshot records and deletes one session snapshot worktree.
func (g *headlessRemovalGit) RemoveContextSnapshot(_ context.Context, _ string, snapshot gitadapter.ContextSnapshot) error {
	g.snapshots = append(g.snapshots, snapshot)
	return os.RemoveAll(snapshot.Worktree)
}

// CreateContextSnapshot is outside the removal test contract.
func (*headlessRemovalGit) CreateContextSnapshot(context.Context, string, string, string) (gitadapter.ContextSnapshot, error) {
	return gitadapter.ContextSnapshot{}, errors.New("unexpected snapshot")
}

// sessionResourcesForTest are the host resources a session-role invocation
// left when the coordinator stopped while it was active.
type sessionResourcesForTest struct {
	snapshot, packetRoot string
}

// addActiveSessionRole stores session-1 with an ideation invocation that was
// active when the coordinator stopped, and creates its snapshot worktree and
// packet directory beside the run worktrees.
func (f headlessRemovalFixture) addActiveSessionRole(t *testing.T) sessionResourcesForTest {
	t.Helper()
	worktrees := filepath.Dir(f.worktreePath)
	resources := sessionResourcesForTest{
		snapshot:   filepath.Join(worktrees, "invocation-1"),
		packetRoot: filepath.Join(worktrees, ".factory-sessions", "session-1"),
	}
	invocationRoot := filepath.Join(resources.packetRoot, "invocation-1")
	for _, path := range []string{resources.snapshot, filepath.Join(invocationRoot, "packet"), filepath.Join(invocationRoot, "results")} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	opened, err := store.Open(t.Context(), f.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	when := time.Now().UTC().Add(-time.Hour)
	session, err := opened.CreateIdeationSession(t.Context(), store.IdeationSession{
		ID: "session-1", RepositoryPath: f.repositoryPath, State: store.IdeationStateAwaitingUser, CreatedAt: when,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, _, err = opened.AppendIdeationTurn(t.Context(), store.IdeationWrite{
		SessionID: session.ID, ExpectedRevision: session.Revision, State: store.IdeationStateAwaitingUser, At: when,
	}, store.IdeationTurn{ID: "turn-1", Author: store.IdeationTurnAuthorUser, Body: "Export run history."})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := opened.StartSessionInvocation(t.Context(), store.IdeationWrite{
		SessionID: session.ID, ExpectedRevision: session.Revision, State: store.IdeationStateRoleActive, At: when,
	}, store.SessionInvocation{
		ID: "invocation-1", Role: "ideation", TriggerID: "turn-1", ContextRevision: "0123456789abcdef0123456789abcdef01234567",
		Harness: "codex", Model: "gpt", PromptVersion: "ideation-v2", PromptSHA256: "b2c88fe7a651a61953d3c9f1d0d5ea219cdfa0cd4a5090e4ef0440e19bbc134d",
		Image: "ghcr.io/example/worker", ImageDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		CredentialStoreID: f.repositoryPath, WorkerID: "session-session-1-ideation", SnapshotPath: resources.snapshot,
		InvocationDirectory: filepath.Join(invocationRoot, "packet"), ResultDirectory: filepath.Join(invocationRoot, "results"),
	}); err != nil {
		t.Fatal(err)
	}
	return resources
}

// TestResetRemovesSessionRoleResources verifies that a reset previews and
// then removes the Docker role resources of every session role, the snapshot
// worktree of a role that was active when the coordinator stopped, and the
// session packet directories, so no manual step remains.
func TestResetRemovesSessionRoleResources(t *testing.T) {
	fixture := newHeadlessRemovalFixture(t, "codex")
	resources := fixture.addActiveSessionRole(t)

	preview, err := fixture.service.Reset(t.Context(), factory.ResetRequest{})
	var confirmation *factory.ResetConfirmationRequiredError
	if !errors.As(err, &confirmation) || len(preview.Plan.Sessions) != 1 {
		t.Fatalf("Reset(preview) = %#v, %v; want one session in the plan", preview.Plan, err)
	}
	planned := preview.Plan.Sessions[0]
	if planned.SessionID != "session-1" || !slices.Equal(planned.WorkerIDs, []string{"session-session-1-ideation"}) ||
		!slices.Equal(planned.Roles, []string{"ideation"}) || !slices.Equal(planned.Snapshots, []string{resources.snapshot}) ||
		!slices.Equal(planned.PacketRoots, []string{resources.packetRoot}) {
		t.Fatalf("planned session = %#v", planned)
	}
	if len(fixture.worker.cleanups) != 0 || len(fixture.git.snapshots) != 0 {
		t.Fatal("reset preview removed a session resource")
	}

	result, err := fixture.service.Reset(t.Context(), factory.ResetRequest{Confirm: true})
	if err != nil {
		t.Fatalf("Reset(confirm) = %#v, %v", result, err)
	}
	want := worker.CleanupRequest{RunID: "session-session-1", WorkerIDs: []string{"session-session-1-ideation"}, Roles: []string{"ideation"}}
	if !slices.ContainsFunc(fixture.worker.cleanups, func(request worker.CleanupRequest) bool {
		return request.RunID == want.RunID && slices.Equal(request.WorkerIDs, want.WorkerIDs) && slices.Equal(request.Roles, want.Roles)
	}) {
		t.Fatalf("worker cleanups = %#v, want the session role resources", fixture.worker.cleanups)
	}
	for _, path := range []string{resources.snapshot, resources.packetRoot} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("session target %q error = %v, want absent", path, err)
		}
	}
}
