// Command vulnscan evaluates one pinned scanner report for one artifact.
//
// It reads govulncheck or grype JSON on standard input, applies the reviewed
// suppression policy, prints a summary, and optionally appends a content-free
// metadata record. Exit status 0 means clean, 1 means actionable findings or
// an unapproved Go release, and 2 means the scanner or policy input is
// unusable.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Stevie1704/sw-factory/internal/vulnscan"
)

// Exit statuses of the command.
const (
	exitClean    = 0
	exitFindings = 1
	exitFailure  = 2
)

// scanRecord is the reproducible, content-free metadata of one evaluation.
type scanRecord struct {
	Artifact      string                 `json:"artifact"`
	Identity      string                 `json:"identity"`
	GoVersion     string                 `json:"go_version,omitempty"`
	Scanner       vulnscan.Scanner       `json:"scanner"`
	Result        string                 `json:"result"`
	Actionable    []vulnscan.Finding     `json:"actionable"`
	Suppressed    []vulnscan.Finding     `json:"suppressed"`
	Informational int                    `json:"informational"`
	Expired       []vulnscan.Suppression `json:"expired_suppressions,omitempty"`
	Unused        []vulnscan.Suppression `json:"unused_suppressions,omitempty"`
	EvaluatedAt   string                 `json:"evaluated_at"`
}

// main evaluates one report from standard input and exits with its status.
func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, time.Now()))
}

// run evaluates one report and returns the command's exit status.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, now time.Time) int {
	flags := flag.NewFlagSet("vulnscan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	scanner := flags.String("scanner", "", "report format: govulncheck or grype")
	artifact := flags.String("artifact", "", "artifact name that suppressions are scoped to")
	identity := flags.String("identity", "", "artifact identity, such as a file SHA-256 or an image digest")
	expectGo := flags.String("expect-go", "", "approved Go release the scan must have evaluated, such as go1.27.1")
	suppressionsPath := flags.String("suppressions", "", "reviewed suppression policy file")
	metadataPath := flags.String("metadata", "", "file to append one JSON metadata record to")
	if err := flags.Parse(args); err != nil {
		return exitFailure
	}
	if *artifact == "" || *identity == "" {
		fmt.Fprintln(stderr, "vulnscan: -artifact and -identity are required")
		return exitFailure
	}

	scan, err := parseReport(*scanner, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "vulnscan: %s scan of %s failed: %v\n", *scanner, *artifact, err)
		return exitFailure
	}
	suppressions, err := loadSuppressions(*suppressionsPath)
	if err != nil {
		fmt.Fprintf(stderr, "vulnscan: %v\n", err)
		return exitFailure
	}

	verdict := vulnscan.Evaluate(scan, *artifact, suppressions, now)
	unapprovedGo := *expectGo != "" && scan.GoVersion != *expectGo
	record := newRecord(*artifact, *identity, scan, verdict, unapprovedGo, now)
	report(stdout, record, *expectGo)
	if *metadataPath != "" {
		if err := appendRecord(*metadataPath, record); err != nil {
			fmt.Fprintf(stderr, "vulnscan: %v\n", err)
			return exitFailure
		}
	}
	if unapprovedGo || !verdict.Clean() {
		return exitFindings
	}
	return exitClean
}

// parseReport reads the report format the caller named.
func parseReport(scanner string, r io.Reader) (vulnscan.Scan, error) {
	switch scanner {
	case "govulncheck":
		return vulnscan.ParseGovulncheck(r)
	case "grype":
		return vulnscan.ParseGrype(r)
	default:
		return vulnscan.Scan{}, fmt.Errorf("unsupported scanner %q", scanner)
	}
}

// loadSuppressions reads the policy file, or none when no path is given.
func loadSuppressions(path string) ([]vulnscan.Suppression, error) {
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open suppressions: %w", err)
	}
	defer file.Close()
	suppressions, err := vulnscan.LoadSuppressions(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return suppressions, nil
}

// newRecord builds the metadata record for one evaluation.
func newRecord(artifact, identity string, scan vulnscan.Scan, verdict vulnscan.Verdict, unapprovedGo bool, now time.Time) scanRecord {
	result := "clean"
	switch {
	case unapprovedGo:
		result = "unapproved_go"
	case !verdict.Clean():
		result = "findings"
	}
	return scanRecord{
		Artifact:      artifact,
		Identity:      identity,
		GoVersion:     scan.GoVersion,
		Scanner:       scan.Scanner,
		Result:        result,
		Actionable:    nonNil(verdict.Actionable),
		Suppressed:    nonNil(verdict.Suppressed),
		Informational: verdict.Informational,
		Expired:       verdict.Expired,
		Unused:        verdict.Unused,
		EvaluatedAt:   now.UTC().Format(time.RFC3339),
	}
}

// nonNil keeps empty finding lists as [] in the metadata.
func nonNil(findings []vulnscan.Finding) []vulnscan.Finding {
	if findings == nil {
		return []vulnscan.Finding{}
	}
	return findings
}

// report prints the human-readable outcome of one evaluation.
func report(w io.Writer, record scanRecord, expectGo string) {
	fmt.Fprintf(w, "%s (%s): %s with %s %s", record.Artifact, record.Identity, record.Result, record.Scanner.Name, record.Scanner.Version)
	if record.GoVersion != "" {
		fmt.Fprintf(w, ", Go %s", record.GoVersion)
	}
	fmt.Fprintf(w, ", %d informational\n", record.Informational)
	if record.Result == "unapproved_go" {
		fmt.Fprintf(w, "  scanned Go release %s is not the approved %s\n", record.GoVersion, expectGo)
	}
	for _, finding := range record.Actionable {
		fmt.Fprintf(w, "  actionable %s in %s %s %s\n", finding.Advisory, finding.Package, finding.Version, finding.Severity)
	}
	for _, finding := range record.Suppressed {
		fmt.Fprintf(w, "  suppressed %s in %s %s\n", finding.Advisory, finding.Package, finding.Version)
	}
	for _, suppression := range record.Expired {
		fmt.Fprintf(w, "  expired suppression %s in %s (owner %s, review by %s)\n", suppression.Advisory, suppression.Package, suppression.Owner, suppression.ReviewBy)
	}
	for _, suppression := range record.Unused {
		fmt.Fprintf(w, "  unused suppression %s in %s; remove it\n", suppression.Advisory, suppression.Package)
	}
}

// appendRecord appends one JSON line to the metadata file.
func appendRecord(path string, record scanRecord) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open metadata: %w", err)
	}
	if err := json.NewEncoder(file).Encode(record); err != nil {
		file.Close()
		return fmt.Errorf("write metadata: %w", err)
	}
	return file.Close()
}
