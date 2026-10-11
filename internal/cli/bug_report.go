package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"time"

	"github.com/Stevie1704/sw-factory/internal/bugreport"
	"github.com/Stevie1704/sw-factory/internal/config"
	"github.com/Stevie1704/sw-factory/internal/github"
	"github.com/Stevie1704/sw-factory/internal/version"
)

// exitDuplicateCandidate is the exit status of a submission that stopped for
// the operator's duplicate disposition.
const exitDuplicateCandidate = 3

// bugReportGhRunner is the gh seam of the submission adapter. Nil selects the
// host gh executable; tests replace it with a scripted runner.
var bugReportGhRunner github.CommandRunner

// runBugReport prepares a local suspected-defect draft, or, with the submit
// subcommand, publishes a reviewed draft. Preparation is offline and works
// without a host configuration, so it needs no configuration resolution.
func runBugReport(ctx context.Context, args []string, _ string, output, errorsOutput io.Writer) int {
	if len(args) > 0 && args[0] == "submit" {
		return runBugReportSubmit(ctx, args[1:], output, errorsOutput)
	}
	defaultConfigPath, _ := config.DefaultHostConfigPath()
	flags := newFlagSet("bug-report", errorsOutput)
	configPath := flags.String("config", defaultConfigPath, "host configuration path")
	runID := flags.String("run-id", "", "Run to describe; omit for an installation or startup failure")
	outputPath := flags.String("output", "", "path of the Markdown draft to write")
	force := flags.Bool("force", false, "replace an existing draft at --output")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *outputPath == "" {
		writeError(errorsOutput, errors.New("bug-report requires --output <file> and accepts no positional arguments; use 'bug-report submit <file>' to submit"))
		return 2
	}
	evidence, err := newService(*configPath).BugReportEvidence(ctx, *runID)
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	id, err := bugreport.NewID()
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	draft := bugreport.Render(bugreport.Draft{ID: id, Identity: version.Current(), Evidence: evidence, CreatedAt: time.Now().UTC()})
	if err := writePrivateDraft(*outputPath, draft, *force); err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if !writeWorkerProposals(output, errorsOutput, evidence) {
		return 1
	}
	if !writeOutput(output, errorsOutput, "draft written: %s (report %s)\nComplete the title and the required sections, review the whole file, then run: factory bug-report submit %s\n", *outputPath, id, *outputPath) {
		return 1
	}
	return 0
}

// writeWorkerProposals shows the worker-authored text of each defect
// proposal on the terminal only. The draft names the proposal by code; the
// operator decides what of this text to write into the draft.
func writeWorkerProposals(output, errorsOutput io.Writer, evidence bugreport.Evidence) bool {
	if evidence.Run == nil {
		return true
	}
	for _, proposal := range evidence.Run.DefectProposals {
		if !writeOutput(output, errorsOutput, "worker defect proposal (worker-authored, not in the draft): invocation=%s code=%s\n  expected: %s\n  observed: %s\n  reason: %s\n", proposal.InvocationID, proposal.Code, proposal.Expected, proposal.Observed, proposal.Reason) {
			return false
		}
	}
	return true
}

// writePrivateDraft creates the draft with owner-only permissions. An
// existing regular file is replaced only when force is set.
func writePrivateDraft(path string, content []byte, force bool) error {
	if force {
		info, err := os.Lstat(path)
		switch {
		case err == nil && !info.Mode().IsRegular():
			return fmt.Errorf("%s exists and is not a regular file", path)
		case err == nil:
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("replace draft: %w", err)
			}
		case !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("inspect draft path: %w", err)
		}
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s already exists; choose another --output or add --force to replace it", path)
	}
	if err != nil {
		return fmt.Errorf("create draft: %w", err)
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return fmt.Errorf("write draft: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close draft: %w", err)
	}
	return nil
}

// runBugReportSubmit publishes one reviewed draft to Factory's upstream
// tracker. Its invocation is the publication action.
func runBugReportSubmit(ctx context.Context, args []string, output, errorsOutput io.Writer) int {
	flags := newFlagSet("bug-report submit", errorsOutput)
	var distinctFrom stringList
	flags.Var(&distinctFrom, "distinct-from", "reviewed duplicate-candidate issue number that is a different defect; may be repeated")
	confirmNotCreated := flags.Bool("confirm-not-created", false, "confirm that an earlier ambiguous creation produced no upstream issue")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		writeError(errorsOutput, errors.New("bug-report submit requires exactly one draft path after its flags"))
		return 2
	}
	options := bugreport.SubmitOptions{ConfirmNotCreated: *confirmNotCreated}
	for _, value := range distinctFrom {
		number, err := strconv.Atoi(value)
		if err != nil || number <= 0 {
			writeError(errorsOutput, fmt.Errorf("--distinct-from %q is not an issue number", value))
			return 2
		}
		options.DistinctFrom = append(options.DistinctFrom, number)
	}
	result, err := bugreport.Submit(ctx, flags.Arg(0), github.NewUpstreamIssues(bugReportGhRunner), options)
	if err != nil {
		var failure *bugreport.SubmitError
		kind := bugreport.FailureKind("unknown")
		if errors.As(err, &failure) {
			kind = failure.Kind
		}
		writeOutput(output, errorsOutput, "outcome: failed (%s)\n", kind)
		writeError(errorsOutput, err)
		return 1
	}
	return writeSubmitResult(output, errorsOutput, result, flags.Arg(0))
}

// writeSubmitResult prints the submission outcome and returns the exit status.
func writeSubmitResult(output, errorsOutput io.Writer, result bugreport.Result, draftPath string) int {
	if !writeOutput(output, errorsOutput, "outcome: %s\nreport: %s\n", result.Outcome, result.ReportID) {
		return 1
	}
	if result.Issue != nil {
		if !writeOutput(output, errorsOutput, "issue: %s\n", result.Issue.URL) {
			return 1
		}
		return 0
	}
	for _, candidate := range result.Candidates {
		if !writeOutput(output, errorsOutput, "duplicate candidate: #%d %s %s\n", candidate.Number, candidate.URL, candidate.Title) {
			return 1
		}
	}
	if !writeOutput(output, errorsOutput, "Nothing was created. If an existing issue describes this defect, add your evidence there. If every candidate is a different defect, run: factory bug-report submit --distinct-from <number> %s (repeat --distinct-from for each candidate)\n", draftPath) {
		return 1
	}
	return exitDuplicateCandidate
}
