package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/checkpoint"
	"github.com/Stevie1704/sw-factory/internal/config"
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
func (f cliExternal) StatusComment(context.Context, int, string) (checkpoint.StatusCommentState, error) {
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
