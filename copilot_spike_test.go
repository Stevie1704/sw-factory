package swfactory_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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
// fixtures must show.
type copilotProbe struct {
	Name           string   `json:"name"`
	Evidence       string   `json:"evidence"`
	Exit           int      `json:"exit"`
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

// readFixture returns one fixture file of a probe.
func readFixture(t *testing.T, name, extension string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(copilotFixtureDirectory, name+"."+extension))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestCopilotFixturesMatchManifest verifies that every offline probe in the
// manifest has captured output whose exit status and text show the recorded
// outcome, so the record cannot claim behavior the fixtures do not hold.
func TestCopilotFixturesMatchManifest(t *testing.T) {
	manifest := readCopilotManifest(t)
	if len(manifest.Probes) == 0 {
		t.Fatal("manifest records no probes")
	}
	for _, probe := range manifest.Probes {
		if probe.Evidence != "observed" {
			t.Errorf("%s: offline probe evidence is %q, want observed", probe.Name, probe.Evidence)
		}
		exit, err := strconv.Atoi(strings.TrimSpace(readFixture(t, probe.Name, "exit")))
		if err != nil || exit != probe.Exit {
			t.Errorf("%s: fixture exit %d (%v), manifest says %d", probe.Name, exit, err, probe.Exit)
		}
		stdout := readFixture(t, probe.Name, "stdout")
		stderr := readFixture(t, probe.Name, "stderr")
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
		for _, text := range probe.StderrContains {
			if !strings.Contains(stderr, text) {
				t.Errorf("%s: stderr lacks %q", probe.Name, text)
			}
		}
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
	if !strings.Contains(readFixture(t, "version", "stdout"), "GitHub Copilot CLI "+version+".") {
		t.Errorf("version fixture does not show %s", version)
	}
}

// TestCopilotCommandTemplatesUseObservedFlags verifies that every option in
// the recorded command templates is offered by the pinned CLI's help, and that
// no template grants blanket path or URL permission or selects a session in a
// way that can create a replacement.
func TestCopilotCommandTemplatesUseObservedFlags(t *testing.T) {
	manifest := readCopilotManifest(t)
	help := readFixture(t, "help", "stdout")
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
