package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var evaluationTime = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// TestExitStatusSeparatesFindingsFromScannerFailure verifies the release
// contract: findings and an unapproved toolchain fail with 1, while unusable
// scanner output fails with 2 and is never reported as clean.
func TestExitStatusSeparatesFindingsFromScannerFailure(t *testing.T) {
	oldBinary := readFile(t, "../../internal/vulnscan/testdata/govulncheck-go1.25.5.json")
	cleanBinary := `{"config":{"scanner_name":"govulncheck","scanner_version":"v1.8.0","scan_mode":"binary"}}
{"SBOM":{"go_version":"go1.27.1"}}`

	for name, test := range map[string]struct {
		args   []string
		report string
		want   int
	}{
		"clean":              {[]string{"-scanner", "govulncheck", "-expect-go", "go1.27.1"}, cleanBinary, 0},
		"findings":           {[]string{"-scanner", "govulncheck"}, oldBinary, 1},
		"unapproved go":      {[]string{"-scanner", "govulncheck", "-expect-go", "go1.27.1"}, oldBinary, 1},
		"empty output":       {[]string{"-scanner", "govulncheck"}, "", 2},
		"wrong scanner name": {[]string{"-scanner", "grype"}, cleanBinary, 2},
		"unknown scanner":    {[]string{"-scanner", "trivy"}, cleanBinary, 2},
	} {
		t.Run(name, func(t *testing.T) {
			args := append(test.args, "-artifact", "factory", "-identity", "sha256:0")
			var stdout, stderr bytes.Buffer
			if got := run(args, strings.NewReader(test.report), &stdout, &stderr, evaluationTime); got != test.want {
				t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", got, test.want, stdout.String(), stderr.String())
			}
		})
	}
}

// TestMetadataRecordsReproducibleContentFreeIdentity verifies the appended
// metadata names the artifact, toolchain, scanner, database, and advisories.
func TestMetadataRecordsReproducibleContentFreeIdentity(t *testing.T) {
	metadata := filepath.Join(t.TempDir(), "scans.jsonl")
	args := []string{"-scanner", "govulncheck", "-artifact", "factory", "-identity", "sha256:abc", "-metadata", metadata}
	report := readFile(t, "../../internal/vulnscan/testdata/govulncheck-go1.25.5.json")
	for range 2 {
		run(args, strings.NewReader(report), &bytes.Buffer{}, &bytes.Buffer{}, evaluationTime)
	}

	lines := strings.Split(strings.TrimSpace(readFile(t, metadata)), "\n")
	if len(lines) != 2 {
		t.Fatalf("metadata lines = %d, want one per scan", len(lines))
	}
	var record scanRecord
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatal(err)
	}
	if record.Artifact != "factory" || record.Identity != "sha256:abc" || record.GoVersion != "go1.25.5" ||
		record.Scanner.Version != "v1.8.0" || record.Scanner.DatabaseBuilt != "2026-10-01T20:24:15Z" ||
		record.Result != "findings" || len(record.Actionable) != 3 || record.EvaluatedAt != "2026-10-05T12:00:00Z" {
		t.Fatalf("unexpected record: %+v", record)
	}
}

// readFile returns the content of a test input file.
func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
