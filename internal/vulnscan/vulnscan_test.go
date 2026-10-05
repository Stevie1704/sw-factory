package vulnscan

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

var reviewDay = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// TestGovulncheckReportsOnlySymbolFindingsAsActionable verifies that the
// three advisories found in a go1.25.5 coordinator are actionable while an
// advisory present only at module level stays informational.
func TestGovulncheckReportsOnlySymbolFindingsAsActionable(t *testing.T) {
	scan := parseFixture(t, "testdata/govulncheck-go1.25.5.json", ParseGovulncheck)

	if scan.Scanner != (Scanner{Name: "govulncheck", Version: "v1.8.0", Mode: "binary", Database: "https://vuln.go.dev", DatabaseBuilt: "2026-10-01T20:24:15Z"}) {
		t.Fatalf("unexpected scanner identity: %+v", scan.Scanner)
	}
	if scan.GoVersion != "go1.25.5" {
		t.Fatalf("go version = %q, want go1.25.5", scan.GoVersion)
	}

	verdict := Evaluate(scan, "factory", nil, reviewDay)
	if got := advisories(verdict.Actionable); got != "GO-2026-4341 net/url,GO-2026-4601 net/url,GO-2026-4602 os" {
		t.Fatalf("actionable = %s", got)
	}
	if verdict.Informational != 1 {
		t.Fatalf("informational = %d, want 1", verdict.Informational)
	}
}

// TestGovulncheckWithoutConfigIsAScannerFailure verifies that empty or
// foreign output can never be read as a clean scan.
func TestGovulncheckWithoutConfigIsAScannerFailure(t *testing.T) {
	for name, input := range map[string]string{
		"empty":   "",
		"foreign": `{"matches": []}`,
		"broken":  `{"config": {"scanner_name": "govulncheck"`,
		"no sbom": `{"config": {"scanner_name": "govulncheck", "scanner_version": "v1.8.0"}}`,
	} {
		if _, err := ParseGovulncheck(strings.NewReader(input)); !errors.Is(err, ErrScannerOutput) {
			t.Errorf("%s: err = %v, want ErrScannerOutput", name, err)
		}
	}
}

// TestGrypeReportsFixableHighSeverityFindingsAsActionable verifies the image
// policy threshold and that one advisory in one package is reported once.
func TestGrypeReportsFixableHighSeverityFindingsAsActionable(t *testing.T) {
	scan := parseFixture(t, "testdata/grype-worker.json", ParseGrype)

	if scan.Scanner != (Scanner{Name: "grype", Version: "0.120.0", Mode: "image", Database: "v6.1.10", DatabaseBuilt: "2026-10-05T06:45:38Z"}) {
		t.Fatalf("unexpected scanner identity: %+v", scan.Scanner)
	}

	verdict := Evaluate(scan, "worker-image", nil, reviewDay)
	if got := advisories(verdict.Actionable); got != "CVE-2026-63076 openssl,GHSA-23hp-3jrh-7fpw tar" {
		t.Fatalf("actionable = %s", got)
	}
	if verdict.Informational != 3 {
		t.Fatalf("informational = %d, want 3", verdict.Informational)
	}
}

// TestGrypeWithoutDescriptorIsAScannerFailure verifies that output without
// grype's identity is a scanner failure rather than an empty match list.
func TestGrypeWithoutDescriptorIsAScannerFailure(t *testing.T) {
	for name, input := range map[string]string{
		"empty":      "",
		"no matches": `{"descriptor": {"name": "grype", "version": "0.120.0"}}`,
		"foreign":    `{"matches": [], "descriptor": {"name": "trivy"}}`,
	} {
		if _, err := ParseGrype(strings.NewReader(input)); !errors.Is(err, ErrScannerOutput) {
			t.Errorf("%s: err = %v, want ErrScannerOutput", name, err)
		}
	}
}

// TestSuppressionAppliesOnlyToItsExactAdvisoryArtifactAndPackage verifies that
// a suppression cannot spread to another artifact or package.
func TestSuppressionAppliesOnlyToItsExactAdvisoryArtifactAndPackage(t *testing.T) {
	scan := parseFixture(t, "testdata/govulncheck-go1.25.5.json", ParseGovulncheck)
	suppressions := []Suppression{
		validSuppression("GO-2026-4602", "factory", "os"),
		validSuppression("GO-2026-4601", "factory-report", "net/url"),
		validSuppression("GO-2026-4341", "factory", "net/http"),
	}

	verdict := Evaluate(scan, "factory", suppressions, reviewDay)
	if got := advisories(verdict.Suppressed); got != "GO-2026-4602 os" {
		t.Fatalf("suppressed = %s", got)
	}
	if got := advisories(verdict.Actionable); got != "GO-2026-4341 net/url,GO-2026-4601 net/url" {
		t.Fatalf("actionable = %s", got)
	}
	if len(verdict.Unused) != 1 || verdict.Unused[0].Package != "net/http" {
		t.Fatalf("unused = %+v, want only the net/http suppression for this artifact", verdict.Unused)
	}
}

// TestExpiredSuppressionNoLongerHidesTheFinding verifies that a suppression
// past its review date returns the finding to the actionable set.
func TestExpiredSuppressionNoLongerHidesTheFinding(t *testing.T) {
	scan := parseFixture(t, "testdata/grype-worker.json", ParseGrype)
	expired := validSuppression("GHSA-23hp-3jrh-7fpw", "worker-image", "tar")
	expired.ReviewBy = "2026-10-04"

	verdict := Evaluate(scan, "worker-image", []Suppression{expired}, reviewDay)
	if got := advisories(verdict.Actionable); got != "CVE-2026-63076 openssl,GHSA-23hp-3jrh-7fpw tar" {
		t.Fatalf("actionable = %s", got)
	}
	if len(verdict.Expired) != 1 {
		t.Fatalf("expired = %+v, want one expired suppression", verdict.Expired)
	}
}

// TestSuppressionsMustBeNarrowAndReviewable verifies the policy file rejects
// incomplete, malformed, and wildcard entries.
func TestSuppressionsMustBeNarrowAndReviewable(t *testing.T) {
	valid := `schema_version: 1
suppressions:
  - advisory: GHSA-23hp-3jrh-7fpw
    artifact: worker-image
    package: tar
    reason: npm's bundled tar is not used by any worker command.
    owner: Stevie1704
    review_by: 2026-11-01
`
	loaded, err := LoadSuppressions(strings.NewReader(valid))
	if err != nil {
		t.Fatalf("valid suppressions: %v", err)
	}
	if len(loaded) != 1 || loaded[0] != (Suppression{Advisory: "GHSA-23hp-3jrh-7fpw", Artifact: "worker-image", Package: "tar", Reason: "npm's bundled tar is not used by any worker command.", Owner: "Stevie1704", ReviewBy: "2026-11-01"}) {
		t.Fatalf("loaded = %+v", loaded)
	}

	for name, input := range map[string]string{
		"missing owner":  strings.Replace(valid, "    owner: Stevie1704\n", "", 1),
		"missing reason": strings.Replace(valid, "    reason: npm's bundled tar is not used by any worker command.\n", "", 1),
		"bad date":       strings.Replace(valid, "2026-11-01", "soon", 1),
		"wildcard":       strings.Replace(valid, "package: tar", "package: \"*\"", 1),
		"ecosystem":      strings.Replace(valid, "advisory: GHSA-23hp-3jrh-7fpw", "advisory: npm", 1),
		"unknown field":  valid + "    scope: all\n",
		"wrong schema":   strings.Replace(valid, "schema_version: 1", "schema_version: 2", 1),
	} {
		if _, err := LoadSuppressions(strings.NewReader(input)); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func parseFixture(t *testing.T, path string, parse func(io.Reader) (Scan, error)) Scan {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scan, err := parse(file)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return scan
}

func validSuppression(advisory, artifact, pkg string) Suppression {
	return Suppression{Advisory: advisory, Artifact: artifact, Package: pkg, Reason: "reviewed", Owner: "Stevie1704", ReviewBy: "2026-12-31"}
}

func advisories(findings []Finding) string {
	keys := make([]string, 0, len(findings))
	for _, finding := range findings {
		keys = append(keys, finding.Advisory+" "+finding.Package)
	}
	return strings.Join(keys, ",")
}
