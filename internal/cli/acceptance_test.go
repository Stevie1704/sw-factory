package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/acceptance"
	"github.com/Stevie1704/sw-factory/internal/cli"
)

// scenariosDocument is the part of the acceptance scenarios JSON that an
// operator agent relies on.
type scenariosDocument struct {
	SchemaVersion int `json:"schema_version"`
	Scenarios     []struct {
		ID         string   `json:"id"`
		Objective  string   `json:"objective"`
		Interfaces []string `json:"interfaces"`
		Success    []string `json:"success"`
		Cleanup    []string `json:"cleanup"`
	} `json:"scenarios"`
}

// TestAcceptanceScenariosListsTheVersionedManifestOffline verifies that an
// installed binary prints the scenario manifest without host configuration,
// so a live run from a release needs no source checkout.
func TestAcceptanceScenariosListsTheVersionedManifestOffline(t *testing.T) {
	t.Setenv("FACTORY_CONFIG", "/nonexistent/config.yaml")
	var out, stderr bytes.Buffer
	if code := cli.Run(t.Context(), []string{"acceptance", "scenarios", "--json"}, &out, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var doc scenariosDocument
	decodeSingleDocument(t, out.String(), &doc)
	if doc.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", doc.SchemaVersion)
	}
	ids := map[string]bool{}
	for _, scenario := range doc.Scenarios {
		if scenario.Objective == "" || len(scenario.Interfaces) == 0 || len(scenario.Success) == 0 || len(scenario.Cleanup) == 0 {
			t.Errorf("scenario %q is incomplete: %+v", scenario.ID, scenario)
		}
		ids[scenario.ID] = true
	}
	for _, want := range []string{"onboard-from-release", "onboard-instructions-only", "resume-interrupted-onboarding", "diagnose-failures", "recover-operations", "inspect-history", "upgrade-maintenance", "defect-report", "scoped-authority", "contributor-maintenance"} {
		if !ids[want] {
			t.Errorf("manifest lacks scenario %q", want)
		}
	}
}

// reportDocument is the part of the acceptance report that a reader of the
// release evidence relies on.
type reportDocument struct {
	SchemaVersion int `json:"schema_version"`
	Release       struct {
		Version  string `json:"version"`
		Revision string `json:"revision"`
		Platform string `json:"platform"`
	} `json:"release"`
	HostHarness struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"host_harness"`
	WorkerDigest string `json:"worker_digest"`
	Scenarios    []struct {
		ID            string        `json:"id"`
		Live          reportOutcome `json:"live"`
		Deterministic reportOutcome `json:"deterministic"`
		Gaps          []struct {
			Issue int `json:"issue"`
		} `json:"gaps"`
	} `json:"scenarios"`
	Summary struct {
		Live          map[string]int `json:"live"`
		Deterministic map[string]int `json:"deterministic"`
	} `json:"summary"`
	InterventionsByCause map[string]int `json:"interventions_by_cause"`
	DurationSeconds      int            `json:"duration_seconds"`
}

// reportOutcome is one evidence kind's result for one scenario.
type reportOutcome struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	Usage   *struct {
		Measured bool   `json:"measured"`
		Reason   string `json:"reason"`
	} `json:"usage"`
}

// liveResults is a recorded live run with one live pass, one live block, and
// one deterministic pass.
const liveResults = `{
  "schema_version": 1,
  "host_harness": {"name": "claude", "version": "2.1.0"},
  "worker_digest": "sha256:` + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" + `",
  "results": [
    {"scenario": "inspect-history", "evidence": "live", "outcome": "pass", "duration_seconds": 120,
     "interventions": [{"cause": "authorization", "summary": "operator approved read access to the run"}],
     "usage": {"measured": false, "reason": "the host harness reports no cost"}},
    {"scenario": "upgrade-maintenance", "evidence": "live", "outcome": "blocked", "reason": "no upgrade preflight ships", "duration_seconds": 30,
     "usage": {"measured": true, "input_tokens": 1000, "output_tokens": 200}},
    {"scenario": "onboard-from-release", "evidence": "deterministic", "outcome": "pass"}
  ]
}`

// writeAcceptanceResults writes a results file and returns its path and a
// missing output directory below the test's temporary directory.
func writeAcceptanceResults(t *testing.T, contents string) (string, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "results.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, filepath.Join(root, "report")
}

// TestAcceptanceReportKeepsLiveAndDeterministicEvidenceApart verifies that the
// report names the release and harness, records every manifest scenario, and
// counts an unrecorded scenario as not run rather than passed.
func TestAcceptanceReportKeepsLiveAndDeterministicEvidenceApart(t *testing.T) {
	results, output := writeAcceptanceResults(t, liveResults)
	var out, stderr bytes.Buffer
	if code := cli.Run(t.Context(), []string{"acceptance", "report", "--results", results, "--output", output, "--json"}, &out, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var printed reportDocument
	decodeSingleDocument(t, out.String(), &printed)
	data, err := os.ReadFile(filepath.Join(output, "acceptance-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report reportDocument
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if printed.SchemaVersion != report.SchemaVersion || report.SchemaVersion != 1 {
		t.Fatalf("schema versions: printed %d, file %d", printed.SchemaVersion, report.SchemaVersion)
	}
	if report.Release.Version != "development" || report.Release.Revision == "" || report.Release.Platform != runtime.GOOS+"/"+runtime.GOARCH {
		t.Fatalf("release identity: %+v", report.Release)
	}
	if report.HostHarness.Name != "claude" || report.HostHarness.Version != "2.1.0" || !strings.HasPrefix(report.WorkerDigest, "sha256:") {
		t.Fatalf("observed environment: %+v %q", report.HostHarness, report.WorkerDigest)
	}
	manifest, err := acceptance.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Scenarios) != len(manifest.Scenarios) {
		t.Fatalf("report has %d scenarios, want every manifest scenario", len(report.Scenarios))
	}
	byID := map[string]int{}
	for index, scenario := range report.Scenarios {
		byID[scenario.ID] = index
	}
	inspect := report.Scenarios[byID["inspect-history"]]
	if inspect.Live.Outcome != "pass" || inspect.Deterministic.Outcome != "not-run" || inspect.Live.Usage == nil || inspect.Live.Usage.Measured {
		t.Fatalf("inspect-history: %+v", inspect)
	}
	onboard := report.Scenarios[byID["onboard-from-release"]]
	if onboard.Deterministic.Outcome != "pass" || onboard.Live.Outcome != "not-run" || onboard.Live.Reason == "" {
		t.Fatalf("a deterministic pass must not count as live: %+v", onboard)
	}
	if upgrade := report.Scenarios[byID["upgrade-maintenance"]]; upgrade.Live.Outcome != "blocked" || len(upgrade.Gaps) == 0 {
		t.Fatalf("upgrade-maintenance: %+v", upgrade)
	}
	if report.Summary.Live["pass"] != 1 || report.Summary.Live["blocked"] != 1 || report.Summary.Live["not-run"] != len(manifest.Scenarios)-2 || report.Summary.Deterministic["pass"] != 1 {
		t.Fatalf("summary: %+v", report.Summary)
	}
	if report.InterventionsByCause["authorization"] != 1 || report.DurationSeconds != 150 {
		t.Fatalf("interventions %v, duration %d", report.InterventionsByCause, report.DurationSeconds)
	}
	markdown, err := os.ReadFile(filepath.Join(output, "acceptance-report.md"))
	if err != nil {
		t.Fatal(err)
	}
	for id := range byID {
		if !strings.Contains(string(markdown), id) {
			t.Errorf("Markdown report omits %q", id)
		}
	}
}

// TestAcceptanceReportRefusesUnsupportedClaims verifies that the report
// refuses results that would overstate or leak evidence, and writes nothing
// for them.
func TestAcceptanceReportRefusesUnsupportedClaims(t *testing.T) {
	live := func(result string) string {
		return `{"schema_version": 1, "host_harness": {"name": "codex", "version": "0.50.0"}, "results": [` + result + `]}`
	}
	usage := `"usage": {"measured": false, "reason": "not reported"}`
	for name, contents := range map[string]string{
		"pass with an unsupported workaround": live(`{"scenario": "recover-operations", "evidence": "live", "outcome": "pass", "duration_seconds": 1, ` + usage + `, "workarounds": [{"kind": "operational-store-edit", "summary": "edited the run row"}]}`),
		"unknown scenario":                    live(`{"scenario": "teleport", "evidence": "live", "outcome": "pass", "duration_seconds": 1, ` + usage + `}`),
		"duplicate result":                    live(`{"scenario": "inspect-history", "evidence": "deterministic", "outcome": "pass"}, {"scenario": "inspect-history", "evidence": "deterministic", "outcome": "fail", "reason": "x"}`),
		"unknown outcome":                     live(`{"scenario": "inspect-history", "evidence": "deterministic", "outcome": "skipped"}`),
		"unknown evidence":                    live(`{"scenario": "inspect-history", "evidence": "simulated", "outcome": "pass"}`),
		"failure without a reason":            live(`{"scenario": "inspect-history", "evidence": "deterministic", "outcome": "fail"}`),
		"live result without usage":           live(`{"scenario": "inspect-history", "evidence": "live", "outcome": "pass", "duration_seconds": 1}`),
		"live result without duration":        live(`{"scenario": "inspect-history", "evidence": "live", "outcome": "pass", ` + usage + `}`),
		"measured usage without a value":      live(`{"scenario": "inspect-history", "evidence": "live", "outcome": "pass", "duration_seconds": 1, "usage": {"measured": true}}`),
		"unknown intervention cause":          live(`{"scenario": "inspect-history", "evidence": "live", "outcome": "pass", "duration_seconds": 1, ` + usage + `, "interventions": [{"cause": "luck", "summary": "x"}]}`),
		"multi-line text":                     live(`{"scenario": "inspect-history", "evidence": "deterministic", "outcome": "fail", "reason": "line one\nline two"}`),
		"host path in text":                   live(`{"scenario": "inspect-history", "evidence": "deterministic", "outcome": "fail", "reason": "see /Users/someone/run.log"}`),
		"live result without a host harness":  `{"schema_version": 1, "results": [{"scenario": "inspect-history", "evidence": "live", "outcome": "pass", "duration_seconds": 1, ` + usage + `}]}`,
		"malformed worker digest":             `{"schema_version": 1, "worker_digest": "latest", "results": []}`,
		"unknown field":                       `{"schema_version": 1, "results": [], "transcript": "private"}`,
	} {
		t.Run(name, func(t *testing.T) {
			results, output := writeAcceptanceResults(t, contents)
			var out, stderr bytes.Buffer
			if code := cli.Run(t.Context(), []string{"acceptance", "report", "--results", results, "--output", output, "--json"}, &out, &stderr); code != 1 {
				t.Fatalf("exit %d, want 1; stdout %q stderr %q", code, out.String(), stderr.String())
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("refused report wrote %s: %v", output, err)
			}
		})
	}
}

// TestAcceptanceReportNeverReplacesAnEarlierReport verifies that a second
// report into the same directory fails and keeps the first report intact.
func TestAcceptanceReportNeverReplacesAnEarlierReport(t *testing.T) {
	results, output := writeAcceptanceResults(t, `{"schema_version": 1, "results": []}`)
	args := []string{"acceptance", "report", "--results", results, "--output", output, "--json"}
	var out, stderr bytes.Buffer
	if code := cli.Run(t.Context(), args, &out, &stderr); code != 0 {
		t.Fatalf("first report exit %d: %s", code, stderr.String())
	}
	first, err := os.ReadFile(filepath.Join(output, "acceptance-report.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(output, "acceptance-report.json")); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	if code := cli.Run(t.Context(), args, &out, &stderr); code != 1 {
		t.Fatalf("second report exit %d, want 1", code)
	}
	second, err := os.ReadFile(filepath.Join(output, "acceptance-report.md"))
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("earlier report changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(output, "acceptance-report.json")); !os.IsNotExist(err) {
		t.Fatalf("refused report left a partial JSON file: %v", err)
	}
}

// TestAcceptanceReportRecordsALiveScenarioThatDidNotRun verifies that a
// baseline without a live run records why each live scenario did not run,
// without observing a harness or usage.
func TestAcceptanceReportRecordsALiveScenarioThatDidNotRun(t *testing.T) {
	results, output := writeAcceptanceResults(t, `{"schema_version": 1, "results": [{"scenario": "inspect-history", "evidence": "live", "outcome": "not-run", "reason": "no approved target"}]}`)
	var out, stderr bytes.Buffer
	if code := cli.Run(t.Context(), []string{"acceptance", "report", "--results", results, "--output", output, "--json"}, &out, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var report reportDocument
	decodeSingleDocument(t, out.String(), &report)
	for _, scenario := range report.Scenarios {
		if scenario.ID == "inspect-history" && (scenario.Live.Outcome != "not-run" || scenario.Live.Reason != "no approved target") {
			t.Fatalf("inspect-history live: %+v", scenario.Live)
		}
	}
}
