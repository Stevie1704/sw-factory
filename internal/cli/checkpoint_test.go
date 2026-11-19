package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/checkpoint"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/factory"
	gitadapter "github.com/Stevie1704/sw-factory/internal/git"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/store"
)

// cliExternal is a tracker, code host, and Git remote that knows every
// object, or none when unreachable is set.
type cliExternal struct{ unreachable bool }

// BranchHead returns a fixed head.
func (f cliExternal) BranchHead(context.Context, string) (string, error) {
	if f.unreachable {
		return "", errors.New("remote unreachable")
	}
	return "aaa", nil
}

// PullRequest returns the requested pull request.
func (f cliExternal) PullRequest(_ context.Context, number int, branch string) (checkpoint.PullRequestState, error) {
	return checkpoint.PullRequestState{Number: number, HeadBranch: branch, State: "merged"}, nil
}

// Issue returns the requested issue.
func (f cliExternal) Issue(_ context.Context, number int) (checkpoint.IssueState, error) {
	return checkpoint.IssueState{Number: number, FactoryLabels: []string{}}, nil
}

// StatusComment is never needed by the fixture run.
func (f cliExternal) StatusComment(context.Context, int, string, string) (checkpoint.StatusCommentState, error) {
	return checkpoint.StatusCommentState{}, errors.New("no status comment")
}

// useCheckpointExternal replaces the external state seam for one test.
func useCheckpointExternal(t *testing.T, reader checkpoint.ExternalReader) {
	t.Helper()
	previous := checkpointExternalForCLI
	t.Cleanup(func() { checkpointExternalForCLI = previous })
	checkpointExternalForCLI = func(string, config.RepositoryRegistration, string) checkpoint.ExternalReader { return reader }
}

// checkpointOutput returns a new destination in a private directory.
func checkpointOutput(t *testing.T) string {
	t.Helper()
	parent := filepath.Join(t.TempDir(), "checkpoints")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(parent, "before-upgrade")
}

// runCheckpointCommand runs one checkpoint command and decodes its document.
func runCheckpointCommand(t *testing.T, document any, args ...string) int {
	t.Helper()
	var output, errorsOutput bytes.Buffer
	code := Run(context.Background(), append([]string{"checkpoint"}, args...), &output, &errorsOutput)
	if document != nil && output.Len() != 0 {
		if err := json.Unmarshal(output.Bytes(), document); err != nil {
			t.Fatalf("decode %s: %v (stderr %s)", output.String(), err, errorsOutput.String())
		}
	}
	return code
}

// TestCheckpointCreateWritesAVerifiedCheckpoint verifies the created
// outcome and its document.
func TestCheckpointCreateWritesAVerifiedCheckpoint(t *testing.T) {
	fixture := newUpgradeFixture(t, store.StatusComplete, fakeUpgradeImages{release: "v1.4.0"})
	useCheckpointExternal(t, cliExternal{})
	output := checkpointOutput(t)

	var document checkpointCreateDocument
	code := runCheckpointCommand(t, &document, "create", "--config", fixture.hostConfig, "--output", output, "--json")

	if code != 0 || document.SchemaVersion != 1 || document.Command != "checkpoint create" || document.Outcome != "created" || document.Checkpoint == nil || document.Checkpoint.Path != output || len(document.Refusals) != 0 {
		t.Fatalf("checkpoint create = exit %d, %+v; want the created outcome", code, document)
	}
	manifest, err := checkpoint.Verify(context.Background(), output)
	if err != nil || manifest.CheckpointID != document.Checkpoint.Manifest.CheckpointID || manifest.TargetBranchHead != "aaa" {
		t.Fatalf("Verify() = %+v, %v; want the reported manifest", manifest, err)
	}
}

// TestCheckpointCreateReportsRefusals verifies the refused outcome of an
// active run and of an unreadable external object.
func TestCheckpointCreateReportsRefusals(t *testing.T) {
	for name, test := range map[string]struct {
		status   store.Status
		external cliExternal
		code     string
	}{
		"active run":         {store.StatusActive, cliExternal{}, "checkpoint.not_quiescent"},
		"unreachable remote": {store.StatusComplete, cliExternal{unreachable: true}, "checkpoint.external_unverified"},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newUpgradeFixture(t, test.status, fakeUpgradeImages{release: "v1.4.0"})
			useCheckpointExternal(t, test.external)
			output := checkpointOutput(t)

			var document checkpointCreateDocument
			code := runCheckpointCommand(t, &document, "create", "--config", fixture.hostConfig, "--output", output, "--json")

			if code != 1 || document.Outcome != "refused" || len(document.Refusals) == 0 || document.Refusals[0].Code != test.code || document.Checkpoint != nil {
				t.Fatalf("checkpoint create = exit %d, %+v; want %s", code, document, test.code)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("a checkpoint exists after a refusal: %v", err)
			}
		})
	}
}

// TestCheckpointNeedsASubcommandOutputAndJSON verifies the argument errors.
func TestCheckpointNeedsASubcommandOutputAndJSON(t *testing.T) {
	for _, args := range [][]string{{}, {"restore"}, {"create", "--output", "x"}, {"create", "--json"}} {
		if code := runCheckpointCommand(t, nil, args...); code != 2 {
			t.Fatalf("checkpoint %v exit = %d, want 2", args, code)
		}
	}
}

// createdCheckpoint creates one checkpoint of a complete store and returns
// its directory.
func createdCheckpoint(t *testing.T) string {
	t.Helper()
	fixture := newUpgradeFixture(t, store.StatusComplete, fakeUpgradeImages{release: "v1.4.0"})
	useCheckpointExternal(t, cliExternal{})
	output := checkpointOutput(t)
	var document checkpointCreateDocument
	if code := runCheckpointCommand(t, &document, "create", "--config", fixture.hostConfig, "--output", output, "--json"); code != 0 {
		t.Fatalf("checkpoint create exit = %d, want 0", code)
	}
	return output
}

// TestCheckpointInspectVerifiesAValidCheckpoint verifies that a valid
// checkpoint prints its manifest and verified: true, without a configuration.
func TestCheckpointInspectVerifiesAValidCheckpoint(t *testing.T) {
	directory := createdCheckpoint(t)

	var document checkpointInspectDocument
	code := runCheckpointCommand(t, &document, "inspect", "--path", directory, "--json")

	if code != 0 || document.SchemaVersion != 1 || document.Command != "checkpoint inspect" || document.Outcome != "verified" || !document.Verified || document.Checkpoint == nil || document.Checkpoint.Path != directory || len(document.Refusals) != 0 {
		t.Fatalf("checkpoint inspect = exit %d, %+v; want the verified outcome", code, document)
	}
	manifest, err := checkpoint.Verify(context.Background(), directory)
	if err != nil || document.Checkpoint.Manifest.CheckpointID != manifest.CheckpointID {
		t.Fatalf("manifest = %+v, %v; want the verified manifest", document.Checkpoint, err)
	}
}

// TestCheckpointInspectNeedsNoHostConfig verifies that inspection runs when
// neither HOME nor FACTORY_CONFIG resolves a host configuration path.
func TestCheckpointInspectNeedsNoHostConfig(t *testing.T) {
	directory := createdCheckpoint(t)
	for _, name := range []string{"HOME", "FACTORY_CONFIG"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}

	var document checkpointInspectDocument
	code := runCheckpointCommand(t, &document, "inspect", "--path", directory, "--json")

	if code != 0 || document.Outcome != "verified" || !document.Verified {
		t.Fatalf("checkpoint inspect without host config = exit %d, %+v; want verified", code, document)
	}
}

// TestCheckpointInspectRefusesADamagedCheckpoint verifies that each damage
// refuses with the code that restore reports.
func TestCheckpointInspectRefusesADamagedCheckpoint(t *testing.T) {
	for name, test := range map[string]struct {
		damage func(t *testing.T, directory string)
		code   string
	}{
		"changed byte": {func(t *testing.T, directory string) {
			path := filepath.Join(directory, checkpoint.StoreFile)
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			content[len(content)-1] ^= 0xff
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
		}, checkpoint.CodeCorrupt},
		"missing store": {func(t *testing.T, directory string) {
			if err := os.Remove(filepath.Join(directory, checkpoint.StoreFile)); err != nil {
				t.Fatal(err)
			}
		}, checkpoint.CodeIncomplete},
		"extra file": {func(t *testing.T, directory string) {
			if err := os.WriteFile(filepath.Join(directory, "notes.txt"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, checkpoint.CodeCorrupt},
		"unknown manifest schema": {func(t *testing.T, directory string) {
			if err := os.WriteFile(filepath.Join(directory, checkpoint.ManifestFile), []byte(`{"schema_version":2}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}, checkpoint.CodeCorrupt},
	} {
		t.Run(name, func(t *testing.T) {
			directory := createdCheckpoint(t)
			test.damage(t, directory)

			var document checkpointInspectDocument
			code := runCheckpointCommand(t, &document, "inspect", "--path", directory, "--json")

			if code != 1 || document.Outcome != "refused" || document.Verified || document.Checkpoint != nil || len(document.Refusals) != 1 || document.Refusals[0].Code != test.code {
				t.Fatalf("checkpoint inspect = exit %d, %+v; want refused with %s", code, document, test.code)
			}
		})
	}
}

// fileState is the bytes and modification time of one path.
type fileState struct {
	content []byte
	modTime time.Time
}

// directoryState records every path under directory, including the
// directory itself, so a test can prove that inspection changed nothing.
func directoryState(t *testing.T, directory string) map[string]fileState {
	t.Helper()
	state := map[string]fileState{}
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		var content []byte
		if entry.Type().IsRegular() {
			if content, err = os.ReadFile(path); err != nil {
				return err
			}
		}
		state[path] = fileState{content: content, modTime: info.ModTime()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// TestCheckpointInspectChangesNoFile verifies that inspection leaves the
// bytes and the modification times of a valid and of a damaged checkpoint
// unchanged.
func TestCheckpointInspectChangesNoFile(t *testing.T) {
	valid := createdCheckpoint(t)
	damaged := createdCheckpoint(t)
	if err := os.WriteFile(filepath.Join(damaged, "notes.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, directory := range map[string]string{"valid": valid, "damaged": damaged} {
		t.Run(name, func(t *testing.T) {
			before := directoryState(t, directory)
			var document checkpointInspectDocument
			runCheckpointCommand(t, &document, "inspect", "--path", directory, "--json")
			after := directoryState(t, directory)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("inspect changed the checkpoint:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

// cliOwned lists no factory-owned object that the fixture checkpoint does
// not know, or fails when unreachable is set.
type cliOwned struct{ unreachable bool }

// FactoryBranches lists no unknown branch.
func (f cliOwned) FactoryBranches(context.Context) ([]string, error) {
	if f.unreachable {
		return nil, errors.New("remote unreachable")
	}
	return nil, nil
}

// FactoryPullRequests lists no unknown pull request.
func (cliOwned) FactoryPullRequests(context.Context) ([]checkpoint.PullRequestState, error) {
	return nil, nil
}

// FactoryLabeledIssues lists no unknown issue.
func (cliOwned) FactoryLabeledIssues(context.Context) ([]checkpoint.IssueState, error) {
	return nil, nil
}

// The production remote and the hosted adapters list factory-owned objects,
// so a restore of a hosted installation can verify precondition 8.
var (
	_ factory.RemoteBranchLister    = (*gitadapter.LocalWorktreeManager)(nil)
	_ factory.PullRequestHeadLister = (*github.CodeHostClient)(nil)
	_ factory.LabeledIssueLister    = (*github.GhClient)(nil)
)

// useCheckpointOwned replaces the owned-object seam for one test.
func useCheckpointOwned(t *testing.T, lister checkpoint.OwnedObjectLister) {
	t.Helper()
	previous := checkpointOwnedForCLI
	t.Cleanup(func() { checkpointOwnedForCLI = previous })
	checkpointOwnedForCLI = func(string, config.RepositoryRegistration, string) checkpoint.OwnedObjectLister { return lister }
}

// TestCheckpointRestorePreviewsThenRestores verifies the preview and the
// restored outcome of a confirm with the previewed digest.
func TestCheckpointRestorePreviewsThenRestores(t *testing.T) {
	fixture := newUpgradeFixture(t, store.StatusComplete, fakeUpgradeImages{release: "v1.4.0"})
	useCheckpointExternal(t, cliExternal{})
	useCheckpointOwned(t, cliOwned{})
	output := checkpointOutput(t)
	if code := runCheckpointCommand(t, nil, "create", "--config", fixture.hostConfig, "--output", output, "--json"); code != 0 {
		t.Fatalf("checkpoint create exit = %d, want 0", code)
	}

	var preview checkpointRestoreDocument
	code := runCheckpointCommand(t, &preview, "restore", "--config", fixture.hostConfig, "--path", output, "--json")
	if code != 0 || preview.SchemaVersion != 1 || preview.Command != "checkpoint restore" || preview.Outcome != "preview" || preview.Plan == nil || preview.Plan.Digest == "" || len(preview.Plan.Steps) == 0 || len(preview.Refusals) != 0 {
		t.Fatalf("checkpoint restore preview = exit %d, %+v; want a plan", code, preview)
	}

	var stale checkpointRestoreDocument
	code = runCheckpointCommand(t, &stale, "restore", "--config", fixture.hostConfig, "--path", output, "--json", "--confirm", "sha256:stale")
	if code != 1 || stale.Outcome != "refused" || len(stale.Refusals) != 1 || stale.Refusals[0].Code != "restore.plan_stale" {
		t.Fatalf("checkpoint restore with a stale digest = exit %d, %+v; want restore.plan_stale", code, stale)
	}

	var restored checkpointRestoreDocument
	code = runCheckpointCommand(t, &restored, "restore", "--config", fixture.hostConfig, "--path", output, "--json", "--confirm", preview.Plan.Digest)
	if code != 0 || restored.Outcome != "restored" || restored.Plan == nil || len(restored.Plan.PreservedPaths) == 0 {
		t.Fatalf("checkpoint restore confirm = exit %d, %+v; want restored", code, restored)
	}
	for _, path := range restored.Plan.PreservedPaths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("the preserved original %s is missing: %v", path, err)
		}
	}
}

// TestCheckpointRestoreReportsAnUnverifiedSource verifies the refused
// outcome of an unreadable divergence source.
func TestCheckpointRestoreReportsAnUnverifiedSource(t *testing.T) {
	fixture := newUpgradeFixture(t, store.StatusComplete, fakeUpgradeImages{release: "v1.4.0"})
	useCheckpointExternal(t, cliExternal{})
	output := checkpointOutput(t)
	if code := runCheckpointCommand(t, nil, "create", "--config", fixture.hostConfig, "--output", output, "--json"); code != 0 {
		t.Fatalf("checkpoint create exit = %d, want 0", code)
	}
	useCheckpointOwned(t, cliOwned{unreachable: true})

	var document checkpointRestoreDocument
	code := runCheckpointCommand(t, &document, "restore", "--config", fixture.hostConfig, "--path", output, "--json")
	if code != 1 || document.Outcome != "refused" || len(document.Refusals) != 1 || document.Refusals[0].Code != "restore.divergence_unverified" {
		t.Fatalf("checkpoint restore = exit %d, %+v; want restore.divergence_unverified", code, document)
	}
}

// TestCheckpointRestoreNeedsPathAndJSON verifies the argument errors.
func TestCheckpointRestoreNeedsPathAndJSON(t *testing.T) {
	for _, args := range [][]string{{"restore", "--path", "x"}, {"restore", "--json"}} {
		if code := runCheckpointCommand(t, nil, args...); code != 2 {
			t.Fatalf("checkpoint %v exit = %d, want 2", args, code)
		}
	}
}

// TestCheckpointInspectNeedsPathAndJSON verifies the argument errors.
func TestCheckpointInspectNeedsPathAndJSON(t *testing.T) {
	for _, args := range [][]string{{"inspect"}, {"inspect", "--path", "x"}, {"inspect", "--json"}} {
		if code := runCheckpointCommand(t, nil, args...); code != 2 {
			t.Fatalf("checkpoint %v exit = %d, want 2", args, code)
		}
	}
}
