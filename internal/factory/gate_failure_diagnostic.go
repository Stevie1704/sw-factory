package factory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Stevie1704/sw-factory/internal/gate"
	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/worker"
)

const (
	// gateFailureDiagnosticDirectoryName is the run-local directory holding one
	// diagnostic per failed gate phase. It sits beside the invocation
	// directories so every artifact of one run has a single root.
	gateFailureDiagnosticDirectoryName = "gate-failure"
	// maxGateFailureCauseBytes bounds the deterministic cause carried in a
	// lifecycle reason, which is a single operator-visible line.
	maxGateFailureCauseBytes = 240
	// gateFailureCauseLines is how many leading output lines one cause keeps.
	// A failing command commonly states the failure on its first line and what
	// to change on the next, so one line alone is not actionable.
	gateFailureCauseLines = 2
)

// gateFailureCause renders one bounded line naming the failed setup or gate
// command and what it printed. The typed failures deliberately exclude command
// output from their own Error text, so the coordinator builds the
// operator-visible cause here rather than widening those contracts.
//
// An error carrying no command observation returns an empty cause, which keeps
// an infrastructure category from claiming a deterministic blocker.
func gateFailureCause(suiteErr error) string {
	if suiteErr == nil {
		return ""
	}
	var setupFailure *gate.SetupFailure
	if errors.As(suiteErr, &setupFailure) {
		return gateFailureCauseLine(setupFailure.Error(), setupFailure.Result)
	}
	var gateFailure *gate.GateFailure
	if errors.As(suiteErr, &gateFailure) {
		return gateFailureCauseLine(gateFailure.Error(), gateFailure.Result)
	}
	return ""
}

// gateFailureCauseLine joins the typed failure with the leading lines the
// command printed. Both halves are needed on one line: the typed text names
// the failure mode, because a timeout and a non-zero exit are not
// distinguishable from output alone, and the output names the defect.
func gateFailureCauseLine(typed string, result worker.CommandResult) string {
	detail := leadingOutputLines(result.Stderr, gateFailureCauseLines)
	if detail == "" {
		detail = leadingOutputLines(result.Stdout, gateFailureCauseLines)
	}
	if detail != "" {
		typed += ": " + detail
	}
	return boundedText(safeStatusCommentValue(typed), maxGateFailureCauseBytes)
}

// leadingOutputLines joins at most limit non-blank leading lines into one line.
func leadingOutputLines(output string, limit int) string {
	lines := make([]string, 0, limit)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lines = append(lines, line)
		if len(lines) == limit {
			break
		}
	}
	return strings.Join(lines, "; ")
}

// withGateFailureCause appends a deterministic cause to a category reason so a
// parked run names the blocker instead of its category. The category stays the
// prefix, because it is the part the coordinator and an operator match on.
func withGateFailureCause(reason, cause string) string {
	if cause == "" {
		return reason
	}
	return reason + ": " + cause
}

// writeGateFailureDiagnostic records the bounded setup and gate output of one
// failed suite beside the run's other artifacts. A suite error carrying no
// command observation records nothing, because a transport or persistence
// failure would otherwise file the passing suite that preceded it as the cause
// of the pause.
//
// The file stays on the coordinator host. Writing it is best-effort, because
// losing a diagnostic must never mask the failure the caller is reporting.
func writeGateFailureDiagnostic(run store.Run, phase gate.Phase, results []gate.Result, suiteErr error, observedAt time.Time) {
	name := gateFailureDiagnosticName(phase)
	if name == "" || gateFailureCause(suiteErr) == "" || strings.TrimSpace(run.ID) == "" || strings.TrimSpace(run.Worktree) == "" {
		return
	}
	directory := filepath.Join(runArtifactRoot(run), gateFailureDiagnosticDirectoryName)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(directory, name), []byte(renderGateFailureDiagnostic(run, phase, results, suiteErr, observedAt)), 0o600)
}

// gateFailureDiagnosticName maps a declared phase to its fixed file name. An
// unknown phase writes nothing, so no caller can build a path from free text.
func gateFailureDiagnosticName(phase gate.Phase) string {
	switch phase {
	case gate.PhaseBaseline:
		return "baseline.log"
	case gate.PhaseCheckpoint:
		return "checkpoint.log"
	default:
		return ""
	}
}

// renderGateFailureDiagnostic renders the exact checkpoint identity, the setup
// observation shared by every gate, and each gate that did not pass. Command
// output passes through boundedRepairDiagnostic, so one policy governs every
// retained diagnostic.
func renderGateFailureDiagnostic(run store.Run, phase gate.Phase, results []gate.Result, suiteErr error, observedAt time.Time) string {
	checkpoint := run.CheckpointSHA
	if len(results) != 0 && results[0].CheckpointSHA != "" {
		checkpoint = results[0].CheckpointSHA
	}
	body := &strings.Builder{}
	fmt.Fprintf(body, "observed at: %s\nrun: %s\nphase: %s\ncheckpoint: %s\nfailure: %v\n", observedAt.Format(time.RFC3339), run.ID, phase, checkpoint, suiteErr)
	if len(results) != 0 && results[0].SetupRan {
		fmt.Fprintf(body, "\nsetup: exit code %d\n", results[0].Setup.ExitCode)
		writeCommandOutput(body, results[0].Setup)
	}
	for _, result := range results {
		if result.Outcome == gate.OutcomePassed {
			continue
		}
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

// writeCommandOutput appends the bounded streams of one command observation.
func writeCommandOutput(body *strings.Builder, result worker.CommandResult) {
	for _, stream := range []struct {
		name  string
		value string
	}{{name: "stdout", value: result.Stdout}, {name: "stderr", value: result.Stderr}} {
		if strings.TrimSpace(stream.value) == "" {
			continue
		}
		fmt.Fprintf(body, "%s:\n%s\n", stream.name, boundedRepairDiagnostic(stream.value))
	}
}
