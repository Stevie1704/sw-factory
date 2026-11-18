package factory

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/checkpoint"
	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/doctor"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
	"github.com/Stevie1704/sw-factory/internal/version"
)

// restoreRemote is a fake Git remote with one head per branch.
type restoreRemote struct {
	heads map[string]string
	err   error
}

// RemoteBranchHead returns the head of one branch, or "" for no branch.
func (r *restoreRemote) RemoteBranchHead(_ context.Context, request gitadapter.PushRequest) (string, error) {
	return r.heads[request.Branch], r.err
}

// RemoteBranches lists the branches below prefix.
func (r *restoreRemote) RemoteBranches(_ context.Context, _ string, prefix string) ([]string, error) {
	var names []string
	for name := range r.heads {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, r.err
}

// restoreTracker is a fake work tracker that only reads. Any write panics
// through the nil embedded client.
type restoreTracker struct {
	tracker.Client
	issues  map[int]tracker.Issue
	comment tracker.Comment
	err     error
}

// Issue returns one issue.
func (f *restoreTracker) Issue(_ context.Context, _ tracker.Repository, number int) (tracker.Issue, error) {
	return f.issues[number], f.err
}

// FindStatusComment returns the status comment.
func (f *restoreTracker) FindStatusComment(context.Context, tracker.Repository, int, string) (tracker.Comment, error) {
	return f.comment, f.err
}

// ListIssuesWithLabels lists the issues that carry any of labels.
func (f *restoreTracker) ListIssuesWithLabels(_ context.Context, _ tracker.Repository, labels []string) ([]tracker.Issue, error) {
	var found []tracker.Issue
	for _, issue := range f.issues {
		if slices.ContainsFunc(issue.Labels, func(label string) bool { return slices.Contains(labels, label) }) {
			found = append(found, issue)
		}
	}
	return found, f.err
}

// restoreCodeHost is a fake code host that only reads.
type restoreCodeHost struct {
	codehost.PullRequestClient
	pullRequests []codehost.PullRequest
	err          error
}

// FindPullRequest returns the pull request from headBranch.
func (f *restoreCodeHost) FindPullRequest(_ context.Context, _ tracker.Repository, headBranch, _ string) (codehost.PullRequest, error) {
	for _, pullRequest := range f.pullRequests {
		if pullRequest.HeadBranch == headBranch {
			return pullRequest, f.err
		}
	}
	return codehost.PullRequest{}, f.err
}

// ListPullRequestsByHeadPrefix lists the pull requests whose head branch
// starts with prefix.
func (f *restoreCodeHost) ListPullRequestsByHeadPrefix(_ context.Context, _ tracker.Repository, prefix string) ([]codehost.PullRequest, error) {
	var found []codehost.PullRequest
	for _, pullRequest := range f.pullRequests {
		if strings.HasPrefix(pullRequest.HeadBranch, prefix) {
			found = append(found, pullRequest)
		}
	}
	return found, f.err
}

// restoreAdapters are the fake tracker, code host, and remote of one
// installation, in the state that the fixture run left.
type restoreAdapters struct {
	remote   *restoreRemote
	tracker  *restoreTracker
	codeHost *restoreCodeHost
}

// newRestoreAdapters returns the external state of the complete run-1.
func newRestoreAdapters() restoreAdapters {
	return restoreAdapters{
		remote:   &restoreRemote{heads: map[string]string{"main": "aaa", "factory/run-1": "bbb"}},
		tracker:  &restoreTracker{issues: map[int]tracker.Issue{107: {Number: 107, State: "closed", Labels: []string{tracker.LabelAgentComplete}}}, comment: tracker.Comment{ID: "comment-9", UpdatedAt: time.Unix(500, 0).UTC()}},
		codeHost: &restoreCodeHost{pullRequests: []codehost.PullRequest{{Number: 7, State: "closed", Merged: true, HeadBranch: "factory/run-1", HeadSHA: "bbb"}}},
	}
}

// sources returns the read seams of the adapters.
func (a restoreAdapters) sources() ExternalStateSources {
	return ExternalStateSources{Tracker: a.tracker, PullRequests: a.codeHost, Remote: a.remote, TargetBranch: "main"}
}

// restoreFixture is a quiescent installation with a checkpoint of its store.
type restoreFixture struct {
	checkpointFixture
	adapters   restoreAdapters
	checkpoint string
}

// newRestoreFixture creates the installation and its checkpoint through the
// fake adapters.
func newRestoreFixture(t *testing.T) restoreFixture {
	t.Helper()
	fixture := restoreFixture{checkpointFixture: newCheckpointFixture(t), adapters: newRestoreAdapters()}
	request := fixture.request("checkpoint")
	request.External = NewExternalStateReader(fixture.registration, fixture.adapters.sources())
	result, err := CreateCheckpoint(context.Background(), request)
	if err != nil || len(result.Refusals) != 0 {
		t.Fatalf("CreateCheckpoint() = %+v, %v", result, err)
	}
	fixture.checkpoint = result.Path
	return fixture
}

// restoreRequest returns a preview request of the fixture checkpoint.
func (f restoreFixture) restoreRequest() RestoreRequest {
	reader := NewExternalStateReader(f.registration, f.adapters.sources())
	return RestoreRequest{
		ConfigPath:   "/host/config.yaml",
		Registration: f.registration,
		Path:         f.checkpoint,
		Build:        version.Identity{Version: "v1.2.3", Build: "release", Revision: "abc123"},
		External:     reader,
		Owned:        NewOwnedObjectLister(f.registration, f.adapters.sources()),
		Workers:      idleWorkers{},
	}
}

// storeBytes returns the bytes of the live store.
func (f restoreFixture) storeBytes(t *testing.T) []byte {
	t.Helper()
	content, err := os.ReadFile(f.registration.OperationalDataPath)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// assertRestoreRefusal fails unless the result is refused with exactly the
// codes, in order, and the live store bytes did not change.
func assertRestoreRefusal(t *testing.T, fixture restoreFixture, before []byte, result RestoreResult, err error, codes ...string) {
	t.Helper()
	if err != nil {
		t.Fatalf("RestoreCheckpoint() error = %v", err)
	}
	got := make([]string, 0, len(result.Refusals))
	for _, refusal := range result.Refusals {
		got = append(got, refusal.Code)
	}
	if result.Outcome != RestoreOutcomeRefused || strings.Join(got, ",") != strings.Join(codes, ",") {
		t.Fatalf("result = %s %+v; want refused with %v", result.Outcome, result.Refusals, codes)
	}
	if before != nil && !bytes.Equal(before, fixture.storeBytes(t)) {
		t.Fatal("a refused restore changed the live store")
	}
}

// preview runs a restore preview and fails unless it returns a plan.
func preview(t *testing.T, request RestoreRequest) RestorePlan {
	t.Helper()
	result, err := RestoreCheckpoint(context.Background(), request)
	if err != nil || result.Outcome != RestoreOutcomePreview || result.Plan == nil || result.Plan.Digest == "" {
		t.Fatalf("preview = %+v, %v; want a plan with a digest", result, err)
	}
	return *result.Plan
}

// TestRestoreCheckpointPreviewThenConfirm covers the healthy row: the
// preview changes nothing; the confirm with its digest restores, keeps the
// original, and leaves a store that factory doctor reports ready. No row
// checks the pinned worker image: a missing image does not stop a restore.
func TestRestoreCheckpointPreviewThenConfirm(t *testing.T) {
	t.Parallel()
	fixture := newRestoreFixture(t)
	before := fixture.storeBytes(t)
	request := fixture.restoreRequest()

	plan := preview(t, request)
	if !bytes.Equal(before, fixture.storeBytes(t)) {
		t.Fatal("the preview changed the live store")
	}
	if len(plan.Steps) == 0 || len(plan.PreservedPaths) != 1 || !strings.HasSuffix(plan.PreservedPaths[0], filepath.Join("original", "factory.db")) {
		t.Fatalf("plan = %+v, want steps and the preserved store", plan)
	}

	request.Confirm = plan.Digest
	result, err := RestoreCheckpoint(context.Background(), request)
	if err != nil || result.Outcome != RestoreOutcomeRestored {
		t.Fatalf("confirm = %+v, %v; want restored", result, err)
	}
	preserved, err := os.ReadFile(plan.PreservedPaths[0])
	if err != nil || !bytes.Equal(preserved, before) {
		t.Fatalf("preserved original = %v; want the original bytes", err)
	}
	snapshot, err := os.ReadFile(filepath.Join(fixture.checkpoint, checkpoint.StoreFile))
	if err != nil || !bytes.Equal(snapshot, fixture.storeBytes(t)) {
		t.Fatalf("live store is not the snapshot: %v", err)
	}
	check := doctor.Run(context.Background(), store.StartupCheck(fixture.registration.OperationalDataPath)).Results[0]
	if check.Code != "store.sqlite.ready" {
		t.Fatalf("doctor after the restore = %+v, want store.sqlite.ready", check)
	}
	if held, err := coordinatorLockHeld(coordinatorLockPath(fixture.registration)); err != nil || held {
		t.Fatalf("lock held after the restore = %v, %v; want released", held, err)
	}
}

// TestRestoreCheckpointOfADamagedLiveStore verifies that a damaged live
// store is replaced, and that the preview says that precondition 6 is not
// checked and why.
func TestRestoreCheckpointOfADamagedLiveStore(t *testing.T) {
	t.Parallel()
	fixture := newRestoreFixture(t)
	if err := os.WriteFile(fixture.registration.OperationalDataPath, []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := fixture.restoreRequest()
	plan := preview(t, request)
	if !slices.ContainsFunc(plan.Steps, func(step string) bool { return strings.Contains(step, "damaged") && strings.Contains(step, "not a database") }) {
		t.Fatalf("steps = %v, want a step that names the damaged live store and why", plan.Steps)
	}
	request.Confirm = plan.Digest
	if result, err := RestoreCheckpoint(context.Background(), request); err != nil || result.Outcome != RestoreOutcomeRestored {
		t.Fatalf("confirm = %+v, %v; want restored", result, err)
	}
}

// TestRestoreCheckpointRefusesALiveStoreThatDoesNotOpen verifies that a
// live store that exists but does not open, and is not damaged, refuses:
// its state is unknown evidence, so precondition 6 is not proved.
func TestRestoreCheckpointRefusesALiveStoreThatDoesNotOpen(t *testing.T) {
	t.Parallel()
	fixture := newRestoreFixture(t)
	if err := os.Chmod(fixture.registration.OperationalDataPath, 0o644); err != nil {
		t.Fatal(err)
	}
	before := fixture.storeBytes(t)
	result, err := RestoreCheckpoint(context.Background(), fixture.restoreRequest())
	assertRestoreRefusal(t, fixture, before, result, err, RestoreNotQuiescent)
	if !strings.HasPrefix(result.Refusals[0].Detail, "store.unreadable: ") {
		t.Fatalf("refusal = %+v, want store.unreadable", result.Refusals[0])
	}
}

// TestRestoreCheckpointIntoAMissingStoreDirectory covers the missing store
// directory row: the preview names the directory, and the restore creates
// it.
func TestRestoreCheckpointIntoAMissingStoreDirectory(t *testing.T) {
	t.Parallel()
	fixture := newRestoreFixture(t)
	directory := filepath.Dir(fixture.registration.OperationalDataPath)
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	request := fixture.restoreRequest()
	plan := preview(t, request)
	if !slices.ContainsFunc(plan.Steps, func(step string) bool { return strings.Contains(step, directory) && strings.Contains(step, "create") }) {
		t.Fatalf("steps = %v, want a step that creates %s", plan.Steps, directory)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("the preview created the store directory: %v", err)
	}
	request.Confirm = plan.Digest
	if result, err := RestoreCheckpoint(context.Background(), request); err != nil || result.Outcome != RestoreOutcomeRestored {
		t.Fatalf("confirm = %+v, %v; want restored", result, err)
	}
	if _, err := store.OpenReadOnly(context.Background(), fixture.registration.OperationalDataPath); err != nil {
		t.Fatalf("OpenReadOnly() after the restore error = %v", err)
	}
}

// TestRestoreCheckpointRefusesAStaleDigest verifies that a confirm whose
// digest is not the digest of the current plan is refused.
func TestRestoreCheckpointRefusesAStaleDigest(t *testing.T) {
	t.Parallel()
	fixture := newRestoreFixture(t)
	request := fixture.restoreRequest()
	request.Confirm = preview(t, request).Digest
	if err := os.WriteFile(fixture.registration.OperationalDataPath+"-shm", []byte("new sidecar"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := fixture.storeBytes(t)
	result, err := RestoreCheckpoint(context.Background(), request)
	assertRestoreRefusal(t, fixture, before, result, err, RestorePlanStale)
}

// rewriteCheckpointSchema sets the schema version of the checkpoint
// snapshot and seals the manifest again, so the checkpoint verifies.
func rewriteCheckpointSchema(t *testing.T, directory string, schema int) {
	t.Helper()
	path := filepath.Join(directory, checkpoint.StoreFile)
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE schema_metadata SET version = ?", schema); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	inspection, err := store.InspectSnapshot(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(directory, checkpoint.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var manifest checkpoint.Manifest
	if err := json.Unmarshal(content, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.StoreSchemaVersion = schema
	manifest.Files[0].Size, manifest.Files[0].SHA256 = inspection.File.Size, inspection.File.SHA256
	if err := os.Remove(filepath.Join(directory, checkpoint.ManifestFile)); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.WriteManifest(directory, manifest); err != nil {
		t.Fatal(err)
	}
}

// TestRestoreCheckpointRefusesEachUnsupportedRelease covers the schema and
// release rows.
func TestRestoreCheckpointRefusesEachUnsupportedRelease(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]struct {
		code  string
		apply func(*testing.T, restoreFixture, *RestoreRequest)
	}{
		"newer snapshot schema": {RestoreSchemaNewer, func(t *testing.T, f restoreFixture, _ *RestoreRequest) {
			rewriteCheckpointSchema(t, f.checkpoint, store.CurrentSchemaVersion+1)
		}},
		"older snapshot schema": {RestoreReleaseMismatch, func(t *testing.T, f restoreFixture, _ *RestoreRequest) {
			rewriteCheckpointSchema(t, f.checkpoint, store.CurrentSchemaVersion-1)
		}},
		"other release":  {RestoreReleaseMismatch, func(_ *testing.T, _ restoreFixture, r *RestoreRequest) { r.Build.Version = "v1.2.4" }},
		"other revision": {RestoreReleaseMismatch, func(_ *testing.T, _ restoreFixture, r *RestoreRequest) { r.Build.Revision = "def456" }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newRestoreFixture(t)
			request := fixture.restoreRequest()
			change.apply(t, fixture, &request)
			before := fixture.storeBytes(t)
			result, err := RestoreCheckpoint(context.Background(), request)
			assertRestoreRefusal(t, fixture, before, result, err, change.code)
		})
	}
}

// TestRestoreCheckpointRefusesAChangedRegistration covers the changed
// registration row.
func TestRestoreCheckpointRefusesAChangedRegistration(t *testing.T) {
	t.Parallel()
	fixture := newRestoreFixture(t)
	request := fixture.restoreRequest()
	request.Registration.GitHub.Repository = "other"
	before := fixture.storeBytes(t)
	result, err := RestoreCheckpoint(context.Background(), request)
	assertRestoreRefusal(t, fixture, before, result, err, RestoreRegistrationChanged)
}

// TestRestoreCheckpointRefusesALiveCoordinator covers the live coordinator
// row.
func TestRestoreCheckpointRefusesALiveCoordinator(t *testing.T) {
	t.Parallel()
	fixture := newRestoreFixture(t)
	owner, err := acquireCoordinatorLock(coordinatorLockPath(fixture.registration), LockHolderCoordinator)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.release() }()
	before := fixture.storeBytes(t)
	result, err := RestoreCheckpoint(context.Background(), fixture.restoreRequest())
	assertRestoreRefusal(t, fixture, before, result, err, RestoreCoordinatorRunning)
}

// TestRestoreCheckpointRefusesAnActiveWorkerOrRoleSession covers the active
// worker or role session row: a live harness, unknown Docker evidence, and
// a role session without a proved resource release each refuse.
func TestRestoreCheckpointRefusesAnActiveWorkerOrRoleSession(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*testing.T, restoreFixture, *RestoreRequest){
		"live harness": func(_ *testing.T, _ restoreFixture, r *RestoreRequest) { r.Workers = liveWorkers{} },
		"Docker unreachable": func(_ *testing.T, _ restoreFixture, r *RestoreRequest) {
			r.Workers = liveWorkers{err: errors.New("Cannot connect to the Docker daemon")}
		},
		"unreleased role session": func(t *testing.T, f restoreFixture, _ *RestoreRequest) {
			f.execStore(t, "UPDATE ideation_invocations SET resources_released_at = NULL")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newRestoreFixtureWithProcesses(t)
			request := fixture.restoreRequest()
			change(t, fixture, &request)
			result, err := RestoreCheckpoint(context.Background(), request)
			assertRestoreRefusal(t, fixture, nil, result, err, RestoreWorkerActive)
		})
	}
}

// newRestoreFixtureWithProcesses is a restore fixture whose snapshot holds
// an ended run invocation and an ended, released role session invocation.
func newRestoreFixtureWithProcesses(t *testing.T) restoreFixture {
	t.Helper()
	base := newCheckpointFixture(t)
	base.execStore(t, `INSERT INTO invocations (id, run_id, harness, role, stage, status, created_at, updated_at) VALUES ('inv-1', 'run-1', 'codex', 'implementation', 'implementation', 'completed', '2026-11-01T00:00:00Z', '2026-11-01T00:00:00Z')`)
	base.execStore(t, `INSERT INTO ideation_invocations (session_id, id, role, trigger_id, session_revision, context_revision, harness, prompt_version, prompt_sha256, worker_id, status, resources_released_at, created_at, updated_at) VALUES ('session-1', 'session-inv-1', 'ideation', 'turn-1', 1, 'context-1', 'codex', 'ideation-v1', 'digest', 'worker-1', 'accepted', '2026-11-01T00:00:00Z', '2026-11-01T00:00:00Z', '2026-11-01T00:00:00Z')`)
	fixture := restoreFixture{checkpointFixture: base, adapters: newRestoreAdapters()}
	request := fixture.request("checkpoint")
	request.External = NewExternalStateReader(fixture.registration, fixture.adapters.sources())
	result, err := CreateCheckpoint(context.Background(), request)
	if err != nil || len(result.Refusals) != 0 {
		t.Fatalf("CreateCheckpoint() = %+v, %v", result, err)
	}
	fixture.checkpoint = result.Path
	return fixture
}

// TestRestoreCheckpointRefusesALiveStoreThatIsNotQuiescent covers the
// non-terminal run and pending effect rows, and a live store with a record
// that the snapshot does not have.
func TestRestoreCheckpointRefusesALiveStoreThatIsNotQuiescent(t *testing.T) {
	t.Parallel()
	for name, statement := range map[string]string{
		"non-terminal run": "UPDATE operational_runs SET status = 'active'",
		"pending effect":   `INSERT INTO pending_effects (run_id, effect_id, kind, payload, created_at, updated_at) VALUES ('run-1', 'effect-1', 'status_comment', '{}', '2026-11-01T00:00:00Z', '2026-11-01T00:00:00Z')`,
		"newer run":        `INSERT INTO operational_runs (id, repository_path, issue_number, stage, status, created_at, updated_at) SELECT 'run-2', repository_path, 108, stage, 'complete', '2030-01-01T00:00:00Z', '2030-01-01T00:00:00Z' FROM operational_runs WHERE id = 'run-1'`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newRestoreFixture(t)
			fixture.execStore(t, statement)
			before := fixture.storeBytes(t)
			result, err := RestoreCheckpoint(context.Background(), fixture.restoreRequest())
			assertRestoreRefusal(t, fixture, before, result, err, RestoreNotQuiescent)
		})
	}
}

// TestRestoreCheckpointRefusesExternalDivergence covers the external effect
// row through the fake tracker and code-host adapters.
func TestRestoreCheckpointRefusesExternalDivergence(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(restoreAdapters){
		"moved known branch":         func(a restoreAdapters) { a.remote.heads["factory/run-1"] = "ccc" },
		"changed pull request head":  func(a restoreAdapters) { a.codeHost.pullRequests[0].HeadSHA = "ccc" },
		"changed pull request state": func(a restoreAdapters) { a.codeHost.pullRequests[0].Merged = false },
		"changed issue label": func(a restoreAdapters) {
			a.tracker.issues[107] = tracker.Issue{Number: 107, State: "closed", Labels: []string{tracker.LabelAgentFailed}}
		},
		"unknown factory branch": func(a restoreAdapters) { a.remote.heads["factory/run-2"] = "ddd" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newRestoreFixture(t)
			change(fixture.adapters)
			before := fixture.storeBytes(t)
			result, err := RestoreCheckpoint(context.Background(), fixture.restoreRequest())
			assertRestoreRefusal(t, fixture, before, result, err, RestoreExternalDivergence)
		})
	}
}

// TestRestoreCheckpointRefusesAnUnreadableSource covers the unreadable
// divergence source row.
func TestRestoreCheckpointRefusesAnUnreadableSource(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(restoreAdapters){
		"tracker":   func(a restoreAdapters) { a.tracker.err = errors.New("tracker unreachable") },
		"code host": func(a restoreAdapters) { a.codeHost.err = errors.New("code host unreachable") },
		"remote":    func(a restoreAdapters) { a.remote.err = errors.New("remote unreachable") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newRestoreFixture(t)
			change(fixture.adapters)
			result, err := RestoreCheckpoint(context.Background(), fixture.restoreRequest())
			if err != nil || result.Outcome != RestoreOutcomeRefused || len(result.Refusals) == 0 {
				t.Fatalf("result = %+v, %v; want refused", result, err)
			}
			for _, refusal := range result.Refusals {
				if refusal.Code != RestoreDivergenceUnverified {
					t.Fatalf("refusals = %+v, want only %s", result.Refusals, RestoreDivergenceUnverified)
				}
			}
		})
	}
}

// TestRestoreCheckpointRefusesABadCheckpoint covers the incomplete and the
// corrupt checkpoint rows.
func TestRestoreCheckpointRefusesABadCheckpoint(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]struct {
		code  string
		apply func(*testing.T, string)
	}{
		"missing manifest": {checkpoint.CodeIncomplete, func(t *testing.T, directory string) {
			if err := os.Remove(filepath.Join(directory, checkpoint.ManifestFile)); err != nil {
				t.Fatal(err)
			}
		}},
		"changed byte": {checkpoint.CodeCorrupt, func(t *testing.T, directory string) {
			path := filepath.Join(directory, checkpoint.StoreFile)
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			content[len(content)-1] ^= 0xff
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newRestoreFixture(t)
			change.apply(t, fixture.checkpoint)
			before := fixture.storeBytes(t)
			result, err := RestoreCheckpoint(context.Background(), fixture.restoreRequest())
			assertRestoreRefusal(t, fixture, before, result, err, change.code)
		})
	}
}

// TestRestoreCheckpointRefusesAnEarlierWorkDirectory verifies that a work
// directory of an earlier restore of the same checkpoint, which holds a
// preserved original, is never reused.
func TestRestoreCheckpointRefusesAnEarlierWorkDirectory(t *testing.T) {
	t.Parallel()
	fixture := newRestoreFixture(t)
	request := fixture.restoreRequest()
	request.Confirm = preview(t, request).Digest
	if result, err := RestoreCheckpoint(context.Background(), request); err != nil || result.Outcome != RestoreOutcomeRestored {
		t.Fatalf("first restore = %+v, %v", result, err)
	}
	request.Confirm = ""
	result, err := RestoreCheckpoint(context.Background(), request)
	assertRestoreRefusal(t, fixture, nil, result, err, RestoreWorkDirectoryExists)
}

// interruptRestore leaves the files of a restore of the fixture checkpoint
// that stopped after the live store moved into original/, as a crash does:
// the journal in the preserving state, the staged replacement in new/, and
// the live store preserved. The restore sequence and its crash points are
// tested in internal/checkpoint.
func interruptRestore(t *testing.T, request RestoreRequest) {
	t.Helper()
	plan := preview(t, request).files
	for _, directory := range []string{plan.WorkDirectory, filepath.Join(plan.WorkDirectory, "new"), filepath.Join(plan.WorkDirectory, "original")} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := os.ReadFile(filepath.Join(plan.CheckpointPath, checkpoint.StoreFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plan.WorkDirectory, "new", checkpoint.StoreFile), snapshot, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(plan.StorePath, filepath.Join(plan.WorkDirectory, "original", filepath.Base(plan.StorePath))); err != nil {
		t.Fatal(err)
	}
	journal, err := json.Marshal(checkpoint.RestoreJournal{SchemaVersion: checkpoint.RestoreJournalSchemaVersion, State: checkpoint.StatePreserving, Factory: checkpointFactory(request.Build), RestorePlan: plan})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.RestoreJournalPath(plan.StorePath), journal, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRestoreCheckpointContinuesAnInterruptedRestore covers the restore
// journal row: while the journal exists, store openers refuse; the restore
// command continues the earlier restore before anything else.
func TestRestoreCheckpointContinuesAnInterruptedRestore(t *testing.T) {
	t.Parallel()
	fixture := newRestoreFixture(t)
	before := fixture.storeBytes(t)
	request := fixture.restoreRequest()
	interruptRestore(t, request)
	if _, err := store.Open(context.Background(), fixture.registration.OperationalDataPath); !errors.As(err, new(*store.RestoreInProgressError)) {
		t.Fatalf("Open() during the restore = %v, want a restore in progress", err)
	}
	check := doctor.Run(context.Background(), store.StartupCheck(fixture.registration.OperationalDataPath)).Results[0]
	if check.Code != "store.sqlite.restore_in_progress" {
		t.Fatalf("doctor during the restore = %+v, want store.sqlite.restore_in_progress", check)
	}

	result, err := RestoreCheckpoint(context.Background(), request)
	if err != nil || result.Outcome != RestoreOutcomeInterrupted || result.Journal == nil || result.Journal.State != checkpoint.StatePreserving || result.Journal.Result != checkpoint.ContinuationRolledBack {
		t.Fatalf("continuation = %+v, %v; want the interrupted restore rolled back", result, err)
	}
	if !bytes.Equal(before, fixture.storeBytes(t)) {
		t.Fatal("the continuation did not put the original store back")
	}
}

// TestRestoreCheckpointContinuesOnlyFromTheSameRelease verifies that
// another release does not continue an interrupted restore.
func TestRestoreCheckpointContinuesOnlyFromTheSameRelease(t *testing.T) {
	t.Parallel()
	fixture := newRestoreFixture(t)
	request := fixture.restoreRequest()
	interruptRestore(t, request)
	request.Build.Revision = "def456"
	result, err := RestoreCheckpoint(context.Background(), request)
	assertRestoreRefusal(t, fixture, nil, result, err, RestoreReleaseMismatch)
	if _, err := os.Stat(store.RestoreJournalPath(fixture.registration.OperationalDataPath)); err != nil {
		t.Fatalf("the journal is gone after a refused continuation: %v", err)
	}
}
