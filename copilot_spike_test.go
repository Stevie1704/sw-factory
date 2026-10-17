package swfactory_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// copilotFixtureDirectory holds the sanitized observations of the pinned
// Copilot CLI that the compatibility record cites.
const copilotFixtureDirectory = "docs/spikes/copilot-cli-1.0.98"

// copilotManifest is the machine-readable index of the Copilot CLI
// compatibility evidence. Later adapter tests read the same fixtures.
type copilotManifest struct {
	CLI struct {
		Package string `json:"package"`
		Version string `json:"version"`
	} `json:"cli"`
	ProbeScript      string              `json:"probe_script"`
	Record           string              `json:"record"`
	CommandTemplates map[string][]string `json:"command_templates"`
	Probes           []copilotProbe      `json:"probes"`
	LiveProbes       []copilotLiveProbe  `json:"live_probes"`
	ReleaseBlockers  []copilotBlocker    `json:"release_blockers"`
}

// copilotProbe is one offline, unauthenticated invocation and the outcome the
// record holds. Fixture names the committed output file, when one is kept.
type copilotProbe struct {
	Name           string   `json:"name"`
	Evidence       string   `json:"evidence"`
	Exit           int      `json:"exit"`
	Fixture        string   `json:"fixture"`
	StdoutEmpty    bool     `json:"stdout_empty"`
	StdoutContains []string `json:"stdout_contains"`
	StdoutExcludes []string `json:"stdout_excludes"`
	StderrContains []string `json:"stderr_contains"`
}

// copilotLiveProbe is a probe that needs a Copilot credential.
type copilotLiveProbe struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Blocker string `json:"blocker"`
}

// copilotBlocker is a named release blocker that the record must list.
type copilotBlocker struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// readCopilotManifest loads the fixture manifest.
func readCopilotManifest(t *testing.T) copilotManifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(copilotFixtureDirectory, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest copilotManifest
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	return manifest
}

// readFixture returns one committed fixture file by its full name.
func readFixture(t *testing.T, file string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(copilotFixtureDirectory, file))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestCopilotFixturesMatchManifest verifies that each committed fixture shows
// the outcome its probe records, so the record cannot claim behavior the
// fixtures do not hold. Probes without a committed fixture keep their recorded
// outcome in the manifest only.
func TestCopilotFixturesMatchManifest(t *testing.T) {
	manifest := readCopilotManifest(t)
	if len(manifest.Probes) == 0 {
		t.Fatal("manifest records no probes")
	}
	for _, probe := range manifest.Probes {
		if probe.Evidence != "observed" {
			t.Errorf("%s: offline probe evidence is %q, want observed", probe.Name, probe.Evidence)
		}
		if probe.Fixture != "" {
			checkProbeFixture(t, probe)
		}
	}
}

// checkProbeFixture compares one committed fixture with the expectations that
// apply to the stream it holds. Expectations on the other stream cannot be
// checked from this file.
func checkProbeFixture(t *testing.T, probe copilotProbe) {
	t.Helper()
	content := readFixture(t, probe.Fixture)
	switch filepath.Ext(probe.Fixture) {
	case ".stdout":
		checkProbeStdout(t, probe, content)
	case ".stderr":
		for _, text := range probe.StderrContains {
			if !strings.Contains(content, text) {
				t.Errorf("%s: stderr lacks %q", probe.Name, text)
			}
		}
	default:
		t.Errorf("%s: fixture %s is neither stdout nor stderr", probe.Name, probe.Fixture)
	}
}

// checkProbeStdout compares a stdout fixture with the stdout expectations.
func checkProbeStdout(t *testing.T, probe copilotProbe, stdout string) {
	t.Helper()
	if probe.StdoutEmpty && stdout != "" {
		t.Errorf("%s: stdout is not empty", probe.Name)
	}
	for _, text := range probe.StdoutContains {
		if !strings.Contains(stdout, text) {
			t.Errorf("%s: stdout lacks %q", probe.Name, text)
		}
	}
	for _, text := range probe.StdoutExcludes {
		if strings.Contains(stdout, text) {
			t.Errorf("%s: stdout contains %q", probe.Name, text)
		}
	}
}

// TestCopilotFixturesAreNamedByManifest verifies that the directory holds only
// manifest.json and the fixture files the manifest names, one probe per file,
// so no orphan output can return without a probe that owns it.
func TestCopilotFixturesAreNamedByManifest(t *testing.T) {
	manifest := readCopilotManifest(t)
	owners := map[string]string{}
	for _, probe := range manifest.Probes {
		if probe.Fixture == "" {
			continue
		}
		if owner, claimed := owners[probe.Fixture]; claimed {
			t.Errorf("fixture %s is named by probes %s and %s", probe.Fixture, owner, probe.Name)
		}
		owners[probe.Fixture] = probe.Name
	}
	entries, err := os.ReadDir(copilotFixtureDirectory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "manifest.json" {
			continue
		}
		if _, named := owners[entry.Name()]; !named {
			t.Errorf("%s is not named by any probe fixture", entry.Name())
		}
		delete(owners, entry.Name())
	}
	for fixture, probe := range owners {
		t.Errorf("probe %s names fixture %s, which is missing", probe, fixture)
	}
}

// TestCopilotPinIsConsistent verifies that the probe script, the manifest,
// the record, and the observed version output name the same CLI release.
func TestCopilotPinIsConsistent(t *testing.T) {
	manifest := readCopilotManifest(t)
	if manifest.CLI.Package != "@github/copilot" {
		t.Errorf("package is %q", manifest.CLI.Package)
	}
	version := manifest.CLI.Version
	if !strings.HasSuffix(copilotFixtureDirectory, "-"+version) {
		t.Errorf("fixture directory %s does not name version %s", copilotFixtureDirectory, version)
	}
	script, err := os.ReadFile(manifest.ProbeScript)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), `COPILOT_VERSION="`+version+`"`) {
		t.Errorf("%s does not pin %s", manifest.ProbeScript, version)
	}
	record, err := os.ReadFile(manifest.Record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(record), version) {
		t.Errorf("%s does not name %s", manifest.Record, version)
	}
	if !probeRecordsStdout(manifest, "version", "GitHub Copilot CLI "+version+".") {
		t.Errorf("version probe does not record %s", version)
	}
}

// probeRecordsStdout reports whether the named probe records the text in its
// stdout expectations.
func probeRecordsStdout(manifest copilotManifest, name, text string) bool {
	for _, probe := range manifest.Probes {
		if probe.Name == name {
			return slices.Contains(probe.StdoutContains, text)
		}
	}
	return false
}

// TestCopilotCommandTemplatesUseObservedFlags verifies that every option in
// the recorded command templates is offered by the pinned CLI's help, and that
// no template grants blanket path or URL permission or selects a session in a
// way that can create a replacement.
func TestCopilotCommandTemplatesUseObservedFlags(t *testing.T) {
	manifest := readCopilotManifest(t)
	help := readFixture(t, "help.stdout")
	for _, required := range []string{"fresh", "resume"} {
		if len(manifest.CommandTemplates[required]) == 0 {
			t.Errorf("missing %s command template", required)
		}
	}
	forbidden := map[string]bool{"--allow-all": true, "--yolo": true, "--allow-all-paths": true, "--allow-all-urls": true, "--continue": true, "--remote": true, "--share": true, "--share-gist": true}
	for name, template := range manifest.CommandTemplates {
		for _, argument := range template {
			if !strings.HasPrefix(argument, "--") {
				continue
			}
			option := strings.SplitN(argument, "=", 2)[0]
			if forbidden[option] {
				t.Errorf("%s template uses %s", name, option)
			}
			if !strings.Contains(help, option+" ") && !strings.Contains(help, option+"\n") {
				t.Errorf("%s template uses %s, which the pinned help does not offer", name, option)
			}
		}
	}
	if strings.Contains(strings.Join(manifest.CommandTemplates["resume"], " "), "--session-id") {
		t.Error("resume template uses --session-id, which creates a session when the ID is missing")
	}
}

// TestCopilotLiveEvidenceStaysUnresolved verifies that every credential-
// requiring probe is recorded as not run and maps to a named release blocker
// that the record lists, so a skipped live probe can never read as a pass.
func TestCopilotLiveEvidenceStaysUnresolved(t *testing.T) {
	manifest := readCopilotManifest(t)
	record, err := os.ReadFile(manifest.Record)
	if err != nil {
		t.Fatal(err)
	}
	blockers := map[string]bool{}
	for _, blocker := range manifest.ReleaseBlockers {
		blockers[blocker.ID] = true
		if !strings.Contains(string(record), blocker.ID) {
			t.Errorf("record does not list release blocker %s", blocker.ID)
		}
	}
	if len(manifest.LiveProbes) == 0 {
		t.Fatal("manifest records no live probes")
	}
	for _, probe := range manifest.LiveProbes {
		if probe.Status != "not-run" {
			t.Errorf("live probe %s has status %q without live evidence", probe.Name, probe.Status)
		}
		if !blockers[probe.Blocker] {
			t.Errorf("live probe %s names unknown blocker %q", probe.Name, probe.Blocker)
		}
	}
}

// copilotSecretPatterns match credential shapes, host-specific values, and
// unsanitized timestamps or generated identities.
var copilotSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b(gho|ghp|ghu|ghs|ghr)_[A-Za-z0-9]{8,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{8,}`),
	regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}`),
	regexp.MustCompile(`/Users/|/home/[a-z]+/\.copilot`),
}

// generatedIdentity matches a UUID; only the fixed probe identities, which
// start with eight zero or 'a' digits, may remain in fixtures.
var generatedIdentity = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// TestCopilotFixturesAreSanitized verifies that no fixture carries a
// credential shape, timestamp, host path, or generated session identity.
func TestCopilotFixturesAreSanitized(t *testing.T) {
	entries, err := os.ReadDir(copilotFixtureDirectory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(copilotFixtureDirectory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, pattern := range copilotSecretPatterns {
			if match := pattern.Find(data); match != nil {
				t.Errorf("%s contains unsanitized %q", entry.Name(), match)
			}
		}
		for _, identity := range generatedIdentity.FindAll(data, -1) {
			if !strings.HasPrefix(string(identity), "00000000-") && !strings.HasPrefix(string(identity), "aaaaaaa1-") {
				t.Errorf("%s contains generated identity %s", entry.Name(), identity)
			}
		}
	}
}

// liveProbeLine matches one recorded live probe call and captures the command
// it runs.
var liveProbeLine = regexp.MustCompile(`(?m)^live_probe\s+\S+\s+(\S+)`)

// TestCopilotLiveProbesRunThroughTheProductionLauncher verifies that every
// live probe, which is the RB-7 suppression evidence, runs on the production
// worker image through factory-copilot with its managed settings and its own
// credential projection, never through the bare CLI.
func TestCopilotLiveProbesRunThroughTheProductionLauncher(t *testing.T) {
	manifest := readCopilotManifest(t)
	script, err := os.ReadFile(manifest.ProbeScript)
	if err != nil {
		t.Fatal(err)
	}
	_, live, found := strings.Cut(string(script), "# Live probes.")
	if !found {
		t.Fatalf("%s has no live probe section", manifest.ProbeScript)
	}
	if !strings.Contains(live, "COPILOT_PROBE_WORKER_IMAGE") {
		t.Errorf("live probes do not run on the production worker image")
	}
	if !strings.Contains(live, "target=/run/factory-auth/copilot-token,readonly") {
		t.Errorf("live probes do not project the credential where factory-copilot reads it")
	}
	if strings.Contains(live, "COPILOT_GITHUB_TOKEN=") {
		t.Errorf("live probes export the token themselves instead of the launcher")
	}
	calls := liveProbeLine.FindAllStringSubmatch(live, -1)
	if len(calls) == 0 {
		t.Fatal("live probe section records no live_probe call")
	}
	launched := 0
	for _, call := range calls {
		switch call[1] {
		case "copilot":
			t.Errorf("live probe %q runs the bare CLI, want factory-copilot", call[0])
		case "factory-copilot":
			launched++
		}
	}
	if launched == 0 {
		t.Error("no live probe runs factory-copilot")
	}
}
