package factory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/codehost"
	"github.com/Stevie1704/sw-factory/internal/gate"
	"github.com/Stevie1704/sw-factory/internal/store"
)

const (
	// gateOutputDirectoryName is the run-local directory holding one bounded
	// gate-output log per evaluated checkpoint.
	gateOutputDirectoryName = "gate-output"
	// gateOutputPacketFileName is the file name of the gate-output log in a
	// PR-writer invocation packet.
	gateOutputPacketFileName = "gate-output.log"
)

// writeCheckpointGateOutput records the bounded setup and gate output of one
// checkpoint suite, passed gates included, so the PR writer can cite observed
// evidence. The log stays on the coordinator host until a PR-writer packet
// copies it. Writing it is best-effort, because a lost log must never fail the
// suite that produced it.
func writeCheckpointGateOutput(run store.Run, results []gate.Result, observedAt time.Time) {
	name := checkpointGateOutputName(run.CheckpointSHA)
	if name == "" || len(results) == 0 || strings.TrimSpace(run.ID) == "" || strings.TrimSpace(run.Worktree) == "" {
		return
	}
	directory := filepath.Join(runArtifactRoot(run), gateOutputDirectoryName)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(directory, name), []byte(renderCheckpointGateOutput(run, results, observedAt)), 0o600)
}

// copyCheckpointGateOutput copies the gate-output log of the run's current
// checkpoint into an invocation packet directory. It reports false when no log
// exists, for example for a checkpoint evaluated before logs were kept. The
// read goes through an os.Root at the run artifact root, so a symbolic link
// cannot lead it outside the run's artifacts.
func copyCheckpointGateOutput(run store.Run, packetDirectory string) (bool, error) {
	name := checkpointGateOutputName(run.CheckpointSHA)
	if name == "" || strings.TrimSpace(run.ID) == "" || strings.TrimSpace(run.Worktree) == "" {
		return false, nil
	}
	root, err := os.OpenRoot(runArtifactRoot(run))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open run artifacts for gate output: %w", err)
	}
	defer func() { _ = root.Close() }()
	read, found, err := readBoundedRegularFile(root, filepath.Join(gateOutputDirectoryName, name))
	if err != nil || !found {
		return false, err
	}
	content := read.content
	if read.truncated {
		content += "\n[truncated]\n"
	}
	if err := os.WriteFile(filepath.Join(packetDirectory, gateOutputPacketFileName), []byte(content), 0o600); err != nil {
		return false, fmt.Errorf("write gate output into invocation packet: %w", err)
	}
	return true, nil
}

// checkpointGateOutputName maps a checkpoint to its fixed log name. An invalid
// SHA gives no name, so no caller can build a path from free text.
func checkpointGateOutputName(checkpointSHA string) string {
	if !codehost.ValidCommitSHA(checkpointSHA) {
		return ""
	}
	return checkpointSHA + ".log"
}

// renderCheckpointGateOutput renders the checkpoint identity, the setup
// observation, and every gate with its outcome and bounded command output.
func renderCheckpointGateOutput(run store.Run, results []gate.Result, observedAt time.Time) string {
	body := &strings.Builder{}
	fmt.Fprintf(body, "observed at: %s\nrun: %s\ncheckpoint: %s\n", observedAt.Format(time.RFC3339), run.ID, run.CheckpointSHA)
	if results[0].SetupRan {
		fmt.Fprintf(body, "\nsetup: exit code %d\n", results[0].Setup.ExitCode)
		writeCommandOutput(body, results[0].Setup)
	}
	for _, result := range results {
		fmt.Fprintf(body, "\ngate %q: outcome=%s status=%s blocking=%t\n", result.GateName, result.Outcome, result.Status.State, result.Blocking)
		if result.SkipReason != "" {
			fmt.Fprintf(body, "skip reason: %s\n", result.SkipReason)
		}
		if !result.Skipped {
			fmt.Fprintf(body, "exit code: %d\n", result.Gate.ExitCode)
			writeCommandOutput(body, result.Gate)
		}
	}
	return body.String()
}
