package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Stevie1704/sw-factory/internal/acceptance"
	"github.com/Stevie1704/sw-factory/internal/version"
)

// runAcceptance dispatches the acceptance subcommands. They need no host
// configuration, so an installed release runs them outside any checkout.
func runAcceptance(_ context.Context, args []string, _ string, output, errorsOutput io.Writer) int {
	if len(args) > 0 && args[0] == "scenarios" {
		return runAcceptanceScenarios(args[1:], output, errorsOutput)
	}
	if len(args) > 0 && args[0] == "report" {
		return runAcceptanceReport(args[1:], output, errorsOutput)
	}
	writeError(errorsOutput, errors.New("acceptance needs a subcommand: scenarios or report"))
	return 2
}

// runAcceptanceScenarios prints the shipped scenario manifest as one JSON
// document.
func runAcceptanceScenarios(args []string, output, errorsOutput io.Writer) int {
	flags := newFlagSet("acceptance scenarios", errorsOutput)
	asJSON := flags.Bool("json", false, "write the scenario manifest as one schema-versioned JSON document; required")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || !*asJSON {
		writeError(errorsOutput, errors.New("acceptance scenarios needs --json and accepts no positional arguments"))
		return 2
	}
	manifest, err := acceptance.LoadManifest()
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if !writeJSONDocument(output, errorsOutput, manifest) {
		return 1
	}
	return 0
}

// Report file names below the --output directory.
const (
	acceptanceReportJSON     = "acceptance-report.json"
	acceptanceReportMarkdown = "acceptance-report.md"
)

// runAcceptanceReport builds the release evidence report from one recorded
// results file, writes its JSON and Markdown forms below a new output
// directory, and prints the JSON form. It never replaces an earlier report.
func runAcceptanceReport(args []string, output, errorsOutput io.Writer) int {
	flags := newFlagSet("acceptance report", errorsOutput)
	resultsPath := flags.String("results", "", "results file that the acceptance run recorded")
	destination := flags.String("output", "", "directory that receives acceptance-report.json and acceptance-report.md; must not hold an earlier report")
	asJSON := flags.Bool("json", false, "write the report as one schema-versioned JSON document; required")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *resultsPath == "" || *destination == "" || !*asJSON {
		writeError(errorsOutput, errors.New("acceptance report needs --results <file>, --output <directory>, and --json"))
		return 2
	}
	report, err := buildAcceptanceReport(*resultsPath)
	if err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if err := writeAcceptanceReport(*destination, report); err != nil {
		writeError(errorsOutput, err)
		return 1
	}
	if !writeJSONDocument(output, errorsOutput, report) {
		return 1
	}
	return 0
}

// buildAcceptanceReport reads the results file and combines it with the
// shipped manifest and the identity of this build.
func buildAcceptanceReport(resultsPath string) (acceptance.Report, error) {
	manifest, err := acceptance.LoadManifest()
	if err != nil {
		return acceptance.Report{}, err
	}
	file, err := os.Open(resultsPath)
	if err != nil {
		return acceptance.Report{}, fmt.Errorf("open acceptance results: %w", err)
	}
	defer func() { _ = file.Close() }()
	results, err := acceptance.DecodeResults(file)
	if err != nil {
		return acceptance.Report{}, err
	}
	return acceptance.BuildReport(manifest, results, version.Current(), time.Now())
}

// writeAcceptanceReport creates both report files with owner-only
// permissions. The operator decides whether to publish them. It refuses an
// output directory that already holds either file, so it never leaves a mixed
// report behind.
func writeAcceptanceReport(destination string, report acceptance.Report) error {
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	files := []struct {
		name    string
		content []byte
	}{
		{acceptanceReportJSON, append(encoded, '\n')},
		{acceptanceReportMarkdown, acceptance.RenderMarkdown(report)},
	}
	for _, file := range files {
		if _, err := os.Lstat(filepath.Join(destination, file.name)); !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s already holds %s; choose a new --output directory", destination, file.name)
		}
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return fmt.Errorf("create report directory: %w", err)
	}
	for _, file := range files {
		if err := writePrivateDraft(filepath.Join(destination, file.name), file.content, false); err != nil {
			return err
		}
	}
	return nil
}
