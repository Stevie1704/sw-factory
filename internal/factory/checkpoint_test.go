package factory

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/checkpoint"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/version"
)

// checkpointExternal is an in-memory tracker, code host, and Git remote
// that knows the objects of checkpointRun. An unreachable object is an
// error.
type checkpointExternal struct {
	unreachable string
}

// BranchHead returns a fixed head for every branch.
func (f checkpointExternal) BranchHead(_ context.Context, branch string) (string, error) {
	if f.unreachable == "branch" {
		return "", errors.New("remote unreachable")
	}
	return "head-of-" + branch, nil
}

// PullRequest returns an open draft pull request.
func (f checkpointExternal) PullRequest(_ context.Context, number int, headBranch string) (checkpoint.PullRequestState, error) {
	if f.unreachable == "pull_request" {
		return checkpoint.PullRequestState{}, errors.New("code host unreachable")
	}
	return checkpoint.PullRequestState{Number: number, HeadBranch: headBranch, HeadSHA: "head-of-" + headBranch, State: "merged"}, nil
}

// Issue returns a closed issue with the complete label.
func (f checkpointExternal) Issue(_ context.Context, number int) (checkpoint.IssueState, error) {
	if f.unreachable == "issue" {
		return checkpoint.IssueState{}, errors.New("tracker unreachable")
	}
	return checkpoint.IssueState{Number: number, FactoryLabels: []string{"agent-complete"}}, nil
}

// StatusComment returns the status comment that checkpointRun records.
func (f checkpointExternal) StatusComment(_ context.Context, issueNumber int, runID string) (checkpoint.StatusCommentState, error) {
	if f.unreachable == "status_comment" {
		return checkpoint.StatusCommentState{}, errors.New("tracker unreachable")
	}
	return checkpoint.StatusCommentState{RunID: runID, IssueNumber: issueNumber, ID: "comment-9", UpdatedAt: time.Unix(500, 0).UTC()}, nil
}

// checkpointFixture is a registered installation with a quiescent store and
// a private directory for checkpoints.
type checkpointFixture struct {
	registration config.RepositoryRegistration
	destinations string
}

// newCheckpointFixture creates the installation. Its one run is complete
// and names a branch, a pull request, an issue, and a status comment.
func newCheckpointFixture(t *testing.T) checkpointFixture {
	t.Helper()
	root := t.TempDir()
	fixture := checkpointFixture{
		registration: config.RepositoryRegistration{
			Path:                filepath.Join(root, "repository"),
			GitHub:              config.GitHubConfig{Owner: "example", Repository: "service"},
			OperationalDataPath: filepath.Join(root, "data", "factory.db"),
			Authentication: config.AuthenticationConfig{
				CodexAuthPath:   filepath.Join(root, "secret-codex-auth.json"),
				ClaudeAuthPath:  filepath.Join(root, "secret-claude-credentials.json"),
				CopilotAuthPath: filepath.Join(root, "secret-copilot-token"),
			},
		},
		destinations: filepath.Join(root, "checkpoints"),
	}
	for _, directory := range []string{fixture.registration.Path, fixture.destinations} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	opened, err := store.Open(context.Background(), fixture.registration.OperationalDataPath)
	if err != nil {
		t.Fatal(err)
	}
	run := store.Run{ID: "run-1", RepositoryPath: fixture.registration.Path, IssueNumber: 107, Stage: store.StageReady, Status: store.StatusComplete, Branch: "factory/run-1", StatusCommentID: "comment-9", PullRequestNumber: 7, PullRequestURL: "https://github.com/example/service/pull/7", CreatedAt: time.Unix(100, 0).UTC(), UpdatedAt: time.Unix(100, 0).UTC()}
	if err := opened.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// request returns a checkpoint request to the named new directory.
func (f checkpointFixture) request(name string) CheckpointRequest {
	return CheckpointRequest{
		ConfigPath:   "/host/config.yaml",
		Registration: f.registration,
		Output:       filepath.Join(f.destinations, name),
		TargetBranch: "main",
		WorkerImage:  "factory-worker@sha256:" + strings.Repeat("a", 64),
		Build:        version.Identity{Version: "v1.2.3", Build: "release", Revision: "abc123"},
		External:     checkpointExternal{},
		Now:          func() time.Time { return time.Unix(1000, 0) },
	}
}

// execStore runs one statement on the store of the fixture.
func (f checkpointFixture) execStore(t *testing.T, statement string, args ...any) {
	t.Helper()
	database, err := sql.Open("sqlite", f.registration.OperationalDataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if _, err := database.Exec(statement, args...); err != nil {
		t.Fatal(err)
	}
}

// assertNothingWritten fails when the destination directory holds any entry.
func (f checkpointFixture) assertNothingWritten(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(f.destinations)
	if err != nil || len(entries) != 0 {
		t.Fatalf("destination directory entries = %v, %v; want none", entries, err)
	}
}

// assertRefusal fails unless the result has exactly the refusal codes.
func assertRefusal(t *testing.T, result CheckpointResult, err error, codes ...string) {
	t.Helper()
	if err != nil {
		t.Fatalf("CreateCheckpoint() error = %v", err)
	}
	got := make([]string, 0, len(result.Refusals))
	for _, refusal := range result.Refusals {
		got = append(got, refusal.Code)
	}
	if strings.Join(got, ",") != strings.Join(codes, ",") || result.Manifest != nil {
		t.Fatalf("refusals = %+v, manifest = %v; want only %v", result.Refusals, result.Manifest, codes)
	}
}

// TestCreateCheckpointOfAQuiescentInstallation verifies the created
// checkpoint: a 0700 directory with 0600 files whose checksums verify, the
// external state of every known object, and a coordinator lock held during
// the snapshot.
func TestCreateCheckpointOfAQuiescentInstallation(t *testing.T) {
	t.Parallel()
	fixture := newCheckpointFixture(t)
	request := fixture.request("first")
	lockHeld := false
	request.afterSnapshot = func() {
		record, err := ObserveCoordinatorLock(fixture.registration)
		lockHeld = err == nil && record.Held && record.Holder == LockHolderCheckpoint
	}

	result, err := CreateCheckpoint(context.Background(), request)
	if err != nil || len(result.Refusals) != 0 || result.Manifest == nil {
		t.Fatalf("CreateCheckpoint() = %+v, %v; want a checkpoint", result, err)
	}
	if !lockHeld {
		t.Fatal("the coordinator lock was not held by the checkpoint during the snapshot")
	}
	if held, err := coordinatorLockHeld(coordinatorLockPath(fixture.registration)); err != nil || held {
		t.Fatalf("lock held after the checkpoint = %v, %v; want released", held, err)
	}
	info, err := os.Stat(request.Output)
	if err != nil || info.Mode().Perm() != 0o700 || result.Path != request.Output {
		t.Fatalf("checkpoint directory = %v, %v at %q; want 0700 at %q", info, err, result.Path, request.Output)
	}
	manifest, err := checkpoint.Verify(context.Background(), request.Output)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	external := manifest.ExternalState
	if external.TargetBranch.Head != "head-of-main" || manifest.TargetBranchHead != "head-of-main" || len(external.Branches) != 1 || len(external.PullRequests) != 1 || len(external.Issues) != 1 || len(external.StatusComments) != 1 {
		t.Fatalf("external state = %+v, want the target branch and each object of run-1", external)
	}
	if manifest.Factory != (checkpoint.Factory{Version: "v1.2.3", Build: "release", Revision: "abc123"}) || manifest.RegistrationIdentity == "" || manifest.RegistrationSummary.Repository != "service" || manifest.WorkerImage != request.WorkerImage || manifest.Watermarks.Runs.Count != 1 {
		t.Fatalf("manifest = %+v, want the build, registration, worker image, and watermarks", manifest)
	}
	if !manifest.CreatedAt.Equal(time.Unix(1000, 0)) {
		t.Fatalf("created_at = %v, want the snapshot transaction time", manifest.CreatedAt)
	}
}

// TestCreateCheckpointManifestHoldsNoCredentialPath scans the written
// manifest for each credential path of the registration.
func TestCreateCheckpointManifestHoldsNoCredentialPath(t *testing.T) {
	t.Parallel()
	fixture := newCheckpointFixture(t)
	request := fixture.request("first")
	if _, err := CreateCheckpoint(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(request.Output, checkpoint.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	authentication := fixture.registration.Authentication
	for _, secret := range []string{authentication.CodexAuthPath, authentication.ClaudeAuthPath, authentication.CopilotAuthPath, "secret"} {
		if bytes.Contains(content, []byte(secret)) {
			t.Fatalf("the manifest contains the credential path %q", secret)
		}
	}
}

// TestCreateCheckpointRefusesEachNonQuiescentCondition verifies that each
// full-quiescence condition refuses, names that condition, and writes
// nothing.
func TestCreateCheckpointRefusesEachNonQuiescentCondition(t *testing.T) {
	t.Parallel()
	for condition, statement := range map[string]string{
		"run.active":                "UPDATE operational_runs SET status = 'active'",
		"effect.pending":            `INSERT INTO pending_effects (run_id, effect_id, kind, payload, created_at, updated_at) VALUES ('run-1', 'effect-1', 'status_comment', '{}', '2026-11-01T00:00:00Z', '2026-11-01T00:00:00Z')`,
		"invocation.active":         `INSERT INTO invocations (id, run_id, harness, role, stage, status, created_at, updated_at) VALUES ('inv-1', 'run-1', 'codex', 'implementation', 'implementation', 'active', '2026-11-01T00:00:00Z', '2026-11-01T00:00:00Z')`,
		"session_invocation.active": `INSERT INTO ideation_invocations (session_id, id, role, trigger_id, session_revision, context_revision, harness, prompt_version, prompt_sha256, worker_id, status, created_at, updated_at) VALUES ('session-1', 'session-inv-1', 'ideation', 'turn-1', 1, 'context-1', 'codex', 'ideation-v1', 'digest', 'worker-1', 'active', '2026-11-01T00:00:00Z', '2026-11-01T00:00:00Z')`,
		"session_command.queued":    `INSERT INTO ideation_commands (request_id, session_id, kind, expected_revision, payload, state, accepted_at) VALUES ('request-1', 'session-1', 'submit_turn', 1, '{}', 'accepted', '2026-11-01T00:00:00Z')`,
		"publication.in_progress":   `INSERT INTO ideation_sessions (id, repository_path, state, revision, created_at, updated_at) VALUES ('session-1', '/work/service', 'publishing', 1, '2026-11-01T00:00:00Z', '2026-11-01T00:00:00Z')`,
	} {
		fixture := newCheckpointFixture(t)
		fixture.execStore(t, statement)
		result, err := CreateCheckpoint(context.Background(), fixture.request("first"))
		assertRefusal(t, result, err, CheckpointNotQuiescent)
		if !strings.HasPrefix(result.Refusals[0].Detail, condition+": ") {
			t.Fatalf("%s refusal detail = %q, want it to name the condition", condition, result.Refusals[0].Detail)
		}
		fixture.assertNothingWritten(t)
	}
}

// TestCreateCheckpointRefusesAHeldCoordinatorLock verifies that a live
// coordinator refuses the checkpoint.
func TestCreateCheckpointRefusesAHeldCoordinatorLock(t *testing.T) {
	t.Parallel()
	fixture := newCheckpointFixture(t)
	owner, err := acquireCoordinatorLock(coordinatorLockPath(fixture.registration), LockHolderCoordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.release() }()

	result, err := CreateCheckpoint(context.Background(), fixture.request("first"))
	assertRefusal(t, result, err, CheckpointCoordinatorRunning)
	fixture.assertNothingWritten(t)
}

// TestCreateCheckpointRefusesAnOlderSchema verifies that a checkpoint never
// migrates and leaves the store bytes unchanged.
func TestCreateCheckpointRefusesAnOlderSchema(t *testing.T) {
	t.Parallel()
	fixture := newCheckpointFixture(t)
	fixture.execStore(t, "UPDATE schema_metadata SET version = ?", store.CurrentSchemaVersion-1)
	before, err := os.ReadFile(fixture.registration.OperationalDataPath)
	if err != nil {
		t.Fatal(err)
	}

	result, err := CreateCheckpoint(context.Background(), fixture.request("first"))
	assertRefusal(t, result, err, CheckpointMigrationRequired)
	after, err := os.ReadFile(fixture.registration.OperationalDataPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the store bytes changed: %v", err)
	}
	fixture.assertNothingWritten(t)
}

// TestCreateCheckpointRefusesAMissingStore verifies the store_missing
// refusal.
func TestCreateCheckpointRefusesAMissingStore(t *testing.T) {
	t.Parallel()
	fixture := newCheckpointFixture(t)
	if err := os.RemoveAll(filepath.Dir(fixture.registration.OperationalDataPath)); err != nil {
		t.Fatal(err)
	}
	result, err := CreateCheckpoint(context.Background(), fixture.request("first"))
	assertRefusal(t, result, err, CheckpointStoreMissing)
	fixture.assertNothingWritten(t)
}

// TestCreateCheckpointRefusesAnUnsafeDestination verifies that a
// destination inside the checkout, inside the store directory, in a
// directory that group or other can read, or at an existing path is
// refused.
func TestCreateCheckpointRefusesAnUnsafeDestination(t *testing.T) {
	t.Parallel()
	for name, destination := range map[string]func(t *testing.T, fixture checkpointFixture) string{
		"inside the checkout": func(_ *testing.T, fixture checkpointFixture) string {
			return filepath.Join(fixture.registration.Path, "checkpoint")
		},
		"inside the store directory": func(_ *testing.T, fixture checkpointFixture) string {
			return filepath.Join(filepath.Dir(fixture.registration.OperationalDataPath), "checkpoint")
		},
		"group-readable directory": func(t *testing.T, fixture checkpointFixture) string {
			if err := os.Chmod(fixture.destinations, 0o750); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(fixture.destinations, "checkpoint")
		},
		"existing path": func(t *testing.T, fixture checkpointFixture) string {
			path := filepath.Join(fixture.destinations, "checkpoint")
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			return path
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newCheckpointFixture(t)
			request := fixture.request("unused")
			request.Output = destination(t, fixture)
			result, err := CreateCheckpoint(context.Background(), request)
			assertRefusal(t, result, err, CheckpointDestinationRefused)
		})
	}
}

// TestCreateCheckpointRefusesAnUnreadableExternalObject verifies that each
// known object that cannot be read refuses with external_unverified and
// writes nothing.
func TestCreateCheckpointRefusesAnUnreadableExternalObject(t *testing.T) {
	t.Parallel()
	for _, object := range []string{"branch", "pull_request", "issue", "status_comment"} {
		fixture := newCheckpointFixture(t)
		request := fixture.request("first")
		request.External = checkpointExternal{unreachable: object}
		result, err := CreateCheckpoint(context.Background(), request)
		if err != nil || len(result.Refusals) == 0 || result.Refusals[0].Code != CheckpointExternalUnverified {
			t.Fatalf("%s: result = %+v, %v; want external_unverified", object, result, err)
		}
		fixture.assertNothingWritten(t)
	}
}

// crashHelperEnvironment names the destination of the crash helper.
const crashHelperEnvironment = "FACTORY_CHECKPOINT_CRASH_DESTINATION"

// TestCreateCheckpointCrashHelper is not a test on its own. Started by
// TestCreateCheckpointCrashLeavesNoCheckpoint, it creates a checkpoint and
// exits the process between VACUUM INTO and the manifest write.
func TestCreateCheckpointCrashHelper(t *testing.T) {
	destinations := os.Getenv(crashHelperEnvironment)
	if destinations == "" {
		t.Skip("runs only as the crash helper process")
	}
	fixture := newCheckpointFixture(t)
	fixture.destinations = destinations
	request := fixture.request("first")
	request.afterSnapshot = func() { os.Exit(3) }
	result, err := CreateCheckpoint(context.Background(), request)
	t.Fatalf("the crash point was not reached: %+v, %v", result, err)
}

// TestCreateCheckpointCrashLeavesNoCheckpoint kills a checkpoint between
// VACUUM INTO and the manifest write and verifies that no directory exists
// at the final path.
func TestCreateCheckpointCrashLeavesNoCheckpoint(t *testing.T) {
	t.Parallel()
	destinations := filepath.Join(t.TempDir(), "checkpoints")
	if err := os.Mkdir(destinations, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestCreateCheckpointCrashHelper$")
	command.Env = append(os.Environ(), crashHelperEnvironment+"="+destinations)
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("crash helper = %v, want exit status 3 at the crash point:\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(destinations, "first")); !os.IsNotExist(err) {
		t.Fatalf("a checkpoint exists at the final path after the crash: %v", err)
	}
}
