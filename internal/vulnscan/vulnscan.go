// Package vulnscan turns pinned vulnerability scanner output into a release
// verdict. It separates a scanner that failed to produce a result from a
// result with findings, and it applies only narrow, dated suppressions.
package vulnscan

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"
)

// ErrScannerOutput marks scanner output that is missing, malformed, or from
// another scanner. Such output is a scanner failure, never a clean scan.
var ErrScannerOutput = errors.New("unusable scanner output")

// Scanner identifies the scanner and vulnerability database that produced a
// scan, so the scan can be reproduced.
type Scanner struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	Mode          string `json:"mode"`
	Database      string `json:"database"`
	DatabaseBuilt string `json:"database_built"`
}

// Finding is one advisory affecting one package of the scanned artifact.
type Finding struct {
	Advisory   string `json:"advisory"`
	Package    string `json:"package"`
	Version    string `json:"version"`
	Severity   string `json:"severity,omitempty"`
	Actionable bool   `json:"-"`
}

// Scan is the scanner-independent result for one artifact. GoVersion is the
// Go release the scanner evaluated the standard library against.
type Scan struct {
	Scanner   Scanner
	GoVersion string
	Findings  []Finding
}

// Verdict is the outcome of applying the suppression policy to a scan.
type Verdict struct {
	Actionable    []Finding
	Suppressed    []Finding
	Informational int
	Expired       []Suppression
	Unused        []Suppression
}

// Clean reports whether the verdict leaves nothing to act on.
func (v Verdict) Clean() bool {
	return len(v.Actionable) == 0
}

// govulncheckMessage is one message of the govulncheck JSON stream.
type govulncheckMessage struct {
	Config *struct {
		ScannerName    string `json:"scanner_name"`
		ScannerVersion string `json:"scanner_version"`
		DB             string `json:"db"`
		DBLastModified string `json:"db_last_modified"`
		ScanMode       string `json:"scan_mode"`
	} `json:"config"`
	SBOM *struct {
		GoVersion string `json:"go_version"`
	} `json:"SBOM"`
	Finding *struct {
		OSV   string             `json:"osv"`
		Trace []govulncheckFrame `json:"trace"`
	} `json:"finding"`
}

// govulncheckFrame is one frame of a govulncheck finding trace. The first
// frame names the vulnerable module, package, and symbol.
type govulncheckFrame struct {
	Module   string `json:"module"`
	Version  string `json:"version"`
	Package  string `json:"package"`
	Function string `json:"function"`
}

// ParseGovulncheck reads a govulncheck -format json stream. An advisory is
// actionable when at least one vulnerable symbol is present in the artifact;
// module- and package-level matches stay informational.
func ParseGovulncheck(r io.Reader) (Scan, error) {
	var scan Scan
	findings := govulncheckFindings{}
	sawConfig := false
	decoder := json.NewDecoder(r)
	for {
		var message govulncheckMessage
		err := decoder.Decode(&message)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Scan{}, fmt.Errorf("%w: govulncheck stream: %v", ErrScannerOutput, err)
		}
		switch {
		case message.Config != nil:
			config := message.Config
			if config.ScannerName != "govulncheck" {
				return Scan{}, fmt.Errorf("%w: scanner %q is not govulncheck", ErrScannerOutput, config.ScannerName)
			}
			sawConfig = true
			scan.Scanner = Scanner{Name: config.ScannerName, Version: config.ScannerVersion, Mode: config.ScanMode, Database: config.DB, DatabaseBuilt: config.DBLastModified}
		case message.SBOM != nil:
			scan.GoVersion = message.SBOM.GoVersion
		case message.Finding != nil && len(message.Finding.Trace) > 0:
			findings.add(message.Finding.OSV, message.Finding.Trace[0])
		}
	}
	if !sawConfig {
		return Scan{}, fmt.Errorf("%w: govulncheck config message missing", ErrScannerOutput)
	}
	if scan.GoVersion == "" {
		return Scan{}, fmt.Errorf("%w: govulncheck did not report the scanned Go version", ErrScannerOutput)
	}
	scan.Findings = findings.list()
	return scan, nil
}

// govulncheckKey identifies one advisory in one package of one module.
// Package is empty for a match that govulncheck reports only at module level.
type govulncheckKey struct {
	advisory, module, pkg string
}

// govulncheckFindings collects govulncheck traces as one finding per
// advisory and package, so a suppression for one package cannot hide another.
type govulncheckFindings map[govulncheckKey]*Finding

// add folds one trace frame into its advisory and package finding, and marks
// it actionable when the frame names a vulnerable symbol.
func (f govulncheckFindings) add(advisory string, frame govulncheckFrame) {
	key := govulncheckKey{advisory: advisory, module: frame.Module, pkg: frame.Package}
	finding, ok := f[key]
	if !ok {
		name := frame.Package
		if name == "" {
			name = frame.Module
		}
		finding = &Finding{Advisory: advisory, Package: name, Version: frame.Version}
		f[key] = finding
	}
	if frame.Function != "" {
		finding.Actionable = true
	}
}

// list returns the findings in a stable order. A module-level match is
// dropped when the same advisory also matched a package of that module,
// because the package findings already report it more precisely.
func (f govulncheckFindings) list() []Finding {
	covered := map[govulncheckKey]bool{}
	for key := range f {
		if key.pkg != "" {
			covered[govulncheckKey{advisory: key.advisory, module: key.module}] = true
		}
	}
	findings := []Finding{}
	for key, finding := range f {
		if key.pkg == "" && covered[key] {
			continue
		}
		findings = append(findings, *finding)
	}
	sortFindings(findings)
	return findings
}

// grypeReport is the subset of grype's JSON report the policy reads.
type grypeReport struct {
	Matches *[]struct {
		Vulnerability struct {
			ID       string `json:"id"`
			Severity string `json:"severity"`
			Fix      struct {
				State string `json:"state"`
			} `json:"fix"`
		} `json:"vulnerability"`
		Artifact struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"artifact"`
	} `json:"matches"`
	Source struct {
		Type string `json:"type"`
	} `json:"source"`
	Descriptor struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		DB      struct {
			Status struct {
				SchemaVersion string `json:"schemaVersion"`
				Built         string `json:"built"`
			} `json:"status"`
		} `json:"db"`
	} `json:"descriptor"`
}

// ParseGrype reads a grype -o json report. A match is actionable when it is
// High or Critical and the package ecosystem has released a fix.
func ParseGrype(r io.Reader) (Scan, error) {
	var report grypeReport
	if err := json.NewDecoder(r).Decode(&report); err != nil {
		return Scan{}, fmt.Errorf("%w: grype report: %v", ErrScannerOutput, err)
	}
	if report.Descriptor.Name != "grype" {
		return Scan{}, fmt.Errorf("%w: scanner %q is not grype", ErrScannerOutput, report.Descriptor.Name)
	}
	if report.Matches == nil {
		return Scan{}, fmt.Errorf("%w: grype report has no match list", ErrScannerOutput)
	}
	scan := Scan{Scanner: Scanner{
		Name:          report.Descriptor.Name,
		Version:       report.Descriptor.Version,
		Mode:          report.Source.Type,
		Database:      report.Descriptor.DB.Status.SchemaVersion,
		DatabaseBuilt: report.Descriptor.DB.Status.Built,
	}}
	seen := map[Finding]bool{}
	for _, match := range *report.Matches {
		vulnerability := match.Vulnerability
		finding := Finding{
			Advisory: vulnerability.ID,
			Package:  match.Artifact.Name,
			Version:  match.Artifact.Version,
			Severity: vulnerability.Severity,
			Actionable: (vulnerability.Severity == "High" || vulnerability.Severity == "Critical") &&
				vulnerability.Fix.State == "fixed",
		}
		if seen[finding] {
			continue
		}
		seen[finding] = true
		scan.Findings = append(scan.Findings, finding)
	}
	return scan, nil
}

// Evaluate applies the suppressions for artifact to a scan. A suppression
// whose review date has passed no longer hides its finding.
func Evaluate(scan Scan, artifact string, suppressions []Suppression, today time.Time) Verdict {
	var verdict Verdict
	used := map[int]bool{}
	for _, finding := range scan.Findings {
		if !finding.Actionable {
			verdict.Informational++
			continue
		}
		index, ok := matchingSuppression(suppressions, artifact, finding)
		if !ok {
			verdict.Actionable = append(verdict.Actionable, finding)
			continue
		}
		used[index] = true
		if suppressions[index].expired(today) {
			verdict.Actionable = append(verdict.Actionable, finding)
			continue
		}
		verdict.Suppressed = append(verdict.Suppressed, finding)
	}
	for index, suppression := range suppressions {
		switch {
		case suppression.Artifact != artifact:
		case used[index] && suppression.expired(today):
			verdict.Expired = append(verdict.Expired, suppression)
		case !used[index]:
			verdict.Unused = append(verdict.Unused, suppression)
		}
	}
	sortFindings(verdict.Actionable)
	sortFindings(verdict.Suppressed)
	return verdict
}

// matchingSuppression returns the suppression for exactly this advisory,
// artifact, and package.
func matchingSuppression(suppressions []Suppression, artifact string, finding Finding) (int, bool) {
	for index, suppression := range suppressions {
		if suppression.Advisory == finding.Advisory && suppression.Artifact == artifact && suppression.Package == finding.Package {
			return index, true
		}
	}
	return 0, false
}

// sortFindings orders findings by advisory and package for stable reports.
func sortFindings(findings []Finding) {
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Advisory != findings[j].Advisory {
			return findings[i].Advisory < findings[j].Advisory
		}
		return findings[i].Package < findings[j].Package
	})
}
